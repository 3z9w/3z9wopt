package main

// 工具库 · 系统信息类（info）
// 新增/修改工具：在 init() 里加一条 regTool(...)，详见 toolkit.go 顶部的扩展手册。

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/windows/registry"
)

func init() {
	regTool(ToolItem{
		ID: "info-hw", Cat: "info", Name: "硬件信息总览",
		Desc: "CPU、内存、主板、BIOS、显卡、网卡、硬盘一览",
		Cmd:  "WMI 汇总查询", Risk: "readonly", OutText: true, Timeout: 120,
	}, toolImpl{Run: toolInfoHW})

	regTool(ToolItem{
		ID: "info-updates", Cat: "info", Name: "已安装更新",
		Desc: "系统补丁清单与安装时间",
		Cmd:  "WMI 查询 Win32_QuickFixEngineering", Risk: "readonly", OutText: true, Timeout: 120,
	}, toolImpl{Run: toolInfoUpdates})

	regTool(ToolItem{
		ID: "info-drivers", Cat: "info", Name: "驱动与服务列表",
		Desc: "驱动名称、类型与运行状态详情",
		Cmd:  "driverquery /v", Risk: "readonly", OutText: true, Timeout: 180,
	}, toolImpl{Steps: []toolStep{{exe: "driverquery.exe", args: []string{"/v", "/FO", "TABLE"}}}})

	regTool(ToolItem{
		ID: "info-battery", Cat: "info", Name: "电池健康报告",
		Desc: "笔记本电池容量损耗与循环次数（生成报告文件）",
		Cmd:  "powercfg /batteryreport", Risk: "readonly", Timeout: 120,
		Note: "报告保存到工具库存档目录，生成后给出路径。台式机会提示无电池。",
	}, toolImpl{Steps: []toolStep{{exe: "powercfg.exe", args: []string{"/batteryreport", "/output", batteryReportPath()}}}})

	regTool(ToolItem{
		ID: "info-uptime", Cat: "info", Name: "系统运行时长",
		Desc: "本次开机时长、系统安装时间与启动时刻",
		Cmd:  "系统计时 + 注册表", Risk: "readonly", OutText: true, Timeout: 30,
	}, toolImpl{Run: toolInfoUptime})
}

// ---- 硬件信息总览（WMI） ----
func toolInfoHW(tc *toolCtx) (int, string) {
	line := func(f string, a ...interface{}) { tc.log("line", fmt.Sprintf(f, a...)) }

	tc.log("head", "▶ 系统")
	os0 := wmiFirst("root\\cimv2",
		"SELECT Caption,Version,BuildNumber,OSArchitecture,InstallDate FROM Win32_OperatingSystem",
		[]string{"Caption", "Version", "BuildNumber", "OSArchitecture", "InstallDate"})
	line("系统版本    %s", emptyDash(os0["Caption"]))
	line("内部版本    %s.%s (%s)", emptyDash(os0["Version"]), emptyDash(os0["BuildNumber"]), emptyDash(os0["OSArchitecture"]))
	if t, ok := parseWmiDate(os0["InstallDate"]); ok {
		line("安装时间    %s", t.Format("2006-01-02 15:04:05"))
	}

	tc.log("head", "▶ 处理器")
	if rows, err := wmiQuery("root\\cimv2",
		"SELECT Name,NumberOfCores,NumberOfLogicalProcessors,MaxClockSpeed FROM Win32_Processor",
		[]string{"Name", "NumberOfCores", "NumberOfLogicalProcessors", "MaxClockSpeed"}); err == nil {
		for _, r := range rows {
			line("%s", emptyDash(r["Name"]))
			line("            核心 %s 核 / %s 线程，主频 %s MHz", emptyDash(r["NumberOfCores"]), emptyDash(r["NumberOfLogicalProcessors"]), emptyDash(r["MaxClockSpeed"]))
		}
	} else {
		line("（查询失败：%s）", shortErr(err))
	}

	tc.log("head", "▶ 内存")
	if rows, err := wmiQuery("root\\cimv2",
		"SELECT Capacity,Speed,Manufacturer FROM Win32_PhysicalMemory",
		[]string{"Capacity", "Speed", "Manufacturer"}); err == nil {
		var tot int64
		for i, r := range rows {
			c, _ := strconv.ParseInt(r["Capacity"], 10, 64)
			tot += c
			line("内存条 %d    %s    %s MHz    %s", i+1, fmtBytes(c), emptyDash(r["Speed"]), emptyDash(r["Manufacturer"]))
		}
		line("合计        %s", fmtBytes(tot))
	} else {
		line("（查询失败：%s）", shortErr(err))
	}

	tc.log("head", "▶ 主板与 BIOS")
	mb := wmiFirst("root\\cimv2", "SELECT Manufacturer,Product FROM Win32_BaseBoard",
		[]string{"Manufacturer", "Product"})
	bios := wmiFirst("root\\cimv2", "SELECT Manufacturer,SMBIOSBIOSVersion,ReleaseDate FROM Win32_BIOS",
		[]string{"Manufacturer", "SMBIOSBIOSVersion", "ReleaseDate"})
	line("主板        %s %s", emptyDash(mb["Manufacturer"]), emptyDash(mb["Product"]))
	line("BIOS        %s %s", emptyDash(bios["Manufacturer"]), emptyDash(bios["SMBIOSBIOSVersion"]))
	if t, ok := parseWmiDate(bios["ReleaseDate"]); ok {
		line("BIOS 日期   %s", t.Format("2006-01-02"))
	}

	tc.log("head", "▶ 显卡")
	if rows, err := wmiQuery("root\\cimv2",
		"SELECT Name,AdapterRAM,DriverVersion FROM Win32_VideoController",
		[]string{"Name", "AdapterRAM", "DriverVersion"}); err == nil {
		for _, r := range rows {
			vram, _ := strconv.ParseInt(r["AdapterRAM"], 10, 64)
			line("%s（显存 %s，驱动 %s）", emptyDash(r["Name"]), fmtBytes(vram), emptyDash(r["DriverVersion"]))
		}
	} else {
		line("（查询失败：%s）", shortErr(err))
	}

	tc.log("head", "▶ 物理硬盘")
	if rows, err := wmiQuery("root\\cimv2",
		"SELECT Model,Size,InterfaceType FROM Win32_DiskDrive",
		[]string{"Model", "Size", "InterfaceType"}); err == nil {
		for i, r := range rows {
			sz, _ := strconv.ParseInt(r["Size"], 10, 64)
			line("磁盘 %d      %s  %s  (%s)", i+1, emptyDash(r["Model"]), fmtBytes(sz), emptyDash(r["InterfaceType"]))
		}
	}

	tc.log("head", "▶ 网卡")
	if rows, err := wmiQuery("root\\cimv2",
		"SELECT Name,MACAddress FROM Win32_NetworkAdapter WHERE PhysicalAdapter = TRUE",
		[]string{"Name", "MACAddress"}); err == nil {
		for _, r := range rows {
			if strings.TrimSpace(r["MACAddress"]) == "" {
				continue
			}
			line("%s（MAC %s）", emptyDash(r["Name"]), r["MACAddress"])
		}
	}
	return 0, ""
}

// ---- 已安装更新（WMI，避免 wmic） ----
func toolInfoUpdates(tc *toolCtx) (int, string) {
	rows, err := wmiQuery("root\\cimv2",
		"SELECT HotFixID,Description,InstalledOn,InstalledBy FROM Win32_QuickFixEngineering",
		[]string{"HotFixID", "Description", "InstalledOn", "InstalledBy"})
	if err != nil {
		tc.log("err", shortErr(err))
		return 1, "查询失败"
	}
	if len(rows) == 0 {
		tc.log("line", "未查询到系统更新记录")
		return 0, ""
	}
	type it struct{ id, desc, on string }
	var list []it
	for _, r := range rows {
		list = append(list, it{r["HotFixID"], r["Description"], r["InstalledOn"]})
	}
	// 安装时间新的在前；无日期的排最后
	sort.Slice(list, func(i, j int) bool {
		ti, _ := parseWmiDate(list[i].on)
		tj, _ := parseWmiDate(list[j].on)
		return ti.After(tj)
	})
	tc.log("line", fmt.Sprintf("共 %d 条更新记录", len(list)))
	tc.log("line", strings.Repeat("-", 60))
	tc.log("line", fmt.Sprintf("%-16s %-26s %s", "补丁编号", "说明", "安装时间"))
	tc.log("line", strings.Repeat("-", 60))
	for _, x := range list {
		d := x.on
		if t, ok := parseWmiDate(x.on); ok {
			d = t.Format("2006-01-02")
		}
		tc.log("line", fmt.Sprintf("%-16s %-26s %s", emptyDash(x.id), emptyDash(x.desc), d))
	}
	return 0, ""
}

// ---- 系统运行时长 ----
func toolInfoUptime(tc *toolCtx) (int, string) {
	ms := systemUptimeMs()
	up := time.Duration(ms) * time.Millisecond
	now := time.Now()
	tc.log("head", "▶ 运行状态")
	tc.log("line", "本次开机时长  "+fmtDuration(up))
	tc.log("line", "开机时刻      "+now.Add(-up).Format("2006-01-02 15:04:05"))
	tc.log("line", "当前时间      "+now.Format("2006-01-02 15:04:05"))

	if k, err := registry.OpenKey(registry.LOCAL_MACHINE, `SOFTWARE\Microsoft\Windows NT\CurrentVersion`, registry.QUERY_VALUE); err == nil {
		if v, _, err := k.GetIntegerValue("InstallDate"); err == nil && v > 0 {
			tc.log("line", "系统安装时间  "+time.Unix(int64(v), 0).Format("2006-01-02 15:04:05"))
		}
		k.Close()
	}
	if up < 10*time.Minute {
		tc.log("dim", "刚开机不久；若做系统优化，部分设置此刻可能尚未完全生效。")
	}
	return 0, ""
}
