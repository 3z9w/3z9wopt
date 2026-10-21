package main

// ============ 系统内置应用（UWP / Appx）============

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
	"unicode/utf16"

	"golang.org/x/sys/windows/registry"
)

type uwpRaw struct {
	Name              string
	PackageFullName   string
	PackageFamilyName string
	Publisher         string
	Version           string
	InstallLocation   string
	IsFramework       bool
	NonRemovable      bool
	SignatureKind     int
}

const uwpPS = `[Console]::OutputEncoding=[System.Text.Encoding]::UTF8; ` +
	`Get-AppxPackage | Select-Object Name,PackageFullName,PackageFamilyName,Publisher,Version,InstallLocation,IsFramework,NonRemovable,SignatureKind | ConvertTo-Json -Compress -Depth 2`

// uwpPSDown 置位后本会话内不再尝试 PowerShell 枚举（引擎缺失 80070002，或它没能给出可管理的包）。
// 2026-09-21 首版只认引擎级损坏；2026-10-09（A1）扩到"返回空"：
// 精简系统上 PS 可能启动成功却不给数据，再试也只是每次白等一个 120 秒超时。
//
// 用原子量：GetUwpApps（前端打开页面）与 lookupApp（卸载/查图标前找条目）会并发读它。
// 置位是单向的、只从 false 变 true，所以这里不需要再上锁 —— 最坏情况只是两个调用
// 同时各试一次 PowerShell，结果一致。
var uwpPSDown atomic.Bool

func (a *App) GetUwpApps() []AppEntry {
	ptrs := fetchUwpApps()
	out := make([]AppEntry, len(ptrs))
	for i, e := range ptrs {
		out[i] = *e
	}
	sort.SliceStable(out, func(i, j int) bool {
		return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name)
	})
	return out
}

func fetchUwpApps() []*AppEntry {
	if !uwpPSDown.Load() {
		raw, err := runHiddenOut(120*time.Second, "powershell.exe", "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-Command", uwpPS)
		if err == nil && strings.TrimSpace(raw) != "" {
			body := strings.TrimSpace(raw)
			if i := strings.Index(body, "["); i > 0 {
				body = body[i:]
			}
			var list []uwpRaw
			if jerr := json.Unmarshal([]byte(body), &list); jerr != nil {
				var one uwpRaw
				if jerr2 := json.Unmarshal([]byte(body), &one); jerr2 == nil && one.PackageFullName != "" {
					list = []uwpRaw{one}
				}
			}
			if len(list) > 0 {
				if out := buildUwpEntries(list); len(out) > 0 {
					return out
				}
			}
			// 走到这里说明 PS 有输出但没给出可管理的包（引擎残缺/被策略限制，或本机确实只有
			// 不可卸载的系统包）—— 同样该换注册表这条路，别把"空"当成"答案"。
			(&App{}).DebugLog("[AC-UWP] PS 有输出但没解出可管理包 → 回退注册表枚举")
			uwpPSDown.Store(true)
		} else {
			// PowerShell 路线失败（精简版系统常把 System.Management.Automation.dll 连同
			// Appx 模块一起精简掉：powershell.exe 能启动，但立刻报「加载托管的 Windows
			// PowerShell 失败 80070002」。本机 2026-10-09 实测 v1.0 目录下只剩模块，引擎 DLL 全被删）
			// → 回退注册表枚举。
			// 这里不去猜错误文本指纹：只要 PS 没给出可管理的包就一律降级，本会话不再重试，
			// 免得每个调用点都白等一次 120 秒超时。原因照实写进 trace，供现场排查。
			msg := truncateStr(strings.TrimSpace(decodeMaybeUTF16(raw)), 160)
			if msg == "" {
				if err != nil {
					msg = err.Error()
				} else {
					msg = "无输出"
				}
			}
			(&App{}).DebugLog("[AC-UWP] PowerShell 枚举未产出可管理包 → 回退注册表枚举：" + msg)
			uwpPSDown.Store(true)
		}
	}
	out := fetchUwpAppsReg(nil)
	if len(out) == 0 {
		(&App{}).DebugLog("[AC-UWP] 注册表枚举同样为空：HKCU AppModel\\Repository\\Packages 没有可管理的内置应用")
	}
	return out
}

// buildUwpEntries PowerShell 结果 → 列表（过滤系统组件 / 运行库）
func buildUwpEntries(list []uwpRaw) []*AppEntry {
	var out []*AppEntry
	for _, r := range list {
		if r.PackageFullName == "" {
			continue
		}
		// 2026-09-20 用户反馈：不可卸载的系统组件与运行库不进列表（本机 115 个包里占 68 个，
		// 全列出来只会淹没真正可管理的内容）。留在包缓存里但不返回。
		if r.NonRemovable || r.SignatureKind == 4 || r.IsFramework {
			continue
		}
		e := &AppEntry{
			ID:        "u." + r.PackageFullName,
			Kind:      "uwp",
			Name:      uwpDisplayName(r.Name),
			Version:   r.Version,
			Publisher: psPublisher(r.Publisher),
			InstallLoc: r.InstallLocation,
			Arch:      "appx",
			PFN:       r.PackageFullName,
			Removable: true,
			// 2026-10-09 修 A1（应用卸载页「系统内置」显示 0）：
			// 这里原来写的是 e.Sys = false，而全仓库再没有第二处给 Sys 赋值 ——
			// 前端「系统内置（N）」页签数的就是 a.sys，于是该页签恒为 0（本机实测 62 个软件里内置 0 个）。
			// Sys 在应用卸载页的语义就是「内置应用」（列表条目按 kind 打「桌面 / 内置」标签），置真即可。
			Sys: true,
		}
		if e.Publisher == "" {
			e.Publisher = "—"
		}
		if r.InstallLocation != "" {
			e.IconKey = "u|" + r.InstallLocation
			e.HasIcon = true
			if t := dirCreateTime(r.InstallLocation); t > 0 {
				e.Date, e.DateSrc = t, "dir"
			}
			e.SizeSrc = "none"
			e.SizePending = true
		} else {
			e.SizeSrc = "none"
		}
		e.Cmd = "Remove-AppxPackage -Package " + r.PackageFullName
		out = append(out, e)
	}
	cacheUwp(out)
	return out
}

// uwpRegFramework 运行库/共享框架包（等价于 Get-AppxPackage 的 IsFramework=true），
// 这些是其它应用依赖的公共库，不该出现在「可卸载应用」列表里
var uwpRegFramework = []string{
	".net.native.framework", ".net.native.runtime", "net.native.framework", "net.native.runtime",
	"vclibs", "microsoft.ui.xaml", "microsoft.net.coruntime", "microsoft.winjs",
	"microsoft.services.store.engagement", "microsoft.advertising.xaml",
	// 2026-09-21 实测补漏：Windows App Runtime 全家（Main/Singleton/DDLM/1.x/2.x）
	// 都是应用运行库，用户不能也不该卸载
	"windowsappruntime", "winappruntime", "appruntime.ddlm",
}

// fetchUwpAppsReg 注册表枚举 UWP 包（不依赖 PowerShell）。
// HKCU\...\AppModel\Repository\Packages 登记了当前用户注册的全部包，
// 每个子键含 PackageID / PackageRootFolder / DisplayName。
// 过滤口径与 PowerShell 版对齐：
//   - 安装目录在 %SystemRoot% 下（SystemApps 等）→ 系统组件（≈NonRemovable / SignatureKind=System）
//   - 已知运行库家族（VCLibs / .NET Native / UI.Xaml 等）→ ≈IsFramework
func fetchUwpAppsReg(_ *App) []*AppEntry {
	const path = `Software\Classes\Local Settings\Software\Microsoft\Windows\CurrentVersion\AppModel\Repository\Packages`
	k, err := registry.OpenKey(registry.CURRENT_USER, path, registry.READ)
	if err != nil {
		return nil
	}
	subs, err := k.ReadSubKeyNames(-1)
	k.Close()
	if err != nil {
		return nil
	}

	sysRoot := strings.ToLower(os.Getenv("SystemRoot"))
	var out []*AppEntry
	best := map[string]int{} // family -> out 下标（同族多版本只留最新）
	for _, sub := range subs {
		sk, err := registry.OpenKey(registry.CURRENT_USER, path+`\`+sub, registry.QUERY_VALUE)
		if err != nil {
			continue
		}
		pfn, _, _ := sk.GetStringValue("PackageID")
		root, _, _ := sk.GetStringValue("PackageRootFolder")
		display, _, _ := sk.GetStringValue("DisplayName")
		sk.Close()
		if pfn == "" {
			pfn = sub
		}

		// 解析包全名：Name_version_arch_hash
		parts := strings.Split(pfn, "_")
		if len(parts) < 4 {
			continue
		}
		baseName := parts[0]
		hash := parts[len(parts)-1]

		// 系统组件：装在 Windows 目录下（SystemApps）或 Windows 自签名
		lroot := strings.ToLower(root)
		if lroot == "" || (sysRoot != "" && strings.HasPrefix(lroot, strings.ToLower(sysRoot)+`\`)) || strings.EqualFold(hash, "cw5n1h2txyewy") {
			continue
		}
		// 运行库
		low := strings.ToLower(baseName)
		isFw := false
		for _, fw := range uwpRegFramework {
			if strings.Contains(low, fw) {
				isFw = true
				break
			}
		}
		if isFw {
			continue
		}

		// 版本取自包名；显示名优先用登记的真实名，但下面三种情况要退回映射表：
		//   · 空的
		//   · "@{...?ms-resource://...}" 资源引用（解不出来）
		//   · 登记的就是一串包名（Microsoft.Office.ActionsServer）—— 2026-10-11 实测
		//     本机 40 个内置应用走的是这条路，不改就会把包名原样甩给用户
		//     （判据：含点且不含空格 = 包名；"WorkBuddy"/"Xbox TCUI" 这类真名不动）
		version := parts[1]
		name := display
		if name == "" || strings.HasPrefix(name, "@{") ||
			(strings.Contains(name, ".") && !strings.Contains(name, " ")) {
			name = uwpDisplayName(baseName)
		}
		// 登记名本身就在映射表里（OfficePushNotificationsUtility 这种连写名）再换一次
		if v, ok := uwpNameMap[strings.ToLower(strings.TrimSpace(name))]; ok && v != "" {
			name = v
		}
		pub := "Microsoft Corporation"
		if !strings.EqualFold(hash, "8wekyb3d8bbwe") {
			pub = truncateStr(hash, 24)
		}
		e := &AppEntry{
			ID:         "u." + pfn,
			Kind:       "uwp",
			Name:       name,
			Version:    version,
			Publisher:  pub,
			InstallLoc: root,
			Arch:       "appx",
			PFN:        pfn,
			Removable:  true,
			// 与 buildUwpEntries 同口径：UWP 条目在应用卸载页就是「系统内置」
			// （运行库已在上面按 uwpRegFramework 滤掉，所以这里不会把运行库算成内置）
			Sys: true,
		}
		if root != "" {
			e.IconKey = "u|" + root
			e.HasIcon = true
			if t := dirCreateTime(root); t > 0 {
				e.Date, e.DateSrc = t, "dir"
			}
			e.SizePending = true
		}
		e.SizeSrc = "none"
		e.Cmd = "Remove-AppxPackage -Package " + pfn

		// 同族去重（如 DesktopAppInstaller 新旧版本同时登记）：保留版本号更大的
		family := strings.ToLower(baseName + "_" + hash)
		if idx, ok := best[family]; ok {
			if uwpVerLess(out[idx].Version, version) {
				out[idx] = e
			}
			continue
		}
		best[family] = len(out)
		out = append(out, e)
	}
	cacheUwp(out)
	return out
}

func cacheUwp(out []*AppEntry) {
	appMu.Lock()
	if appCache == nil {
		appCache = map[string]*AppEntry{}
	}
	for _, e := range out {
		appCache[e.ID] = e
	}
	appMu.Unlock()
}

// uwpVerLess 按「.」分段比较版本号，a < b 返回 true
func uwpVerLess(a, b string) bool {
	as := strings.Split(a, ".")
	bs := strings.Split(b, ".")
	for i := 0; i < len(as) || i < len(bs); i++ {
		av, bv := 0, 0
		if i < len(as) {
			av, _ = strconv.Atoi(as[i])
		}
		if i < len(bs) {
			bv, _ = strconv.Atoi(bs[i])
		}
		if av != bv {
			return av < bv
		}
	}
	return false
}

// uwpDisplayName 常见内置应用的中文显示名映射。
// Get-AppxPackage 的 Name 是英文包名（如 Microsoft.WindowsCalculator），
// 用户搜「计算器」根本搜不到 —— 命中映射就换中文名，未命中回退原包名。
var uwpNameMap = map[string]string{
	"microsoft.windowscalculator":           "计算器",
	"microsoft.windows.photos":              "照片",
	"microsoft.photos":                      "照片",
	"microsoft.bingweather":                 "天气",
	"microsoft.bingnews":                    "新闻",
	"microsoft.bingsearch":                  "必应搜索",
	"microsoft.windowsnotepad":              "记事本",
	"microsoft.paint":                       "画图",
	"microsoft.mspaint":                     "画图 3D",
	"microsoft.windowsterminal":             "Windows 终端",
	"microsoft.windowsstore":                "Microsoft Store",
	"microsoft.xboxapp":                     "Xbox",
	"microsoft.xboxgamingoverlay":           "Xbox Game Bar",
	"microsoft.xboxidentityprovider":        "Xbox 身份验证",
	"microsoft.xboxspeechtolumoverlay":      "Xbox 语音",
	"microsoft.microsoftedge":               "Microsoft Edge",
	"microsoft.microsoftedgebeta":           "Microsoft Edge Beta",
	"microsoft.microsoftedge.stable":        "Microsoft Edge",
	"microsoft.windowscamera":               "相机",
	"microsoft.windowsmaps":                 "地图",
	"microsoft.windowscommunicationsapps":   "邮件和日历",
	"microsoft.windowssoundrecorder":        "录音机",
	"microsoft.windows.soundrecorder":       "录音机",
	"microsoft.windowssnippingtool":         "截图工具",
	"microsoft.screensketch":                "截图和草图",
	"microsoft.msnweather":                  "MSN 天气",
	"microsoft.microsoft3dviewer":           "3D 查看器",
	"microsoft.print3d":                     "Print 3D",
	"microsoft.microsoftofficehub":          "Office 入门",
	"microsoft.microsoftsolitairecollection": "Microsoft 纸牌合集",
	"microsoft.microsoftsticky-notes":       "便笺",
	"microsoft.microsoftstickynotes":        "便笺",
	"microsoft.zunemusic":                   "Groove 音乐",
	"microsoft.zunevideo":                   "电影和电视",
	"microsoft.windowsshell":                "Windows Shell",
	"microsoft.onenote":                     "OneNote",
	"microsoft.onenote.desktopapp":          "OneNote",
	"microsoft.oneconnect":                  "付费 Wi-Fi",
	"microsoft.549981c3f5f10":               "Cortana",
	"microsoft.microsoftpcmanager":          "微软电脑管家",
	"microsoft.skypeapp":                    "Skype",
	"microsoft.getstarted":                  "使用技巧",
	"microsoft.gethelp":                     "获取帮助",
	"microsoft.yourphone":                   "手机连接",
	"microsoft.microsoftquickassist":        "快速助手",
	"microsoft.people":                      "人脉",
	"microsoft.powerautomatedesktop":        "Power Automate",
	"microsoft.microsoftjournal":            "Microsoft Journal",
	"microsoft.clouddownload":               "云下载",
	"microsoft.appinstaller":                "应用安装程序",
	"microsoft.desktopappinstaller":         "应用安装程序",
	"microsoft.winget.source":               "winget 源",
	"microsoft.corporation.microsoftquickassist": "快速助手",
	"microsoftteams":                        "Microsoft Teams",

	// 2026-10-11 补：Win10/11 常见内置应用与「媒体/图像扩展」类包。
	// 这些名字在 Get-AppxPackage 里是一串英文包名，界面上跟计算器/相机混在一起，
	// 用户根本认不出。映射不到就退回 prettifyPkgName 拆词。
	"microsoft.heifimageextension":            "HEIF 图像扩展",
	"microsoft.hevcvideoextension":            "HEVC 视频扩展",
	"microsoft.vp9videoextensions":            "VP9 视频扩展",
	"microsoft.webmediaextensions":            "Web 媒体扩展",
	"microsoft.webpimageextension":            "WebP 图像扩展",
	"microsoft.av1videoextension":             "AV1 视频扩展",
	"microsoft.rawimageextension":             "RAW 图像扩展",
	"microsoft.office.actionsserver":          "Office 操作服务",
	"microsoft.officepushnotificationutility": "Office 推送通知",
	"officepushnotificationsutility":          "Office 推送通知",
	"microsoft.xboxspeechtotextoverlay":       "Xbox 语音转文本",
	"microsoft.xboxtcui":                      "Xbox 界面组件",
	"microsoft.xboxgameoverlay":               "Xbox Game Bar",
	"microsoft.xboxgamecallableui":            "Xbox Game Bar",
	"microsoft.gamingapp":                     "Xbox",
	"microsoft.gamingservices":                "游戏服务",
	"microsoft.storepurchaseapp":              "商店购买",
	"microsoft.windowspackagemanager":         "Windows 程序包管理器",
	"microsoft.windowsalarms":                 "时钟",
	"microsoft.windowsfeedbackhub":            "反馈中心",
	"microsoft.windows.searchapp":             "Windows 搜索",
	"microsoft.windows.sechealthui":           "Windows 安全中心",
	"microsoft.windows.shellexperiencehost":   "Windows 外壳体验",
	"microsoft.windows.startmenuexperiencehost": "开始菜单体验",
	"microsoft.lockapp":                       "锁屏",
	"microsoft.aad.brokerplugin":              "账户登录代理",
	"microsoft.accountscontrol":               "账户设置",
	"microsoft.asyncTextService":              "输入法服务",
	"microsoft.bioenrollment":                 "Windows Hello 设置",
	"microsoft.creddialoghost":                "凭据对话框",
	"microsoft.ecapp":                         "轻松使用设置",
	"microsoft.win32webviewhost":              "WebView 宿主",
	"microsoft.ppiprojection":                 "无线投影",
	"microsoft.microsoftedgedevtoolsclient":   "Edge 开发工具客户端",
	"microsoft.mixedreality.portal":           "混合现实门户",
	"microsoft.3dbuilder":                     "3D Builder",
	"microsoft.remotedesktop":                 "Windows 远程桌面",
	"microsoft.todos":                         "Microsoft To Do",
	"microsoft.clipchamp":                     "Clipchamp 视频编辑器",
	"microsoft.mediaplayer":                   "媒体播放器",
	"microsoft.outlookforwindows":             "Outlook",
	"microsoft.copilot":                       "Microsoft Copilot",
	"microsoft.devhome":                       "Dev Home",
	"microsoft.powertoys":                     "PowerToys",
	"microsoftwindows.client.webexperience":   "Windows Web 体验包",
	"microsoftwindows.client.cbs":             "Windows 客户端 CBS",
	"microsoft.languageexperiencepackzh-cn":   "中文(简体)本地体验包",
	"b9eced6f.asuspcassistant":                "华硕电脑助手",
	"nvidia.controlpanel":                     "NVIDIA 控制面板",

	// 登记表里给的就是这种连写名（既不是包名也不是自然语言），按原名当键再补一刀
	"pdftoolextension":               "PDF 工具扩展",
	"xbox tcui":                      "Xbox 界面组件",
}

func uwpDisplayName(pkgName string) string {
	key := strings.ToLower(strings.TrimSpace(pkgName))
	if v, ok := uwpNameMap[key]; ok && v != "" {
		return v
	}
	// 映射表覆盖不到的（第三方 UWP / 新出的系统组件）不再直接甩一串包名给用户：
	// 去掉发布者前缀、点号换空格、CamelCase 拆词 —— Microsoft.Office.ActionsServer
	// → "Office Actions Server"，至少能读出个大概。含空格的名字原样返回，避免拆坏
	// 本来就是自然语言的名（"Xbox Game Bar" 之类）。
	return prettifyPkgName(pkgName)
}

// prettifyPkgName 包名 → 可读文本。只对「看起来像包名」的输入动手（不含空格）。
func prettifyPkgName(s string) string {
	s = strings.TrimSpace(s)
	if s == "" || strings.Contains(s, " ") {
		return s
	}
	// 去掉第一段（发布者：Microsoft / 46239xxx / B9ECED6F 这类），后面的点换成空格
	if i := strings.Index(s, "."); i > 0 && i < len(s)-1 {
		s = strings.ReplaceAll(s[i+1:], ".", " ")
	}
	// CamelCase 拆词：小/数字→大写处断开；连续大写后接小写处断开（HEIFImage→HEIF Image）
	rs := []rune(s)
	var b []rune
	for i, r := range rs {
		if i > 0 && r >= 'A' && r <= 'Z' {
			p := rs[i-1]
			nxt := rune(0)
			if i+1 < len(rs) {
				nxt = rs[i+1]
			}
			lowPrev := (p >= 'a' && p <= 'z') || (p >= '0' && p <= '9')
			digPrev := p >= '0' && p <= '9'
			if lowPrev && !digPrev {
				b = append(b, ' ')
			} else if p >= 'A' && p <= 'Z' && nxt >= 'a' && nxt <= 'z' {
				b = append(b, ' ')
			}
		}
		b = append(b, r)
	}
	return strings.TrimSpace(string(b))
}

// "CN=Microsoft Corporation, O=..., C=US" → "Microsoft Corporation"
func psPublisher(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	for _, part := range strings.Split(s, ",") {
		p := strings.TrimSpace(part)
		if strings.HasPrefix(strings.ToUpper(p), "CN=") {
			v := strings.TrimSpace(p[3:])
			if v != "" {
				return v
			}
		}
	}
	return truncateStr(s, 60)
}

// decodeMaybeUTF16 PowerShell 引擎早期失败（加载托管失败等）时输出是 UTF-16LE，
// 正常脚本输出是 UTF-8。含 \x00 且长度成对就按 UTF-16 解。
func decodeMaybeUTF16(s string) string {
	b := []byte(s)
	if !bytes.Contains(b, []byte{0}) || len(b)%2 != 0 {
		return s
	}
	u := make([]uint16, len(b)/2)
	for i := range u {
		u[i] = uint16(b[2*i]) | uint16(b[2*i+1])<<8
	}
	return strings.TrimRight(string(utf16.Decode(u)), "\x00\r\n ")
}

// RemoveUwp 移除内置应用。allUsers=true 时同时移除预置包（需管理员）
func (a *App) RemoveUwp(pfn string, allUsers bool) string {
	pfn = strings.TrimSpace(pfn)
	if pfn == "" || len(pfn) > 200 || strings.ContainsAny(pfn, "'\"`;\r\n") {
		return "包名不合法"
	}
	e := lookupApp("u." + pfn)
	if e != nil && e.Blocked != "" {
		return e.Blocked
	}
	name := pfn
	if i := strings.Index(pfn, "_"); i > 0 {
		name = pfn[:i]
	}

	// 注意两点（都是实测踩过的坑）：
	// 1. Get-AppxPackage 没有 -Package 参数，-Package 会被 PS 参数缩写唯一匹配到
	//    -PackageTypeFilter，把包名硬转成枚举报错；正确参数是 -Name。
	// 2. -Name 匹配的是「应用名」而非完整包名 PackageFullName，传完整包名永远查不到；
	//    必须用基础名 + 通配符（'Microsoft.XXX*'）。
	// try/catch + UTF8 输出：错误信息经 stdout 返回，避免 stderr GBK 编码乱码。
	// 包不存在时显式报错：空管道会让 Remove-AppxPackage 静默"成功"，界面会误报。
	script := fmt.Sprintf(
		`[Console]::OutputEncoding=[System.Text.Encoding]::UTF8; `+
			`try { $ErrorActionPreference='Stop'; `+
			`$pkgs = Get-AppxPackage -Name '%s*' -ErrorAction Stop; `+
			`if (-not $pkgs) { throw '该应用未安装在当前账户下（可能只是预置包），请用「⋮ → 彻底移除（所有账户）」' }; `+
			`$pkgs | Remove-AppxPackage -ErrorAction Stop; `+
			`Write-Output 'REMOVAL_OK' } catch { Write-Output ('REMOVAL_ERR: ' + $_.Exception.Message); exit 1 }`, name)
	if allUsers {
		if !isElevated() {
			return "彻底移除需要以管理员身份运行本程序"
		}
		script = fmt.Sprintf(
			`[Console]::OutputEncoding=[System.Text.Encoding]::UTF8; `+
				`try { $ErrorActionPreference='Stop'; `+
				`$pkgs = Get-AppxPackage -AllUsers -Name '%s*' -ErrorAction Stop; `+
				`if ($pkgs) { $pkgs | Remove-AppxPackage -AllUsers -ErrorAction Stop }; `+
				// Remove-AppxProvisionedPackage 的 -PackageName 要完整包名（xxx_version_neutral~_hash），
				// 传基础名会报"参数错误"——按 DisplayName 查出真实 PackageName 再删
				`$prov = Get-AppxProvisionedPackage -Online -ErrorAction Stop | Where-Object { $_.DisplayName -eq '%s' }; `+
				`foreach ($p in $prov) { Remove-AppxProvisionedPackage -Online -PackageName $p.PackageName -ErrorAction Stop | Out-Null }; `+
				`Write-Output 'REMOVAL_OK' } catch { Write-Output ('REMOVAL_ERR: ' + $_.Exception.Message); exit 1 }`,
			pfn, name)
	}
	// 主路线：WinRT COM 直调（2026-09-21，免 PowerShell，精简系统也能用；
	// 语义与 Remove-AppxPackage 当前用户一致，链路已实机实证）
	if !allUsers {
		hr, rerr := removeUwpViaWinRT(pfn, 3*time.Minute)
		if rerr == nil {
			appMu.Lock()
			delete(appCache, "u."+pfn)
			appMu.Unlock()
			return ""
		}
		if isDeployResultHR(hr) {
			// 部署引擎的真实裁决（包不存在/被占用/仓库损坏等），直接反馈用户
			return "移除失败：" + rerr.Error()
		}
		// COM 激活类失败（极少见）→ 降级 PS 重试
		(&App{}).DebugLog("[AC-WINRT] remove fail hr=0x" + fmt.Sprintf("%08X", hr) + " → fallback PS")
	}

	out, err := runHiddenOut(180*time.Second, "powershell.exe", "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-Command", script)
	if err != nil {
		msg := decodeMaybeUTF16(out)
		msg = strings.TrimSpace(msg)
		msg = strings.TrimPrefix(msg, "REMOVAL_ERR: ")
		msg = strings.TrimSpace(msg)
		if msg == "" || strings.Contains(msg, "REMOVAL_OK") {
			msg = err.Error()
		}
		// 精简版系统把 PowerShell 托管引擎精简掉了（实机踩坑：报「加载托管的 Windows
		// PowerShell 失败 80070002」），这种情况列表能枚举出来但移除必然失败，明确告诉用户原因
		if strings.Contains(msg, "80070002") || strings.Contains(msg, "加载托管") {
			return "系统 PowerShell 组件缺失或损坏（精简版系统常见），无法移除内置应用。请先用「系统维护」页的 DISM/SFC 修复系统，或换用系统自带「设置 → 应用」处理"
		}
		return "移除失败：" + truncateStr(msg, 200)
	}
	appMu.Lock()
	delete(appCache, "u."+pfn)
	appMu.Unlock()
	return ""
}

// RemoveUwpAll 彻底移除（所有账户，需管理员）
func (a *App) RemoveUwpAll(id string) string {
	e := lookupApp(id)
	if e == nil || e.PFN == "" {
		return "找不到该应用，请重新扫描"
	}
	return a.uninstallUwp(e, true)
}

// uninstallUwp 内置应用移除（与 Win32 卸载同构）：先记卸载前快照会话，
// 再同步执行移除（RemoveUwp 内部等 PowerShell 跑完），最后挂上会话供
// ScanAppResidue 做差分 —— 没有会话的话 UWP 卸载后扫不出任何残留。
func (a *App) uninstallUwp(e *AppEntry, allUsers bool) string {
	sess := newSessionForEntry(e)
	if msg := a.RemoveUwp(e.PFN, allUsers); msg != "" {
		return msg
	}
	appMu.Lock()
	appSess = sess
	appMu.Unlock()
	return ""
}

/* isElevated 与 win_util.go 里的那份逐字等价 —— v1.1 统一放 win_util.go（有说明），这里不再重复定义。 */

