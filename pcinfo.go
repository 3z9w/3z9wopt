package main

// pcinfo.go —— 本机配置页（2026-09-24 新增）
// 服务大厅的「诊断报告」里本来就有这套 WMI 硬件查询，这里把它抽成结构化数据给页面用。
// 规矩照旧：取不到就留空，前端显示「读不到」，绝不编一个看着像的参数。

import (
	"fmt"
	"os"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"
)

// osHostname 计算机名（WMI 取不到时的兜底）
func osHostname() (string, error) { return os.Hostname() }

type HWInfo struct {
	Computer string   `json:"computer"`
	OS       string   `json:"os"`
	OSVer    string   `json:"osVer"`
	Install  string   `json:"install"`
	Uptime   string   `json:"uptime"`
	CPU      string   `json:"cpu"`
	Cores    string   `json:"cores"`
	MemTotal string   `json:"memTotal"`
	MemStick string   `json:"memStick"`
	MemFree  string   `json:"memFree"`
	MemUsed  float64  `json:"memUsed"`   // 已用百分比（按系统可见内存算压力，才是真占用）
	MemUsedT string   `json:"memUsedT"`  // 说人话的已用描述，如「已用 81%」
	GPUs      []string `json:"gpus"`      // 真显卡（按「像不像真卡」排过序）
	GPUBrands []string `json:"gpuBrands"` // 与 gpus 一一对应的品牌中文名（英特尔 / 英伟达 / AMD…）
	Virts     []string `json:"virts"`     // 虚拟显示器（远程/投屏用），单独放，不混进真卡里
	Board    string   `json:"board"`
	BIOS     string   `json:"bios"`
	Disks    []HWDisk `json:"disks"`
}

// fmtRAM 内存容量按 1024 口径（对齐任务管理器：8GiB 内存就是 8.0 GB）。
// 硬盘仍走 fmtBytes 的 1000 口径（对齐硬盘厂商标称）；内存条规格没有 8.4GB 这种东西，
// 用 1000 口径会显示成 8.4 GB —— 小白看了会以为内存坏了，所以这里单独一套。
func fmtRAM(n int64) string {
	if n <= 0 {
		return "—"
	}
	units := []string{"B", "KB", "MB", "GB", "TB"}
	f := float64(n)
	i := 0
	for f >= 1024 && i < len(units)-1 {
		f /= 1024
		i++
	}
	if i == 0 {
		return fmt.Sprintf("%d B", n)
	}
	// 整数不留「.0」：内存条写「4 GB」比「4.0 GB」像人话，8GiB 也正好显示成「8 GB」
	s := strings.TrimSuffix(strconv.FormatFloat(f, 'f', 1, 64), ".0")
	return s + " " + units[i]
}

// humanUptime 开机时长说人话：不足 1 小时说「X 分钟」，不足 1 天说「X 小时 Y 分」，
// 再往上说「X 天 Y 小时」。「0 天 0 小时 12 分」这种机器话小白看不懂。
func humanUptime(ms int64) string {
	if ms <= 0 {
		return ""
	}
	m := int(ms / 1000 / 60)
	switch {
	case m < 1:
		return "不到 1 分钟"
	case m < 60:
		return fmt.Sprintf("%d 分钟", m)
	case m < 60*24:
		return fmt.Sprintf("%d 小时 %d 分", m/60, m%60)
	default:
		return fmt.Sprintf("%d 天 %d 小时", m/60/24, m/60%24)
	}
}

// stickLabel 内存条规格说人话：两条同容量写「2 条 4 GB」，不同容量直接列出来。
func stickLabel(caps []int64, maker string) string {
	if len(caps) == 0 {
		return ""
	}
	s := ""
	same := true
	for i := 1; i < len(caps); i++ {
		if caps[i] != caps[0] {
			same = false
			break
		}
	}
	if same {
		s = fmt.Sprintf("%d 条 %s", len(caps), fmtRAM(caps[0]))
	} else {
		var parts []string
		for _, c := range caps {
			parts = append(parts, fmtRAM(c))
		}
		s = fmt.Sprintf("%d 条（%s）", len(caps), strings.Join(parts, " + "))
	}
	if maker != "" {
		s += " · " + maker
	}
	return s
}

type HWDisk struct {
	Drive string `json:"drive"`
	Label string `json:"label"`
	Total int64  `json:"total"`
	Free  int64  `json:"free"`
	Model string `json:"model"`
	/* SSD 这块盘是不是固态（2026-10-10 补）。
	   ⚠️ 之前这个字段根本不存在，前端 `!!d.ssd` 拿 undefined 取反得 false，
	   于是**每一块盘都被标成「机械」** —— 固态盘也标机械。
	   信息其实一直在手上：下面填 h.Disks 时用的就是 GetDiskList() 的 DiskInfo，
	   那个结构体里有 SSD bool，只是转成 HWDisk 时漏掉了。 */
	SSD bool `json:"ssd"`
}

/* PCFull 本机配置页一次性要的所有数据。
   为什么合成一个接口：这页要 CPUID(+WMI) + SMBIOS + 注册表 + IOCTL 好几路数据，
   分开调会让前端连发四五个请求，页面上一片一片地跳，看着像卡。一次拿全再渲染。 */
type PCFull struct {
	HWInfo
	/* ReadAt 这次读取的完成时刻 —— 页面上摆出来当「确实是刚从这台机器读的」证据。
	   用户明确要求过：不能换了机器还显示原来那台的配置，所以数据一律现读、不留缓存，
	   页面上给个时间戳，用户自己能对得上。 */
	ReadAt    string        `json:"readAt"`
	Spec      hwSpec        `json:"spec"`
	PhysDisks []hwDiskDrive `json:"physDisks"`
	Nets      []hwNetCard   `json:"nets"`
	Sounds    []string      `json:"sounds"`
	VRAMs     []vramInfo    `json:"vrams"`
	Chipset   string        `json:"chipset"`
	Display   []string      `json:"display"` // [0] 当前分辨率 [1] 支持的模式
	/* Fw 固件与启动状态（2026-10-10 新增，见 fwboot.go）。
	   跟上面"硬件规格"是两类东西，单独一个字段：横幅要摆BIOS/启动模式/
	   安全启动/UEFI证书，而这几项现有任何一个字段都不覆盖。
	   任一项读不到就是空字符串，前端写「未获取到」—— 见 fwboot.go 顶部的坑位记录。 */
	Fw FwBoot `json:"fw"`
	/* Temps ACPI 温度区（复用 perf.go 的读法，见 hwlive.go readTemps）。
	   ⚠️ 固件**不告诉我们**这些区是 CPU 封装还是主板哪块，猜了就是编，
	   所以前端必须照实标「温度区」不许冒称「CPU 温度」。
	   CPU/主板真实温度需要签名内核驱动读 MSR，用户态没有接口 —— 见 fwboot.go 说明。 */
	Temps []TempItem `json:"temps"`
}

// GetPCFull 本机配置页的全量数据（合成接口）
//
// ⚠️ 这个函数**每次都真的去读机器**（WMI / SMBIOS / CPUID / 注册表 / DXGI），
// 不读本地缓存、不写本地缓存 —— 换一台机器跑，读出来就是那台机器的配置。
// 谁要是想在这里加缓存提速，请先问用户：他要的就是「读的是当前这台机器」。
func (a *App) GetPCFull() PCFull {
	var f PCFull
	f.HWInfo = a.GetHardwareInfo()
	f.Spec = readHWSpec()
	f.PhysDisks = readDiskDrives()
	f.Nets = readNetCards()
	f.Sounds = readSoundCards()
	f.VRAMs = readVRAM()
	f.Chipset = readChipset()
	f.Display = readDisplayModes()
	f.Fw = readFwBoot()
	f.Temps = readTemps()
	f.ReadAt = time.Now().Format("2006-01-02 15:04:05")
	// 显存：注册表读到的补进 CPUID/WMI 没覆盖的那一格
	if f.Spec.VRAM == "" && len(f.VRAMs) > 0 {
		for _, v := range f.VRAMs {
			if v.Size != "" {
				f.Spec.VRAM = v.Size
				break
			}
		}
	}
	return f
}

// GetHardwareInfo 本机硬件 / 系统信息（WMI，几十到几百毫秒，页面异步取）
func (a *App) GetHardwareInfo() HWInfo {
	var h HWInfo

	os0 := wmiFirst(`root\cimv2`,
		"SELECT Caption,Version,BuildNumber,OSArchitecture,InstallDate,CSName FROM Win32_OperatingSystem",
		[]string{"Caption", "Version", "BuildNumber", "OSArchitecture", "InstallDate", "CSName"})
	h.Computer = strings.TrimSpace(os0["CSName"])
	if h.Computer == "" {
		if n, err := osHostname(); err == nil {
			h.Computer = n
		}
	}
	h.OS = strings.TrimSpace(os0["Caption"])
	ver, bn, arch := strings.TrimSpace(os0["Version"]), strings.TrimSpace(os0["BuildNumber"]), strings.TrimSpace(os0["OSArchitecture"])
	if bn != "" && !strings.Contains(ver, bn) {
		ver += " build " + bn
	}
	h.OSVer = ver
	if arch != "" {
		h.OSVer += "（" + arch + "）"
	}
	if t, ok := parseWmiDate(os0["InstallDate"]); ok {
		h.Install = t.Format("2006-01-02")
	}
	h.Uptime = humanUptime(systemUptimeMs())

	if rows, err := wmiQuery(`root\cimv2`,
		"SELECT Name,NumberOfCores,NumberOfLogicalProcessors FROM Win32_Processor",
		[]string{"Name", "NumberOfCores", "NumberOfLogicalProcessors"}); err == nil && len(rows) > 0 {
		r := rows[0]
		h.CPU = strings.TrimSpace(r["Name"])
		cores, threads := strings.TrimSpace(r["NumberOfCores"]), strings.TrimSpace(r["NumberOfLogicalProcessors"])
		switch {
		case cores != "" && threads != "":
			h.Cores = cores + " 核 / " + threads + " 线程"
		case threads != "":
			h.Cores = threads + " 线程"
		case cores != "":
			h.Cores = cores + " 核"
		default:
			// 精简系统 / 虚拟机常不上报核心数（本机实测就是这样），
			// 退回 runtime.NumCPU()——它是系统真实的逻辑处理器数，不是编的
			h.Cores = strconv.Itoa(runtime.NumCPU()) + " 线程"
		}
	}

	/* 内存总量：优先用「插了几条、每条多大」加出来的标称容量。
	   系统报的 TotalPhysicalMemory 是扣掉硬件保留后的可用量（本机实测 8 GiB 的条子报 8.4 GB，
	   十进制口径），拿来当「这台电脑多大内存」会让人以为内存条是怪规格。
	   条数 / 单条容量同时给出来，副标一句「2 条 4 GB · Kingston」小白一看就懂。 */
	var stickCaps []int64
	var stickMakers []string
	if rows, err := wmiQuery(`root\cimv2`,
		"SELECT Capacity,Manufacturer FROM Win32_PhysicalMemory",
		[]string{"Capacity", "Manufacturer"}); err == nil {
		for _, r := range rows {
			if c, e := strconv.ParseInt(strings.TrimSpace(r["Capacity"]), 10, 64); e == nil && c > 0 {
				stickCaps = append(stickCaps, c)
				stickMakers = append(stickMakers, strings.TrimSpace(r["Manufacturer"]))
			}
		}
	}
	mem := wmiFirst(`root\cimv2`, "SELECT TotalPhysicalMemory FROM Win32_ComputerSystem", []string{"TotalPhysicalMemory"})
	osTot, _ := strconv.ParseInt(strings.TrimSpace(mem["TotalPhysicalMemory"]), 10, 64)
	var nominal int64
	for _, c := range stickCaps {
		nominal += c
	}
	if nominal <= 0 {
		nominal = osTot // 读不到内存条就退回系统可见总量，不编
	}
	if nominal > 0 {
		h.MemTotal = fmtRAM(nominal)
	}
	if len(stickCaps) > 0 {
		maker := ""
		same := true
		for i := 1; i < len(stickMakers); i++ {
			if stickMakers[i] != stickMakers[0] {
				same = false
				break
			}
		}
		if same {
			maker = stickMakers[0]
		}
		h.MemStick = stickLabel(stickCaps, maker)
	}
	// 占用率 / 剩余量走 GlobalMemoryStatusEx —— 和顶栏芯片同一个来源、同一个时点。
	//（占用看的是「系统手里还剩多少」，用系统可见总量算，不是条子的标称容量）
	if tot, avail := memPhys(); tot > 0 {
		h.MemFree = fmtRAM(int64(avail))
		h.MemUsed = float64(tot-avail) / float64(tot) * 100
		h.MemUsedT = fmt.Sprintf("已用 %.0f%%", h.MemUsed)
	}

	/* 显卡：系统会混进一堆虚拟适配器（本机实测有 GameViewer、向日葵 OrayIddDriver，
	   都是远程/投屏用的虚拟显示器），真卡 RX 580 被挤到第 3 个。按「像不像真卡」排：
	   有 VideoProcessor 名称 +2（虚拟适配器这个字段是空的），PNP 设备 ID 以 PCI 开头 +1
	   —— 实测真卡是 PCI\VEN_1002...，虚拟的是 ROOT\DISPLAY\0000。 */
	/* 真卡判据（本机实测校准）：VideoProcessor 非空 + PNP 以 PCI 开头。
	   虚拟的是 ROOT\DISPLAY\xxxx 且 VideoProcessor 为空 —— 本机混进过 GameViewer、
	   向日葵 OrayIddDriver 这类远程/投屏用的虚拟显示器。
	   虚拟卡不作为「显卡」摆在台面上（小白会以为自己有两块显卡），
	   只留一句人话说明有几个虚拟显示器。 */
	type gpu struct {
		name  string
		score int
		ord   int
	}
	var gs, vs []gpu
	if rows, err := wmiQuery(`root\cimv2`,
		"SELECT Name,DriverVersion,VideoProcessor,PNPDeviceID FROM Win32_VideoController",
		[]string{"Name", "DriverVersion", "VideoProcessor", "PNPDeviceID"}); err == nil {
		for i, r := range rows {
			name := strings.TrimSpace(r["Name"])
			if name == "" {
				continue
			}
			sc := 0
			if strings.TrimSpace(r["VideoProcessor"]) != "" {
				sc += 2
			}
			if strings.HasPrefix(strings.ToUpper(strings.TrimSpace(r["PNPDeviceID"])), "PCI") {
				sc++
			}
			if sc == 0 {
				vs = append(vs, gpu{name, sc, i}) // 虚拟显示器：名字照实留，但不冒充显卡
				continue
			}
			if dv := strings.TrimSpace(r["DriverVersion"]); dv != "" {
				name += "（驱动 " + dv + "）"
			}
			/* 品牌单独给一列（中文），型号本体保留原文但去掉 (R)(TM) 这种噪点 ——
			   用户要的是「一眼看懂」：品牌看中文，型号看英文（游戏加加/鲁大师都这么摆）。 */
			gs = append(gs, gpu{cleanModel(name), sc, i})
		}
	}
	byScore := func(l []gpu) {
		sort.SliceStable(l, func(i, j int) bool {
			if l[i].score != l[j].score {
				return l[i].score > l[j].score
			}
			return l[i].ord < l[j].ord
		})
	}
	byScore(gs)
	byScore(vs)
	for _, g := range gs {
		h.GPUs = append(h.GPUs, g.name)
		/* 品牌按显卡名判，判不出再交给前端用 DXGI 的 VendorID 兜底（gpuBrand 认 0x8086/0x10DE/0x1002） */
		h.GPUBrands = append(h.GPUBrands, gpuBrand(g.name, 0))
	}
	for _, g := range vs {
		h.Virts = append(h.Virts, cleanModel(g.name))
	}

	mb := wmiFirst(`root\cimv2`, "SELECT Manufacturer,Product FROM Win32_BaseBoard", []string{"Manufacturer", "Product"})
	h.Board = strings.TrimSpace(emptyDash(mb["Manufacturer"]) + " " + emptyDash(mb["Product"]))
	bios := wmiFirst(`root\cimv2`, "SELECT SMBIOSBIOSVersion,ReleaseDate FROM Win32_BIOS", []string{"SMBIOSBIOSVersion", "ReleaseDate"})
	if t, ok := parseWmiDate(bios["ReleaseDate"]); ok {
		h.BIOS = fmt.Sprintf("%s（%s）", emptyDash(bios["SMBIOSBIOSVersion"]), t.Format("2006-01-02"))
	} else {
		h.BIOS = emptyDash(bios["SMBIOSBIOSVersion"])
	}

	// 盘型号（Win32_DiskDrive 序号与盘符对不上，只作补充，取不到就留空）
	var models []string
	if rows, err := wmiQuery(`root\cimv2`,
		"SELECT Model,MediaType FROM Win32_DiskDrive", []string{"Model", "MediaType"}); err == nil {
		for _, r := range rows {
			if m := strings.TrimSpace(r["Model"]); m != "" {
				models = append(models, m)
			}
		}
	}
	for _, d := range a.GetDiskList() {
		it := HWDisk{Drive: d.Drive, Label: d.Label, Total: d.Total, Free: d.Free, SSD: d.SSD}
		if len(models) == 1 {
			it.Model = models[0] // 只有一块物理盘时才标，多盘不猜是哪块
		}
		h.Disks = append(h.Disks, it)
	}
	return h
}
