package main

// 驱动管理：枚举本机第三方驱动（DISM++ 同款思路）、备份导出、删除。
// 数据源是系统自带的 pnputil（不需要管理员即可枚举/备份），删除需要管理员权限。

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"golang.org/x/sys/windows/registry"
	"github.com/wailsapp/wails/v2/pkg/runtime"
)

// DriverInfo 一个驱动包
type DriverInfo struct {
	OEM      string `json:"oem"`      // oem0.inf（驱动存储里的发布名）
	Name     string `json:"name"`     // 原始 inf 名
	Provider string `json:"provider"` // 提供程序（厂商）
	Class    string `json:"class"`    // 设备类名
	Version  string `json:"version"`  // 驱动版本
	Date     string `json:"date"`     // 发布日期
	Signed   string `json:"signed"`   // 签名者
	Builtin  bool   `json:"builtin"`  // 是否系统内置驱动（pnputil 只列第三方；DISM /all 才有内置）
	Folder   string `json:"folder"`   // 内置驱动在 DriverStore\FileRepository 下的文件夹名（导出时直接拷贝）
	Path     string `json:"path"`     // 内置驱动所在文件夹的完整路径（导出时直接拷这个，避免再拼路径出错）
}

// driverBackupDir 备份根目录
// LOCALAPPDATA 取不到时（服务账户、极端精简系统）会退化成相对路径，写文件就跑到程序目录里去了，
// 所以统一走 appDataDir() 的三级兜底（LOCALAPPDATA → UserCacheDir → TEMP）。
func driverBackupDir() string {
	return filepath.Join(appDataDir(), "driver_backup")
}

// ---- 枚举 ----

// OSSystem 一个可选的 Windows 系统（支持多系统：当前在线系统 + 其他盘上的离线系统）
type OSSystem struct {
	ID     string `json:"id"`     // "online" 或 "offline:D:\"
	Name   string `json:"name"`   // 展示名
	Path   string `json:"path"`   // 系统根路径（离线系统用）
	Online bool   `json:"online"` // 是否当前运行中的系统
}

// GetOSSystems 枚举本机可管理的 Windows：当前系统 + 其他分区里的 Windows（离线）
func (a *App) GetOSSystems() []OSSystem {
	sys := os.Getenv("SystemDrive")
	if sys == "" {
		sys = "C:"
	}
	sys = strings.TrimSuffix(sys, `\`) + `\`
	info := a.GetOSInfo()
	list := []OSSystem{{ID: "online", Name: "当前系统 " + sys + " · " + info["os"], Path: sys, Online: true}}
	for _, d := range fixedDrives() {
		root := d
		if !strings.HasSuffix(root, `\`) {
			root += `\`
		}
		if strings.EqualFold(root, sys) {
			continue
		}
		// 有 Windows\explorer.exe 就认为是一个 Windows 安装
		if _, err := os.Stat(filepath.Join(root, "Windows", "explorer.exe")); err != nil {
			continue
		}
		ed := offlineEdition(root)
		name := "其他系统 " + root
		if ed != "" {
			name += " · Windows " + ed
		}
		list = append(list, OSSystem{ID: "offline:" + root, Name: name, Path: root, Online: false})
	}
	return list
}

// offlineEdition 读离线 Windows 的版本（DISM /get-current-edition），失败留空
func offlineEdition(root string) string {
	out, err := runHiddenOut(60*time.Second, resolveExe("dism.exe"), "/image:"+root, "/get-current-edition")
	if err != nil {
		return ""
	}
	for _, ln := range strings.Split(decodeConsole([]byte(out)), "\n") {
		ln = strings.TrimSpace(ln)
		if ln == "" {
			continue
		}
		if strings.Contains(ln, "版本") || strings.Contains(ln, "edition") {
			if i := strings.Index(ln, ":"); i >= 0 {
				v := strings.TrimSpace(ln[i+1:])
				if v != "" && !strings.Contains(v, "Windows") {
					return v
				}
			}
		}
	}
	return ""
}

// GetDrivers 读取指定系统的驱动列表。
// sysID: "online" = 当前系统；"offline:D:\" = 其他盘上的 Windows。
//
// 速度说明：曾用 dism /online /get-drivers /all 读内置驱动，DISM 服务初始化就要
// 几十秒（2026-09-21 用户实测「太慢」）。改成**直接扫描驱动存储目录**读 inf，
// 快一个数量级，且离线系统也不再需要管理员就能列出。
func (a *App) GetDrivers(sysID string, showAll bool) []DriverInfo {
	offline := strings.HasPrefix(sysID, "offline:")
	sysRoot := suSystemRoot()
	if offline {
		sysRoot = filepath.Join(strings.TrimPrefix(sysID, "offline:"), "Windows")
	}
	store := filepath.Join(sysRoot, "System32", "DriverStore", "FileRepository")
	if !offline {
		// 在线系统：32 位进程访问 System32 会被重定向到 SysWOW64（那里没有 DriverStore），
		// 用 nativeSystem32 直达真实 System32，保证在 32 位编译的机器上也能扫到驱动
		store = filepath.Join(nativeSystem32(), "DriverStore", "FileRepository")
	}

	// 扫一遍驱动存储，拿到所有包：既能建「原始 INF 名 → 文件夹」映射（DISM++ 同款定位方式，
	// 不用 pnputil 那套 oem 间接层），也能在 showAll 时直接当作内置驱动列表。
	all := enumStore(store, nil)
	origFolder := map[string]string{}
	for _, d := range all {
		if d.Name != "" {
			origFolder[strings.ToLower(d.Name)] = d.Path
		}
	}

	var list []DriverInfo
	if offline {
		// 离线系统：不调 pnputil，直接读驱动存储（免管理员、秒出），全部视为可导出
		for _, d := range all {
			dd := d
			dd.Builtin = true // 离线系统只做导出，不提供删除
			list = append(list, dd)
		}
		return sortDrivers(list)
	}
	// 当前系统：第三方走 pnputil（带 oem 号、分类清晰），并补上真实驱动存储路径
	list = []DriverInfo{}
	if o, err := runHiddenOut(60*time.Second, resolveSystemExe("pnputil.exe"), "/enum-drivers"); err == nil {
		list = parsePnpEnum(decodeConsole([]byte(o)))
	} else {
		// pnputil 不可用时**不能就这么交白卷**：Win7 的 pnputil 不支持 /enum-drivers，
		// 精简系统缺组件、被安全策略拦截也会失败（本机实测 schtasks 就被拦过）。
		// 退化成「直接拿驱动存储扫出来的包」—— 少了 oem 号与厂商，但看得到、也照样能导出
		// （导出优先走拷贝 DriverStore，不依赖 pnputil）。只列 inf，不会把目录树全铺出来。
		cleanTrace("驱动：pnputil /enum-drivers 不可用，退化为驱动存储扫描 —— " + shortErr(err))
		for _, d := range all {
			dd := d
			dd.Builtin = false
			list = append(list, dd)
		}
	}
	for i := range list {
		// 用「原始 INF 名」在驱动存储里定位到真实包文件夹 —— 这正是 DISM++ 导出时直接拷的目录，
		// 比 pnputil 的 oem 间接层可靠，且第三方/内置导出走同一套逻辑。
		if p, ok := origFolder[strings.ToLower(list[i].Name)]; ok {
			list[i].Path = p
		}
	}
	if showAll {
		// 内置驱动 = 系统自带、且这台机正在用的（Dism++ 口径）
		skip := map[string]bool{}
		for _, d := range list {
			if d.Name != "" {
				skip[strings.ToLower(d.Name)] = true
			}
		}
		inbox := enumInstalledInbox(skip)
		if len(inbox) == 0 {
			// WMI 不可用（精简系统砍了 WMI、服务被禁）时**不能就这么交白卷**：
			// 退回「驱动存储里有、pnputil 没列出来的全部包」——条数多（本机 663），但看得到也导得出。
			cleanTrace("驱动：Win32_PnPSignedDriver 读不到，内置驱动退回驱动存储全量扫描")
			for _, d := range all {
				if skip[strings.ToLower(d.Name)] {
					continue
				}
				dd := d
				dd.Builtin = true
				inbox = append(inbox, dd)
			}
		}
		list = append(list, inbox...)
	}
	return sortDrivers(list)
}

// enumInstalledInbox 系统自带、且这台机正在用的驱动（Dism++ 的「显示内置驱动」口径）。
//
// 为什么不用「驱动存储里 pnputil 没列出的全部包」：那有 663 条（本机），里面 153 个 Modem、
// 86 个 Net 都是系统自带但**这台机根本没插这些设备**的包，列出来只会淹掉真正要看的项。
// Dism++ 列的是跟这台机有关的 —— 即已经装到设备上的驱动，本机 42 条（第三方 37 条之外的那批）。
//
// 数据源 Win32_PnPSignedDriver（WMI COM，不走 PowerShell/wmic）：同一驱动被多个设备用时会有
// 多行（本机 220 行 → 42 个不同 inf），所以按 inf 名去重；oemNN.inf 是第三方（pnputil 那边已列），跳过。
// 读不到返回 nil，由调用方决定降级路径 —— **绝不返回空列表冒充成功**。
func enumInstalledInbox(skip map[string]bool) []DriverInfo {
	rows, err := wmiQuery(`root\cimv2`,
		"SELECT DeviceName,InfName,DriverVersion,Manufacturer,DriverProviderName,DeviceClass,DriverDate FROM Win32_PnPSignedDriver",
		[]string{"DeviceName", "InfName", "DriverVersion", "Manufacturer", "DriverProviderName", "DeviceClass", "DriverDate"})
	if err != nil || len(rows) == 0 {
		return nil
	}
	store := filepath.Join(nativeSystem32(), "DriverStore", "FileRepository")
	all := enumStore(store, nil) // 只为建「inf 名 → 驱动存储包目录」映射，导出时按目录整包拷
	folder := map[string]string{}
	for _, d := range all {
		if d.Name != "" {
			folder[strings.ToLower(d.Name)] = d.Path
		}
	}
	sysInf := filepath.Join(suSystemRoot(), "inf")

	var out []DriverInfo
	seen := map[string]bool{}
	for _, r := range rows {
		inf := strings.ToLower(strings.TrimSpace(r["InfName"]))
		// oemNN.inf = 第三方（pnputil 已列）；空 inf = 系统自己起的，没有可导出的包
		if inf == "" || isOemName(inf) || seen[inf] || skip[inf] {
			continue
		}
		seen[inf] = true
		prov := strings.TrimSpace(r["DriverProviderName"])
		if prov == "" {
			prov = strings.TrimSpace(r["Manufacturer"])
		}
		d := DriverInfo{
			Name:     inf,
			Provider: prov,
			Class:    classCN(r["DeviceClass"]),
			Version:  strings.TrimSpace(r["DriverVersion"]),
			Date:     wmiDate(r["DriverDate"]),
			Builtin:  true,
			Folder:   filepath.Base(folder[inf]),
			Path:     folder[inf],
		}
		if d.Path == "" {
			// 驱动存储里没这个包（少数 inbox 驱动不走驱动存储）：至少把系统 inf 目录里那份留下来
			p := filepath.Join(sysInf, inf)
			if _, e := os.Stat(p); e == nil {
				d.Path = p
			}
		}
		out = append(out, d)
	}
	return out
}

// wmiDate Win32_PnPSignedDriver 的日期是 DMTF 格式（20060621000000.******+***），
// 取前 8 位转成与 inf / pnputil 一致的 2006/06/21
func wmiDate(s string) string {
	s = strings.TrimSpace(s)
	if len(s) < 8 {
		return ""
	}
	return s[:4] + "/" + s[4:6] + "/" + s[6:8]
}

// enumStore 扫描驱动存储目录里的 inf 得到驱动列表。
// skip 里有的 inf 名（pnputil 已列出的第三方）会被跳过，避免重复。
func enumStore(store string, skip map[string]bool) []DriverInfo {
	entries, err := os.ReadDir(store)
	if err != nil {
		return nil
	}
	var out []DriverInfo
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		infs, _ := filepath.Glob(filepath.Join(store, e.Name(), "*.inf"))
		for _, inf := range infs {
			base := filepath.Base(inf)
			lb := strings.ToLower(base)
			if isOemName(base) || (skip != nil && skip[lb]) {
				continue
			}
			d := parseDriverInf(inf)
			d.Name = base
			d.Builtin = true
			d.Folder = e.Name()                       // 文件夹名（兜底）
			d.Path = filepath.Join(store, e.Name())    // 完整路径（导出直接拷这个，最稳）
			out = append(out, d)
		}
	}
	return out
}

// parseDriverInf 读 inf 的 [Version] 段：类名 / 提供程序 / 版本与日期
func parseDriverInf(path string) DriverInfo {
	var d DriverInfo
	f, err := os.Open(path)
	if err != nil {
		return d
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	lines := 0
	for sc.Scan() && lines < 400 { // Version 段在文件开头，读完就停
		ln := strings.TrimSpace(sc.Text())
		lines++
		if strings.HasPrefix(ln, "[") && !strings.EqualFold(ln, "[Version]") {
			break // 已经过了 Version 段
		}
		lower := strings.ToLower(ln)
		switch {
		case strings.HasPrefix(lower, "class=") && d.Class == "":
			d.Class = classCN(strings.TrimSpace(ln[len("Class="):]))
		case strings.HasPrefix(lower, "provider=") && d.Provider == "":
			p := strings.TrimSpace(ln[len("Provider="):])
			p = strings.Trim(p, "%")
			if strings.EqualFold(p, "MSFT") {
				p = "Microsoft"
			}
			if strings.EqualFold(p, "Microsoft") {
				p = "Microsoft"
			}
			d.Provider = p
		case strings.HasPrefix(lower, "driverver=") && d.Version == "":
			v := strings.TrimSpace(ln[len("DriverVer="):])
			parts := strings.SplitN(v, ",", 2)
			if len(parts) == 2 {
				d.Date = infDate(parts[0])
				d.Version = strings.TrimSpace(parts[1])
			} else {
				d.Version = v
			}
		}
	}
	return d
}

// infDate "06/21/2006" → "2006/06/21"（与 pnputil 的日期格式统一，便于排序）
func infDate(s string) string {
	p := strings.Split(strings.TrimSpace(s), "/")
	if len(p) != 3 {
		return strings.TrimSpace(s)
	}
	return p[2] + "/" + p[0] + "/" + p[1]
}

// classCN inf 里的类名是英文（Display/Net/System…），转成中文方便普通用户看。
// ⚠️ 大小写不敏感：inf 里是驼峰（PrintQueue），WMI 的 DeviceClass 是全大写（PRINTQUEUE），
// 少转一次就会显示成英文类名（WMI 那条路实测全中招）。
func classCN(c string) string {
	if c == "" {
		return "未分类"
	}
	m := classTable()
	if v, ok := m[c]; ok {
		return v
	}
	if v, ok := m[strings.ToLower(c)]; ok {
		return v
	}
	return c
}

// classTable 类名翻译表（懒加载，避免启动时初始化一大坨 map）
func classTable() map[string]string {
	if infClassCN != nil {
		return infClassCN
	}
	m := map[string]string{
		"System": "系统设备", "Display": "显示适配器", "Net": "网络适配器", "USB": "通用串行总线控制器",
		"HIDClass": "人机接口设备", "Mouse": "鼠标和其他指针设备", "Keyboard": "键盘",
		"Media": "声音、视频和游戏控制器", "Image": "图像设备", "Printer": "打印机",
		"Ports": "端口 (COM 和 LPT)", "DiskDrive": "磁盘驱动器", "SCSIAdapter": "存储控制器",
		"CDROM": "DVD/CD-ROM 驱动器", "Processor": "处理器", "Monitor": "监视器", "Battery": "电池",
		"Bluetooth": "蓝牙", "Camera": "照相机", "Volume": "存储卷", "SmartCardReader": "智能卡阅读器",
		"Firmware": "固件", "SoftwareDevice": "软件设备", "Sensor": "传感器", "NetClient": "网络客户端",
		"NetService": "网络服务", "NetTrans": "网络协议", "Modem": "调制解调器", "Computer": "计算机",
		"Security": "安全设备", "Unknown": "其他设备", "WPD": "便携设备", "USBDevice": "USB 设备",
		"MTD": "内存技术设备", "MultiFunction": "多功能设备", "SDHost": "SD 主机适配器", "Extension": "扩展",
		"HDC": "IDE ATA/ATAPI 控制器", "SCSI": "SCSI 和存储控制器", "Dot4": "DOT4 打印机",
		"PrintQueue": "打印队列", "LegacyDriver": "旧版硬件", "VolumeSnapshot": "存储卷影副本",
		"AudioEndpoint": "音频端点", "SoftwareComponent": "软件组件", "AudioProcessingObject": "音频处理对象",
	}
	// 全大写（WMI）与驼峰（inf）都能命中：补一份小写索引（先收集再合并，别边遍历边改）
	low := map[string]string{}
	for k, v := range m {
		low[strings.ToLower(k)] = v
	}
	for k, v := range low {
		if _, ok := m[k]; !ok {
			m[k] = v
		}
	}
	infClassCN = m
	return m
}

// infClassCN 类名翻译表（懒加载，避免启动时初始化一大坨 map）
var infClassCN map[string]string

func sortDrivers(list []DriverInfo) []DriverInfo {
	sort.SliceStable(list, func(i, j int) bool {
		return list[i].Date > list[j].Date
	})
	return list
}

// isOemName 第三方驱动的发布名固定是 oem数字.inf；内置驱动是原始 inf 名
func isOemName(s string) bool {
	s = strings.ToLower(s)
	if !strings.HasPrefix(s, "oem") || !strings.HasSuffix(s, ".inf") {
		return false
	}
	for _, r := range s[3 : len(s)-4] {
		if r < '0' || r > '9' {
			return false
		}
	}
	return len(s) > 7
}

// parsePnpEnum 解析 pnputil /enum-drivers 的输出。
// 兼容两代格式：Win10 旧版字段叫「已发布名称 / 驱动程序包提供程序 / 发布日期 + 驱动程序版本（分行）」；
// Win11 新版叫「发布名称 / 提供程序名称」，且**日期和版本挤在同一行**（值形如 "03/09/2023 23.10.0.2"）。
// 英文系统字段为 Published Name / Provider / Class / Version / Date。
func parsePnpEnum(text string) []DriverInfo {
	var out []DriverInfo
	cur := DriverInfo{}
	flush := func() {
		if cur.OEM != "" {
			out = append(out, cur)
		}
		cur = DriverInfo{}
	}
	for _, ln := range strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n") {
		ln = strings.TrimSpace(ln)
		if ln == "" {
			continue
		}
		kv := strings.SplitN(ln, ":", 2)
		if len(kv) < 2 {
			continue
		}
		k, v := strings.TrimSpace(kv[0]), strings.TrimSpace(kv[1])
		switch {
		case strings.Contains(k, "发布名称") || strings.EqualFold(k, "Published Name"): // 同时命中「已发布名称/发布名称」
			flush()
			cur.OEM = v
		case strings.Contains(k, "原始名称") || strings.EqualFold(k, "Original Name"):
			cur.Name = v
		case strings.Contains(k, "提供程序") || strings.Contains(k, "Provider"):
			cur.Provider = v
		case strings.Contains(k, "类名") || strings.EqualFold(k, "Class Name") || strings.EqualFold(k, "Class"):
			cur.Class = v
		case strings.Contains(k, "签名"):
			cur.Signed = v
		case strings.Contains(k, "日期"):
			cur.Date = v
		case strings.Contains(k, "版本") || strings.Contains(k, "Version"):
			// 新版格式：值 = "MM/DD/YYYY x.y.z.w"（日期在前，空格分隔）
			f := strings.Fields(v)
			if len(f) >= 2 && strings.Contains(f[0], "/") {
				if cur.Date == "" {
					cur.Date = f[0]
				}
				cur.Version = strings.Join(f[1:], " ")
			} else {
				cur.Version = v
			}
		}
	}
	flush()
	return out
}

// ---- 备份 ----

// pickDir 传入的目录为空时用默认备份目录
func pickDir(dir string) string {
	if strings.TrimSpace(strings.Trim(dir, `"`)) == "" {
		return driverBackupDir()
	}
	return strings.TrimSpace(strings.Trim(dir, `"`))
}

// DriverExport 待导出的驱动（name 用于建子目录，方便以后导入时认出是谁）
type DriverExport struct {
	OEM    string `json:"oem"`
	Name   string `json:"name"`
	Cat    string `json:"cat"`    // 列表里的类别（如「显示适配器」），导出时用它建一级目录
	Folder string `json:"folder"` // 内置驱动在驱动存储里的文件夹名（兜底用）
	Path   string `json:"path"`   // 内置驱动所在文件夹的完整路径（导出直接拷这个）
}

// ExportDrivers 批量导出：目录结构和界面上的列表一一对应
//   <备份目录>\<类别>\<驱动名_oem号>\     ← 第三方驱动（pnputil 导出）
//   <备份目录>\<类别>\<驱动存储文件夹名>\  ← 内置驱动 / 离线系统的驱动（拷贝 DriverStore）
// 重装后照着列表里的分类找，就知道该装哪个（2026-09-22 用户要求：含内置驱动）。
// 过程中推 drv:progress 事件；返回空串 = 成功，否则返回失败文本。
func (a *App) ExportDrivers(items []DriverExport, sysID string, dir string) string {
	dir = pickDir(dir)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return "创建备份目录失败：" + shortErr(err)
	}
	store := driverStoreRoot(sysID)
	total := len(items)
	ok, fail := 0, 0
	var fails []string
	var adminHint bool
	for i, it := range items {
		oem := strings.TrimSpace(it.OEM)
		if oem == "" && strings.TrimSpace(it.Folder) == "" && strings.TrimSpace(it.Path) == "" {
			continue
		}
		a.emit("drv:progress", i, total, it.Name)
		// 目标：<备份目录>\<类别>\<驱动名_oem号>，例如 备份目录\显示适配器\nvlt.inf_oem14
		// exportRelDir 已经把类别带进去了，这里直接拼到 dir，避免重复套一层类别目录
		sub := filepath.Join(dir, exportRelDir(it.Cat, it.Name, oem, it.Folder))
		var err error
		var out string
		// 优先用「驱动存储里的真实包文件夹」直接拷贝 —— 这是 DISM++ 的导出方式：
		// 第三方/内置/离线驱动统一走这一条，不依赖 pnputil 的 oem 间接层，
		// 自然避开了 pnputil「目标目录必须存在」「导出后又套一层自动命名子目录」那些怪毛病，
		// 在任何 Windows 上都稳。
		src := strings.TrimSpace(it.Path)
		if src == "" && strings.TrimSpace(it.Folder) != "" {
			src = filepath.Join(store, it.Folder) // 极少数拿不到完整路径时的兜底
		}
		if src != "" {
			if mkErr := os.MkdirAll(filepath.Dir(sub), 0755); mkErr != nil {
				err = mkErr
			} else if st, stErr := os.Stat(src); stErr == nil && !st.IsDir() {
				// 少数系统自带驱动在驱动存储里没有整包，只剩 %SystemRoot%\inf 里那份 inf —— 单文件拷贝
				err = os.MkdirAll(sub, 0755)
				if err == nil {
					err = copyFile(src, filepath.Join(sub, filepath.Base(src)))
				}
			} else {
				err = copyDir(src, sub)
			}
		} else if oem != "" {
			// 终极兜底：连驱动存储路径都拿不到时，才退回 pnputil 导出
			// （pnputil 要求目标目录先存在，且会在里面再建一个自动命名子目录，所以拷贝完要提上来）
			if mkErr := os.MkdirAll(sub, 0755); mkErr != nil {
				err = mkErr
			} else {
				out, err = runHiddenOut(180*time.Second, resolveSystemExe("pnputil.exe"), "/export-driver", oem, sub)
				collapsePnputil(sub)
			}
		} else {
			err = fmt.Errorf("驱动缺少来源路径，无法导出")
		}
		// 关键：pnputil 偶尔退出码非 0 但文件其实已经写出来了，以「目录里有没有 .inf」为准，避免误报失败
		if err != nil && hasInf(sub) {
			err = nil
		}
		if err != nil {
			// 失败留下的是空文件夹，清掉，免得像之前那样出现一堆空目录
			if isEmptyDir(sub) {
				os.Remove(sub)
			}
			fail++
			txt := decodeConsole([]byte(out))
			if looksAdmin(txt, err) {
				adminHint = true
			}
			reason := strings.TrimSpace(txt)
			if reason == "" {
				reason = shortErr(err)
			}
			if isPnpUnsupported(reason) {
				reason += "（驱动导出需要 Windows 10 或更高版本；当前系统的 pnputil 可能不支持 /export-driver）"
			}
			fails = append(fails, it.Name+"（"+reason+"）")
		} else {
			ok++
		}
	}
	a.emit("drv:progress", total, total, "")
	if fail == 0 {
		return ""
	}
	// 失败原因具体化：列出驱动名 + 真实错误，而不是笼统地说「需要管理员权限」
	msg := fmt.Sprintf("导出完成：成功 %d 个，失败 %d 个", ok, fail)
	if adminHint {
		msg += "（其中部分驱动需要以管理员身份运行本软件才能操作）"
	}
	if len(fails) > 0 {
		cap := fails
		if len(cap) > 8 {
			cap = cap[:8]
		}
		msg += "\n失败清单：" + strings.Join(cap, "；")
		if len(fails) > len(cap) {
			msg += " 等"
		}
	}
	// 把完整失败清单写进备份目录，免得 toast 一闪而过看不清
	if logErr := os.WriteFile(filepath.Join(dir, "导出失败清单.txt"), []byte(strings.Join(fails, "\n")), 0644); logErr == nil {
		msg += "\n完整清单已保存到：导出失败清单.txt"
	}
	return msg
}

// driverStoreRoot 驱动存储目录
//   - 离线系统：读 <盘>\Windows\System32（另一块盘的 Windows 不会被 WOW64 重定向，直接用字面路径）
//   - 在线系统：用 nativeSystem32，绕开 32 位进程的 SysWOW64 重定向，保证能找到真正的 DriverStore
func driverStoreRoot(sysID string) string {
	if strings.HasPrefix(sysID, "offline:") {
		sysRoot := filepath.Join(strings.TrimPrefix(sysID, "offline:"), "Windows")
		return filepath.Join(sysRoot, "System32", "DriverStore", "FileRepository")
	}
	return filepath.Join(nativeSystem32(), "DriverStore", "FileRepository")
}

// copyDir 递归拷贝目录（内置驱动没有 oem 号，只能整文件夹拷）
func copyDir(src, dst string) error {
	entries, err := os.ReadDir(src)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dst, 0755); err != nil {
		return err
	}
	for _, e := range entries {
		s := filepath.Join(src, e.Name())
		d := filepath.Join(dst, e.Name())
		if e.IsDir() {
			if err := copyDir(s, d); err != nil {
				return err
			}
			continue
		}
		if err := copyFile(s, d); err != nil {
			return err
		}
	}
	return nil
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()
	_, err = io.Copy(out, in)
	return err
}

// exportRelDir 导出时的相对路径：<类别>\<驱动名_oem号> 或 <类别>\<驱动存储文件夹名>
func exportRelDir(cat, name, oem, folder string) string {
	n := strings.TrimSpace(name)
	if n == "" {
		n = strings.TrimSpace(oem)
	}
	leaf := safeDirName(n)
	if o := safeDirName(oem); o != "" && !strings.EqualFold(leaf, o) {
		leaf = leaf + "_" + o
	}
	if leaf == "" {
		leaf = safeDirName(folder)
	}
	if leaf == "" {
		leaf = "driver"
	}
	if c := safeDirName(cat); c != "" {
		return filepath.Join(c, leaf)
	}
	return leaf
}

// ExportAllDrivers 一键导出整个系统的全部驱动到用户选择的目录。
//
// 2026-10-12 重写：原实现走 PowerShell 的 Export-WindowsDriver cmdlet。精简系统 / 受损系统上
// PowerShell 整条不可用（实测本机报 80070002「组件缺失」），一键导出会 100% 失败 —— 和启动项
// 模块踩的是同一个坑。现在改成两条都不依赖脚本宿主的路：
//   ① 当前系统 → pnputil /export-driver * <dir>（系统自带，实测本机 37 个包全出）
//   ② 离线系统 → 直接拷 DriverStore 里已经枚举出来的包目录（连 pnputil 都不需要）
// ① 失败时自动降级到 ②，保证只要能列出来就能导出去。
func (a *App) ExportAllDrivers(sysID string, dir string) string {
	dir = pickDir(dir)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return "创建备份目录失败：" + shortErr(err)
	}
	a.emit("drv:progress", 0, 1, "正在导出全部驱动，请稍候…")
	offline := strings.HasPrefix(sysID, "offline:")
	if !offline {
		out, err := runHiddenOut(30*time.Minute, resolveSystemExe("pnputil.exe"), "/export-driver", "*", dir)
		// pnputil 偶尔退出码非 0 但文件已经写出来了，以「目录里有没有 .inf」为准
		if err == nil || hasInf(dir) {
			a.emit("drv:progress", 1, 1, "")
			return ""
		}
		cleanTrace("驱动：pnputil 全量导出失败，降级为逐包拷贝 —— " + shortErr(err) + " " + strings.TrimSpace(decodeConsole([]byte(out))))
	}
	// 离线系统，或在线的 pnputil 全量导出不可用：用枚举结果走 ExportDrivers 的逐包拷贝
	list := a.GetDrivers(sysID, true)
	if len(list) == 0 {
		a.emit("drv:progress", 1, 1, "")
		if offline {
			return "没有读到该系统的驱动，无法导出"
		}
		return "没有读到驱动，无法导出"
	}
	items := make([]DriverExport, 0, len(list))
	for _, d := range list {
		items = append(items, DriverExport{OEM: d.OEM, Name: d.Name, Cat: d.Class, Folder: d.Folder, Path: d.Path})
	}
	return a.ExportDrivers(items, sysID, dir)
}

// ---- 删除 ----

// DeleteDriver 从驱动存储删除指定驱动（需管理员；已安装且在使用中的要先 /uninstall）
func (a *App) DeleteDriver(oem string) string {
	oem = strings.TrimSpace(oem)
	if oem == "" {
		return "缺少驱动标识"
	}
	out, err := runHiddenOut(180*time.Second, resolveSystemExe("pnputil.exe"), "/delete-driver", oem, "/uninstall", "/force")
	txt := decodeConsole([]byte(out))
	if err != nil {
		if strings.Contains(txt, "正在使用") || strings.Contains(txt, "in use") {
			return "该驱动正在被设备使用，无法删除（先卸载对应设备再试）"
		}
		return "删除失败：" + shortErr(err) + "（需要管理员权限）"
	}
	return ""
}

// hasInf 判断目录里是否已经导出了 .inf 文件（pnputil 偶尔退出码非0但文件已写出，用它兜底）
func hasInf(dir string) bool {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false
	}
	for _, e := range entries {
		if e.IsDir() {
			if hasInf(filepath.Join(dir, e.Name())) {
				return true
			}
			continue
		}
		if strings.EqualFold(filepath.Ext(e.Name()), ".inf") {
			return true
		}
	}
	return false
}

// collapsePnputil pnputil 会在目标目录里再建一个它自己命名的子目录（如 oem14.inf_amd64_xxx），
// 把该子目录里的文件/子目录上提到目标目录根，并删除空的子目录，
// 让导出的驱动文件直接躺在 <驱动名> 文件夹里，而不是又套一层自动命名目录。
func collapsePnputil(dir string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		inner := filepath.Join(dir, e.Name())
		if !hasInf(inner) {
			continue // 只处理 pnputil 真正导出的子目录
		}
		ies, _ := os.ReadDir(inner)
		for _, f := range ies {
			dst := filepath.Join(dir, f.Name())
			if _, statErr := os.Stat(dst); statErr == nil {
				continue // 同名已存在则跳过，避免覆盖
			}
			_ = os.Rename(filepath.Join(inner, f.Name()), dst)
		}
		_ = os.Remove(inner) // 内容已上提，删掉空的自动命名目录
	}
}

// isPnpUnsupported 判断 pnputil 输出是否是「系统不支持该命令/选项」（多见于 Windows 7/8
// 这种没有 /export-driver 的旧系统），好给出明确的升级系统提示，而不是笼统地说失败。
func isPnpUnsupported(txt string) bool {
	s := strings.ToLower(txt)
	for _, k := range []string{"不支持", "unknown option", "unrecognized", "not supported", "unsupported", "用法", "usage", "不是内部或外部", "不是内部"} {
		if strings.Contains(s, k) {
			return true
		}
	}
	return false
}

// isEmptyDir 判断目录是否为空（导出失败的空文件夹要清掉，免得留一堆空目录）
func isEmptyDir(p string) bool {
	entries, err := os.ReadDir(p)
	if err != nil {
		return false
	}
	return len(entries) == 0
}

// looksAdmin 从命令输出/错误里判断是否真和管理员权限有关（不要一失败就赖管理员）
func looksAdmin(txt string, err error) bool {
	s := strings.ToLower(txt) + " " + strings.ToLower(err.Error())
	for _, k := range []string{"拒绝访问", "需要管理员", "administrator", "access is denied", "privileg", "权限"} {
		if strings.Contains(s, k) {
			return true
		}
	}
	return false
}

// safeDirName 把文本变成合法的 Windows 目录名：保留中文/字母/数字/空格等，
// 只去掉 \ / : * ? " < > | 这些非法字符（类别名是中文，不能按 ASCII 过滤）。
func safeDirName(s string) string {
	s = strings.TrimSpace(s)
	s = strings.TrimSuffix(s, ".inf")
	s = strings.TrimSuffix(s, ".INF")
	var b strings.Builder
	for _, r := range s {
		switch r {
		case '\\', '/', ':', '*', '?', '"', '<', '>', '|':
			continue
		}
		if r < 32 {
			continue
		}
		b.WriteRune(r)
	}
	out := strings.TrimRight(b.String(), " .")
	if out == "" {
		return ""
	}
	return out
}

// GetOSInfo 系统信息（顶部信息条用）：Windows 版本 + 架构
func (a *App) GetOSInfo() map[string]string {
	// 直接读注册表，不调 reg.exe：精简版系统常把 reg.exe 移除，那样首页/顶栏
	// 的系统版本会整行为空。WOW64_64KEY 与原 reg.exe 的默认视图保持一致。
	regVal := func(name string) string {
		k, err := registry.OpenKey(registry.LOCAL_MACHINE,
			`SOFTWARE\Microsoft\Windows NT\CurrentVersion`,
			registry.QUERY_VALUE|registry.WOW64_64KEY)
		if err != nil {
			return ""
		}
		defer k.Close()
		s, _, err := k.GetStringValue(name)
		if err != nil {
			return ""
		}
		return strings.TrimSpace(s)
	}
	arch := os.Getenv("PROCESSOR_ARCHITECTURE")
	switch arch {
	case "AMD64":
		arch = "x64"
	case "ARM64":
		arch = "ARM64"
	default:
		arch = "x86"
	}
	osStr := strings.TrimSpace(strings.TrimSpace(regVal("ProductName")) + " " + regVal("DisplayVersion")) + " (" + arch + ")"
	return map[string]string{"os": osStr}
}

// SelectFolder 弹出系统目录选择框，取消返回空
func (a *App) SelectFolder(title, defDir string) string {
	if a.ctx == nil {
		return ""
	}
	if strings.TrimSpace(title) == "" {
		title = "选择文件夹"
	}
	dir, err := runtime.OpenDirectoryDialog(a.ctx, runtime.OpenDialogOptions{
		Title:            title,
		DefaultDirectory: defDir,
	})
	if err != nil || dir == "" {
		return ""
	}
	return dir
}

// AddDriver 从文件夹安装驱动（递归所有 .inf），成功返回空串
func (a *App) AddDriver(dir string) string {
	dir = strings.TrimSpace(strings.Trim(dir, `"`))
	if dir == "" {
		return "未选择文件夹"
	}
	out, err := runHiddenOut(15*time.Minute, resolveExe("pnputil.exe"),
		"/add-driver", filepath.Join(dir, "*.inf"), "/subdirs", "/install")
	txt := decodeConsole([]byte(out))
	if err != nil {
		if strings.Contains(txt, "管理员") || strings.Contains(txt, "administrator") || strings.Contains(txt, "拒绝访问") {
			return "安装失败：需要管理员权限"
		}
		return "安装失败：" + shortErr(err)
	}
	return ""
}
