package main

// hwform.go —— 判断「这台机器是台式机还是笔记本」（2026-09-24）
//
// 用户要求：页面一上来先说清这是台式机还是笔记本，并用图形做背景。
// 所以这里要给出一个**能画图的类别**（desktop / laptop / tablet / other），不是一句文字。
//
// 关键约束：这是要跑在任意一台用户机器上的，不能只认我这台。
// SMBIOS 的机箱类型在很多主板上是 1（其他）/2（未知）——尤其是台式机自己攒的、OEM 白牌板。
// 所以必须有多条降级证据，一条判不出就换下一条：

import (
	"strings"
	"syscall"
	"unsafe"
)

// hwForm 机型判断结果
type hwForm struct {
	Factor string `json:"factor"` // desktop / laptop / tablet / other —— 前端据此画图形
	Name   string `json:"name"`   // 台式机 / 笔记本 / 平板 / 电脑（页面大字）
	Reason string `json:"reason"` // 判出来的依据，人话。读不到依据也要说清楚是「没判出来」
}

/* readFormFactor 机型判断链
     ① SMBIOS 机箱类型（明确时最权威）
     ② 有没有电池（最硬的便携证据：台式机没电池）
     ③ 内存是不是 SODIMM（笔记本条）
     ④ 都判不出 → other，页面画通用电脑图形 + 写「没判出来」
   顺序说明：② 放在 ① 之后，是因为一体机/迷你主机这类「长得像台式但确实没电池」的机器
   应该听机箱类型的；只有在机箱类型没说人话（未知/其他/压根没读到）时才靠电池下结论。 */
func readFormFactor(chassisType byte, chassisName string, sticks []hwMemStick) hwForm {
	if g := chassisGroup[chassisType]; g != "" {
		return formOf(g, "机箱类型写着「"+chassisName+"」")
	}

	// 机箱类型没说人话 —— 靠电池和内存形态推断
	if hasBattery() {
		return formOf("laptop", "查到机器带电池（机箱类型没写清楚，按电池判的）")
	}
	if soDimm(sticks) {
		return formOf("laptop", "内存是笔记本条（SODIMM），机箱类型没写清楚")
	}
	if chassisName != "" && chassisName != "其他" && chassisName != "未知" {
		// 有名字但没归类：至少别把「一体机」这种画成别的
		return hwForm{Factor: "desktop", Name: chassisName, Reason: "机箱类型写着「" + chassisName + "」"}
	}
	return hwForm{Factor: "other", Name: "电脑", Reason: "机箱类型没写清楚，也没查到电池 —— 判不出是台式还是笔记本"}
}

func formOf(factor, reason string) hwForm {
	switch factor {
	case "desktop":
		return hwForm{Factor: "desktop", Name: "台式机", Reason: reason}
	case "laptop":
		return hwForm{Factor: "laptop", Name: "笔记本", Reason: reason}
	case "tablet":
		return hwForm{Factor: "tablet", Name: "平板", Reason: reason}
	}
	return hwForm{Factor: "other", Name: "电脑", Reason: reason}
}

// soDimm 有没有笔记本内存条（SMBIOS 类型 17 的形态字段：SODIMM / DIMM / ...）
func soDimm(sticks []hwMemStick) bool {
	for _, m := range sticks {
		f := strings.ToUpper(strings.TrimSpace(m.Form))
		if strings.Contains(f, "SODIMM") || strings.Contains(f, "SO-DIMM") {
			return true
		}
	}
	return false
}

type sysPowerStatus struct {
	ACLineStatus   byte
	BatteryFlag    byte
	BatteryLifePct byte
	SystemStatus   byte
	LifeTime       uint32
	FullLifeTime   uint32
}

/* hasBattery 这台机器带不带电池。
   两条路都试：GetSystemPowerStatus 最快（微秒级），Win32_Battery 是补充证据。
   BatteryFlag 的 128 = 没有电池；255 = 状态未知 —— 未知不能当成「有电池」，那会把台式机误判成笔记本。 */
func hasBattery() bool {
	k := syscall.NewLazyDLL("kernel32.dll").NewProc("GetSystemPowerStatus")
	var s sysPowerStatus
	r, _, _ := k.Call(uintptr(unsafe.Pointer(&s)))
	if r != 0 && s.BatteryFlag != 128 && s.BatteryFlag != 255 && s.BatteryFlag != 0 {
		return true
	}
	if rows, err := wmiQuery(`root\cimv2`, "SELECT Name FROM Win32_Battery", []string{"Name"}); err == nil && len(rows) > 0 {
		return true
	}
	if rows, err := wmiQuery(`root\cimv2`, "SELECT Name FROM Win32_PortableBattery", []string{"Name"}); err == nil && len(rows) > 0 {
		return true
	}
	return false
}
