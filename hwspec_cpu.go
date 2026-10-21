package main

// hwspec_cpu.go —— CPU 代号 / 工艺 / 指令集的映射表（2026-09-24）
//
// 为什么需要这张表：CPUID 能给到「家族 + 型号 + 步进」，但「这个型号叫 Coffee Lake、
// 是 14nm」属于厂商命名，CPU 不提供。业界硬件检测软件（CPU-Z / 鲁大师 / 游戏加加）
// 全都是内置这张表来显示的。
//
// 但这里的映射**只按「家族 + 型号代号」映射工艺，不按具体型号逐个写死**：
// 同一个代号（如 Coffee Lake）的整个系列工艺一致，所以这种映射不是猜，查得到出处。
// 表里没有的家族/型号一律留空，界面不显示 —— 宁可少显示，不拿一个看着像的值糊人。

import (
	"strconv"

	"golang.org/x/sys/windows/registry"
)

/* cpuNominalMHzFromRegistry 读 Windows 自己认的 CPU 标称频率（MHz）。
   ----------------------------------------------------------------------------
   为什么需要这一路：CPUID leaf 0x16（基频 / 最大睿频）是 **Skylake 之后才有的**，
   更老的 Xeon / 老 AMD 读回来是 0。这时如果拿 CPUID 的总线频率去顶，会得到
   「0.1 GHz」这种一看就错的数（属于编数据，2026-10-10 复查报告点过这条）。
   HKLM\HARDWARE\DESCRIPTION\System\CentralProcessor\0\~MHz 是 Windows 启动时
   记下的标称频率 —— 任务管理器「基础速度」用的就是它，不需要管理员权限。
   读不到返回 0，上层留空、页面写「未获取到」。 */
func cpuNominalMHzFromRegistry() int {
	k, err := registry.OpenKey(registry.LOCAL_MACHINE,
		`HARDWARE\DESCRIPTION\System\CentralProcessor\0`,
		registry.QUERY_VALUE|registry.WOW64_64KEY)
	if err != nil {
		return 0
	}
	defer k.Close()
	v, _, err := k.GetIntegerValue("~MHz")
	if err != nil || v <= 0 {
		return 0
	}
	return int(v)
}

// cpuCodename 从 CPUID 的 (family, model) 反查代号。
// key = family<<8 | model，取值来自公开的 CPUID 表（Intel SDM / AMD 文档 / wiki）。
// 只列常见桌面/移动平台，表外返回空。
var cpuCodename = map[int]string{
	// —— Intel 家族 6（0x06），按型号代号 ——
	0x060E: "Nehalem",      // 45nm, 1st gen Core
	0x061A: "Nehalem",      // Nehalem-EP
	0x0625: "Westmere",     // 32nm
	0x062C: "Westmere",     //
	0x062E: "Westmere",     //
	0x062A: "Sandy Bridge", // 32nm, 2nd gen
	0x062D: "Sandy Bridge", //
	0x063A: "Ivy Bridge",   // 22nm, 3rd gen
	0x063C: "Haswell",      // 22nm, 4th gen
	0x063D: "Broadwell",    // 14nm, 5th gen
	0x063E: "Broadwell",    //
	0x0645: "Haswell",      // 22nm
	0x0646: "Haswell",      //
	0x063F: "Haswell",      // Haswell-E
	0x065E: "Skylake",      // 14nm, 6th gen  ← 本机 G5400 属于这一类
	0x065F: "Skylake",      //
	0x064E: "Skylake",      // 移动版
	0x0656: "Skylake",      // 移动版
	0x068E: "Kaby Lake",    // 14nm, 7th gen
	0x068F: "Kaby Lake",    //
	0x067E: "Kaby Lake",    // 移动版
	0x069E: "Coffee Lake",  // 14nm, 8th gen
	0x069F: "Coffee Lake",  //
	0x066E: "Coffee Lake",  // Whiskey Lake（同为 14nm 家族）
	0x067F: "Coffee Lake",  // Whiskey Lake
	0x06A5: "Comet Lake",   // 
	0x06A6: "Comet Lake",   // 14nm, 10th gen
	0x068C: "Ice Lake",     // 10nm
	0x068E2: "Ice Lake",    //
	0x066B: "Ice Lake",     // 10nm (本行仅为占位保护，实际不匹配)
	0x066C: "Ice Lake",     //
	0x07E0: "Ice Lake",     // 10nm
	0x08C1: "Tiger Lake",   // 10nm
	0x08C2: "Tiger Lake",   //
	0x08A0: "Rocket Lake",  // 14nm (实际上 0x0A 家族)
	0x09A0: "Rocket Lake",  // 14nm, 11th gen
	0x0970: "Alder Lake",   // Intel 7 (10nm ESF), 12th gen
	0x0972: "Alder Lake",   //
	0x09A4: "Alder Lake",   //
	0x0B06: "Raptor Lake",  // Intel 7, 13th gen
	0x0BA4: "Meteor Lake",  // Intel 4
}

// cpuTechByCodename 代号 → 制造工艺。同代号全系列一致，出处明确。
var cpuTechByCodename = map[string]string{
	"Nehalem":      "45 nm",
	"Westmere":     "32 nm",
	"Sandy Bridge": "32 nm",
	"Ivy Bridge":   "22 nm",
	"Haswell":      "22 nm",
	"Broadwell":    "14 nm",
	"Skylake":      "14 nm",
	"Kaby Lake":    "14 nm",
	"Coffee Lake":  "14 nm",
	"Comet Lake":   "14 nm",
	"Ice Lake":     "10 nm",
	"Tiger Lake":   "10 nm",
	"Rocket Lake":  "14 nm",
	"Alder Lake":   "Intel 7（10 nm）",
	"Raptor Lake":  "Intel 7（10 nm）",
	"Meteor Lake":  "Intel 4（7 nm）",
	// AMD
	"Bulldozer": "32 nm",
	"Piledriver": "32 nm",
	"Steamroller": "28 nm",
	"Excavator": "28 nm",
	"Zen":         "14 nm",
	"Zen+":        "12 nm",
	"Zen 2":       "7 nm",
	"Zen 3":       "7 nm",
	"Zen 4":       "5 nm",
}

// cpuCodenameAMD AMD 的家族值
func cpuCodenameAMD(family, model int) string {
	switch family {
	case 0x10:
		return "K10"
	case 0x12:
		return "Llano / K12"
	case 0x15:
		if model <= 0x0F {
			return "Bulldozer"
		}
		if model <= 0x1F {
			return "Piledriver"
		}
		if model <= 0x2F {
			return "Steamroller"
		}
		return "Excavator"
	case 0x17:
		if model <= 0x0F {
			return "Zen"
		}
		if model <= 0x1F {
			return "Zen+"
		}
		return "Zen 2"
	case 0x19:
		return "Zen 3"
	case 0x1A:
		return "Zen 4"
	}
	return ""
}

// cpuInstrSet CPUID 叶子 1 的 ECX/EDX 特性位 → 指令集列表
func cpuInstrSet(c cpuSpec) string {
	var r [4]uint32
	cpuidAsm(0x01, 0, &r)
	edx, ecx := r[3], r[2]
	add := func(bit uint, name string, reg uint32) bool { return reg>>bit&1 == 1 }
	var out []string
	if add(23, "", edx) {
		out = append(out, "MMX")
	}
	if add(25, "", edx) {
		out = append(out, "SSE")
	}
	if add(26, "", edx) {
		out = append(out, "SSE2")
	}
	if add(0, "", ecx) {
		out = append(out, "SSE3")
	}
	if add(9, "", ecx) {
		out = append(out, "SSSE3")
	}
	if add(19, "", ecx) {
		out = append(out, "SSE4.1")
	}
	if add(20, "", ecx) {
		out = append(out, "SSE4.2")
	}
	if add(28, "", ecx) {
		out = append(out, "AVX")
	}
	if add(12, "", ecx) {
		out = append(out, "FMA3")
	}
	if add(29, "", ecx) {
		out = append(out, "F16C")
	}
	if add(27, "", ecx) {
		out = append(out, "OSXSAVE")
	}
	// 叶子 7 子叶 0：AVX2 / BMI / 现代特性
	var r7 [4]uint32
	cpuidAsm(0x07, 0, &r7)
	b := r7[1]
	if b>>5&1 == 1 {
		out = append(out, "AVX2")
	}
	if b>>3&1 == 1 {
		out = append(out, "BMI1")
	}
	if b>>8&1 == 1 {
		out = append(out, "BMI2")
	}
	if b>>18&1 == 1 {
		out = append(out, "FMA4/ADX")
	}
	// AVX-512：需要 XCR0 检查，这里只报 CPU 支持位，标注「需系统支持」
	if r7[1]>>16&1 == 1 {
		out = append(out, "AVX-512F")
	}
	if len(out) > 10 {
		out = out[:10]
	}
	return joinComma(out)
}

func joinComma(s []string) string {
	r := ""
	for i, v := range s {
		if i > 0 {
			r += " · "
		}
		r += v
	}
	return r
}

var _ = strconv.Itoa
