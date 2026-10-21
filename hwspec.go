package main

// hwspec.go —— 硬件细节读取（2026-09-24 新增）
//
// 背景：本机配置页要做到「像硬件检测软件那样把参数摆全」，而 WMI 有一大片读不到：
// Win32_Processor 的 MaxClockSpeed / NumberOfCores 在本机是空的，
// Win32_PhysicalMemory 的 Speed / ConfiguredClockSpeed 也是空的，
// 工艺（14nm）、内存类型（DDR4）WMI 根本没有对应字段。
//
// 这些参数**不需要猜**，系统里本来就存着，只是不在 WMI 里：
//   ① CPU 主频 / 总线 / 缓存 / 核心数 → 直接执行 CPUID 指令（x86 架构内置）
//   ② 内存类型 / 速率 / 通道 / 厂商 / 型号 → SMBIOS 类型 17 结构（主板固件表）
//   ③ BIOS / 主板 / 机箱 / 内存阵列 → SMBIOS 类型 0 / 2 / 3 / 16
//   ④ 显存 → 注册表 HardwareInformation.qwMemorySize（>4GB 的卡也准，AdapterRAM 会溢出）
//   ⑤ 显示模式 / 刷新率 → EnumDisplaySettings（WMI 的 Current* 字段在核显上是空的）
//
// 规矩不变：读不到就留空，前端写「读不到」，**绝不拿型号对照表猜一个看着像的值**。

import (
	"encoding/binary"
	"fmt"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"unsafe"

	"github.com/go-ole/go-ole"
	"github.com/go-ole/go-ole/oleutil"
)

func numCPU() int { return runtime.NumCPU() }

/* ==================== ① CPUID：CPU 参数（架构内置，零依赖） ==================== */

var procFirmware = syscall.NewLazyDLL("kernel32.dll").NewProc("GetSystemFirmwareTable")

// cpuid 执行 CPUID 指令。Go 汇编不便内联，用固定字节序列：
//   0F A2        CPUID        （读 EAX/ECX，回写 EAX/EBX/ECX/EDX）
//   89 D8        MOV EAX,EBX
//   89 D1        MOV ECX,EDX
//   C3           RET          —— 与 cpu.X86 的标准实现一致
//
//go:noescape
func cpuidAsm(ax, cx uint32, out *[4]uint32)

var procCPUIDAsm = cpuidAsm // 便于测试直接引用

type cpuSpec struct {
	Brand      string // 商标串，如 Intel(R) Pentium(R) Gold G5400
	BaseMHz    int    // 基频（标称主频）
	TurboMHz   int    // 最大睿频
	BusMHz     int    // 总线（基频）
	Cores      int    // 物理核心数
	Threads    int    // 逻辑处理器数
	CacheL1DKB int
	CacheL1IKB int
	CacheL2KB  int
	CacheL3KB  int
	L2Inst     int    // L2 实例数（= 物理核心数，可用来交叉验证）
	TDP        int    // 只有部分型号能从 MSR 读到，读不到留 0
	ExtFamily  int
	ExtModel   int
	Stepping   int
	Family     int
	Model      int
	SocketName string
}

// CPUID 叶子 0x16 = Processor Frequency Information：
//   EAX = 基频 MHz，EBX = 最大睿频 MHz，ECX = 总线（基频）MHz
func (c *cpuSpec) readFreq() {
	var r [4]uint32
	cpuidAsm(0x16, 0, &r)
	if r[0] == 0 && r[1] == 0 {
		return // 该叶子在 2015 年前的 CPU 上不存在
	}
	c.BusMHz = int(r[2])
	// 叶子 0x16 的语义：EAX = 基频，EBX = 最大睿频。
	// 用户认的是「这颗 CPU 多少 GHz」，标称频率就是基频本身，
	// 所以主值取基频（跟 WMI 的 MaxClockSpeed ≙ 基频口径一致），睿频另存。
	c.BaseMHz = int(r[0])
	c.TurboMHz = int(r[1])
}

/* CPUID 叶子 0x04（确定性缓存参数）：
     ECX = 子叶索引（从 0 开始递增，EAX[4:0]=0 表示没了）
     EAX[4:0]  = 缓存类型（1=数据 2=指令 3=统一）
     EAX[7:5]  = 缓存层级（1=L1D 2=L1I 3=L2 4=L3）
     EBX[11:0] = 行大小-1，EBX[21:12]=分区-1，EBX[31:22]=路数-1，ECX[31:0]=组数-1
   容量 = (路数+1) × (分区+1) × (行大小+1) × (组数+1)

   坑（本机实测栽过）：**ECX 不是「缓存层号」**。早先写成 cpuidAsm(0x04, level<<5|idx)，
   把层号塞进 ECX 高 5 位，结果一个缓存都读不到（L2/L3 全是空）。
   正确写法是 ECX 传递增的子叶号，再从 EAX[7:5] 里认层级。 */
/* 层级（EAX[7:5]）与类型（EAX[4:0]）是两个独立字段：
     level: 1 = L1, 2 = L2, 3 = L3
     type : 1 = 数据(Data), 2 = 指令(Instructions), 3 = 统一(Unified)
   所以 L1D 是 (level=1, type=1)、L1I 是 (level=1, type=2)，不存在「level=2 是 L1I」这种编码。

   **每核一份的缓存，CPUID 只报一个子叶**。本机 G5400 实测 4 个子叶：
     idx0  type=1 level=1  32 KB   ← L1D 单份
     idx1  type=2 level=1  32 KB   ← L1I 单份
     idx2  type=3 level=2  256 KB  ← L2 单份
     idx3  type=3 level=3  4096 KB ← L3 共享（sharedThreads=15）
   拷贝数等于物理核心数，所以总量 = 单份 × 核心数（主流软件写的「2 × 32 KB」就是这个口径）。 */
const (
	cacheL1   = 1
	cacheL2   = 2
	cacheL3   = 3
	ctypeData = 1
	ctypeInst = 2
)

// cpuCacheKB 取单个实例的容量（KB）。ctype 传 0 表示不限类型。
// 找不到返回 -1（调用方必须判 > 0，别把 -1 当 0 KB 显示出去）。
func cpuCacheKB(level, ctype uint32) int {
	for idx := uint32(0); idx < 32; idx++ {
		var r [4]uint32
		cpuidAsm(0x04, idx, &r)
		if r[0]&0x1F == 0 {
			break // 没有更多子叶
		}
		if (r[0]>>5)&0x7 != level {
			continue
		}
		if ctype != 0 && r[0]&0x1F != ctype {
			continue
		}
		ways := int(r[1]>>22)&0x3FF + 1
		parts := int(r[1]>>12)&0x3FF + 1
		lineSz := int(r[1]&0xFFF) + 1
		sets := int(r[2]) + 1
		sz := ways * parts * lineSz * sets
		if sz <= 0 {
			continue
		}
		return sz / 1024
	}
	return -1
}

// readCPUSpec 读 CPU 参数（主频 / 缓存必读行；核心数拿不到时退回 runtime）
func readCPUSpec() cpuSpec {
	var c cpuSpec
	// 叶子 0：厂商串 + 最大叶子号
	var r0 [4]uint32
	cpuidAsm(0x00, 0, &r0)
	maxLeaf := r0[0]
	// 叶子 0x80000000：扩展最大叶子号
	var re0 [4]uint32
	cpuidAsm(0x80000000, 0, &re0)

	// 商标串：0x80000002..0x80000004，共 48 字节
	if re0[0] >= 0x80000004 {
		var buf [48]byte
		for i := uint32(0); i < 3; i++ {
			var r [4]uint32
			cpuidAsm(0x80000002+i, 0, &r)
			binary.LittleEndian.PutUint32(buf[i*16:], r[0])
			binary.LittleEndian.PutUint32(buf[i*16+4:], r[1])
			binary.LittleEndian.PutUint32(buf[i*16+8:], r[2])
			binary.LittleEndian.PutUint32(buf[i*16+12:], r[3])
		}
		c.Brand = strings.TrimSpace(strings.TrimRight(string(buf[:]), "\x00"))
		c.Brand = strings.Join(strings.Fields(c.Brand), " ")
	}

	if maxLeaf >= 0x01 {
		var r [4]uint32
		cpuidAsm(0x01, 0, &r)
		stepping := int(r[0] & 0xF)
		model := int(r[0] >> 4 & 0xF)
		family := int(r[0] >> 8 & 0xF)
		extModel := int(r[0] >> 16 & 0xF)
		extFamily := int(r[0] >> 20 & 0xFF)
		if family == 0xF {
			family += extFamily
		}
		if family == 0x6 || family == 0xF {
			model += extModel << 4
		}
		c.Family, c.Model, c.Stepping, c.ExtModel, c.ExtFamily = family, model, stepping, extModel, extFamily
		// 逻辑处理器数 = EBX[23:16]
		c.Threads = int(r[1] >> 16 & 0xFF)
		// L2 / L3 字节数在 EDX / ECX 高位，仅对老 CPU 有效，这里不用
	}

	// 叶子 0x16：频率
	c.readFreq()

	// 缓存读的是**单份**容量，总量要在组装展示时乘核心数（见 readHWSpec）
	c.CacheL1DKB = cpuCacheKB(cacheL1, ctypeData)
	c.CacheL1IKB = cpuCacheKB(cacheL1, ctypeInst)
	c.CacheL2KB = cpuCacheKB(cacheL2, 0)
	c.CacheL3KB = cpuCacheKB(cacheL3, 0)

	/* 核心数 / 线程数：走 CPUID 叶子 0x0B 的「扩展拓扑枚举」，这是 Intel 官方算法。

	   踩过的坑（本机实测，别再犯）：**不能用叶子 0x04 数子叶当核心数**。
	   0x04 是「确定性缓存参数」，每个核心的 L1D/L1I/L2 各占一个子叶、L3 再占一个，
	   子叶个数和核心数没有倍数关系 —— 本机 G5400（2 核 4 线程）被数成 4，
	   配上面那个更离谱的写法一度算出「8 核 16 线程」。

	   0x0B 的正确读法：
	     ECX[15:8] = 层级类型：1 = SMT（超线程层）、2 = Core（核心层）
	     EBX[15:0] = 该层级能寻址的逻辑处理器数
	     → 每核线程数 = SMT 层的 EBX，整机逻辑处理器数 = Core 层的 EBX
	     → 物理核心数 = 整机逻辑处理器数 / 每核线程数
	   本机实测：SMT 层 EBX=2、Core 层 EBX=4 → 2 核 4 线程 ✓ */
	if maxLeaf >= 0x0B {
		perCore, perPkg := 0, 0
		for sub := uint32(0); sub < 16; sub++ {
			var r [4]uint32
			cpuidAsm(0x0B, sub, &r)
			levelType := (r[2] >> 8) & 0xFF
			if levelType == 0 {
				break
			}
			switch levelType {
			case 1: // SMT：一个核上有几个逻辑处理器
				perCore = int(r[1] & 0xFFFF)
			case 2: // Core：一个封装上有几个逻辑处理器
				perPkg = int(r[1] & 0xFFFF)
			}
		}
		if perPkg > 0 {
			c.Threads = perPkg
			if perCore > 1 {
				c.Cores = perPkg / perCore
			} else {
				c.Cores = perPkg
			}
		}
	}
	// 老 CPU 没有 0x0B：退到「整机逻辑处理器数」当线程数，核数留给 SMBIOS / WMI 补
	if c.Threads == 0 {
		c.Threads = numCPUFallback()
	}
	if c.Cores == 0 {
		c.Cores = c.Threads // 核数未知时先按线程数占位（SMBIOS 类型 4 更准，会覆盖）
	}
	return c
}

/* ==================== ② SMBIOS：内存类型 / 速率 / 槽位等 ==================== */

// smbiosTable 取固件表原始字节（RSMB）。
// firmwareTableRawSMBIOS = 0x52534D42（'RSMB'），即 SMBIOS 结构表。
// Windows 需要管理员权限 + 程序清单声明了受支持的 OS 版本才给真实数据
// （app.manifest 已补全 supportedOS，否则拿到的是兼容性垫片后的降级结果）。
// smbiosTable 取固件表原始字节。**两条路都走，先通先用** —— 这是通用性要求，
// 不能因为「某一台机器走 A 路失败」就判定这台机器读不到 SMBIOS：
//
//   路 A（推荐，实测在这台机器上通）：WMI root\WMI 的 MSSMBios_RawSMBiosTables.SMBiosData
//        —— 系统内置 WMI 提供程序，不需要驱动、不需要管理员，Win7 起就有。
//   路 B（兜底）：GetSystemFirmwareTable('RSMB')
//        —— 部分机器（含本机）这条 API 会「返回长度正常、缓冲区却一个非零字节都没有」，
//           只认它就会误判成「主板没写 SMBIOS」，实测本机就是这样栽的。
//
// 两条路都以「非零字节数」为准判定成功，不只看返回长度。
func smbiosTable() []byte {
	if b := smbiosViaWMI(); len(b) > 0 {
		return b
	}
	return smbiosViaFirmwareTable()
}

// smbiosViaWMI 走 WMI 拿原始 SMBIOS 表（MSSMBios_RawSMBiosTables.SMBiosData 是 uint8 数组）
func smbiosViaWMI() []byte {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	_ = ole.CoInitializeEx(0, ole.COINIT_MULTITHREADED)
	defer ole.CoUninitialize()

	loc, err := oleutil.CreateObject("WbemScripting.SWbemLocator")
	if err != nil {
		return nil
	}
	defer loc.Release()
	wmi, err := loc.QueryInterface(ole.IID_IDispatch)
	if err != nil {
		return nil
	}
	defer wmi.Release()
	svcRaw, err := oleutil.CallMethod(wmi, "ConnectServer", nil, `root\WMI`)
	if err != nil {
		return nil
	}
	svc := svcRaw.ToIDispatch()
	defer svc.Release()
	resRaw, err := oleutil.CallMethod(svc, "ExecQuery", "SELECT * FROM MSSMBios_RawSMBiosTables")
	if err != nil {
		return nil
	}
	res := resRaw.ToIDispatch()
	defer res.Release()
	cnt, err := oleutil.GetProperty(res, "Count")
	if err != nil || int(cnt.Val) == 0 {
		return nil
	}
	itemRaw, err := oleutil.CallMethod(res, "ItemIndex", 0)
	if err != nil {
		return nil
	}
	item := itemRaw.ToIDispatch()
	if item == nil {
		return nil
	}
	defer item.Release()
	v, err := oleutil.GetProperty(item, "SMBiosData")
	if err != nil || v == nil {
		return nil
	}
	arr := v.ToArray()
	if arr == nil {
		return nil
	}
	vals := arr.ToValueArray()
	out := make([]byte, 0, len(vals))
	for _, e := range vals {
		if u, ok := e.(uint8); ok {
			out = append(out, u)
		} else {
			return nil // 类型不对，别拿半截数据糊过去
		}
	}
	if !hasNonZero(out) {
		return nil
	}
	return out
}

// smbiosViaFirmwareTable 走 GetSystemFirmwareTable('RSMB')
func smbiosViaFirmwareTable() []byte {
	const rsmb = 0x52534D42
	/*
	   坑（本机实测）：**不要先探测长度再取数据**。探测调用传空缓冲区在某些机器上会把
	   后续调用带坏，表现为「长度拿对了，但整块缓冲区全是 0」。
	   业界通用写法是直接给足缓冲区一次取回，这里给 64KB。
	*/
	const cap64k = 64 << 10
	buf := make([]byte, cap64k)
	n, _, _ := procFirmware.Call(uintptr(rsmb), 0, uintptr(cap64k), uintptr(unsafe.Pointer(&buf[0])))
	if n == 0 || int(n) > len(buf) {
		return nil
	}
	buf = buf[:n]
	if !hasNonZero(buf) {
		return nil // 长度对但内容空 —— 这条路在这台机器上不通
	}
	return buf
}

func hasNonZero(b []byte) bool {
	for _, x := range b {
		if x != 0 {
			return true
		}
	}
	return false
}

type smbiosStruct struct {
	Type byte
	Data []byte // 不含头 4 字节的格式化区
	Strs []string
}

// parseSMBIOS 把固件表拆成结构列表（含字符串表）
func parseSMBIOS(b []byte) []smbiosStruct {
	var out []smbiosStruct
	i := 0
	for i+4 <= len(b) {
		t := b[i]
		l := int(b[i+1])
		if l < 4 || i+l > len(b) {
			break
		}
		// Data 保留完整结构头（Type/Length/Handle 4 字节都留着）：
		// 这样下面所有偏移量都能直接照 SMBIOS 规范写（规范里的偏移就是从 Type 字节数起的）。
		// 早先这里砍掉了头 4 字节、偏移却仍按规范写，整表错位 4 字节 ——
		// 实测把 BIOS 厂商读成了日期「06/14/2018」、内存读成「FLASH 1541 MHz 0.5 GB」。
		st := smbiosStruct{Type: t, Data: b[i : i + l]}
		// 字符串表：结构体格式化区之后，以双 NUL 结尾
		j := i + l
		for j < len(b) {
			if b[j] == 0 {
				if j+1 < len(b) && b[j+1] == 0 {
					j += 2
					break
				}
				j++
				continue
			}
			k := j
			for k < len(b) && b[k] != 0 {
				k++
			}
			st.Strs = append(st.Strs, string(b[j:k]))
			j = k
		}
		out = append(out, st)
		i = j
	}
	return out
}

// str 取字符串索引（1 起，0 表示无）
func (s smbiosStruct) str(idx byte) string {
	if idx == 0 || int(idx) > len(s.Strs) {
		return ""
	}
	return strings.TrimSpace(s.Strs[idx-1])
}

func (s smbiosStruct) u8(off int) byte {
	if off < 0 || off >= len(s.Data) {
		return 0
	}
	return s.Data[off]
}

func (s smbiosStruct) u16(off int) int {
	if off+1 >= len(s.Data) {
		return 0
	}
	return int(binary.LittleEndian.Uint16(s.Data[off:]))
}

func (s smbiosStruct) u32(off int) uint32 {
	if off+3 >= len(s.Data) {
		return 0
	}
	return binary.LittleEndian.Uint32(s.Data[off:])
}

func (s smbiosStruct) u64(off int) uint64 {
	if off+7 >= len(s.Data) {
		return 0
	}
	return binary.LittleEndian.Uint64(s.Data[off:])
}

// smbiosMemType SMBIOS 类型 17 偏移 0x12 的内存类型编码
var smbiosMemType = map[byte]string{
	1: "其他", 2: "未知", 3: "DRAM", 4: "EDRAM", 5: "VRAM", 6: "SRAM", 7: "RAM",
	8: "ROM", 9: "FLASH", 10: "EEPROM", 11: "FEPROM", 12: "EPROM", 13: "CDRAM",
	14: "3DRAM", 15: "SDRAM", 16: "SGRAM", 17: "RDRAM", 18: "DDR", 19: "DDR2",
	20: "DDR2 FB-DIMM", 24: "DDR3", 25: "FBD2", 26: "DDR4", 27: "LPDDR",
	28: "LPDDR2", 29: "LPDDR3", 30: "LPDDR4", 31: "Logical non-volatile device",
	32: "HBM", 33: "HBM2", 34: "DDR5", 35: "LPDDR5",
}

// smbiosFormFactor SMBIOS 类型 17 偏移 0x0E 的外形编码
var smbiosFormFactor = map[byte]string{
	1: "其他", 2: "未知", 3: "SIMM", 4: "SIP", 5: "芯片", 6: "DIP", 7: "ZIP",
	8: "SOJ", 9: "Proprietary", 10: "SIMM", 11: "DIMM", 12: "TSOP", 13: "PGA",
	14: "RIMM", 15: "SODIMM", 16: "SRIMM", 17: "SMD", 18: "SSMP", 19: "QFP",
	20: "TQFP", 21: "SOIC", 22: "LCC", 23: "引脚", 24: "BGA", 25: "FPBGA", 26: "LGA",
}

/* smbiosChassis SMBIOS 类型 3 机箱类型
   全部写中文：这一页是给小白看的，冒出 "Notebook" / "Docking Station" 这种半吊子英文
   等于没说（2026-09-24 用户点名「表述要用中文表述，让人一眼就看懂」）。
   值同时要能被下面的 formFactor 归类，所以这里只写「人话名」，归类逻辑单独走一张表。 */
var smbiosChassis = map[byte]string{
	1: "其他", 2: "未知", 3: "台式机", 4: "薄型台式机", 5: "披萨盒机箱", 6: "小型机箱",
	7: "塔式机箱", 8: "便携机", 9: "笔记本", 10: "笔记本", 11: "掌上设备", 12: "扩展底座",
	13: "一体机", 14: "超轻笔记本", 15: "紧凑型机箱", 16: "午餐盒机箱", 17: "主系统机箱",
	18: "扩展机箱", 19: "子机箱", 20: "总线扩展机箱", 21: "外设机箱", 22: "存储机箱",
	23: "机架式服务器", 24: "密封机箱", 30: "平板", 31: "可翻转平板", 32: "可拆分平板", 35: "迷你主机",
}

/* chassisGroup 把 SMBIOS 机箱类型归一成四类，给页面画图形用。
   归类只管「长得像什么」，人话名还是用 smbiosChassis 那张表。 */
var chassisGroup = map[byte]string{
	3: "desktop", 4: "desktop", 5: "desktop", 6: "desktop", 7: "desktop",
	13: "desktop", 15: "desktop", 16: "desktop", 17: "desktop", 22: "desktop",
	23: "desktop", 24: "desktop", 35: "desktop",
	8: "laptop", 9: "laptop", 10: "laptop", 11: "laptop", 14: "laptop",
	30: "tablet", 31: "tablet", 32: "tablet",
}

// hwMemStick 一条内存条（来自 SMBIOS 类型 17，比 Win32_PhysicalMemory 全）
type hwMemStick struct {
	Slot     string `json:"slot"`     // 槽位，如 ChannelA-DIMM0
	Size     string `json:"size"`     // 4 GB
	Type     string `json:"type"`     // DDR4
	Speed    string `json:"speed"`    // 2400 MHz
	Conf     string `json:"conf"`     // 实跑频率
	Maker    string `json:"maker"`    // Kingston
	Part     string `json:"part"`     // 型号
	Serial   string `json:"serial"`   //
	Form     string `json:"form"`     // DIMM / SODIMM
	Rank     string `json:"rank"`     // 单/双面
	Volt     string `json:"volt"`     // 1.2 V
	Width    string `json:"width"`    // 数据位宽
	TotalW   string `json:"totalw"`   // 总线位宽
	HasECC   bool   `json:"hasEcc"`   //
	TypeCode byte   `json:"-"`
}

// hwSpec 补出来的硬件细节（每一项读不到就是空字符串，前端不显示）
type hwSpec struct {
	// CPU
	CPUBase    string `json:"cpuBase"`    // 3.7 GHz
	CPUTurbo   string `json:"cpuTurbo"`   // 最大睿频
	CPUCache   string `json:"cpuCache"`   // L2 512 KB · L3 4 MB
	CPUCore    string `json:"cpuCore"`    // 2 核 4 线程
	CPUSocket  string `json:"cpuSocket"`  // 插座
	CPUCode    string `json:"cpuCode"`    // 代号
	CPUFamily  string `json:"cpuFamily"`  // 家族/型号/步进（CPUID 原始值）
	CPUBus     string `json:"cpuBus"`     // 总线频率
	CPUTDP     string `json:"cpuTdp"`     //
	CPUTech    string `json:"cpuTech"`    // 制造工艺
	CPUInstr   string `json:"cpuInstr"`   // 指令集
	CPUVendor  string `json:"cpuVendor"`  // 厂商
	CPUBrandID string `json:"cpuBrandId"` // CPUID 商标串
	CPUBrand   string `json:"cpuBrand"`   // 品牌（中文，如「英特尔」）
	CPUModelCn string `json:"cpuModel"`   // 清洗过的型号（去掉 (R) 和尾部 @ x GHz）
	MemBrand   string `json:"memBrand"`   // 内存品牌（中文，取第一条内存条的厂商）
	BoardBrand string `json:"boardBrand"` // 主板品牌（中文）

	// 内存
	MemType    string       `json:"memType"`    // DDR4
	MemSpeed   string       `json:"memSpeed"`   // 2400 MHz
	MemUsedSpd string       `json:"memUsedSpd"` // 实跑 2133 MHz
	MemChan    string       `json:"memChan"`    // 双通道
	MemSlots   string       `json:"memSlots"`   // 2 / 2 条已用
	MemVolt    string       `json:"memVolt"`    //
	MemMaxCap  string       `json:"memMaxCap"`  // 最大支持 32 GB
	MemSticks  []hwMemStick `json:"memSticks"`  // 每条明细

	// 显存 / 显示
	VRAM       string `json:"vram"`       // 128 MB / 2 GB
	VRAM2      string `json:"vram2"`      // 备用口径
	GPUDriver  string `json:"gpuDriver"`  // 驱动日期
	GPUMode    string `json:"gpuMode"`    // 1920 x 1080 @ 60 Hz
	GPUCount   string `json:"gpuCount"`   //
	GPUSlot    string `json:"gpuSlot"`    // 接口
	GPUVRAMAll string `json:"gpuVramAll"` // 总显存

	// 主板 / BIOS / 机箱
	BoardChip   string `json:"boardChip"`   // H310 芯片组
	BoardVer    string `json:"boardVer"`    //
	BIOSVendor  string `json:"biosVendor"`  // American Megatrends
	BIOSDate    string `json:"biosDate"`    //
	BIOSVer     string `json:"biosVer"`     //
	SMBIOSVer   string `json:"smbiosVer"`   //
	Chassis     string `json:"chassis"`     // 台式机（人话名）
	ChassisType byte   `json:"-"`           // SMBIOS 原始机箱类型，给 formFactor 归类用
	Form        hwForm `json:"form"`        // 机型归类 + 判断依据（页面画图形靠它）
	SysType     string `json:"sysType"`     // x64-based PC
	SysVendor   string `json:"sysVendor"`   //
	SysModel    string `json:"sysModel"`    //
	SysSerial   string `json:"sysSerial"`   //
	BoardSerial string `json:"boardSerial"` //

	// 显示器 / 声卡 / 网卡
	Monitors  []string `json:"monitors"`
	NetCards  []string `json:"netCards"`
	SoundCard []string `json:"soundCard"`

	// 读取来源（排错用，也让「读不到」有迹可循）
	Sources string `json:"sources"`
}

func numCPUFallback() int { return numCPU() }

/* ==================== 组装 ==================== */

// readHWSpec 把 WMI 补不到的那部分参数读出来（全部走真实数据源）
func readHWSpec() hwSpec {
	var s hwSpec
	var src []string

	// ---------- CPU：CPUID ----------
	c := readCPUSpec()
	if c.Brand != "" {
		s.CPUBrandID = c.Brand
		s.Sources += "cpuid;"
	}
	if c.BaseMHz > 0 {
		s.CPUBase = fmtMHz(c.BaseMHz)
		if c.TurboMHz > c.BaseMHz {
			s.CPUTurbo = fmtMHz(c.TurboMHz)
		}
	} else if mhz := cpuNominalMHzFromRegistry(); mhz > 0 {
		/* CPUID 那条路（leaf 0x16）是 Skylake 之后才有的，老 Xeon / 老 AMD 读到 0。
		   旧代码这时拿**总线频率**顶上去 —— 会显示「0.1 GHz」这种明显错的数，
		   那就是编数据。改读 Windows 自己记的标称频率（任务管理器同源）；读不到就留空。 */
		s.CPUBase = fmtMHz(mhz)
	}
	if c.BusMHz > 0 {
		s.CPUBus = fmtMHz(c.BusMHz)
	}
	if c.Threads > 0 && c.Cores > 0 {
		s.CPUCore = strconv.Itoa(c.Cores) + " 核 " + strconv.Itoa(c.Threads) + " 线程"
	} else if c.Threads > 0 {
		s.CPUCore = strconv.Itoa(c.Threads) + " 线程"
	}
	/* 缓存：L1 指令/数据分开报（软件里一般各写一条），L2 每核一份会标「2 × 256 KB」。
	   注意 cpuCacheKB 读不到时返回 -1，这里必须判 >0，不能只判 !=0（负数是「读不到」不是「0 KB」）。 */
	var cache []string
	// 每核一份 → 写「2 × 32 KB」；核数没读出来就只写单份，不硬乘一个假倍数
	per := func(kb int) string {
		if c.Cores > 1 {
			return fmt.Sprintf("%d × %s", c.Cores, fmtKB(kb))
		}
		return fmtKB(kb)
	}
	if c.CacheL1DKB > 0 {
		cache = append(cache, "L1D "+per(c.CacheL1DKB))
	}
	if c.CacheL1IKB > 0 {
		cache = append(cache, "L1I "+per(c.CacheL1IKB))
	}
	if c.CacheL2KB > 0 {
		cache = append(cache, "L2 "+per(c.CacheL2KB))
	}
	if c.CacheL3KB > 0 {
		cache = append(cache, "L3 "+fmtKB(c.CacheL3KB)+"（共享）")
	}
	s.CPUCache = strings.Join(cache, " · ")
	if c.Family > 0 {
		s.CPUFamily = "Family " + strconv.Itoa(c.Family) + " · Model " + strconv.Itoa(c.Model) + " · Stepping " + strconv.Itoa(c.Stepping)
	}
	if c.Threads > 0 {
		src = append(src, "cpuid")
	}

	// ---------- SMBIOS 表 ----------
	tbl := smbiosTable()
	if len(tbl) > 0 {
		src = append(src, "smbios")
	}
	structs := parseSMBIOS(tbl)
	var memSticks []hwMemStick
	maxCapKB := 0
	slotsTotal, slotsUsed := 0, 0
	for _, st := range structs {
		switch st.Type {
		case 0: // BIOS
			s.BIOSVendor = st.str(st.u8(0x04))
			s.BIOSVer = st.str(st.u8(0x05))
			if d := st.str(st.u8(0x08)); d != "" {
				s.BIOSDate = normalizeBIOSDate(d)
			}
			maj, min := st.u8(0x14), st.u8(0x15)
			if maj > 0 {
				s.SMBIOSVer = strconv.Itoa(int(maj)) + "." + strconv.Itoa(int(min))
			}
		case 1: // 系统
			s.SysVendor = cleanVendor(st.str(st.u8(0x04)))
			s.SysModel = st.str(st.u8(0x05))
			s.SysSerial = validSerial(st.str(st.u8(0x07)))
		case 2: // 主板
			if s.BoardVer == "" {
				s.BoardVer = st.str(st.u8(0x05))
			}
			if s.BoardSerial == "" {
				s.BoardSerial = validSerial(st.str(st.u8(0x07)))
			}
		case 3: // 机箱
			ct := st.u8(0x05) & 0x7F
			s.ChassisType = ct // 原始值留着，好让 formFactor 归类（人话名和图形类别是两回事）
			if t := smbiosChassis[ct]; t != "" {
				s.Chassis = t
			}
		case 16: // 内存阵列：最大支持容量（规范偏移 0x07 = Maximum Capacity，单位 KB）
			if kb := st.u32(0x07); kb > 0 && kb != 0xFFFFFFFF {
				maxCapKB = int(kb)
			}
			if n := int(st.u16(0x0D)); n > 0 { // 0x0D = Number of Memory Devices
				slotsTotal = n
			}
		case 17: // 内存设备
			sz := st.u16(0x0C)
			if sz == 0 || sz == 0xFFFF {
				continue // 空槽
			}
			mb := sz
			if sz == 0x7FFF { // 扩展：偏移 0x1C 是 32 位 MB
				mb = int(st.u32(0x1C) & 0x7FFFFFFF)
			}
			stk := hwMemStick{
				Slot:   st.str(st.u8(0x10)),
				Size:   fmtMBToGB(mb),
				Type:   smbiosMemType[st.u8(0x12)],
				Maker:  cleanVendor(st.str(st.u8(0x17))),
				Part:   strings.TrimSpace(st.str(st.u8(0x1A))),
				Serial: validSerial(st.str(st.u8(0x18))),
				Form:   smbiosFormFactor[st.u8(0x0E)],
			}
			if sp := st.u16(0x15); sp > 0 && sp != 0xFFFF {
				stk.Speed = strconv.Itoa(sp) + " MHz"
			}
			if sp := st.u16(0x20); sp > 0 && sp != 0xFFFF {
				stk.Conf = strconv.Itoa(sp) + " MHz"
			}
			if v := st.u16(0x26); v > 0 && v != 0xFFFF { // 0x26 = Configured Voltage (mV)
				stk.Volt = fmtFloat1(float64(v)/1000) + " V"
			}
			if w := st.u16(0x08); w > 0 && w != 0xFFFF {
				stk.Width = strconv.Itoa(w) + " 位"
			}
			if w := st.u16(0x0A); w > 0 && w != 0xFFFF {
				stk.TotalW = strconv.Itoa(w) + " 位"
			}
			// 0x1B = Attributes，bit3:0 是 Rank 数（0 表示未知）
			if n := int(st.u8(0x1B) & 0x0F); n > 0 {
				stk.Rank = strconv.Itoa(n) + " Rank"
			}
			memSticks = append(memSticks, stk)
			slotsUsed++
		}
	}
	s.MemSticks = memSticks
	/* 内存品牌取「第一个有厂商的内存条」并去重展示：
	   混插不同品牌很常见，所以前端会把每条的品牌都列在明细里，
	   这里只给卡片上一个主品牌（取第一条有值的）。 */
	for _, m := range memSticks {
		if b := brandCN(m.Maker); b != "" && b != m.Maker {
			s.MemBrand = b
			break
		}
		if m.Maker != "" && s.MemBrand == "" {
			s.MemBrand = m.Maker
		}
	}
	if slotsTotal > 0 {
		s.MemSlots = strconv.Itoa(slotsUsed) + " / " + strconv.Itoa(slotsTotal)
	} else if slotsUsed > 0 {
		s.MemSlots = strconv.Itoa(slotsUsed) + " 条"
	}
	if maxCapKB > 0 {
		s.MemMaxCap = fmtKB(maxCapKB) // 字段单位就是 KB，别再乘 1024
	}

	// 内存类型 / 速率 / 通道：同类聚合，不一致就列全
	if len(memSticks) > 0 {
		types := uniqJoin(memSticks, func(m hwMemStick) string { return m.Type })
		s.MemType = types
		speed := uniqJoin(memSticks, func(m hwMemStick) string { return m.Speed })
		s.MemSpeed = speed
		s.MemUsedSpd = uniqJoin(memSticks, func(m hwMemStick) string { return m.Conf })
			/* 通道数在 SMBIOS 里**没有可靠字段**（Type 5/6 早已淘汰，厂商 OEM 结构各家不同），
			   WMI 也拿不到 → 按铁律**不猜**。
			   旧实现是「≥2 条内存就写双通道」：8 条内存的服务器会写成「8 条 · 双通道」，
			   插错槽的单通道机也写双通道 —— 属于凭空编数据。现在只如实给条数。 */
			if len(memSticks) >= 2 && s.MemType != "" {
				if s.MemType == "DDR" || s.MemType == "DDR2" || s.MemType == "DDR3" || s.MemType == "DDR4" || s.MemType == "DDR5" {
					s.MemChan = strconv.Itoa(len(memSticks)) + " 条"
				}
			}
		if v := uniqJoin(memSticks, func(m hwMemStick) string { return m.Volt }); v != "" {
			s.MemVolt = v
		}
	}

	// CPU 代号：从 CPUID 的家族+型号代号反查（表在 hwspec_cpu.go，表外留空）
	if c.Family > 0 {
		if c.Family == 0x17 || c.Family == 0x19 || c.Family == 0x1A || c.Family == 0x15 || c.Family == 0x10 {
			s.CPUCode = cpuCodenameAMD(c.Family, c.Model)
		} else if code, ok := cpuCodename[int(c.Family)<<8|c.Model]; ok {
			s.CPUCode = code
		} else if code, ok := cpuCodename[c.Family<<8|c.Model]; ok {
			s.CPUCode = code
		}
	}
	// 工艺按「代号」映射：同一代号的整个系列工艺一致，查得到出处，不是逐个型号猜
	if tech := cpuTechByCodename[s.CPUCode]; tech != "" {
		s.CPUTech = tech
	}
	if c.Threads > 0 {
		s.CPUInstr = cpuInstrSet(c)
	}
	// CPU 厂商
	if strings.Contains(strings.ToLower(c.Brand), "intel") {
		s.CPUVendor = "Intel"
	} else if strings.Contains(strings.ToLower(c.Brand), "amd") {
		s.CPUVendor = "AMD"
	}
	/* 插座 / 核心数：SMBIOS 类型 4（Processor Information）
	   注：SMBIOS 里**没有 TDP 字段**，别拿别的偏移当 TDP 用 —— 实测那样会读出「69 W」这种
	   看着很像、实际是 Status 位拼出来的假数。宁可留空。

	   ⚠️ 多路机器必须扫完**所有**类型 4 结构（2026-10-10 修）：
	   SMBIOS 是**每颗物理 CPU 一条类型 4**。旧实现遇到第一条就 break，
	   双路 Xeon/EPYC（32 核 64 线程）会被写成 16 核 32 线程 —— 少报一半，
	   而且用户完全看不出来。现在求和，插座按出现次数标 ×N。 */
	var totCore, totThread int
	missThread := false
	sockCount := map[string]int{}
	var sockOrder []string
	for _, st := range structs {
		if st.Type != 4 {
			continue
		}
		if sock := cleanVendor(st.str(st.u8(0x04))); sock != "" {
			if sockCount[sock] == 0 {
				sockOrder = append(sockOrder, sock)
			}
			sockCount[sock]++
		}
		/* 0x23 Core Count / 0x25 Thread Count（SMBIOS 2.6+）。
		   主板固件自己报的核心数比 CPUID 推出来的更权威（CPUID 遇到屏蔽核心的型号会算多），
		   所以这里**覆盖** CPUID 的结果，不是只在空的时候补。 */
		if n := int(st.u8(0x23)); n > 0 {
			totCore += n
			if th := int(st.u8(0x25)); th > 0 {
				totThread += th
			} else {
				missThread = true /* 有任一颗没报线程数 → 整串就不完整，退回只写核心数（不拿核心数顶线程数） */
			}
		}
	}
	if len(sockOrder) > 0 {
		/* 两颗同型号（双路最常见）写「LGA3647 × 2」，比把同一个名字列两遍清楚 */
		parts := make([]string, 0, len(sockOrder))
		for _, sk := range sockOrder {
			if sockCount[sk] > 1 {
				parts = append(parts, fmt.Sprintf("%s × %d", sk, sockCount[sk]))
			} else {
				parts = append(parts, sk)
			}
		}
		s.CPUSocket = strings.Join(parts, " / ")
	}
	if totCore > 0 {
		if !missThread && totThread > 0 {
			s.CPUCore = strconv.Itoa(totCore) + " 核 " + strconv.Itoa(totThread) + " 线程"
		} else {
			s.CPUCore = strconv.Itoa(totCore) + " 核"
		}
	}

	s.Sources = strings.Join(src, "+")
	/* 机型判断要等内存条解析完才能下结论（SODIMM 是降级证据之一），所以放最后 */
	s.Form = readFormFactor(s.ChassisType, s.Chassis, s.MemSticks)
	/* 品牌 / 型号清洗：品牌翻中文，型号只去噪不翻译 */
	model := firstNonEmpty(s.CPUBrandID, s.CPUVendor)
	s.CPUBrand = cpuBrand(model)
	s.CPUModelCn = cpuModelCN(model)
	s.BoardBrand = brandCN(s.SysVendor)
	return s
}

// firstNonEmpty 取第一个非空字符串
func firstNonEmpty(v ...string) string {
	for _, s := range v {
		if strings.TrimSpace(s) != "" {
			return strings.TrimSpace(s)
		}
	}
	return ""
}

func uniqJoin(items []hwMemStick, f func(hwMemStick) string) string {
	var seen []string
	for _, it := range items {
		v := f(it)
		if v == "" {
			continue
		}
		dup := false
		for _, x := range seen {
			if x == v {
				dup = true
				break
			}
		}
		if !dup {
			seen = append(seen, v)
		}
	}
	return strings.Join(seen, " / ")
}

// cleanVendor 有些主板把厂商名写成占位串
func cleanVendor(s string) string {
	l := strings.ToLower(strings.TrimSpace(s))
	if l == "" || l == "default string" || l == "to be filled by o.e.m." || l == "none" || l == "unknown" || l == "system manufacturer" || l == "system product name" {
		return ""
	}
	return strings.TrimSpace(s)
}

func validSerial(s string) string {
	l := strings.ToLower(strings.TrimSpace(s))
	if l == "" || l == "default string" || l == "to be filled by o.e.m." || l == "none" || l == "0" {
		return ""
	}
	return strings.TrimSpace(s)
}

// normalizeBIOSDate SMBIOS 日期形如 06/14/2018
func normalizeBIOSDate(s string) string {
	p := strings.Split(strings.TrimSpace(s), "/")
	if len(p) == 3 {
		y := p[2]
		if len(y) == 2 {
			if y[0] > '7' {
				y = "19" + y
			} else {
				y = "20" + y
			}
		}
		return y + "-" + p[0] + "-" + p[1]
	}
	return strings.TrimSpace(s)
}

func fmtMHz(mhz int) string {
	if mhz >= 1000 {
		return fmtFloat1(float64(mhz)/1000) + " GHz"
	}
	return strconv.Itoa(mhz) + " MHz"
}

// fmtKB 入参单位是 KB。注意 ≥1 GB 那一档的单位必须是 GB ——
// 写成 MB 会把 64 GB 的最大支持内存显示成「65536 MB」（本机实测踩过）。
func fmtKB(kb int) string {
	if kb >= 1024*1024 {
		return fmtFloat1(float64(kb)/1024/1024) + " GB"
	}
	if kb >= 1024 {
		v := float64(kb) / 1024
		if v == float64(int(v)) {
			return strconv.Itoa(int(v)) + " MB"
		}
		return fmtFloat1(v) + " MB"
	}
	return strconv.Itoa(kb) + " KB"
}

// fmtMBToGB 内存条容量：MB → GB，整数不留小数
func fmtMBToGB(mb int) string {
	if mb <= 0 {
		return ""
	}
	if mb%1024 == 0 {
		return strconv.Itoa(mb/1024) + " GB"
	}
	return fmtFloat1(float64(mb)/1024) + " GB"
}

func fmtFloat1(v float64) string {
	s := strconv.FormatFloat(v, 'f', 2, 64)
	s = strings.TrimRight(s, "0")
	s = strings.TrimSuffix(s, ".")
	return s
}
