package main

// ============================================================================
// 3z9wOpt v1.1 · 系统优化（自定义优化）后端
//
// 血统：优化项数据来自 ZyperWin++ v4.1（151 项，见 optimize_data.go）；
//       执行语义与 v1.0 的 optimize.go 对齐，逐条核对过：
//         · 检测：读 checkKey 下 checkValue 与 Optimized 比字符串；
//           **值不存在时，若期望值 Optimized 为空串（=「删掉该值才是优化态」）则视为已优化**
//         · 动作只有 4 类 + 外壳刷新：regwrite / regdelete / service / cmd / shellnotify
//         · 服务启动类型走 SCM 直调（ChangeServiceConfigW），不走 sc.exe
//         · cmd 型动作支持 Skiperr（可忽略的退出码，逗号分隔）
//         · 注册表 REG_BINARY 的 Data 有「纯 hex」与「逗号分隔 hex」两种写法，都要吃
//
// 红线（用户 2026-09-19 定，照搬不擅改）：
//   · 基本优化 87 项 / 深度优化 127 项名单照源项目原样搬，不擅自删减
//   · 安全组（关防火墙 / 关 UAC / 关 SmartScreen 等）源软件也不默认勾，保持一致
//   · 改名单前必须先问用户
//
// 与 v1.0 的差别（v1.1 取舍，写在这里免得以后有人以为是漏了）：
//   · 待重启提醒（optimize_reboot.go / GetPendingReboot）与体感快照（GetSysSnapshot）已随真数据一并移植
//   · 系统优化页的界面是 1.1 自己的（分类手风琴 + 状态徽标 + 预设按钮），不是 v1.0 那套
// ============================================================================

import (
	"context"
	"encoding/hex"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows/registry"
)

// ---------- 返回前端的数据结构 ----------

type OptItemStatus struct {
	ID        string `json:"id"`
	Cat       string `json:"cat"`
	CatName   string `json:"catName"`
	Name      string `json:"name"`
	Desc      string `json:"desc"` // 一句人话说明「这项会改什么」，由动作自动生成（见 optDesc）
	Basic     bool   `json:"basic"`
	Deep      bool   `json:"deep"`
	Optimized bool   `json:"optimized"`
	Reboot    bool   `json:"reboot"` // 改了要重启电脑才完全生效（服务启动类型 / 内核与会话参数 / UAC 总开关等）
}

type OptResult struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	OK   bool   `json:"ok"`
	Err  string `json:"err"`
}

// ---------- 注册表路径解析 ----------

// regRoot 把 "HKEY_CURRENT_USER\SOFTWARE\..." 拆成根键 + 子键
func regRoot(full string) (registry.Key, string, error) {
	i := strings.Index(full, `\`)
	if i < 0 {
		return 0, "", fmt.Errorf("非法注册表路径: %s", full)
	}
	root := strings.ToUpper(full[:i])
	sub := full[i+1:]
	switch root {
	case "HKEY_CURRENT_USER":
		return registry.CURRENT_USER, sub, nil
	case "HKEY_LOCAL_MACHINE":
		return registry.LOCAL_MACHINE, sub, nil
	case "HKEY_CLASSES_ROOT":
		return registry.CLASSES_ROOT, sub, nil
	case "HKEY_USERS":
		return registry.USERS, sub, nil
	}
	return 0, "", fmt.Errorf("不支持的根键: %s", root)
}

// normOpt 归一化比较串：去空格、小写
func normOpt(s string) string {
	return strings.ToLower(strings.ReplaceAll(strings.TrimSpace(s), " ", ""))
}

// binHex 把二进制数据转成两种可比格式：无分隔 hex / 逗号分隔 hex（源 XML 两种都有）
func binHex(b []byte) (string, string) {
	h := make([]string, len(b))
	for i, x := range b {
		h[i] = fmt.Sprintf("%02x", x)
	}
	return strings.Join(h, ""), strings.Join(h, ",")
}

// readCheckValue 读取检测值并归一化成字符串（dword→十进制，sz→原文，binary→两种 hex 拼起来）
func readCheckValue(it *OptItem) (string, error) {
	root, sub, err := regRoot(it.CheckKey)
	if err != nil {
		return "", err
	}
	k, err := registry.OpenKey(root, sub, registry.QUERY_VALUE)
	if err != nil {
		return "", err
	}
	defer k.Close()
	// 顺序与源软件一致：先整数（DWORD/QWORD），再字符串，最后二进制
	if v, _, err := k.GetIntegerValue(it.CheckValue); err == nil {
		return strconv.FormatUint(v, 10), nil
	}
	if v, _, err := k.GetStringValue(it.CheckValue); err == nil {
		return v, nil
	}
	if b, _, err := k.GetBinaryValue(it.CheckValue); err == nil {
		h1, h2 := binHex(b)
		return h1 + "|" + h2, nil
	}
	return "", registry.ErrNotExist
}

// isOptimized 检测单项是否已优化。
// 关键边界：**值不存在时**，若期望值 Optimized 为空串（该项的"优化态"就是把这个值删掉），
// 则视为已优化；否则视为未优化。（与源 C# 逻辑一致，v1.0 已有单测锁住这条）
func isOptimized(it *OptItem) bool {
	got, err := readCheckValue(it)
	if err != nil {
		return it.Optimized == ""
	}
	want := normOpt(it.Optimized)
	for _, part := range strings.Split(got, "|") {
		if normOpt(part) == want {
			return true
		}
	}
	return false
}

// ---------- 界面文案：项名与副标题（2026-10-11 新增） ----------
//
// 源数据里的项名带排版序号（「1、隐藏任务栏搜索框」），换到我们界面里既没用又占地方；
// 而光有名字也看不出"到底改成什么样"，所以每条再配一句副标题。
// 副标题不手写 154 遍：直接从该项的 Optimize 动作里推 —— 改了注册表哪个值、服务启动类型变成什么。
// 推不出来（动作类型不认识）就返回空串，界面自然不显示副标题，**不编一句"优化系统设置"糊弄**。

// optItemName 去掉「数字+、」形式的序号前缀。
// 只认顿号这一种写法：名字里带小数点的（如「2.4G 频段」）不会被误伤。
func optItemName(raw string) string {
	s := strings.TrimSpace(raw)
	i := 0
	for i < len(s) && s[i] >= '0' && s[i] <= '9' {
		i++
	}
	if i > 0 && i < len(s) {
		if rs := []rune(s[i:]); len(rs) > 0 && rs[0] == '、' {
			if cut := strings.TrimSpace(string(rs[1:])); cut != "" {
				return cut
			}
		}
	}
	return s
}

// shortCmd 把命令/数据压成一行并截断（长的会把副标题那行撑爆）。
func shortCmd(s string) string {
	out := strings.Join(strings.Fields(s), " ") // Fields 顺带把换行 / 多空格压成单空格
	if rs := []rune(out); len(rs) > 46 {
		return string(rs[:46]) + "…"
	}
	return out
}

// lastRegSeg 取注册表路径最后一段，作为 Value 为空时的兜底显示名。
func lastRegSeg(k string) string {
	if i := strings.LastIndex(k, `\`); i >= 0 && i+1 < len(k) {
		return k[i+1:]
	}
	return k
}

// optDesc 生成「这项会改什么」的一句副标题。
func optDesc(it *OptItem) string {
	if len(it.Optimize) == 0 {
		return ""
	}
	a := it.Optimize[0]
	var s string
	switch a.Kind {
	case "regwrite":
		name := a.Value
		if name == "" {
			name = lastRegSeg(a.Key)
		}
		s = "注册表 " + name + " → " + shortCmd(a.Data)
	case "regdelete":
		name := a.Value
		if name == "" {
			name = lastRegSeg(a.Key)
		}
		s = "删除注册表值 " + name
	case "service":
		s = "服务 " + a.Name + " 启动类型 → " + a.Start
	case "cmd":
		s = "执行 " + shortCmd(a.Cmd)
	case "shellnotify":
		s = "刷新资源管理器"
	default:
		return ""
	}
	if n := len(it.Optimize); n > 1 {
		s += fmt.Sprintf("（共 %d 处改动）", n)
	}
	return s
}

// GetOptimizeCatalog 返回全部优化项 + 每项实时状态（每项一次注册表读，毫秒级）
func (a *App) GetOptimizeCatalog() []OptItemStatus {
	out := make([]OptItemStatus, 0, len(optCatalog))
	for i := range optCatalog {
		it := &optCatalog[i]
		out = append(out, OptItemStatus{
			ID: it.ID, Cat: it.Cat, CatName: it.CatName,
			Name: optItemName(it.Name), Desc: optDesc(it),
			Basic: it.Basic, Deep: it.Deep, Optimized: isOptimized(it),
			Reboot: itemNeedsReboot(it),
		})
	}
	return out
}

// ---------- 动作执行器 ----------

// decodeRegData 按类型解码 regwrite 的 Data 串
func decodeRegData(vtype, data string) (dword uint64, str string, bin []byte, err error) {
	switch strings.ToUpper(vtype) {
	case "REG_DWORD":
		s := strings.TrimSpace(data)
		base := 10
		if strings.HasPrefix(strings.ToLower(s), "0x") {
			base = 0
		}
		v, perr := strconv.ParseUint(s, base, 32)
		if perr != nil {
			return 0, "", nil, fmt.Errorf("REG_DWORD 数据无法解析: %q", data)
		}
		return v, "", nil, nil
	case "REG_SZ", "REG_EXPAND_SZ":
		return 0, data, nil, nil
	case "REG_BINARY":
		s := normOpt(data)
		if strings.Contains(s, ",") { // 逗号分隔 hex，如 22,22,22,00
			parts := strings.Split(s, ",")
			bin = make([]byte, 0, len(parts))
			for _, p := range parts {
				b, perr := strconv.ParseUint(strings.TrimSpace(p), 16, 8)
				if perr != nil {
					return 0, "", nil, fmt.Errorf("REG_BINARY 数据无法解析: %q", data)
				}
				bin = append(bin, byte(b))
			}
			return 0, "", bin, nil
		}
		b, perr := hex.DecodeString(s)
		if perr != nil {
			return 0, "", nil, fmt.Errorf("REG_BINARY 数据无法解析: %q", data)
		}
		return 0, "", b, nil
	}
	return 0, "", nil, fmt.Errorf("不支持的类型: %s", vtype)
}

func execRegWrite(a OptAction) error {
	root, sub, err := regRoot(a.Key)
	if err != nil {
		return err
	}
	k, _, err := registry.CreateKey(root, sub, registry.SET_VALUE)
	if err != nil {
		return err
	}
	defer k.Close()
	dw, str, bin, err := decodeRegData(a.Vtype, a.Data)
	if err != nil {
		return err
	}
	switch strings.ToUpper(a.Vtype) {
	case "REG_DWORD":
		return k.SetDWordValue(a.Value, uint32(dw))
	case "REG_SZ", "REG_EXPAND_SZ":
		return k.SetStringValue(a.Value, str)
	case "REG_BINARY":
		return k.SetBinaryValue(a.Value, bin)
	}
	return fmt.Errorf("不支持的类型: %s", a.Vtype)
}

func execRegDelete(a OptAction) error {
	root, sub, err := regRoot(a.Key)
	if err != nil {
		return err
	}
	if a.Value != "" {
		k, err := registry.OpenKey(root, sub, registry.SET_VALUE)
		if err != nil {
			if err == registry.ErrNotExist {
				return nil // 键本来就不存在 = 已经是目标态，不算失败
			}
			return err
		}
		defer k.Close()
		if err := k.DeleteValue(a.Value); err != nil && err != registry.ErrNotExist {
			return err
		}
		return nil
	}
	// Value 为空 = 删整个键：在父键上 DeleteKey(叶子名)
	i := strings.LastIndex(sub, `\`)
	if i < 0 {
		return fmt.Errorf("无法定位父键: %s", a.Key)
	}
	parent, leaf := sub[:i], sub[i+1:]
	k, err := registry.OpenKey(root, parent, registry.SET_VALUE|registry.ENUMERATE_SUB_KEYS)
	if err != nil {
		if err == registry.ErrNotExist {
			return nil
		}
		return err
	}
	defer k.Close()
	if err := registry.DeleteKey(k, leaf); err != nil && err != registry.ErrNotExist {
		return err
	}
	return nil
}

// ---------- 服务启动类型（SCM 直调，与清理模块同款：不依赖 sc.exe） ----------

const svcChangeConfig = 0x0002

var (
	procChangeSvcCfg  = advapi32.NewProc("ChangeServiceConfigW")
	procChangeSvcCfg2 = advapi32.NewProc("ChangeServiceConfig2W")
)

// setServiceStart 改服务启动类型：2 自动 / 3 手动 / 4 禁用 / 5 自动（延迟启动，源 XML 里出现过）
func setServiceStart(name string, start uint32) error {
	s, ok := openServiceHandle(name, svcChangeConfig|svcQueryStatus)
	if !ok {
		return fmt.Errorf("无法打开服务 %s（不存在或权限不足）", name)
	}
	defer procCloseSvcH.Call(s)
	realStart := start
	delayed := false
	if start == 5 {
		realStart = 2
		delayed = true
	}
	const noChange = 0xFFFFFFFF
	r, _, e := procChangeSvcCfg.Call(s, noChange, uintptr(realStart), noChange, 0, 0, 0, 0, 0, 0, 0)
	if r == 0 {
		return fmt.Errorf("修改服务 %s 启动类型失败: %v", name, e)
	}
	if delayed {
		var b uint32 = 1 // SERVICE_CONFIG_DELAYED_AUTO_START_INFO = 3
		procChangeSvcCfg2.Call(s, 3, uintptr(unsafe.Pointer(&b)))
	}
	return nil
}

// shellNotify 通知外壳刷新（SHChangeNotify SHCNE_ASSOCCHANGED）
func shellNotify() {
	sh := syscall.NewLazyDLL("shell32.dll").NewProc("SHChangeNotify")
	if err := sh.Find(); err != nil {
		return
	}
	sh.Call(0x08000000 /*SHCNE_ASSOCCHANGED*/, 0x1000 /*SHCNF_FLUSH*/, 0, 0)
}

// execOptAction 执行单条动作；ctx 取消时 cmd 型子进程会被杀掉
func execOptAction(ctx context.Context, a OptAction) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	switch a.Kind {
	case "regwrite":
		return execRegWrite(a)
	case "regdelete":
		return execRegDelete(a)
	case "service":
		st, err := strconv.ParseUint(strings.TrimSpace(a.Start), 10, 32)
		if err != nil {
			return fmt.Errorf("服务启动类型无法解析: %q", a.Start)
		}
		return setServiceStart(a.Name, uint32(st))
	case "cmd":
		// 2026-10-11：cmd.exe 走 sysExe 取 System32 绝对路径，不裸写 "cmd"。
		// 理由同 toolkit.go/win_exec.go：本进程是 requireAdministrator 提权进程，
		// 裸命令名走 PATH 查找，一旦被劫持等于把管理员权限交给别人；
		// 精简版系统 PATH 被裁剪时也会直接 exec: not found（sysExe 找不到时才退回裸名）。
		err := runHiddenCtx(ctx, 2*time.Minute, sysExe("cmd.exe", ""), "/c", a.Cmd)
		if err != nil && a.Skiperr != "" {
			if ee, ok := err.(*exec.ExitError); ok {
				for _, code := range strings.Split(a.Skiperr, ",") {
					if n, perr := strconv.Atoi(strings.TrimSpace(code)); perr == nil && ee.ExitCode() == n {
						return nil // 源 XML 标了"这个退出码可以忽略"
					}
				}
			}
		}
		return err
	case "shellnotify":
		shellNotify()
		return nil
	}
	return fmt.Errorf("未知动作: %s", a.Kind)
}

// ---------- 应用 / 还原 / 停止 ----------

// 优化停止通道（与清理停止同款模式，包级单实例）
var (
	optMu     sync.Mutex
	optCancel context.CancelFunc
)

// StopOptimize 停止正在进行的优化/还原：当前项就地中断，后续项不再开始
func (a *App) StopOptimize() {
	optMu.Lock()
	defer optMu.Unlock()
	if optCancel != nil {
		cleanTrace("StopOptimize: 用户请求停止优化")
		optCancel()
	}
}

// runOptimize 是 ApplyOptimize / RestoreOptimize 的公共骨架：逐项执行动作块，进度事件 opt:progress
func (a *App) runOptimize(ids []string, restore bool) []OptResult {
	known := map[string]*OptItem{}
	for i := range optCatalog {
		known[optCatalog[i].ID] = &optCatalog[i]
	}
	ctx, cancel := context.WithCancel(context.Background())
	optMu.Lock()
	optCancel = cancel
	optMu.Unlock()
	defer func() {
		optMu.Lock()
		optCancel = nil
		optMu.Unlock()
		cancel()
	}()

	results := []OptResult{} // 必须初始化成空切片：空结果序列化成 null 会把前端打崩
	total := len(ids)
	done := 0
	for _, id := range ids {
		if ctx.Err() != nil {
			break
		}
		it, ok := known[id]
		if !ok {
			continue // 白名单之外的 ID 直接拒绝
		}
		a.emit("opt:progress", done, total, it.Name, it.ID)
		acts := it.Optimize
		if restore {
			acts = it.Restore
		}
		res := OptResult{ID: it.ID, Name: it.Name, OK: true}
		if len(acts) == 0 {
			res.OK, res.Err = false, "该项没有定义动作"
		}
		for _, act := range acts {
			if ctx.Err() != nil {
				res.OK, res.Err = false, "已停止"
				break
			}
			if err := execOptAction(ctx, act); err != nil {
				res.OK, res.Err = false, err.Error()
				break // 一条失败就停该项：后面的动作多半依赖前面的结果
			}
		}
		results = append(results, res)
		done++
		cleanTrace(fmt.Sprintf("  opt %s ok=%v err=%q", res.ID, res.OK, res.Err))
		a.emit("opt:progress", done, total, "", "")
	}
	return results
}

// ApplyOptimize 按 ID 应用优化项（前端只能传目录里已有的 ID）
func (a *App) ApplyOptimize(ids []string) []OptResult { return a.runOptimize(ids, false) }

// RestoreOptimize 按 ID 还原优化项（每项都有自己的还原动作，逐项可撤销）
func (a *App) RestoreOptimize(ids []string) []OptResult { return a.runOptimize(ids, true) }

// ---------- 体感快照（优化前后各拍一次，让"更流畅"可量化） ----------

// SysSnapshot 体感快照
type SysSnapshot struct {
	FreeGB    float64 `json:"freeGB"`  // 可用物理内存
	TotalGB   float64 `json:"totalGB"` // 物理内存总量
	ProcN     int     `json:"procN"`   // 运行中进程数
	AutoN     int     `json:"autoN"`   // 开机自启动项数（HKCU Run + HKLM Run）
	Optimized int     `json:"optimized"`
	Ts        string  `json:"ts"`
}

var psapiDLL = syscall.NewLazyDLL("psapi.dll")

// memStatusEx 对应 Win32 MEMORYSTATUSEX（64 字节，7 个 uint64 一个都不能少）
type memStatusEx struct {
	length, memoryLoad           uint32
	totalPhys, availPhys         uint64
	totalPageFile, availPageFile uint64
	totalVirtual, availVirtual   uint64
	availExtendedVirtual         uint64
}

// runKeyCount 数一个 Run 键下的自启动项数量
func runKeyCount(root registry.Key, path string) int {
	k, err := registry.OpenKey(root, path, registry.QUERY_VALUE)
	if err != nil {
		return 0
	}
	defer k.Close()
	names, err := k.ReadValueNames(-1)
	if err != nil {
		return 0
	}
	return len(names)
}

// GetSysSnapshot 拍一张当前体感快照（内存 / 进程数 / 自启动项 / 已优化项数），毫秒级
func (a *App) GetSysSnapshot() SysSnapshot {
	snap := SysSnapshot{Ts: time.Now().Format("2006-01-02 15:04:05")}

	// 物理内存：GlobalMemoryStatusEx
	var ms memStatusEx
	ms.length = uint32(unsafe.Sizeof(ms))
	if p := k32.NewProc("GlobalMemoryStatusEx"); p.Find() == nil {
		if r, _, _ := p.Call(uintptr(unsafe.Pointer(&ms))); r != 0 {
			snap.TotalGB = float64(ms.totalPhys) / 1024 / 1024 / 1024
			snap.FreeGB = float64(ms.availPhys) / 1024 / 1024 / 1024
		}
	}

	// 进程数：EnumProcesses（psapi）
	if p := psapiDLL.NewProc("EnumProcesses"); p.Find() == nil {
		var ids [4096]uint32
		var ret uint32
		if r, _, _ := p.Call(uintptr(unsafe.Pointer(&ids[0])), unsafe.Sizeof(ids), uintptr(unsafe.Pointer(&ret))); r != 0 && ret > 0 {
			snap.ProcN = int(ret / 4)
		}
	}

	// 自启动项：HKCU + HKLM 的 Run 键
	snap.AutoN = runKeyCount(registry.CURRENT_USER, `SOFTWARE\Microsoft\Windows\CurrentVersion\Run`) +
		runKeyCount(registry.LOCAL_MACHINE, `SOFTWARE\Microsoft\Windows\CurrentVersion\Run`)

	// 当前已优化项数（每项一次注册表读，毫秒级）
	n := 0
	for i := range optCatalog {
		if isOptimized(&optCatalog[i]) {
			n++
		}
	}
	snap.Optimized = n
	return snap
}

// ---------- 需重启判定 / 重启 ----------

// itemNeedsReboot 判断这一项要不要重启电脑才完全生效（规则照 v1.0 的 optimize.go，逐条核对过）：
//   - service：改服务启动类型 —— 开机加载的服务要重启才换过来
//   - cmd 里含 bcdedit / dism：引导与组件存储类
//   - regwrite/regdelete 打在这些「重启才生效」的写点上：
//     · ...\CurrentControlSet\Services 与 ControlSet001\Services（开机加载的服务）
//     · \Control\SessionManager（内存管理 / 内核缓解 / 预读）
//     · \Control\PriorityControl（CPU 调度）
//     · \Control\GraphicsDrivers（GPU 硬件加速调度）
//     · \Control\DeviceGuard（内存完整性 HVCI / 虚拟化安全）
//     · \Control 根级（进程共享阈值 / 关服务超时）
//     · \ReserveManager（保留存储）
//     · Policies\System 的 EnableLUA（UAC 总开关）
//
// 明确即时生效、**不算重启**的：powercfg（切电源计划 / 关休眠）、资源管理器与任务栏外观类、
// HKLM\SOFTWARE\Policies 下的应用策略（Edge / 商店 / 遥测等，最多重启对应软件）。
func itemNeedsReboot(it *OptItem) bool {
	for _, act := range it.Optimize {
		switch act.Kind {
		case "service":
			return true
		case "cmd":
			low := strings.ToLower(act.Cmd)
			if strings.Contains(low, "bcdedit") || strings.Contains(low, "dism") {
				return true
			}
		case "regwrite", "regdelete":
			k := strings.ToUpper(strings.ReplaceAll(act.Key, " ", ""))
			v := strings.ToUpper(act.Value)
			switch {
			case strings.Contains(k, `SYSTEM\CURRENTCONTROLSET\SERVICES`),
				strings.Contains(k, `SYSTEM\CONTROLSET001\SERVICES`),
				strings.Contains(k, `\CONTROL\SESSIONMANAGER`),
				strings.Contains(k, `\CONTROL\PRIORITYCONTROL`),
				strings.Contains(k, `\CONTROL\GRAPHICSDRIVERS`),
				strings.Contains(k, `\CONTROL\DEVICEGUARD`),
				strings.HasSuffix(k, `\CONTROL`),
				strings.Contains(k, `\RESERVEMANAGER`),
				strings.Contains(k, `POLICIES\SYSTEM`) && v == "ENABLELUA":
				return true
			}
		}
	}
	return false
}

// RebootPC 重启电脑（前端二次确认后才调用）：给 5 秒缓冲让用户保存工作。
// 重启后那些「需重启才生效」的服务 / 内核 / 设备防护 / UAC 类设置自动完成生效。
func (a *App) RebootPC() error {
	return runHiddenCtx(context.Background(), 30*time.Second, sysExe("shutdown.exe", ""), "/r", "/t", "5")
}

// ---------- 自检 / 给前端分组用 ----------

// OptCatalogCheck 自检用：总项数 / 分类数 / 基本预设数 / 深度预设数。
// 真数据到位后，命令行自检会拿它核对「151 项 / 7 组 / 基本 87 / 深度 127」。
func OptCatalogCheck() (total, cats, basic, deep int) {
	seen := map[string]bool{}
	for i := range optCatalog {
		it := &optCatalog[i]
		total++
		if !seen[it.Cat] {
			seen[it.Cat] = true
			cats++
		}
		if it.Basic {
			basic++
		}
		if it.Deep {
			deep++
		}
	}
	return
}

// optCategoryOrder 分类出现顺序（前端按它排版，不靠 map 随机序）
func optCategoryOrder() []string {
	var out []string
	seen := map[string]bool{}
	for i := range optCatalog {
		c := optCatalog[i].Cat
		if !seen[c] {
			seen[c] = true
			out = append(out, c)
		}
	}
	return out
}
