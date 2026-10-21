package main

// ============ 应用清理：程序卸载 / 残留扫描 ============
// 卸载闭环：启动卸载 → 轮询「注册表键消失 + 安装目录消失 + 进程退出」→ 残留差分扫描 → 用户勾选清理

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

// ---------- 数据结构 ----------

type AppEntry struct {
	ID          string `json:"id"`
	Kind        string `json:"kind"` // win32 | uwp
	Name        string `json:"name"`
	Version     string `json:"version"`
	Publisher   string `json:"publisher"`
	InstallLoc  string `json:"installLoc"`
	Bytes       int64  `json:"bytes"`
	Files       int    `json:"files"`
	SizeSrc     string `json:"sizeSrc"`     // reg(注册表申报) dir(实测) none(未知)
	SizePending bool   `json:"sizePending"` // 待后台实测
	Date        int64  `json:"date"`        // unix 秒，0 未知
	DateSrc     string `json:"dateSrc"`     // reg | dir | none
	LastUsed    int64  `json:"lastUsed"`
	NeverRan    bool   `json:"neverRan"`
	IconKey     string `json:"iconKey"`
	HasIcon     bool   `json:"hasIcon"`
	Arch        string `json:"arch"` // x64 | x86 | user | appx
	Blocked     string `json:"blocked"`
	ForceOnly   bool   `json:"forceOnly"`
	Cmd         string `json:"cmd"`
	Sys         bool   `json:"sys"`
	Removable   bool   `json:"removable"`
	Framework   bool   `json:"framework"`
	PFN         string `json:"pfn"`

	regRoot   registry.Key
	regPath   string
	uninstall string
	mainExe   string
}

type ResidueItem struct {
	ID     string `json:"id"`
	Kind   string `json:"kind"` // reg | dir
	Path   string `json:"path"`
	Val    string `json:"val"` // 非空 = 注册表「值」级残留（MuiCache / Run 启动项，只删值不删键）
	Bytes  int64  `json:"bytes"`
	Files  int    `json:"files"`
	Risk   string `json:"risk"` // high | mid | low —— 属于这个程序的把握（不是"删它的风险"）
	Reason string `json:"reason"`
	App    string `json:"app"`
	/* Safe / Guard 是**能不能安全删**的裁决（2026-10-11 加，用户要求「尽量安全别激进」）：
	   · Safe=true   —— 确定属于这个程序（键名/目录名命中程序名、发布者、GUID、包名），可放心删
	   · Safe=false  —— 判不准或落在系统区，Guard 写明原因；前端默认**不勾选**，
	                    后端 CleanAppResidue 也会直接拒绝（防前端绕过）
	   注意 Risk 与 Safe 不是一回事：Risk 说"是不是它的"，Safe 说"删了会不会出事"。 */
	Safe  bool   `json:"safe"`
	Guard string `json:"guard"`
}

type ResidueResult struct {
	ID     string `json:"id"`
	Path   string `json:"path"`
	Kind   string `json:"kind"`
	Bytes  int64  `json:"bytes"`
	Err    string `json:"err"`
	Backup string `json:"backup"` // 删之前导出的 .reg 备份路径（仅整键删除有）
}

type UninstallStatus struct {
	State    string   `json:"state"` // running | done | timeout
	Elapsed  int      `json:"elapsed"`
	RegGone  bool     `json:"regGone"`
	DirGone  bool     `json:"dirGone"`
	ProcN    int      `json:"procN"`
	Procs    []string `json:"procs"`
	LeftReg  int      `json:"leftReg"`
	LeftDir  int      `json:"leftDir"`
	Name     string   `json:"name"`
	Message  string   `json:"message"`
	HasForce bool     `json:"hasForce"`
}

// ---------- 全局状态 ----------

var (
	appMu    sync.Mutex
	appCache = map[string]*AppEntry{}
	appSess  *uninstSession

	resPoolMu sync.Mutex
	resPool   = map[string]ResidueItem{}
	resSeq    int
)

type uninstSession struct {
	mu      sync.Mutex
	id      string
	name    string
	kind    string
	pfn     string
	loc     string
	regKeys []string
	dirs    []string
	procs   []string
	before  map[string]bool
	uwpReg  []string
	residue []ResidueItem
	started time.Time
	state   string
	scanned bool
}

var uninstallRoots = []struct {
	root registry.Key
	path string
	tag  string
}{
	{registry.LOCAL_MACHINE, `SOFTWARE\Microsoft\Windows\CurrentVersion\Uninstall`, "hk64"},
	{registry.LOCAL_MACHINE, `SOFTWARE\WOW6432Node\Microsoft\Windows\CurrentVersion\Uninstall`, "hk32"},
	{registry.CURRENT_USER, `SOFTWARE\Microsoft\Windows\CurrentVersion\Uninstall`, "hku"},
}

var (
	kbPatchRe  = regexp.MustCompile(`(?i)^(kb\d{6,}|security update|update for|hotfix for|补丁)`)
	guidRe     = regexp.MustCompile(`\{[0-9A-Fa-f]{8}-[0-9A-Fa-f]{4}-[0-9A-Fa-f]{4}-[0-9A-Fa-f]{4}-[0-9A-Fa-f]{12}\}`)
	winEnvRe   = regexp.MustCompile(`%([^%]+)%`)
	noRemoveRe = regexp.MustCompile(`(?i)uninstallstring|uninstall`)

	// runtimeCompRe 运行库 / 运行组件：SystemComponent=1 且名字命中这些的，
	// 卸掉会让别的软件起不来，不进列表。只认"运行库家族"的关键词，泛 "Runtime"
	// 不算（Edge WebView2 Runtime 是可卸的独立组件，控制面板里也显示）。
	runtimeCompRe = regexp.MustCompile(`(?i)(visual c\+\+|redistributable|minimum runtime|additional runtime|` +
		`visual studio|\.net native|net\.native|vclibs|ui\.xaml|windowsapp ?runtime|appruntime)`)
)

var kernel32ac = syscall.NewLazyDLL("kernel32.dll")
var procLocalFree = kernel32ac.NewProc("LocalFree")
var procCmdToArgv = shell32lazy.NewProc("CommandLineToArgvW")

// ---------- 工具 ----------

func regStr(k registry.Key, name string) string {
	s, _, err := k.GetStringValue(name)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(s)
}

func regDword(k registry.Key, name string) (uint32, bool) {
	v, _, err := k.GetIntegerValue(name)
	if err != nil {
		return 0, false
	}
	return uint32(v), true
}

func expandWinEnv(s string) string {
	if s == "" {
		return ""
	}
	return winEnvRe.ReplaceAllStringFunc(s, func(m string) string {
		v := os.Getenv(m[1 : len(m)-1])
		if v == "" {
			return m
		}
		return v
	})
}

func normPath(s string) string {
	s = strings.TrimSpace(strings.Trim(s, `"`))
	s = expandWinEnv(s)
	s = strings.ReplaceAll(s, "/", `\`)
	return strings.TrimRight(s, `\ `)
}

// 解析 "C:\a\b.exe",0 这类图标引用
func parseIconRef(s string) (string, int) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", 0
	}
	idx := 0
	if i := strings.LastIndex(s, ","); i > 0 {
		if n, err := strconv.Atoi(strings.TrimSpace(s[i+1:])); err == nil {
			idx = n
			s = strings.TrimSpace(s[:i])
		}
	}
	s = normPath(s)
	if s == "" || !strings.Contains(s, `\`) {
		return "", 0
	}
	return s, idx
}

// 目录创建时间（比 ModTime 更接近安装时间）
func dirCreateTime(p string) int64 {
	f, err := os.Open(p)
	if err != nil {
		return 0
	}
	defer f.Close()
	var bi windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(windows.Handle(f.Fd()), &bi); err != nil {
		return 0
	}
	t := time.Unix(0, bi.CreationTime.Nanoseconds())
	if t.Year() < 1995 || t.After(time.Now().AddDate(0, 0, 1)) {
		return 0
	}
	return t.Unix()
}

func parseInstallDate(s string) int64 {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0
	}
	for _, layout := range []string{"20060102", "2006-01-02", "2006/01/02", "01/02/2006", "2006-1-2", "2006.01.02"} {
		if t, err := time.Parse(layout, s); err == nil {
			if t.Year() >= 1995 && !t.After(time.Now().AddDate(0, 0, 1)) {
				return t.Unix()
			}
		}
	}
	if len(s) >= 8 {
		if n, err := strconv.Atoi(s[:8]); err == nil && n > 19950101 {
			if t, err2 := time.Parse("20060102", s[:8]); err2 == nil {
				return t.Unix()
			}
		}
	}
	return 0
}

// 解析命令行（Windows 规则），失败返回 nil
func splitCommandLine(s string) []string {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	p16, err := syscall.UTF16PtrFromString(s)
	if err != nil {
		return nil
	}
	var argc int32
	r, _, _ := procCmdToArgv.Call(uintptr(unsafe.Pointer(p16)), uintptr(unsafe.Pointer(&argc)))
	if r == 0 || argc <= 0 || argc > 64 {
		return nil
	}
	defer procLocalFree.Call(r)
	/* CommandLineToArgvW 把指针当返回值交出来（uintptr）。直接 uintptr→unsafe.Pointer 会被 go vet
	   判为误用；这里先做一次类型重解释拿到指针（二进制语义与转换完全等价），再按数组读。 */
	base := *(*unsafe.Pointer)(unsafe.Pointer(&r))
	ptrs := unsafe.Slice((**uint16)(base), int(argc))
	out := make([]string, 0, argc)
	for _, p := range ptrs {
		out = append(out, windows.UTF16PtrToString(p))
	}
	return out
}

// ---------- Prefetch（判断"久未使用"）----------

var prefetchOnce sync.Once
var prefetchMap map[string]int64
var prefetchOK bool

func prefetchIndex() (map[string]int64, bool) {
	prefetchOnce.Do(func() {
		prefetchMap = map[string]int64{}
		dir := filepath.Join(os.Getenv("SystemRoot"), "Prefetch")
		ents, err := os.ReadDir(dir)
		if err != nil {
			return
		}
		for _, e := range ents {
			n := strings.ToUpper(e.Name())
			if !strings.HasSuffix(n, ".PF") {
				continue
			}
			i := strings.LastIndex(n, "-")
			if i <= 0 {
				continue
			}
			info, ierr := e.Info()
			if ierr != nil {
				continue
			}
			t := info.ModTime().Unix()
			exe := n[:i]
			if old, ok := prefetchMap[exe]; !ok || t > old {
				prefetchMap[exe] = t
			}
		}
		prefetchOK = len(prefetchMap) > 0
	})
	return prefetchMap, prefetchOK
}

// ---------- Win32 枚举 ----------

func (a *App) GetWin32Apps() []AppEntry {
	ptrs := enumerateWin32()
	appMu.Lock()
	appCache = map[string]*AppEntry{}
	for _, e := range ptrs {
		appCache[e.ID] = e
	}
	appMu.Unlock()
	out := make([]AppEntry, len(ptrs))
	for i, e := range ptrs {
		out[i] = *e
	}
	sort.SliceStable(out, func(i, j int) bool {
		return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name)
	})
	return out
}

func enumerateWin32() []*AppEntry {
	pf, pfOK := prefetchIndex()
	var out []*AppEntry
	seen := map[string]bool{}
	for _, r := range uninstallRoots {
		k, err := registry.OpenKey(r.root, r.path, registry.READ)
		if err != nil {
			continue
		}
		subs, err := k.ReadSubKeyNames(-1)
		k.Close()
		if err != nil {
			continue
		}
		for _, sub := range subs {
			sk, err := registry.OpenKey(r.root, r.path+`\`+sub, registry.READ)
			if err != nil {
				continue
			}
			e := readUninstallKey(sk, r.tag, r.path+`\`+sub, sub, r.root, pf, pfOK)
			sk.Close()
			if e == nil {
				continue
			}
			dup := strings.ToLower(e.Name) + "|" + strings.ToLower(e.Version) + "|" + strings.ToLower(e.InstallLoc)
			if seen[dup] {
				continue
			}
			seen[dup] = true
			out = append(out, e)
		}
	}
	return out
}

func readUninstallKey(k registry.Key, tag, fullPath, sub string, root registry.Key, pf map[string]int64, pfOK bool) *AppEntry {
	name := regStr(k, "DisplayName")
	if name == "" || len(name) > 200 {
		return nil
	}
	// SystemComponent=1 的微软本意是「这是某个程序的组件，别单列」。但控制面板自己执行得
	// 并不一致（同一台机：Office Click-to-Run 组件、Edge WebView2 运行库会显示，VC++
	// 运行库不显示）。2026-10-11 用户实测「控制面板 48 项，我们只有 44 项」就是一刀切
	// 隐藏砍掉 19 个留下的缺口。这里改成按「能不能卸、该不该卸」分三类：
	//   · 没有卸载命令（AMD DVR64 / NVIDIA Install Application 这类）→ 隐藏，
	//     它们在控制面板里点了也没反应，列出来只会让人以为软件坏了
	//   · 运行库 / 运行组件（VC++ Redistributable、.NET Native 等）→ 隐藏，
	//     卸掉会让别的软件起不来
	//   · 其余（Office Click-to-Run 组件、WebView2 运行库等）→ 照实列出
	if v, ok := regDword(k, "SystemComponent"); ok && v == 1 {
		if regStr(k, "UninstallString") == "" && regStr(k, "QuietUninstallString") == "" {
			return nil
		}
		if runtimeCompRe.MatchString(name) {
			return nil
		}
	}
	if regStr(k, "ParentKeyName") != "" || regStr(k, "ParentDisplayName") != "" {
		return nil
	}
	switch regStr(k, "ReleaseType") {
	case "Security Update", "Update Rollup", "Hotfix", "ServicePack", "MsiPatch":
		return nil
	}
	if kbPatchRe.MatchString(name) {
		return nil
	}
	if guidRe.MatchString(name) && len(name) < 45 {
		return nil
	}

	e := &AppEntry{
		ID:        "w." + tag + "." + sub,
		Kind:      "win32",
		Name:      name,
		Version:   regStr(k, "DisplayVersion"),
		Publisher: regStr(k, "Publisher"),
		InstallLoc: normPath(regStr(k, "InstallLocation")),
		Arch:      map[string]string{"hk64": "x64", "hk32": "x86", "hku": "user"}[tag],
		regRoot:   root,
		regPath:   fullPath,
		Removable: true,
	}
	if e.Publisher == "" {
		e.Publisher = "—"
	}

	// 大小：注册表申报（KB）
	if v, ok := regDword(k, "EstimatedSize"); ok && v > 0 {
		e.Bytes = int64(v) * 1024
		e.SizeSrc = "reg"
	} else {
		e.SizeSrc = "none"
	}

	// 安装时间
	if t := parseInstallDate(regStr(k, "InstallDate")); t > 0 {
		e.Date, e.DateSrc = t, "reg"
	}

	// 卸载命令
	us := regStr(k, "UninstallString")
	if us == "" {
		us = regStr(k, "QuietUninstallString")
	}
	e.uninstall = us
	if us == "" {
		e.ForceOnly = true
	} else {
		e.Cmd = truncateStr(us, 150)
	}

	// 图标
	iconSrc, iconIdx := parseIconRef(regStr(k, "DisplayIcon"))
	if iconSrc == "" {
		if il := e.InstallLoc; il != "" {
			if exe := firstExeIn(il, 1); exe != "" {
				iconSrc, iconIdx = exe, 0
			}
		}
	}
	if iconSrc == "" {
		if parts := splitCommandLine(us); len(parts) > 0 && strings.Contains(parts[0], `\`) {
			iconSrc, iconIdx = normPath(parts[0]), 0
		}
	}
	if iconSrc != "" {
		if _, err := os.Stat(iconSrc); err == nil {
			e.IconKey = "w|" + iconSrc + "|" + strconv.Itoa(iconIdx)
			e.HasIcon = true
		}
	}

	// 主程序 exe（用于"久未使用"判断）
	e.mainExe = ""
	if parts := splitCommandLine(us); len(parts) > 0 {
		bn := strings.ToLower(filepath.Base(parts[0]))
		if bn != "" && !strings.Contains(bn, "unins") && !strings.Contains(bn, "uninstall") {
			e.mainExe = bn
		}
	}
	if e.mainExe == "" && iconSrc != "" {
		bn := strings.ToLower(filepath.Base(iconSrc))
		if !strings.Contains(bn, "unins") && !strings.Contains(bn, "setup") {
			e.mainExe = bn
		}
	}
	if e.mainExe == "" && e.InstallLoc != "" {
		if exe := firstExeIn(e.InstallLoc, 1); exe != "" {
			e.mainExe = strings.ToLower(filepath.Base(exe))
		}
	}
	if pfOK && e.mainExe != "" {
		if t, ok := pf[strings.ToUpper(e.mainExe)]; ok {
			e.LastUsed = t
		} else {
			e.NeverRan = true // 预读取里没有记录 → 装了基本没跑过
		}
	}

	// 大小/时间兜底：安装目录实测（交给前端按需触发，避免一次性扫描几百个目录）
	if e.SizeSrc == "none" && e.InstallLoc != "" && dirUsable(e.InstallLoc) {
		e.SizePending = true
	}
	if e.Date == 0 && e.InstallLoc != "" && dirUsable(e.InstallLoc) {
		if t := dirCreateTime(e.InstallLoc); t > 0 {
			e.Date, e.DateSrc = t, "dir"
		}
	}

	// 黑名单：核心组件禁止在这里卸载
	if reason := appBlockReason(e); reason != "" {
		e.Blocked = reason
		e.Removable = false
	}
	if e.ForceOnly && e.InstallLoc == "" {
		e.Removable = false
		e.Blocked = "找不到卸载程序与安装目录，无法在此卸载"
	}
	return e
}

func truncateStr(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

func dirUsable(p string) bool {
	if p == "" || len(p) < 8 {
		return false
	}
	cp := strings.ToLower(strings.TrimRight(p, `\`))
	for _, root := range []string{
		strings.ToLower(os.Getenv("SystemRoot")),
		strings.ToLower(os.Getenv("ProgramFiles")),
		strings.ToLower(os.Getenv("ProgramFiles(x86)")),
		strings.ToLower(os.Getenv("ProgramData")),
		strings.ToLower(os.Getenv("LOCALAPPDATA")),
		strings.ToLower(os.Getenv("APPDATA")),
		strings.ToLower(os.Getenv("USERPROFILE")),
		`c:\`, `d:\`, `e:\`, `f:\`,
	} {
		if root != "" && (cp == strings.TrimRight(root, `\`) || cp == `c:`) {
			return false
		}
	}
	st, err := os.Stat(p)
	return err == nil && st.IsDir()
}

func firstExeIn(dir string, maxDepth int) string {
	dir = normPath(dir)
	if !dirUsable(dir) {
		return ""
	}
	ents, err := os.ReadDir(dir)
	if err != nil {
		return ""
	}
	for _, e := range ents {
		if e.IsDir() {
			continue
		}
		if strings.EqualFold(filepath.Ext(e.Name()), ".exe") {
			return filepath.Join(dir, e.Name())
		}
	}
	return ""
}

func appBlockReason(e *AppEntry) string {
	l := strings.ToLower(e.Name + " " + e.Publisher + " " + e.InstallLoc)
	for _, kw := range []string{
		"microsoft visual c++", "visual c++ 20", "microsoft .net", ".net framework",
		"windows sdk", "windows driver kit", "microsoft edge webview", "microsoft edge update",
		"windows defender", "microsoft store", "windows 应用", "microsoft windows",
		"nvidia 驱动程序", "nvidia graphics driver", "intel(r) graphics", "amd chipset",
		"realtek ", "wintrust", "windows app runtime", "directx", "msxml",
	} {
		if strings.Contains(l, kw) {
			return "系统运行库/驱动，卸载会导致其它软件异常"
		}
	}
	return ""
}

// ---------- 目录体积（带上限，防止 InstallLocation 乱填根目录）----------

func dirSizeLimited(root string, maxFiles int) (int64, int) {
	var total int64
	var n int
	_ = filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if p != root && isReparsePoint(p) {
				return filepath.SkipDir
			}
			return nil
		}
		n++
		if n > maxFiles {
			return filepath.SkipAll
		}
		if info, e := d.Info(); e == nil {
			total += info.Size()
		}
		return nil
	})
	return total, n
}

// CalcAppSize 前端按需触发：实测某个程序的安装目录体积。
//
// 2026-10-09 修 A2（列表里「实测」永远是 0 个）：
//   这个函数早就写好了，但前端从来没调用过 —— 页面上只有注册表申报值，
//   所以「实测」计数恒为 0。现在前端按需调它（单个 + 批量逐个）。
//
// 返回值里的 readable 必须区分「目录读不到」和「目录是空的」：
//   - 读不到（无权限 / 路径不存在 / 所有文件都被权限拒绝）→ readable=false，
//     此时绝不把 sizeSrc 改成 dir，否则界面上会把「未知」谎报成「实测 0 GB」。
//   - 读得到 → 照实写实测值。这一步是"不编造数据"红线的具体落实。
func (a *App) CalcAppSize(id string) map[string]interface{} {
	res := map[string]interface{}{"id": id, "bytes": 0, "files": 0, "readable": false}
	e := lookupApp(id)
	if e == nil || e.InstallLoc == "" || !dirUsable(e.InstallLoc) {
		return res
	}
	b, n := dirSizeLimited(e.InstallLoc, 200000)

	// 有文件才敢认这是"实测"；一个文件都没读到，说明是被权限挡住了（或目录真的空，
	// 那种情况实测 0 也没有展示价值），两种都保持原样，不冒充实测数据。
	if n == 0 {
		(&App{}).DebugLog("[AC-SIZE] 目录没读到文件，保持未知: " + e.InstallLoc)
		return res
	}

	appMu.Lock()
	e.Bytes, e.Files, e.SizeSrc, e.SizePending = b, n, "dir", false
	appMu.Unlock()
	res["bytes"], res["files"], res["readable"] = b, n, true
	return res
}

func lookupApp(id string) *AppEntry {
	appMu.Lock()
	e := appCache[id]
	appMu.Unlock()
	if e != nil {
		return e
	}
	// 缓存失效（例如切换过页面）→ 重新枚举一次
	if strings.HasPrefix(id, "u.") {
		_ = fetchUwpApps()
	} else {
		ptrs := enumerateWin32()
		appMu.Lock()
		for _, x := range ptrs {
			appCache[x.ID] = x
		}
		appMu.Unlock()
	}
	appMu.Lock()
	e = appCache[id]
	appMu.Unlock()
	return e
}

// ---------- 关键词 ----------

var kwStop = map[string]bool{
	"microsoft": true, "corporation": true, "corp": true, "inc": true, "ltd": true,
	"llc": true, "limited": true, "co": true, "gmbh": true, "software": true,
	"technologies": true, "technology": true, "company": true, "windows": true,
	"system": true, "systems": true, "studio": true, "client": true, "manager": true,
	"service": true, "services": true, "update": true, "updater": true, "version": true,
	"the": true, "and": true, "for": true, "with": true, "apps": true, "app": true,
	"group": true, "global": true, "china": true, "beijing": true, "shanghai": true,
	"shenzhen": true, "network": true, "networks": true, "cn": true, "com": true,
	"product": true, "products": true, "team": true, "info": true, "data": true,
	"tool": true, "tools": true, "pro": true, "plus": true, "lite": true, "setup": true,
	"installer": true, "uninstall": true, "64": true, "32": true, "bit": true,
	"x64": true, "x86": true, "desktop": true, "player": true, "edition": true,
}

func kwClean(s string) []string {
	var out []string
	low := strings.ToLower(s)
	// 先按分隔符切
	fields := strings.FieldsFunc(low, func(r rune) bool {
		return !(r >= 'a' && r <= 'z') && !(r >= '0' && r <= '9') && r != '+' && r != '#'
	})
	for _, f := range fields {
		f = strings.TrimSpace(f)
		if len(f) < 4 || kwStop[f] {
			continue
		}
		out = append(out, f)
	}
	// 中文名整串保留
	if hasCJK(low) && len([]rune(low)) >= 2 {
		out = append(out, low)
	}
	return out
}

func hasCJK(s string) bool {
	for _, r := range s {
		if r >= 0x4e00 && r <= 0x9fff {
			return true
		}
	}
	return false
}

func appKeywords(e *AppEntry) (strong, weak []string) {
	set := map[string]bool{}
	add := func(dst *[]string, s string) {
		s = strings.ToLower(strings.TrimSpace(s))
		if s == "" || len(s) < 3 {
			return
		}
		if set[s] {
			return
		}
		set[s] = true
		*dst = append(*dst, s)
	}
	for _, k := range kwClean(e.Name) {
		add(&strong, k)
	}
	if e.InstallLoc != "" {
		add(&strong, strings.ToLower(filepath.Base(e.InstallLoc)))
	}
	for _, g := range guidRe.FindAllString(e.uninstall+" "+e.regPath+" "+e.ID, -1) {
		add(&strong, strings.ToLower(g))
	}
	if e.PFN != "" {
		add(&strong, strings.ToLower(e.PFN))
		// FamilyName（name_发布者哈希）：UWP 注册表残留路径用的是它而非完整包名
		if parts := strings.Split(e.PFN, "_"); len(parts) >= 3 {
			add(&strong, strings.ToLower(parts[0]+"_"+parts[len(parts)-1]))
		}
	}
	for _, k := range kwClean(e.Publisher) {
		add(&weak, k)
	}
	for _, k := range kwClean(e.mainExe) {
		add(&weak, k)
	}
	// 强关键词也允许出现在弱集合里（用于评分降级判断）
	return strong, weak
}

func kwHit(name string, kws []string) string {
	ln := strings.ToLower(name)
	for _, kw := range kws {
		if len(kw) >= 3 && strings.Contains(ln, kw) {
			return kw
		}
	}
	return ""
}

// ---------- 残留：卸载前关联清单 + 卸载后差分 ----------

// regHit 一条注册表残留命中。val 非空表示键下某个「值」（MuiCache / Run），
// 这类容器键是所有应用共用的，绝不能整树删除，只能摘掉单个值。
type regHit struct {
	path string
	val  string
}

func (h regHit) key() string {
	if h.val != "" {
		return h.path + "|" + h.val
	}
	return h.path
}

// isSharedRegKey 所有应用共用的容器键（只能按值清理，禁止整树删除）
func isSharedRegKey(p string) bool {
	lp := strings.ToLower(p)
	return strings.HasSuffix(lp, `\muicache`) || strings.Contains(lp, `currentversion\run`)
}

/* ── 残留清理的安全底线（2026-10-11）───────────────────────────────────────
   判定规则**照抄 GeekUninstaller**（用户：「它那个我用了很长时间，没出过错」）：
     · 扫描阶段按程序名 / 发布者 / MSI 的 GUID / UWP 包名做关键词匹配（路径、值名、
       值数据任意命中即算），卸载前后做差分 —— 这一步 appKeywords + scanRegRelated
       已经就是 Geek 那套；
     · **命中的一律列为残留并默认勾选**，不做"我发明的"二次收紧（试过按「键名必须
       命中」收紧，会把大量真残留判成"不确定"，反而不像 Geek）；
     · 删除时整键删除（Geek 就是这么干的），共享容器键除外。
   只在两处加了 Geek 没有的兜底，都不改变"命中即清"的行为：
     1. 共用容器键（MuiCache / Run）只摘值不删键 —— 这两个键是所有软件共用的，
        整树删一次会清掉别人的启动项和缓存，Geek 也是按值处理的；
     2. 系统硬保护区（Windows NT / Defender / 加密 / .NET / CBS / WinSxS / 组策略）
        与太浅的目录（盘符根、Program Files 本身）永不清理 —— 程序名撞上这些词的
        概率极低，挡住的只有误伤；
     3. 整键删除前先 `reg export` 导出 .reg 备份（Geek 是建议建系统还原点，本版没接
        还原点，用精确导出替代：删错了双击那个 .reg 就能导回来）。
   Safe=false 的项：前端默认不勾、灰显并写明原因，后端 CleanAppResidue 也会拒绝。 */

// resRegBlock 整树删除的硬保护区（键名再像也不删）
var resRegBlock = []string{
	`software\microsoft\windows nt\`,
	`software\microsoft\windows defender\`,
	`software\microsoft\cryptography\`,
	`software\microsoft\systemcertificates\`,
	`software\microsoft\.netframework\`,
	`software\microsoft\windows\currentversion\sidebyside\`,
	`software\microsoft\windows\currentversion\component based servicing\`,
	`software\microsoft\windows\currentversion\winsxs\`,
	`system\currentcontrolset\control\`,
	`software\policies\`,
}

/* resSafeJudge 裁决一条残留能不能删。
   Geek 口径：不在硬保护区、不是共用容器键整树 → 命中即 safe。
   （目录残留也按 Geek 判为可删，但**前端默认不勾目录** —— 删文件不可逆，
     而注册表有 .reg 备份兜底，两者风险不一样。） */
func resSafeJudge(it ResidueItem, strong, weak []string) (bool, string) {
	if it.Kind == "dir" {
		return resDirSafe(it.Path, strong)
	}
	// 值级残留：只摘掉这一个值，别的软件不受影响
	if it.Val != "" {
		return true, ""
	}
	if isSharedRegKey(it.Path) {
		return false, "这是所有程序共用的容器键，整树删除会连累别的软件"
	}
	lp := strings.ToLower(it.Path)
	for _, b := range resRegBlock {
		if strings.Contains(lp, b) {
			return false, "属于系统区域，不清理"
		}
	}
	return true, ""
}

// resDirSafe 目录残留的安全裁决：只挡「系统目录本身」和太浅的路径
func resDirSafe(p string, strong []string) (bool, string) {
	c := strings.TrimRight(filepath.Clean(p), `\`)
	if c == "" || strings.Count(c, `\`) < 2 {
		return false, "目录层级太浅，不清理"
	}
	low := strings.ToLower(c)
	if strings.HasSuffix(low, `:`) {
		return false, "盘符根目录，不清理"
	}
	// SystemRoot 下任何位置都不碰（删错一个 dll 就是开不了机）
	if w := strings.TrimRight(os.Getenv("SystemRoot"), `\`); w != "" && strings.HasPrefix(low, strings.ToLower(w)) {
		return false, "Windows 系统目录，不清理"
	}
	prot := []string{
		os.Getenv("ProgramFiles"), os.Getenv("ProgramFiles(x86)"), os.Getenv("ProgramData"),
		os.Getenv("USERPROFILE"), os.Getenv("PUBLIC"), os.Getenv("APPDATA"), os.Getenv("LOCALAPPDATA"),
		strings.TrimSuffix(systemDrive(), `\`),
	}
	for _, x := range prot {
		x = strings.TrimRight(x, `\`)
		if x != "" && strings.EqualFold(low, strings.ToLower(x)) {
			return false, "这是系统 / 用户的公共目录本身，不清理"
		}
	}
	return true, ""
}

func lastSeg(p string) string {
	if i := strings.LastIndex(p, `\`); i >= 0 {
		return p[i+1:]
	}
	return p
}

// resBackupDir 注册表备份落点（删之前先导出，真删错了能双击导回来）
func resBackupDir() string { return filepath.Join(os.TempDir(), "3z9wOptV11_regbak") }

// ResidueBackupDir 给前端显示「备份放在哪」
func (a *App) ResidueBackupDir() string { return resBackupDir() }

// regExportBackup 把整棵键导出成 .reg。失败时调用方**必须放弃删除** ——
// 没有备份就动手，等于把「删错开不了机」变成不可恢复
func regExportBackup(full string) (string, error) {
	dir := resBackupDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	seg := strings.Map(func(r rune) rune {
		switch r {
		case '\\', '/', ':', '*', '?', '"', '<', '>', '|', '{', '}', ' ':
			return '_'
		}
		return r
	}, lastSeg(full))
	if len(seg) > 40 {
		seg = seg[:40]
	}
	dst := filepath.Join(dir, time.Now().Format("20060102_150405")+"_"+seg+".reg")
	out, err := runHiddenOut(30*time.Second, sysExe("reg.exe", ""), "export", full, dst, "/y")
	if err != nil {
		msg := strings.TrimSpace(out)
		if msg == "" {
			msg = err.Error()
		}
		return "", fmt.Errorf("%s", truncateStr(msg, 120))
	}
	if _, e2 := os.Stat(dst); e2 != nil {
		return "", fmt.Errorf("备份文件没生成")
	}
	return dst, nil
}

// scanRegRelated 按关键词扫注册表残留落点，覆盖面对标 GeekUninstaller：
//   - HKLM/HKCU\SOFTWARE 两级子键（含 WOW6432Node\...\Uninstall 第二层、Classes 第二层）
//   - 系统服务 HKLM\SYSTEM\CurrentControlSet\Services\<名>
//   - App Paths\<exe>（HKLM/HKCU）
//   - Run 启动项（按值名/值数据命中，只摘值）
//   - MuiCache（资源管理器「打开方式/应用名」缓存，卸载后几乎必留，按值名命中）
func scanRegRelated(strong, weak []string) []regHit {
	out := []regHit{}
	seen := map[string]bool{}
	add := func(h regHit) {
		lp := strings.ToLower(h.key())
		if seen[lp] {
			return
		}
		seen[lp] = true
		out = append(out, h)
	}

	// 两级子键枚举：SOFTWARE 根
	for _, r := range []struct {
		k   registry.Key
		p   string
		tag string
	}{
		{registry.LOCAL_MACHINE, `SOFTWARE`, "HKLM"},
		{registry.CURRENT_USER, `SOFTWARE`, "HKCU"},
	} {
		k, err := registry.OpenKey(r.k, r.p, registry.READ)
		if err != nil {
			continue
		}
		l1, err := k.ReadSubKeyNames(-1)
		k.Close()
		if err != nil {
			continue
		}
		for _, s1 := range l1 {
			p1 := r.p + `\` + s1
			if kwHit(s1, strong) != "" || kwHit(s1, weak) != "" {
				add(regHit{path: r.tag + `\` + p1})
			}
			k2, err := registry.OpenKey(r.k, p1, registry.READ)
			if err != nil {
				continue
			}
			l2, err := k2.ReadSubKeyNames(-1)
			k2.Close()
			if err != nil {
				continue
			}
			for _, s2 := range l2 {
				if kwHit(s2, strong) == "" && kwHit(s2, weak) == "" {
					continue
				}
				add(regHit{path: r.tag + `\` + p1 + `\` + s2})
			}
		}
	}

	// 一级子键枚举：服务 / App Paths（子键名本身就是程序标识）
	for _, r := range []struct {
		k   registry.Key
		p   string
		tag string
	}{
		{registry.LOCAL_MACHINE, `SYSTEM\CurrentControlSet\Services`, "HKLM"},
		{registry.LOCAL_MACHINE, `SOFTWARE\Microsoft\Windows\CurrentVersion\App Paths`, "HKLM"},
		{registry.CURRENT_USER, `Software\Microsoft\Windows\CurrentVersion\App Paths`, "HKCU"},
	} {
		k, err := registry.OpenKey(r.k, r.p, registry.READ)
		if err != nil {
			continue
		}
		l1, err := k.ReadSubKeyNames(-1)
		k.Close()
		if err != nil {
			continue
		}
		for _, s1 := range l1 {
			if kwHit(s1, strong) != "" || kwHit(s1, weak) != "" {
				add(regHit{path: r.tag + `\` + r.p + `\` + s1})
			}
		}
	}

	// 值级容器：Run 启动项 / MuiCache（按值名或值数据命中，摘值不删键）
	for _, r := range []struct {
		k registry.Key
		p string
	}{
		{registry.CURRENT_USER, `Software\Microsoft\Windows\CurrentVersion\Run`},
		{registry.LOCAL_MACHINE, `SOFTWARE\Microsoft\Windows\CurrentVersion\Run`},
		{registry.LOCAL_MACHINE, `SOFTWARE\WOW6432Node\Microsoft\Windows\CurrentVersion\Run`},
		{registry.CURRENT_USER, `Software\Classes\Local Settings\Software\Microsoft\Windows\Shell\MuiCache`},
	} {
		k, err := registry.OpenKey(r.k, r.p, registry.READ)
		if err != nil {
			continue
		}
		vn, err := k.ReadValueNames(-1)
		k.Close()
		if err != nil {
			continue
		}
		full := "HKLM"
		if r.k == registry.CURRENT_USER {
			full = "HKCU"
		}
		full += `\` + r.p
		for _, name := range vn {
			if kwHit(name, strong) != "" || kwHit(name, weak) != "" {
				add(regHit{path: full, val: name})
			}
		}
	}

	sort.Slice(out, func(i, j int) bool { return out[i].key() < out[j].key() })
	return out
}

func scanDirsRelated(e *AppEntry, strong, weak []string) []string {
	var out []string
	seen := map[string]bool{}
	add := func(p string) {
		lp := strings.ToLower(p)
		if !seen[lp] {
			seen[lp] = true
			out = append(out, p)
		}
	}
	bases := []string{
		os.Getenv("ProgramFiles"),
		os.Getenv("ProgramFiles(x86)"),
		os.Getenv("ProgramData"),
		os.Getenv("APPDATA"),
		os.Getenv("LOCALAPPDATA"),
	}
	if e != nil && e.InstallLoc != "" {
		if par := filepath.Dir(e.InstallLoc); par != "" && par != e.InstallLoc {
			bases = append(bases, par)
		}
	}
	for _, b := range bases {
		if b == "" {
			continue
		}
		ents, err := os.ReadDir(b)
		if err != nil {
			continue
		}
		for _, en := range ents {
			if !en.IsDir() {
				continue
			}
			if kwHit(en.Name(), strong) != "" {
				add(filepath.Join(b, en.Name()))
				continue
			}
			if kwHit(en.Name(), weak) == "" {
				continue
			}
			add(filepath.Join(b, en.Name()))
			// 厂商目录：再下一层找产品目录
			sub, err := os.ReadDir(filepath.Join(b, en.Name()))
			if err != nil {
				continue
			}
			for _, s := range sub {
				if s.IsDir() && (kwHit(s.Name(), strong) != "" || kwHit(s.Name(), weak) != "") {
					add(filepath.Join(b, en.Name(), s.Name()))
				}
			}
		}
	}
	// UWP 数据目录
	if e != nil && e.PFN != "" {
		p := filepath.Join(os.Getenv("LOCALAPPDATA"), "Packages", e.PFN)
		if st, err := os.Stat(p); err == nil && st.IsDir() {
			add(p)
		}
	}
	sort.Strings(out)
	return out
}

func regExists(path string) bool {
	root, sub, err := regSplit(path)
	if err != nil {
		return false
	}
	k, err := registry.OpenKey(root, sub, registry.READ)
	if err != nil {
		return false
	}
	k.Close()
	return true
}

/* regSplit 与 v1.1 的 junk_reg.go 逐字相同 —— 那份是从这里切出来的，这里不再重复定义，
   统一用 junk_reg.go 里的（改一处就能同时修两个模块）。 */

func residueRisk(path string, strong, weak []string) (string, string) {
	if k := kwHit(path, strong); k != "" {
		return "high", "名称命中「" + k + "」"
	}
	if k := kwHit(path, weak); k != "" {
		return "mid", "厂商命中「" + k + "」"
	}
	return "low", "疑似残留"
}

// residueRiskVal 值级残留按「值名/值数据」定级（MuiCache 的键路径是公共的，
// 关键词命中的是值名里那条 exe 完整路径）
func residueRiskVal(p, val string, strong, weak []string) (string, string) {
	if val != "" {
		if k := kwHit(val, strong); k != "" {
			return "high", "名称命中「" + k + "」"
		}
		if k := kwHit(val, weak); k != "" {
			return "mid", "厂商命中「" + k + "」"
		}
	}
	return residueRisk(p, strong, weak)
}

// regKeyOrValExists 键存在，或（值级命中时）该值仍存在
func regKeyOrValExists(h regHit) bool {
	root, sub, err := regSplit(h.path)
	if err != nil {
		return false
	}
	k, err := registry.OpenKey(root, sub, registry.READ)
	if err != nil {
		return false
	}
	defer k.Close()
	if h.val == "" {
		return true
	}
	_, _, err = k.GetValue(h.val, make([]byte, 0))
	return err == nil
}

// ---------- 卸载会话 ----------

// UninstallApp 启动卸载。返回空字符串表示已启动；否则为错误信息
func (a *App) UninstallApp(id string) string {
	e := lookupApp(id)
	if e == nil {
		return "找不到该程序，请重新扫描"
	}
	if e.Blocked != "" {
		return e.Blocked
	}
	/* 单程序流程（2026-10-11 用户定：不给批量卸载）。每次开新任务先清空残留池 ——
	   池是跨程序累积的，不清会把上一个程序的残留混进这次的清单里。 */
	resetResiduePool()
	if e.Kind == "uwp" {
		return a.uninstallUwp(e, false)
	}
	if e.ForceOnly {
		return "该程序没有卸载程序，请用右侧「⋮」菜单里的强制清理"
	}

	sess := newSessionForEntry(e)
	exe, args, exeName, ok := buildUninstallCmd(e.uninstall)
	if !ok {
		return "无法解析卸载命令：" + truncateStr(e.uninstall, 120)
	}
	if exeName != "" {
		sess.procs = append(sess.procs, exeName)
	}

	cmd := exec.Command(exe, args...)
	if strings.EqualFold(filepath.Base(exe), "cmd.exe") {
		cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	}
	if err := cmd.Start(); err != nil {
		return "启动卸载程序失败：" + err.Error()
	}
	go func() {
		_ = cmd.Wait()
		// GeekUninstaller 式自动复核：卸载程序一退出立即通知前端重扫残留，
		// 用户不用自己盯着手动点「重新扫描」
		a.emit("uninst:exited", sess.id)
	}()

	appMu.Lock()
	appSess = sess
	appMu.Unlock()
	return ""
}

// newSessionForEntry 记录卸载前的「关联清单」——注册表项与目录，
// 卸载后复检这些位置，仍在的即为残留（差分）。
func newSessionForEntry(e *AppEntry) *uninstSession {
	strong, weak := appKeywords(e)
	sess := &uninstSession{
		id:      e.ID,
		name:    e.Name,
		kind:    e.Kind,
		pfn:     e.PFN,
		loc:     e.InstallLoc,
		before:  map[string]bool{},
		started: time.Now(),
		state:   "running",
	}
	for _, h := range scanRegRelated(strong, weak) {
		sess.before["reg:"+strings.ToLower(h.key())] = true
	}
	for _, d := range scanDirsRelated(e, strong, weak) {
		sess.before["dir:"+strings.ToLower(d)] = true
	}
	sess.regKeys = []string{e.regPath}
	if e.regPath != "" {
		// 自身卸载键也进卸载前快照：卸载后它若还在，就是第一条残留（Geek 首查项）
		sess.before["reg:"+strings.ToLower(e.regPath)] = true
	}
	// UWP 专属注册表位置：通用关键词扫描扫不到这些深层路径（AppxAllUserStore、
	// AppContainer Storage 在 HKLM\SOFTWARE 两层枚举之外），必须在卸载前显式记录
	if e.PFN != "" {
		for _, p := range uwpRegPaths(e) {
			sess.uwpReg = append(sess.uwpReg, p)
			sess.before["reg:"+strings.ToLower(p)] = true
			sess.regKeys = append(sess.regKeys, p)
		}
	}
	for k := range sess.before {
		if !strings.HasPrefix(k, "reg:") {
			continue
		}
		p := strings.TrimPrefix(k, "reg:")
		// 值级残留与共享容器键（MuiCache / Run）绝不进 regKeys ——
		// regKeys 会被强制清理整树删除，误删会毁掉全系统自启动/应用缓存
		if strings.Contains(p, "|") || isSharedRegKey(p) {
			continue
		}
		sess.regKeys = append(sess.regKeys, p)
	}
	if e.InstallLoc != "" {
		sess.dirs = append(sess.dirs, e.InstallLoc)
	}
	for k := range sess.before {
		if strings.HasPrefix(k, "dir:") {
			sess.dirs = append(sess.dirs, strings.TrimPrefix(k, "dir:"))
		}
	}
	if e.mainExe != "" {
		sess.procs = append(sess.procs, e.mainExe)
	}
	return sess
}

// uwpRegPaths 内置应用的注册表残留固定落点（按包名精确定位，不靠关键词猜）：
//  1. HKCU AppContainer\Storage\<FamilyName> —— 应用自己的注册表数据区
//  2. HKLM Appx\AppxAllUserStore\Applications\<PackageFullName> —— 卸载后常残留的注册项
func uwpRegPaths(e *AppEntry) []string {
	if e.PFN == "" {
		return nil
	}
	parts := strings.Split(e.PFN, "_")
	if len(parts) < 3 {
		return nil
	}
	family := parts[0] + "_" + parts[len(parts)-1]
	return []string{
		`HKCU\Software\Classes\Local Settings\Software\Microsoft\Windows\CurrentVersion\AppContainer\Storage\` + family,
		`HKLM\SOFTWARE\Microsoft\Windows\CurrentVersion\Appx\AppxAllUserStore\Applications\` + e.PFN,
	}
}

// 解析卸载命令：MSI 的 /I 改 /X，普通程序按原始命令行拆分
func buildUninstallCmd(raw string) (exe string, args []string, exeName string, ok bool) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return "", nil, "", false
	}
	low := strings.ToLower(s)
	if strings.Contains(low, "msiexec") {
		if g := guidRe.FindString(s); g != "" {
			return "msiexec.exe", []string{"/X" + g, "/passive", "/norestart"}, "msiexec.exe", true
		}
		return "msiexec.exe", []string{"/X" + strings.TrimSpace(s)}, "msiexec.exe", true
	}
	if parts := splitCommandLine(s); len(parts) > 0 {
		target := normPath(parts[0])
		if strings.Contains(target, `\`) {
			if _, err := os.Stat(target); err == nil {
				return target, parts[1:], strings.ToLower(filepath.Base(target)), true
			}
		} else {
			return parts[0], parts[1:], strings.ToLower(parts[0]), true
		}
	}
	// 退路：交给 cmd 原样执行
	return "cmd.exe", []string{"/c", s}, "", true
}

// PollUninstall 前端每 1.5s 调用一次
func (a *App) PollUninstall() UninstallStatus {
	appMu.Lock()
	sess := appSess
	appMu.Unlock()
	st := UninstallStatus{State: "done"}
	if sess == nil {
		st.Message = "没有进行中的卸载任务"
		return st
	}
	sess.mu.Lock()
	defer sess.mu.Unlock()
	st.Name = sess.name
	st.Elapsed = int(time.Since(sess.started).Seconds())
	st.HasForce = true

	leftReg := 0
	for _, rk := range sess.regKeys {
		if regExists(rk) {
			leftReg++
		}
	}
	leftDir := 0
	for _, d := range sess.dirs {
		if st, err := os.Stat(d); err == nil && st.IsDir() {
			leftDir++
		}
	}
	procs := []string{}
	alive := 0
	for _, p := range sess.procs {
		if p == "" {
			continue
		}
		if n := len(findProcess(p)); n > 0 {
			alive += n
			procs = append(procs, p)
		}
	}
	st.LeftReg, st.LeftDir, st.ProcN, st.Procs = leftReg, leftDir, alive, procs
	st.RegGone = leftReg == 0
	st.DirGone = leftDir == 0 || sess.loc == ""
	st.Procs = procs

	switch {
	case st.RegGone && alive == 0:
		sess.state = "done"
		st.State = "done"
		st.Message = "卸载进程已退出，注册表项已清除"
	// 2026-10-11：150 秒太短 —— 卸载向导常要用户点好几步（确认 / 是否保留配置 /
	// 是否重启），2 分半就被判超时的例子实测出现过。放宽到 8 分钟。
	case st.Elapsed > 480:
		sess.state = "timeout"
		st.State = "timeout"
		st.Message = "等待超时，可能卸载程序仍在后台或需要你手动确认"
	default:
		st.State = "running"
		if alive > 0 {
			st.Message = "卸载程序运行中：" + strings.Join(procs, "、")
		} else if !st.RegGone {
			st.Message = "等待注册表项清除…"
		} else {
			st.Message = "等待系统回收文件…"
		}
	}
	return st
}

// ScanAppResidue 卸载后的残留差分扫描
func (a *App) ScanAppResidue() []ResidueItem {
	appMu.Lock()
	sess := appSess
	appMu.Unlock()
	if sess == nil {
		return nil
	}
	sess.mu.Lock()
	defer sess.mu.Unlock()

	var e *AppEntry
	if x := lookupApp(sess.id); x != nil {
		e = x
	} else {
		e = &AppEntry{ID: sess.id, Name: sess.name, InstallLoc: sess.loc, PFN: sess.pfn}
	}
	strong, weak := appKeywords(e)

	regAfter := scanRegRelated(strong, weak)
	dirAfter := scanDirsRelated(e, strong, weak)

	var items []ResidueItem
	seen := map[string]bool{}

	push := func(kind, p, val string) {
		lp := strings.ToLower(p)
		if val != "" {
			lp += "|" + strings.ToLower(val)
		}
		if seen[kind+lp] {
			return
		}
		seen[kind+lp] = true
		risk, reason := residueRiskVal(p, val, strong, weak)
		if kind == "reg" {
			// 注册表键：只有卸载前后都存在的才算残留；新出现的按低风险收录
			if !sess.before["reg:"+lp] {
				risk, reason = "low", "卸载过程新写入的项"
			}
		} else {
			if !sess.before["dir:"+lp] && !strings.EqualFold(lp, strings.ToLower(sess.loc)) {
				risk, reason = "low", "卸载过程新产生的目录"
			}
		}
		it := ResidueItem{Kind: kind, Path: p, Val: val, Risk: risk, Reason: reason}
		if kind == "dir" {
			if st, err := os.Stat(p); err == nil && st.IsDir() {
				b, n := dirSizeLimited(p, 50000)
				it.Bytes, it.Files = b, n
			}
		}
		items = append(items, it)
	}

	// 自身卸载键 / 卸载前关联键仍存在 → 直接是残留（Geek 首查项：卸载条目本身还在）
	for _, rk := range sess.regKeys {
		if regExists(rk) && !isSharedRegKey(rk) {
			push("reg", rk, "")
		}
	}
	for _, h := range regAfter {
		if regKeyOrValExists(h) {
			push("reg", h.path, h.val)
		}
	}
	for _, p := range dirAfter {
		if st, err := os.Stat(p); err == nil && st.IsDir() {
			push("dir", p, "")
		}
	}
	if sess.loc != "" {
		if st, err := os.Stat(sess.loc); err == nil && st.IsDir() {
			push("dir", sess.loc, "")
		}
	}
	// UWP 专属注册表落点复检（AppContainer Storage / AppxAllUserStore）
	for _, p := range sess.uwpReg {
		if regExists(p) {
			push("reg", p, "")
		}
	}

	// 排序：高风险在前，其次目录，再按体积
	rank := map[string]int{"high": 0, "mid": 1, "low": 2}
	sort.SliceStable(items, func(i, j int) bool {
		if rank[items[i].Risk] != rank[items[j].Risk] {
			return rank[items[i].Risk] < rank[items[j].Risk]
		}
		if items[i].Kind != items[j].Kind {
			return items[i].Kind == "dir"
		}
		return items[i].Bytes > items[j].Bytes
	})
	for i := range items {
		items[i].ID = "r" + strconv.Itoa(i)
		items[i].Safe, items[i].Guard = resSafeJudge(items[i], strong, weak)
	}
	sess.residue = items
	sess.scanned = true

	// 写入全局残留池：批量卸载时可跨程序累积，id 全局唯一
	resPoolMu.Lock()
	for i := range items {
		resSeq++
		items[i].ID = "r" + strconv.Itoa(resSeq)
		items[i].App = sess.name
		resPool[items[i].ID] = items[i]
	}
	resPoolMu.Unlock()
	return items
}

// resetResiduePool 清空跨程序残留池（新卸载任务开始时调用）
func resetResiduePool() {
	resPoolMu.Lock()
	resPool = map[string]ResidueItem{}
	resSeq = 0
	resPoolMu.Unlock()
}

// CleanAppResidue 清理勾选的残留项（从全局残留池取出）
func (a *App) CleanAppResidue(ids []string) []ResidueResult {
	resPoolMu.Lock()
	items := make([]ResidueItem, 0, len(ids))
	for _, id := range ids {
		if it, ok := resPool[id]; ok {
			items = append(items, it)
		}
	}
	resPoolMu.Unlock()

	var out []ResidueResult
	for _, it := range items {
		r := ResidueResult{ID: it.ID, Path: it.Path, Kind: it.Kind}
		/* 安全闸门再查一遍：池里的 Safe 是扫描时算的，这里兜住"前端绕过界面
		   直接传 id"的情况 —— 不 safe 的一律不动，原因照实回给用户。 */
		if !it.Safe {
			r.Err = "出于安全考虑没有清理：" + it.Guard
			out = append(out, r)
			continue
		}
		if it.Val != "" {
			r.Path = it.Path + " ｜ 值: " + it.Val
			if err := delRegValue(it.Path, it.Val); err != nil {
				r.Err = err.Error()
			}
		} else if it.Kind == "reg" {
			// 先备份再删；备份不成就不删 —— 没有退路的手不动
			bp, berr := regExportBackup(it.Path)
			if berr != nil {
				r.Err = "删前备份失败，已跳过（" + berr.Error() + "）"
				out = append(out, r)
				continue
			}
			r.Backup = bp
			if err := delRegTree(it.Path); err != nil {
				r.Err = err.Error()
			}
		} else {
			st := removeTreeSizedCtx(context.Background(), it.Path)
			r.Bytes = st.freed
			if st.failed > 0 {
				r.Err = fmt.Sprintf("%d 个文件被占用或无权限", st.failed)
			}
		}
		out = append(out, r)
	}
	resPoolMu.Lock()
	for _, it := range items {
		delete(resPool, it.ID)
	}
	resPoolMu.Unlock()
	return out
}

// ForceCleanAppByID 对任意程序执行强制清理：结束进程 → 删注册表项 → 删安装目录
func (a *App) ForceCleanAppByID(id string) []ResidueResult {
	e := lookupApp(id)
	if e == nil {
		return []ResidueResult{{Err: "找不到该程序，请重新扫描"}}
	}
	if e.Kind == "uwp" {
		return []ResidueResult{{Err: "系统内置应用请用「移除」或「彻底移除」"}}
	}
	if e.Blocked != "" {
		return []ResidueResult{{Err: e.Blocked}}
	}
	sess := newSessionForEntry(e)
	appMu.Lock()
	appSess = sess
	appMu.Unlock()
	return a.ForceCleanApp()
}

// ForceCleanApp 强制清理：结束相关进程 → 删注册表项 → 删安装目录
func (a *App) ForceCleanApp() []ResidueResult {
	appMu.Lock()
	sess := appSess
	appMu.Unlock()
	if sess == nil {
		return nil
	}
	sess.mu.Lock()
	regKeys := append([]string(nil), sess.regKeys...)
	dirs := append([]string(nil), sess.dirs...)
	loc := sess.loc
	procs := append([]string(nil), sess.procs...)
	sess.mu.Unlock()

	var out []ResidueResult
	for _, p := range procs {
		for _, pid := range findProcess(p) {
			if h, err := windows.OpenProcess(windows.PROCESS_TERMINATE, false, pid); err == nil {
				_ = windows.TerminateProcess(h, 1)
				windows.CloseHandle(h)
			}
		}
	}
	time.Sleep(400 * time.Millisecond)

	if loc != "" {
		for _, pid := range findProcess(strings.ToLower(filepath.Base(loc))) {
			if h, err := windows.OpenProcess(windows.PROCESS_TERMINATE, false, pid); err == nil {
				_ = windows.TerminateProcess(h, 1)
				windows.CloseHandle(h)
			}
		}
	}
	for _, rk := range regKeys {
		r := ResidueResult{Kind: "reg", Path: rk}
		if regExists(rk) {
			if err := delRegTree(rk); err != nil {
				r.Err = err.Error()
			}
		} else {
			r.Err = "已不存在"
		}
		out = append(out, r)
	}
	for _, d := range dirs {
		if st, err := os.Stat(d); err != nil || !st.IsDir() {
			continue
		}
		r := ResidueResult{Kind: "dir", Path: d}
		st := removeTreeSizedCtx(context.Background(), d)
		r.Bytes = st.freed
		if st.failed > 0 {
			r.Err = fmt.Sprintf("%d 个文件被占用或无权限", st.failed)
		}
		out = append(out, r)
	}
	return out
}

// EndAppSession 结束当前卸载会话（关闭结果弹窗时调用）
func (a *App) EndAppSession() {
	appMu.Lock()
	appSess = nil
	appMu.Unlock()
}

// OpenAppFolder 在资源管理器中定位安装目录
func (a *App) OpenAppFolder(id string) string {
	e := lookupApp(id)
	if e == nil || e.InstallLoc == "" {
		return "该程序未记录安装目录"
	}
	p := e.InstallLoc
	if _, err := os.Stat(p); err != nil {
		return "安装目录不存在：" + p
	}
	c := exec.Command("explorer.exe", p)
	if err := c.Start(); err != nil {
		return "打开失败：" + err.Error()
	}
	go func() { _ = c.Wait() }()
	return ""
}

// 递归删除注册表分支
/* delRegTree / delRegValue 与 junk_reg.go 里的逐字相同（那两份就是从这里切出来的），不再重复定义。 */


// delRegValue 只删某个键下的单个值（MuiCache / Run 启动项等共享容器键专用，
// 绝不能整树删——那会把所有应用的数据一起抹掉）
