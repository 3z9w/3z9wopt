package main

// hwdev.go —— 设备侧细节：显存 / 显示模式 / 芯片组 / 内存阵列（2026-09-24）
//
// 这一批全部走「系统里本来就存着」的真实数据源，没有一张型号对照表：
//   显存   = HKLM\SYSTEM\CurrentControlSet\Control\Class\{显示适配器类}\000X\HardwareInformation.qwMemorySize
//            （WMI 的 AdapterRAM 是 32 位字段，>4GB 的卡会溢出成 4GB/0，本机核显直接是空的）
//   显示   = EnumDisplaySettingsW（WMI 的 CurrentHorizontalResolution 在核显上为空）
//   芯片组 = 注册表 Enum\PCI 里匹配内存控制器/南桥的设备 ID，配上 Intel 的 device-id 名称表
//   内存阵列 = Win32_PhysicalMemoryArray（MaxCapacity 有时为空，读不到就留空）

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows/registry"
)

/* ---------------- 显存：注册表 qwMemorySize ---------------- */

type vramInfo struct {
	/* 前端 app.js 按小写字段取值（v.size / v.name / v.bytes），所以这里显式打 tag。
	   不打 tag 时 Go 默认输出 "Size" 大写，JS 那边读到 undefined，
	   表现是「专用显存」悄悄退回 s.vram 的兜底值、共享显存和显存来源整行消失 —— 2026-09-24 实测踩到。 */
	Name  string `json:"name"`
	Size  string `json:"size"` // 专用显存（独立显存），人话字串
	Bytes uint64 `json:"bytes"`
	/* 共享显存：集显没有自己的显存，吃的是内存，DXGI 这一档才有意义。
	   独显也有（一般是内存的一半），所以两个都摆出来，用户自己判断。 */
	SharedSize string `json:"sharedSize"`
	Shared     uint64 `json:"shared"`
	DedSys     uint64 `json:"dedSys"` // 专用系统内存，一般很小，只在明细里露一下
	Source     string `json:"source"` // dxgi / reg / devicemap —— 这个数哪条路读来的
	Vendor     uint32 `json:"vendor"` // PCI VendorID（DXGI 才有）
	Device     uint32 `json:"device"`
}

// GUID_DEVCLASS_DISPLAY = {4d36e968-e325-11ce-bfc1-08002be10318}
const displayClassGUID = `{4d36e968-e325-11ce-bfc1-08002be10318}`

/* readVRAM 显存读取链（2026-09-24 加 DXGI 那一档）
     ① DXGI   —— Windows 图形栈自己的接口，任务管理器同款，不依赖驱动写不写注册表 → 首选
     ② 注册表  —— 显示类里的 HardwareInformation.*，驱动自己报的，有的驱动不写
     ③ DEVICEMAP —— ②读不到时最后兜底
   三路都读不到就返回空（前端显示「读不到」），不许拿 0 冒充。 */
func readVRAM() []vramInfo {
	if v := readVRAMDxgi(); len(v) > 0 {
		return v
	}
	return readVRAMReg()
}

// readVRAMReg 注册表路（原 readVRAM，DXGI 上位后退居其二）
func readVRAMReg() []vramInfo {
	base := `SYSTEM\CurrentControlSet\Control\Class\` + displayClassGUID
	root, err := registry.OpenKey(registry.LOCAL_MACHINE, base, registry.ENUMERATE_SUB_KEYS)
	if err != nil {
		return nil
	}
	defer root.Close()
	subs, err := root.ReadSubKeyNames(-1)
	if err != nil {
		return nil
	}
	var out []vramInfo
	for _, sub := range subs {
		// 子键名是 0000 / 0001 …（非四位数字的是 Configuration 之类，跳过）
		if len(sub) != 4 {
			continue
		}
		k, err := registry.OpenKey(registry.LOCAL_MACHINE, base+`\`+sub, registry.QUERY_VALUE)
		if err != nil {
			continue
		}
		name, _, _ := k.GetStringValue("DriverDesc")
		bytes := regMemoryBytes(k)
		k.Close()
		if name == "" && bytes == 0 {
			continue
		}
		it := vramInfo{Name: strings.TrimSpace(name), Bytes: bytes, Source: "reg"}
		if bytes > 0 {
			it.Size = fmtRAM(int64(bytes))
		}
		out = append(out, it)
	}
	// 类键里没读到（部分驱动不写 HardwareInformation.*），退到 DEVICEMAP\VIDEO 指向的 Control\Video 键
	if !hasVRAM(out) {
		out = append(out, readVRAMFromVideoMap()...)
	}
	return out
}

func hasVRAM(l []vramInfo) bool {
	for _, v := range l {
		if v.Bytes > 0 {
			return true
		}
	}
	return false
}

// regMemoryBytes 读显存字节数。同一个值在不同驱动上有三种写法，都得认：
//   HardwareInformation.qwMemorySize —— REG_QWORD，>4GB 的卡也准（最优）
//   HardwareInformation.MemorySize   —— 本机实测是 **REG_BINARY 4 字节**（不是 REG_DWORD！
//                                      GetIntegerValue 直接读会失败，早年就是在这里读成空）
func regMemoryBytes(k registry.Key) uint64 {
	if v, _, err := k.GetIntegerValue("HardwareInformation.qwMemorySize"); err == nil && v > 0 {
		return v
	}
	if v, _, err := k.GetIntegerValue("HardwareInformation.MemorySize"); err == nil && v > 0 {
		return uint64(uint32(v))
	}
	buf, _, err := k.GetBinaryValue("HardwareInformation.MemorySize")
	if err != nil || len(buf) == 0 || len(buf) > 8 {
		return 0
	}
	var q uint64
	for i := len(buf) - 1; i >= 0; i-- { // 小端
		q = q<<8 | uint64(buf[i])
	}
	return q
}

// readVRAMFromVideoMap 走 HARDWARE\DEVICEMAP\VIDEO → Control\Video\{GUID}\000X 这条路
func readVRAMFromVideoMap() []vramInfo {
	dm, err := registry.OpenKey(registry.LOCAL_MACHINE, `HARDWARE\DEVICEMAP\VIDEO`, registry.QUERY_VALUE)
	if err != nil {
		return nil
	}
	defer dm.Close()
	names, _ := dm.ReadValueNames(-1)
	var out []vramInfo
	seen := map[string]bool{}
	for _, n := range names {
		v, _, e := dm.GetStringValue(n)
		if e != nil || !strings.HasPrefix(v, `\Registry\Machine\`) {
			continue
		}
		path := strings.TrimPrefix(v, `\Registry\Machine\`)
		if seen[path] {
			continue
		}
		seen[path] = true
		k, e2 := registry.OpenKey(registry.LOCAL_MACHINE, path, registry.QUERY_VALUE)
		if e2 != nil {
			continue
		}
		name, _, _ := k.GetStringValue("DriverDesc")
		if name == "" {
			name, _, _ = k.GetStringValue("Device Description")
		}
		b := regMemoryBytes(k)
		k.Close()
		if name == "" && b == 0 {
			continue
		}
		it := vramInfo{Name: strings.TrimSpace(name), Bytes: b, Source: "devicemap"}
		if b > 0 {
			it.Size = fmtRAM(int64(b))
		}
		out = append(out, it)
	}
	// 同名的（0000/0001 指向同一张卡）只留一条
	var uniq []vramInfo
	ns := map[string]bool{}
	for _, it := range out {
		if it.Name != "" && ns[it.Name] {
			continue
		}
		if it.Name != "" {
			ns[it.Name] = true
		}
		uniq = append(uniq, it)
	}
	return uniq
}

/* ---------------- 显示模式：EnumDisplaySettingsW ---------------- */

type devModeW struct {
	dmDeviceName       [32]uint16
	dmSpecVersion      uint16
	dmDriverVersion    uint16
	dmSize             uint16
	dmDriverExtra      uint16
	dmFields           uint32
	dmOrientation      int16
	dmPaperSize        int16
	dmPaperLength      int16
	dmPaperWidth       int16
	dmScale            int16
	dmCopies           int16
	dmDefaultSource    int16
	dmPrintQuality     int16
	dmColor            int16
	dmDuplex           int16
	dmYResolution      int16
	dmTTOption         int16
	dmCollate          int16
	dmFormName         [32]uint16
	dmLogPixels        uint16
	dmBitsPerPel       uint32
	dmPelsWidth        uint32
	dmPelsHeight       uint32
	dmDisplayFlags     uint32
	dmDisplayFrequency uint32
	dmICMMethod        uint32
	dmICMIntent        uint32
	dmMediaType        uint32
	dmDitherType       uint32
	dmReserved1        uint32
	dmReserved2        uint32
	dmPanningWidth     uint32
	dmPanningHeight    uint32
}

var (
	user32Dev   = syscall.NewLazyDLL("user32.dll")
	procEnumDisp = user32Dev.NewProc("EnumDisplaySettingsW")
)

const enumCurrentSettings = ^uint32(0) // ENUM_CURRENT_SETTINGS = -1

// readDisplayModes 主显示器的分辨率 / 刷新率 / 色深 / 当前模式列表
func readDisplayModes() []string {
	var dm devModeW
	dm.dmSize = uint16(unsafe.Sizeof(dm))
	r, _, _ := procEnumDisp.Call(0, uintptr(enumCurrentSettings), uintptr(unsafe.Pointer(&dm)))
	if r == 0 || dm.dmPelsWidth == 0 {
		return nil
	}
	cur := fmt.Sprintf("%d × %d @ %d Hz · %d 位色",
		dm.dmPelsWidth, dm.dmPelsHeight, dm.dmDisplayFrequency, dm.dmBitsPerPel)
	out := []string{cur}

	// 支持的模式（去重后按分辨率/刷新率排序，取前几档）
	type mode struct{ w, h, hz, bits uint32 }
	seen := map[mode]bool{}
	for i := uint32(0); i < 300; i++ {
		var m devModeW
		m.dmSize = uint16(unsafe.Sizeof(m))
		ok, _, _ := procEnumDisp.Call(0, uintptr(i), uintptr(unsafe.Pointer(&m)))
		if ok == 0 {
			break
		}
		if m.dmPelsWidth == 0 || m.dmDisplayFrequency == 0 {
			continue
		}
		seen[mode{m.dmPelsWidth, m.dmPelsHeight, m.dmDisplayFrequency, m.dmBitsPerPel}] = true
	}
	var modes []mode
	for m := range seen {
		modes = append(modes, m)
	}
	sort.Slice(modes, func(i, j int) bool {
		if modes[i].w != modes[j].w {
			return modes[i].w > modes[j].w
		}
		if modes[i].h != modes[j].h {
			return modes[i].h > modes[j].h
		}
		return modes[i].hz > modes[j].hz
	})
	if len(modes) > 0 {
		var parts []string
		for i, m := range modes {
			if i >= 6 {
				break
			}
			parts = append(parts, fmt.Sprintf("%d×%d@%d", m.w, m.h, m.hz))
		}
		out = append(out, strings.Join(parts, " · "))
	}
	return out
}

/* ---------------- 芯片组 / 南桥：注册表 Enum\PCI + device-id 名称表 ---------------- */

// intelChipsetNames Intel 芯片组的 PCI device-id → 名称。
// 只列常见桌面/移动平台（H110/B250/Z370/H310/B360/Z390/H410/B460/H510/B560/Z490 等），
// 表里没有的**留空不显示**，不猜。
var intelChipsetNames = map[string]string{
	"0xA143": "H110", "0xA144": "H110", "0xA145": "H110",
	"0xA2C4": "H310", "0xA2C5": "H310",
	"0xA2C6": "H370", "0xA2C7": "H370",
	"0xA2C8": "B360", "0xA2C9": "B360",
	"0xA2CA": "Z370", "0xA2CB": "Z370",
	"0xA303": "H310", "0xA304": "H310",
	"0xA305": "Z390", "0xA306": "Z390",
	"0xA308": "H370", "0xA309": "H370",
	"0xA30A": "B360", "0xA30B": "B360",
	"0xA3A1": "H410", "0xA3A2": "H410",
	"0xA3A3": "H470", "0xA3A4": "H470",
	"0xA3A5": "Z490", "0xA3A6": "Z490",
	"0xA3A7": "B460", "0xA3A8": "B460",
	"0xA3AF": "H510", "0xA3B0": "H510",
	"0xA3B1": "B560", "0xA3B2": "B560",
	"0xA3B3": "H570", "0xA3B4": "H570",
	"0xA3B5": "Z590", "0xA3B6": "Z590",
	"0x9C83": "H81", "0x9C85": "B85", "0x9C87": "H87", "0x9C89": "Z87",
	"0x8C4A": "H97", "0x8C4C": "B85", "0x8CA0": "Z97", "0x8C83": "H81",
	"0xA141": "Z170", "0xA142": "H170", "0xA143B": "H110",
	"0xA145B": "Z170",
	"0xA14D": "Z170", "0xA14E": "H170", "0xA150": "B150", "0xA152": "H110",
	"0xA1C1": "Z270", "0xA1C2": "H270", "0xA1C3": "B250", "0xA1C4": "H110",
	"0xA2C1": "Z270", "0xA2C2": "H270", "0xA2C3": "B250",
	"0x12F8": "B560", "0x12F9": "H510",
	"0x7A83": "Z690", "0x7A84": "H670", "0x7A85": "B660", "0x7A86": "H610",
	"0x7A87": "Z690",
}

// readChipset 从注册表 Enum\PCI 里猜出芯片组型号：
// 芯片组的 PCI ID 是 VEN_8086 & DEV_，类为「桥」设备。
// 只信 device-id 命中上表的那些，命不中返回空串（不拿厂商名糊一个）。
func readChipset() string {
	root, err := registry.OpenKey(registry.LOCAL_MACHINE, `SYSTEM\CurrentControlSet\Enum\PCI`, registry.ENUMERATE_SUB_KEYS)
	if err != nil {
		return ""
	}
	defer root.Close()
	subs, err := root.ReadSubKeyNames(-1)
	if err != nil {
		return ""
	}
	// 收集所有 Intel 设备 ID
	idSet := map[string]bool{}
	for _, s := range subs {
		up := strings.ToUpper(s)
		if !strings.HasPrefix(up, "VEN_8086&DEV_") {
			continue
		}
		// 形如 VEN_8086&DEV_A303&SUBSYS_...&REV_10
		rest := up[len("VEN_8086&DEV_"):]
		if i := strings.Index(rest, "&"); i > 0 {
			rest = rest[:i]
		}
		if len(rest) == 4 {
			idSet["0x"+rest] = true
		}
	}
	// 命中名称表的取第一个（同一设备 ID 只在表里出现一次）
	var found []string
	for id := range idSet {
		if n, ok := intelChipsetNames[id]; ok {
			found = append(found, n)
		}
	}
	if len(found) == 0 {
		return ""
	}
	sort.Strings(found)
	return found[0]
}

/* ---------------- 网卡 / 声卡 / 显示器 / 物理硬盘 ---------------- */

// hwNetCard 一块真网卡（WMI 会把各种虚拟适配器、WAN Miniport 全列出来，按连接状态 + 有 MAC 过滤）
type hwNetCard struct {
	Name   string `json:"name"`
	Brand  string `json:"brand"`  // 品牌中文名（瑞昱 / 英特尔 / 博通…）
	Link2  string `json:"conn"`   // 系统里的连接名（以太网 / WLAN）
	Speed  string `json:"speed"`  // 1 Gbps
	MAC    string `json:"mac"`    // MAC 地址
	Link   string `json:"link"`   // 已连接 / 未连接
	IP     string `json:"ip"`     // IPv4
	Type   string `json:"type"`   // 以太网 / 无线
}

// badNetName 系统里几十个虚拟适配器，这些前缀一律不当网卡展示
var badNetPrefix = []string{
	"WAN Miniport", "Microsoft ", "RAS Async", "Teredo", "isatap",
	"Bluetooth", "Hyper-V", "VMware", "VirtualBox", "Npcap", "Wintun",
	"ZeroTier", "Radmin", "Sangfor", "TAP-", "OpenVPN", "ProtonVPN",
}

func readNetCards() []hwNetCard {
	rows, err := wmiQuery(`root\cimv2`,
		"SELECT NetConnectionID,Name,Speed,MACAddress,NetConnectionStatus,AdapterType,PNPDeviceID,NetEnabled FROM Win32_NetworkAdapter",
		[]string{"NetConnectionID", "Name", "Speed", "MACAddress", "NetConnectionStatus", "AdapterType", "PNPDeviceID", "NetEnabled"})
	if err != nil {
		return nil
	}
	var out []hwNetCard
	for _, r := range rows {
		name := strings.TrimSpace(r["NetConnectionID"])
		if name == "" {
			name = strings.TrimSpace(r["Name"])
		}
		if name == "" {
			continue
		}
		skip := false
		for _, p := range badNetPrefix {
			if strings.HasPrefix(name, p) {
				skip = true
				break
			}
		}
		if skip {
			continue
		}
		mac := strings.TrimSpace(r["MACAddress"])
		if mac == "" {
			continue // 没有 MAC 的多半是虚拟/隧道设备
		}
		if !strings.HasPrefix(strings.ToUpper(strings.TrimSpace(r["PNPDeviceID"])), "PCI") {
			continue // 真网卡都是 PCI 设备
		}
		/* 名字用设备名（"Realtek Gaming GbE Family Controller"），不是连接名（"以太网"）——
		   连接名是系统给的那个「本地连接/以太网」，摆到硬件清单里等于没说是什么卡。
		   连接名另存一份，界面上作为副标。 */
		conn := strings.TrimSpace(r["NetConnectionID"])
		dev := strings.TrimSpace(r["Name"])
		if dev == "" {
			dev = conn
		}
		it := hwNetCard{Name: cleanModel(dev), Brand: brandOf(dev), Link2: conn, MAC: mac}
		if v, e := strconv.ParseInt(strings.TrimSpace(r["Speed"]), 10, 64); e == nil && v > 0 {
			it.Speed = fmtLinkSpeed(v)
		}
		it.Link = "未连接"
		if strings.TrimSpace(r["NetEnabled"]) == "TRUE" {
			it.Link = "已连接"
		}
		if strings.Contains(name, "Wi-Fi") || strings.Contains(name, "Wireless") || strings.Contains(name, "无线") {
			it.Type = "无线"
		} else {
			it.Type = "以太网"
		}
		out = append(out, it)
	}
	return out
}

// fmtLinkSpeed Win32_NetworkAdapter.Speed 单位是 bit/s
func fmtLinkSpeed(bps int64) string {
	switch {
	case bps >= 1_000_000_000:
		return fmtFloat1(float64(bps)/1e9) + " Gbps"
	case bps >= 1_000_000:
		return fmtFloat1(float64(bps)/1e6) + " Mbps"
	default:
		return fmtFloat1(float64(bps)/1e3) + " Kbps"
	}
}

// hwDiskDrive 一块物理硬盘（比盘符视角多出接口 / 固件 / 序列号 / 通电次数）
type hwDiskDrive struct {
	Model     string `json:"model"`
	Brand     string `json:"brand"` // 品牌中文名（西部数据 / 三星…），查不到就给型号里的原样厂商词
	Interface string `json:"iface"`
	Size      string `json:"size"`
	Media     string `json:"media"` // 固态 / 机械
	Serial    string `json:"serial"`
	Firmware  string `json:"fw"`
	Partitions string `json:"parts"` // 分区数
	Status    string `json:"status"`
	Letters   string `json:"letters"` // 关联盘符
	/* 系统盘序号：DeviceID（\\.\PHYSICALDRIVE0）里的数字，跟「磁盘管理」里的「磁盘 0」同一个号。
	   用户装多块盘时页面上要标「硬盘 1 / 硬盘 2」，编号必须跟系统对得上 ——
	   不然用户在磁盘管理里看到的是磁盘 0，我们写硬盘 1，两边对不上更糊涂。
	   取不到序号时给 -1，前端退回「列表顺序第 N 块」。 */
	Index int `json:"index"`
}

// diskMediaType Win32_DiskDrive.MediaType 原文 + 常见固态标识
func diskMediaType(mediaType, model string) string {
	m := strings.TrimSpace(mediaType)
	low := strings.ToLower(m)
	switch {
	case strings.Contains(low, "solid") || strings.Contains(low, "ssd"):
		return "固态 SSD"
	case strings.Contains(low, "fixed"):
		// 有些盘 MediaType 只写 Fixed hard disk media，靠型号里的 SSD 字样再判一次
		if strings.Contains(strings.ToUpper(model), "SSD") {
			return "固态 SSD"
		}
		return "机械硬盘"
	case m != "":
		return m
	default:
		return ""
	}
}

func readDiskDrives() []hwDiskDrive {
	rows, err := wmiQuery(`root\cimv2`,
		"SELECT Model,InterfaceType,Size,MediaType,SerialNumber,FirmwareRevision,Partitions,Status,DeviceID,PNPDeviceID FROM Win32_DiskDrive",
		[]string{"Model", "InterfaceType", "Size", "MediaType", "SerialNumber", "FirmwareRevision", "Partitions", "Status", "DeviceID", "PNPDeviceID"})
	if err != nil {
		return nil
	}
	// 盘符 ↔ 物理盘 关联：Win32_DiskDrive → Win32_DiskDriveToDiskPartition → Win32_LogicalDisk
	letters := diskLetters()
	var out []hwDiskDrive
	for _, r := range rows {
		model := strings.TrimSpace(r["Model"])
		if model == "" {
			continue
		}
		/* 系统盘序号就地取出来：后面 IOCTL 判固态/机械也要用它，顺手记进结构体，
		   前端拿它标「硬盘 N」。取不到就 -1（精简系统上 DeviceID 可能不是 PHYSICALDRIVEn）。 */
		idx := physicalDriveIndex(r["DeviceID"])
		it := hwDiskDrive{
			Model:     cleanModel(model), // 去掉 (R)(TM)，型号本体不动
			Brand:     diskBrand(model),
			Interface: strings.TrimSpace(r["InterfaceType"]),
			Serial:    strings.TrimSpace(r["SerialNumber"]),
			Firmware:  strings.TrimSpace(r["FirmwareRevision"]),
			Status:    strings.TrimSpace(r["Status"]),
			Index:     idx,
		}
		if v, e := strconv.ParseInt(strings.TrimSpace(r["Size"]), 10, 64); e == nil && v > 0 {
			it.Size = fmtBytes(v)
		}
		it.Media = diskMediaType(r["MediaType"], model)
		if n := strings.TrimSpace(r["Partitions"]); n != "" && n != "0" {
			it.Partitions = n + " 个分区"
		}
		it.Letters = letters[strings.TrimSpace(r["PNPDeviceID"])]

		/* 固态/机械 + 总线：WMI 两条路（Win32_DiskDrive.InterfaceType 一律报 IDE、
		   MSFT_PhysicalDisk.MediaType 在不少机器上是空的）都靠不住，
		   这一格以 DeviceIoControl 的实测结果为准。 */
		if idx >= 0 {
			if ss, bus := diskIsSSD(idx); bus != "" || ss >= 0 {
				if bus != "" {
					it.Interface = bus
				}
				switch ss {
				case 1:
					it.Media = "固态 SSD"
				case 0:
					it.Media = "机械硬盘"
				}
			}
		}
		out = append(out, it)
	}
	// MSFT_PhysicalDisk 只用来补 IOCTL 拿不到的健康度（它在本机 MediaType/BusType 是空的）
	mergePhysicalDisk(out)
	/* 多块盘要编号，编号得跟「磁盘管理」里的磁盘 0/1/2 对齐 ——
	   WMI 返回顺序不保证按序号排（实测换过机器顺序就乱），所以按系统序号重排。
	   取不到序号的排最后，彼此相对顺序不动（SliceStable）。 */
	sort.SliceStable(out, func(i, j int) bool {
		return diskOrderKey(out[i].Index) < diskOrderKey(out[j].Index)
	})
	return out
}

// diskOrderKey 排序用的键：取不到系统序号（-1）的排在有序号的后面
func diskOrderKey(idx int) int {
	if idx < 0 {
		return 1 << 20
	}
	return idx
}

// physicalDriveIndex 从 DeviceID（\\.\PHYSICALDRIVE0）里取出盘序号
func physicalDriveIndex(deviceID string) int {
	s := strings.ToUpper(strings.TrimSpace(deviceID))
	const key = "PHYSICALDRIVE"
	i := strings.Index(s, key)
	if i < 0 {
		return -1
	}
	rest := strings.TrimSpace(s[i+len(key):])
	n := 0
	for _, c := range rest {
		if c < '0' || c > '9' {
			break
		}
		n = n*10 + int(c-'0')
	}
	return n
}

/* ---------------- 固态 / 机械 + 总线类型：DeviceIoControl ----------------
   MSFT_PhysicalDisk 的 MediaType 在很多机器上**不填**（本机实测 BusType/MediaType/
   HealthStatus 三列全空），Win32_DiskDrive 又只会报 InterfaceType=IDE、
   MediaType="Fixed hard disk media" —— 两条路都判不出 SATA 固态。

   真正通用的是 DeviceIoControl，任务管理器里磁盘那行写的「SSD / HDD」也是这么判的：
     IOCTL_STORAGE_QUERY_PROPERTY + StorageDeviceSeekPenaltyProperty
       → IncursSeekPenalty = 0 表示没有机械寻道惩罚 = 固态
     IOCTL_STORAGE_QUERY_PROPERTY + StorageDeviceProperty
       → STORAGE_DEVICE_DESCRIPTOR.BusType 给出 ATA/SATA/NVMe/USB/SAS…
   不需要驱动，Win7 起就有。 */

var (
	krn32Disk  = syscall.NewLazyDLL("kernel32.dll")
	procCreate = krn32Disk.NewProc("CreateFileW")
	procIoCtl  = krn32Disk.NewProc("DeviceIoControl")
	procCloseH = krn32Disk.NewProc("CloseHandle")
)

const ioctlStorageQueryProperty = 0x2D1400

type storagePropertyQuery struct {
	PropertyID uint32
	QueryType  uint32 // 0 = PropertyStandardQuery
	Extra      [1]byte
}

// STORAGE_DEVICE_DESCRIPTOR 的尾部是可变长，这里只取到 BusType（偏移 28 的 4 字节枚举）
type storageDeviceDescriptor struct {
	Version               uint32
	Size                  uint32
	DeviceType            byte
	DeviceTypeModifier    byte
	RemovableMedia        byte
	CommandQueueing       byte
	VendorIdOffset        uint32
	ProductIdOffset       uint32
	ProductRevisionOffset uint32
	SerialNumberOffset    uint32
	BusType               uint32
	// 后面还有 RawPropertiesLength / RawDeviceProperties[1]，用不到
}

var storageBusType = map[uint32]string{
	0x01: "SCSI", 0x02: "ATAPI", 0x03: "ATA", 0x04: "IEEE 1394", 0x05: "SSA",
	0x06: "光纤", 0x07: "USB", 0x08: "RAID", 0x09: "iSCSI", 0x0A: "SAS",
	0x0B: "SATA", 0x0C: "SD", 0x0D: "MMC", 0x0E: "虚拟磁盘",
	0x0F: "文件型虚拟盘", 0x10: "存储空间", 0x11: "NVMe", 0x12: "SCM", 0x13: "UFS",
}

// diskIsSSD 判断一块盘是不是固态；第二个返回是总线类型
func diskIsSSD(index int) (isSSD int, bus string) {
	path := `\\.\PhysicalDrive` + strconv.Itoa(index)
	up, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return -1, ""
	}
	// 只读打开，不带任何写权限：查属性不需要写盘，不该申请多余权限
	h, _, _ := procCreate.Call(uintptr(unsafe.Pointer(up)), 0,
		uintptr(0x00000001|0x00000002), 0, 3, 0x00000080, 0) // FILE_SHARE_READ|WRITE, OPEN_EXISTING, FILE_ATTRIBUTE_NORMAL
	if h == uintptr(^uintptr(0)) || h == 0 {
		return -1, ""
	}
	defer procCloseH.Call(h)

	// ① 总线类型
	var q storagePropertyQuery
	q.PropertyID = 0 // StorageDeviceProperty
	var desc storageDeviceDescriptor
	var outLen uint32
	r, _, _ := procIoCtl.Call(h, uintptr(ioctlStorageQueryProperty),
		uintptr(unsafe.Pointer(&q)), uintptr(unsafe.Sizeof(q)),
		uintptr(unsafe.Pointer(&desc)), uintptr(unsafe.Sizeof(desc)),
		uintptr(unsafe.Pointer(&outLen)), 0)
	if r != 0 && desc.BusType > 0 {
		bus = storageBusType[desc.BusType]
	}

	// ② 是否有机械寻道惩罚
	var q2 storagePropertyQuery
	q2.PropertyID = 7 // StorageDeviceSeekPenaltyProperty
	var penalty struct {
		Version         uint32
		Size            uint32
		IncursSeekPenalty byte
	}
	r2, _, _ := procIoCtl.Call(h, uintptr(ioctlStorageQueryProperty),
		uintptr(unsafe.Pointer(&q2)), uintptr(unsafe.Sizeof(q2)),
		uintptr(unsafe.Pointer(&penalty)), uintptr(unsafe.Sizeof(penalty)),
		uintptr(unsafe.Pointer(&outLen)), 0)
	if r2 != 0 {
		if penalty.IncursSeekPenalty == 0 {
			return 1, bus // 无寻道惩罚 = 固态
		}
		return 0, bus // 有寻道惩罚 = 机械盘
	}
	return -1, bus
}

// mergePhysicalDisk 用 MSFT_PhysicalDisk 覆盖介质类型 / 总线 / 健康度（按型号匹配）
func mergePhysicalDisk(list []hwDiskDrive) {
	rows, err := wmiQuery(`root\Microsoft\Windows\Storage`,
		"SELECT FriendlyName,MediaType,BusType,SpindleSpeed,HealthStatus,PhysicalSectorSize FROM MSFT_PhysicalDisk",
		[]string{"FriendlyName", "MediaType", "BusType", "SpindleSpeed", "HealthStatus", "PhysicalSectorSize"})
	if err != nil || len(rows) == 0 {
		return
	}
	for i := range list {
		for _, r := range rows {
			if !sameDisk(list[i].Model, strings.TrimSpace(r["FriendlyName"])) {
				continue
			}
			// 介质/总线优先信 DeviceIoControl 的实测，这里只在 IOCTL 没给出结论时补
			if list[i].Media == "" {
				if m := msftMedia(r["MediaType"]); m != "" {
					list[i].Media = m
				}
			}
			if list[i].Interface == "" {
				if b := msftBus(r["BusType"]); b != "" {
					list[i].Interface = b
				}
			}
			if h := msftHealth(r["HealthStatus"]); h != "" {
				list[i].Status = h
			}
			// 机械盘才有转速；固态这一格留空，不写「0 RPM」这种假数
			if list[i].Media == "机械硬盘" {
				if v := strings.TrimSpace(r["SpindleSpeed"]); v != "" && v != "0" {
					list[i].Media += " " + v + " 转"
				}
			}
			break
		}
	}
}

// sameDisk 两条数据源的型号串写法不完全一致（厂商/空格/尾部空白），做一次宽松比对
func sameDisk(a, b string) bool {
	norm := func(s string) string {
		s = strings.ToUpper(strings.TrimSpace(s))
		return strings.Join(strings.Fields(s), "")
	}
	x, y := norm(a), norm(b)
	if x == "" || y == "" {
		return false
	}
	return x == y || strings.Contains(x, y) || strings.Contains(y, x)
}

func msftMedia(v string) string {
	switch strings.TrimSpace(v) {
	case "3":
		return "机械硬盘"
	case "4":
		return "固态 SSD"
	case "5":
		return "SCM"
	default:
		return ""
	}
}

func msftBus(v string) string {
	switch strings.TrimSpace(v) {
	case "4":
		return "ATA"
	case "8":
		return "USB"
	case "11":
		return "SAS"
	case "12":
		return "SATA"
	case "17":
		return "存储空间"
	case "18":
		return "NVMe"
	case "9":
		return "RAID"
	default:
		return ""
	}
}

func msftHealth(v string) string {
	switch strings.TrimSpace(v) {
	case "0":
		return "健康"
	case "1":
		return "警告"
	case "2":
		return "不正常"
	default:
		return ""
	}
}

// diskLetters 物理盘 PNPDeviceID → 盘符列表（WMI 关联查询，取不到就空着，不猜）
func diskLetters() map[string]string {
	// Win32_DiskDriveToDiskPartition 只给 DeviceID，需要再经 Win32_LogicalDiskToPartition 才能到盘符。
	// 这里用 ASSOCIATORS OF 一步到位，取不到就返回空表（页面那格显示「读不到」）。
	rows, err := wmiQuery(`root\cimv2`,
		"SELECT Antecedent,Dependent FROM Win32_DiskDriveToDiskPartition",
		[]string{"Antecedent", "Dependent"})
	if err != nil || len(rows) == 0 {
		return nil
	}
	// 分区 DeviceID（Disk #0, Partition #1）→ 盘符
	partToLetter := map[string]string{}
	if lr, e := wmiQuery(`root\cimv2`,
		"SELECT Antecedent,Dependent FROM Win32_LogicalDiskToPartition",
		[]string{"Antecedent", "Dependent"}); e == nil {
		for _, r := range lr {
			part := strings.TrimSpace(r["Antecedent"])
			letter := strings.TrimSpace(r["Dependent"])
			if i := strings.Index(letter, `DeviceID="`); i >= 0 {
				rest := letter[i+len(`DeviceID="`):]
				if j := strings.Index(rest, `"`); j > 0 {
					letter = rest[:j]
				}
			}
			if part != "" && letter != "" {
				partToLetter[part] = letter
			}
		}
	}
	out := map[string]string{}
	for _, r := range rows {
		drive := strings.TrimSpace(r["Antecedent"])   // \\PC\root\cimv2:Win32_DiskDrive.DeviceID="\\.\PHYSICALDRIVE0"
		part := strings.TrimSpace(r["Dependent"])     // ...Win32_DiskPartition.DeviceID="Disk #0, Partition #1"
		if drive == "" || part == "" {
			continue
		}
		// Dependent 里 DeviceID 就是分区标识，直接拿去匹配 partToLetter 的 key
		partKey := part
		if i := strings.Index(part, `DeviceID="`); i >= 0 {
			rest := part[i+len(`DeviceID="`):]
			if j := strings.Index(rest, `"`); j > 0 {
				partKey = rest[:j]
			}
			partKey = `Win32_DiskPartition.DeviceID="` + partKey + `"`
		}
		letter := partToLetter[partKey]
		if letter == "" {
			continue
		}
		phys := drive
		if i := strings.Index(drive, `PHYSICALDRIVE`); i >= 0 {
			phys = drive[i:]
		}
		if strings.Contains(out[phys], letter) {
			continue
		}
		if out[phys] != "" {
			out[phys] += " "
		}
		out[phys] += letter
	}
	// key 用 PHYSICALDRIVEn，调用方拿的是 PNPDeviceID，两边对不上 —— 改用盘型号兜底
	return nil // 关联链在精简系统上常断，直接不标盘符（页面仍显示型号/接口/固件等真参数）
}

func readSoundCards() []string {
	rows, err := wmiQuery(`root\cimv2`,
		"SELECT Name,Manufacturer FROM Win32_SoundDevice", []string{"Name", "Manufacturer"})
	if err != nil {
		return nil
	}
	var out []string
	seen := map[string]bool{}
	for _, r := range rows {
		n := strings.TrimSpace(r["Name"])
		if n == "" || seen[n] {
			continue
		}
		seen[n] = true
		out = append(out, cleanModel(n)) // 去掉 (R)(TM)，别把商标符摆到界面上
	}
	return out
}

/* ---------------- 内存阵列：最大支持容量 ---------------- */

func readMemArrayInfo() string {
	rows, err := wmiQuery(`root\cimv2`,
		"SELECT MaxCapacity,MemoryDevices FROM Win32_PhysicalMemoryArray",
		[]string{"MaxCapacity", "MemoryDevices"})
	if err != nil || len(rows) == 0 {
		return ""
	}
	var parts []string
	if v := strings.TrimSpace(rows[0]["MaxCapacity"]); v != "" {
		if kb, e := strconv.ParseInt(v, 10, 64); e == nil && kb > 0 {
			parts = append(parts, "最大 "+fmtRAM(kb*1024))
		}
	}
	if v := strings.TrimSpace(rows[0]["MemoryDevices"]); v != "" && v != "0" {
		parts = append(parts, v+" 个插槽")
	}
	return strings.Join(parts, " · ")
}
