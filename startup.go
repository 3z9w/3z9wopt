package main

// 启动项管理（2026-09-20，2026-10-19 重写）：按 Windows 原生机制对齐功能构成——
// 启动项（注册表 Run / RunOnce + 启动文件夹）/ 服务项（Win32 自启动服务）/ 计划任务（非系统路径），
// 每项可启用/禁用（可恢复），带本地建议引擎与操作日志。
//
// 三条口径，本模块逐条对齐：
//   ① 启用/禁用是「双状态」：Win8+ 的禁用状态存在 Explorer\StartupApproved 里，
//      光看 Run 值在不在会和系统（任务管理器）各说各话 —— 读要读全，写要同步。
//   ② 禁用可恢复、删除不可恢复：禁用只改状态，Run 值原样保留；删除才动 Run 值，
//      两个动作在 UI 上是分开的（本模块对应 SetStartupEnable / DeleteStartup）。
//   ③ 扫全再筛：先枚举再按客观判据过滤，读不到和本来没有必须区分开。

import (
	"encoding/binary"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf16"
	"unsafe"

	ole "github.com/go-ole/go-ole"
	"github.com/go-ole/go-ole/oleutil"
	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
	"golang.org/x/text/encoding/simplifiedchinese"
)

type StartupItem struct {
	ID      string `json:"id"`
	Kind    string `json:"kind"` // run | svc | task
	Name    string `json:"name"`
	Desc    string `json:"desc"` // 文件描述 / 服务说明 / 任务说明
	Pub     string `json:"pub"`  // 公司名（取不到为空）
	Cmd     string `json:"cmd"`  // 完整命令行 / 服务映像 / 任务动作
	Loc     string `json:"loc"`  // 人类可读的位置说明
	Enabled bool   `json:"enabled"`
	Advice  string `json:"advice"` // "disable"=建议禁止，""=无建议
	Why     string `json:"why"`    // 建议理由
	IconKey string `json:"iconKey"`
	Sys     bool   `json:"sys"` // 系统自带组件（无建议，谨慎对待）

	// SaKnown 这一项在 StartupApproved 里**有**审批记录（系统那边有话要说）。
	// 不导出给前端，只在服务端用来决定「该听系统的还是听本机的备份」。见 ListStartup。
	SaKnown bool `json:"-"`

	// IsSys 这一项是 Windows 自带的（程序躺在系统目录里，或由系统在 \Microsoft\ 下注册）。
	// 不导出给前端：它只在服务端决定「默认列不列这一条」，见 suShowSys。
	IsSys bool `json:"-"`
}

type startupOp struct {
	Time int64  `json:"time"`
	Act  string `json:"act"` // disable | enable
	Kind string `json:"kind"`
	Name string `json:"name"`
	ID   string `json:"id"`
}

// 忽略区：用户明确不想被优化/提示的启动项，列表中隐藏，可随时取消
type suIgnItem struct {
	ID   string `json:"id"`
	Kind string `json:"kind"`
	Name string `json:"name"`
	Desc string `json:"desc"`
	Time int64  `json:"time"`
}

var (
	suMu       sync.Mutex
	suDisabled = map[string]json.RawMessage{} // id → 恢复载荷
	suLog      []startupOp
	suIgnored  []suIgnItem
	suLoaded   bool
)

// appDataDir 本机状态存放目录。
// LOCALAPPDATA 取不到时（服务账户、极端精简系统）会退化成相对路径 "3z9wOpt\..."，
// 写进进程工作目录 —— 换一种启动方式历史记录就全不见了。这里逐层兜底。
func appDataDir() string {
	if d := os.Getenv("LOCALAPPDATA"); d != "" {
		return filepath.Join(d, "3z9wOpt")
	}
	if d, err := os.UserCacheDir(); err == nil && d != "" {
		return filepath.Join(d, "3z9wOpt")
	}
	return filepath.Join(os.TempDir(), "3z9wOpt")
}

func suStatePath() string { return filepath.Join(appDataDir(), "startup_state.json") }
func suLogPath() string   { return filepath.Join(appDataDir(), "startup_log.json") }
func suIgnPath() string   { return filepath.Join(appDataDir(), "startup_ignore.json") }

func suLoadState() {
	if suLoaded {
		return
	}
	suLoaded = true
	if b, err := os.ReadFile(suStatePath()); err == nil {
		_ = json.Unmarshal(b, &suDisabled)
	}
	if b, err := os.ReadFile(suLogPath()); err == nil {
		_ = json.Unmarshal(b, &suLog)
	}
	if b, err := os.ReadFile(suIgnPath()); err == nil {
		_ = json.Unmarshal(b, &suIgnored)
	}
}

func suSaveState() {
	b, _ := json.MarshalIndent(suDisabled, "", " ")
	_ = os.WriteFile(suStatePath(), b, 0o644)
	lb, _ := json.MarshalIndent(suLog, "", " ")
	_ = os.WriteFile(suLogPath(), lb, 0o644)
	ib, _ := json.MarshalIndent(suIgnored, "", " ")
	_ = os.WriteFile(suIgnPath(), ib, 0o644)
}

func suBackup(id string, payload any) {
	b, _ := json.Marshal(payload)
	suDisabled[id] = b
}

func suLogAdd(act, kind, name, id string) {
	suLog = append(suLog, startupOp{Time: time.Now().Unix(), Act: act, Kind: kind, Name: name, ID: id})
	if len(suLog) > 200 {
		suLog = suLog[len(suLog)-200:]
	}
}

// GetStartupLog 最近操作记录（新的在前）
func (a *App) GetStartupLog() []startupOp {
	suMu.Lock()
	defer suMu.Unlock()
	suLoadState()
	out := make([]startupOp, len(suLog))
	for i, op := range suLog {
		out[len(suLog)-1-i] = op
	}
	return out
}

// HasStartupBackup 前端判断某禁用项是否可恢复（基本恒 true，保留扩展位）
func (a *App) HasStartupBackup(id string) bool {
	suMu.Lock()
	defer suMu.Unlock()
	suLoadState()
	_, ok := suDisabled[id]
	return ok
}

// StartupIgnore 加入/移出忽略区。加入时带快照信息用于忽略区展示。
func (a *App) StartupIgnore(id, name, kind, desc string, on bool) error {
	suMu.Lock()
	defer suMu.Unlock()
	suLoadState()
	if on {
		for _, g := range suIgnored {
			if g.ID == id {
				return nil
			}
		}
		suIgnored = append(suIgnored, suIgnItem{ID: id, Kind: kind, Name: name, Desc: desc, Time: time.Now().Unix()})
	} else {
		var kept []suIgnItem
		for _, g := range suIgnored {
			if g.ID != id {
				kept = append(kept, g)
			}
		}
		suIgnored = kept
	}
	suSaveState()
	return nil
}

// GetStartupIgnored 忽略区列表（新的在前）
func (a *App) GetStartupIgnored() []suIgnItem {
	suMu.Lock()
	defer suMu.Unlock()
	suLoadState()
	out := make([]suIgnItem, len(suIgnored))
	for i, g := range suIgnored {
		out[len(suIgnored)-1-i] = g
	}
	return out
}

// ---------- PE 版本信息（文件描述 / 公司名） ----------

var (
	modVer       = windows.NewLazySystemDLL("version.dll")
	procVerSize  = modVer.NewProc("GetFileVersionInfoSizeW")
	procVerInfo  = modVer.NewProc("GetFileVersionInfoW")
	procVerQuery = modVer.NewProc("VerQueryValueW")
	verInfoMu    sync.Mutex
)

func verInfo(exe string) (desc, company string) {
	verInfoMu.Lock()
	defer verInfoMu.Unlock()
	p := uintptr(unsafe.Pointer(windows.StringToUTF16Ptr(exe)))
	sz, _, _ := procVerSize.Call(p, 0)
	if sz == 0 {
		return
	}
	buf := make([]byte, sz)
	if r, _, _ := procVerInfo.Call(p, 0, sz, uintptr(unsafe.Pointer(&buf[0]))); r == 0 {
		return
	}
	/* VerQueryValue 的 Translation 查询回传的是「语言 + 代码页」两个 uint16 的指针。
	   v1.0 把它收在 uintptr 里再转回指针，go vet 会判为 unsafe.Pointer 误用；
	   这里直接声明成 *uint16（指针的指针），回来后用 unsafe.Slice 读两个元素 —— 语义一样，类型安全。 */
	var transPtr *uint16
	var tl uint32
	sub := utf16Ptr(`\VarFileInfo\Translation`)
	if r, _, _ := procVerQuery.Call(uintptr(unsafe.Pointer(&buf[0])), sub,
		uintptr(unsafe.Pointer(&transPtr)), uintptr(unsafe.Pointer(&tl))); r == 0 || tl < 4 || transPtr == nil {
		return
	}
	langCp := unsafe.Slice(transPtr, 2)
	lang := uint32(langCp[0])
	cp := uint32(langCp[1])
	for _, field := range []string{"FileDescription", "CompanyName"} {
		var val *uint16
		var vl uint32
		sp := utf16Ptr(fmt.Sprintf(`\StringFileInfo\%04x%04x\%s`, lang, cp, field))
		if r, _, _ := procVerQuery.Call(uintptr(unsafe.Pointer(&buf[0])), sp,
			uintptr(unsafe.Pointer(&val)), uintptr(unsafe.Pointer(&vl))); r != 0 && vl > 0 && val != nil {
			s := windows.UTF16PtrToString(val)
			if field == "FileDescription" {
				desc = s
			} else {
				company = s
			}
		}
	}
	return
}

func utf16Ptr(s string) uintptr {
	return uintptr(unsafe.Pointer(windows.StringToUTF16Ptr(s)))
}

// ---------- 命令行 → 主程序 ----------

var reQuotedExe = regexp.MustCompile(`^"([^"]+)"`)

// isExeName 判断一段文字是不是"像可执行文件"（以可执行后缀结尾）。
func isExeName(s string) bool {
	l := strings.ToLower(strings.TrimSpace(s))
	for _, x := range []string{".exe", ".com", ".bat", ".cmd", ".vbs", ".vbe", ".js", ".ps1", ".scr", ".msi"} {
		if strings.HasSuffix(l, x) {
			return true
		}
	}
	return false
}

// trimToExe 把一段路径截到 ".exe"（或其它可执行后缀）结尾，丢掉后面的残留参数。
func trimToExe(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.Index(strings.ToLower(s), ".exe"); i >= 0 {
		return s[:i+4]
	}
	return s
}

// cmdToExe 从一条命令行里取出可执行文件的完整路径。
//
// ⚠️ 2026-10-19 修正：原实现只认两种形态——带引号的 "xxx.exe" 和「第一个空格前面的整串」。
// 没加引号、路径里又带空格的命令行（系统里非常常见，Office 的计划任务就是这么写的：
//
//	<Command>C:\Program Files\Common Files\Microsoft Shared\ClickToRun\OfficeC2RClient.exe</Command>
//
// ）会被截成 "C:\Program" —— 图标取不到、文件存在性判断也跟着错。
// 在新增的「无效项」标记下，这个错误直接把**活着的** Office 任务标成了「该启动项无效」。
//
// 现在按"最长可存在前缀"取：从整串往回退，第一个「像可执行文件 && 磁盘上确实存在」的前缀就是它。
func cmdToExe(cmd string) string {
	cmd = strings.TrimSpace(cmd)
	// ① 带引号：引号里就是完整路径，最可靠
	if m := reQuotedExe.FindStringSubmatch(cmd); m != nil {
		return expandEnv(trimToExe(m[1]))
	}
	fields := strings.Fields(cmd)
	if len(fields) == 0 {
		return ""
	}
	// ② 没加引号：逐段加长试探，取存在的那个最长前缀
	//    C:\Program Files\a b\foo.exe /x  →  "C:\Program Files\a b\foo.exe"
	for i := len(fields); i >= 1; i-- {
		c := expandEnv(strings.Join(fields[:i], " "))
		if isExeName(c) && fileExists(c) {
			return c
		}
	}
	// ③ 全都对不上（程序已卸载、或路径含未展开变量）：退回第一个"像 exe"的切点，
	//    宁可保留一个看得出问题苗头的完整路径，也不要截成 "C:\Program" 这种假路径。
	for i, f := range fields {
		if isExeName(f) {
			return expandEnv(strings.Join(fields[:i+1], " "))
		}
	}
	return expandEnv(trimToExe(fields[0]))
}

// ---------- 建议引擎（本地启发式，宁少勿错） ----------

var reAutoAdvice = regexp.MustCompile(`(?i)update|upgrade|autoupdate|helper|assistant|crashpad|crashreport|reporter|telemetry|hotfix|promo|gameservice|anticheat|launcher`)

// 永不建议禁用的：安全软件 / 输入法 / 系统防护——新手绝对不该动的东西
var reNeverAdvice = regexp.MustCompile(`(?i)hips|huorong|360safe|360tray|360sd|360zip|zhudongfangyu|qqpct|guanjia|kaspersky|mcafee|norton|avast|avira|\bavg\b|eset|defender|security|wscsvc|sogou|inputmethod|ctfmon|baiduinput|qqpinyin|iflyime|kanji|keymouse|mousekey`)

func adviceFor(kind, name, exe string) (string, string, bool) {
	// why 一律给出可读理由：前端「保持现状」徽标与行内小字都展示它，
	// 否则新手看到「没建议却给了开关」会不知道该不该关（2026-09-23 走查观感问题）
	if exe != "" {
		if lp := strings.ToLower(expandEnv(exe)); strings.HasPrefix(lp, strings.ToLower(os.Getenv("SystemRoot"))) {
			return "", "Windows 自带组件，禁用可能影响系统功能，建议保留", true // 系统自带
		}
	}
	hay := strings.ToLower(name + " " + exe)
	if reNeverAdvice.MatchString(hay) {
		return "", "安全软件 / 输入法 / 系统防护类，禁用会让防护或输入失效，建议保留", false
	}
	if reAutoAdvice.MatchString(hay) {
		return "disable", "自动更新 / 辅助组件：禁用后软件主功能不受影响，需要时它会自己再启动", false
	}
	return "", "常用软件开机自启，不影响系统稳定；想加快开机也能手动禁用", false
}

// suMarkInvalid 标记「无效项」。
//
// 建议实际需要**两档**：`建议禁止`（东西还在，只是不用它开机跑）和 `建议删除`
// （程序已经不在了 —— 界面上标「该启动项无效」）。以前只有前一条，对不上。
// 这类项留着只会让人以为是活的，越积越多。
func suMarkInvalid(it *StartupItem) {
	if it.Cmd == "" {
		return
	}
	exe := cmdToExe(it.Cmd)
	if exe == "" || fileExists(exe) {
		return
	}
	it.Advice = "delete"
	it.Why = "这个启动项指向的程序已经不在了，留着也不会再启动，建议清理掉"
	it.Sys = false
	if !strings.Contains(it.Loc, "无效项") {
		it.Loc += " · 无效项"
	}
}

// adviceRank 排序权重：建议删除 > 建议禁止 > 保持现状（批量优化也按这个顺序挑）。
func adviceRank(a string) int {
	switch a {
	case "delete":
		return 0
	case "disable":
		return 1
	default:
		return 2
	}
}

// ---------- Windows 启动审批状态（StartupApproved）----------
//
// Win8 之后，Windows 自己的「禁用启动项」**不是删 Run 值**，而是在
//
//	HKCU|HKLM\Software\Microsoft\Windows\CurrentVersion\Explorer\StartupApproved\
//	    Run            —— 64 位 Run 键
//	    Run32          —— 32 位 Run 键（Wow6432Node）
//	    StartupFolder  —— 启动文件夹里的快捷方式（值名 = lnk 的完整路径）
//
// 下写一个 12 字节 blob，前 4 字节是小端 DWORD：
//
//	2 = 启用   3 = 用户禁用   6 = 策略禁用
//
// 任务管理器「启动」页、设置 → 启动，读的就是这里。
//
// ⚠️ 只认 Run 值在不在（2026-10-19 前的实现）会出两个错：
//
//	① 系统里已禁用的项，这里仍显示「已启用」—— 用户以为禁用没生效；
//	② 这里禁用后任务管理器仍显示「已启用」—— 两套状态各说各话。
//
// 正确做法是两边同步：禁用改这里，Run 值原样保留（所以可恢复）。
//
// 值名是**明文**（本机实测：HKLM\...\StartupApproved\Run 下值名就是 "AweSun"，
// 无内嵌 NUL，类型 REG_BINARY，12 字节）—— 可以直接按明文读写，不需要 base64。
const (
	saStateEnabled  = 2
	saStateDisabled = 3
)

const saBase = `Software\Microsoft\Windows\CurrentVersion\Explorer\StartupApproved`

// saSupported 这台机器认不认 StartupApproved 这套机制。
//
// ⚠️ 换机器就会踩：它是 Windows 8（NT 6.2）才引入的。Win7 / Server 2008 R2 上根本没有这个键，
// 照写也能写进去（CreateKey 会新建），但**系统不会去读它** ——
// 界面显示「已禁用」，开机它照跑。这种静默失效比直接报错危险得多。
// 检测不到就退回「删值 / 移走文件」的老办法：那条路径一定生效，而且仍然可恢复。
//
// 用 RtlGetVersion 而不是 GetVersionEx：后者在 Win8.1+ 上会按 manifest 说谎，
// 没嵌入 manifest 时永远报 6.2。
var (
	saSupOnce sync.Once
	saSupVal  bool
)

func saSupported() bool {
	saSupOnce.Do(func() {
		v := windows.RtlGetVersion()
		saSupVal = v != nil && (v.MajorVersion > 6 || (v.MajorVersion == 6 && v.MinorVersion >= 2))
	})
	return saSupVal
}

// saRead 读某个启动项的审批状态。返回 (state, 是否存在)。
// 读不到 / 类型不对 / 长度不足 / 系统不支持 → (0,false)，调用方按「未禁用」处理。
func saRead(hive registry.Key, sub, name string) (uint32, bool) {
	if !saSupported() {
		return 0, false
	}
	k, err := registry.OpenKey(hive, saBase+`\`+sub, registry.QUERY_VALUE)
	if err != nil {
		return 0, false
	}
	defer k.Close()
	b, typ, err := k.GetBinaryValue(name)
	if err != nil || typ != registry.BINARY || len(b) < 4 {
		return 0, false
	}
	return binary.LittleEndian.Uint32(b[0:4]), true
}

// saWrite 写审批状态。已存在的 blob 只改前 4 字节，保留后 8 字节的原有时间戳。
// 系统不支持时**直接报错**，让调用方退回「删值」—— 不能假装写成功了。
func saWrite(hive registry.Key, sub, name string, state uint32) error {
	if !saSupported() {
		return fmt.Errorf("此系统不支持启动审批状态（需要 Windows 8 及以上）")
	}
	k, _, err := registry.CreateKey(hive, saBase+`\`+sub, registry.QUERY_VALUE|registry.SET_VALUE)
	if err != nil {
		return err
	}
	defer k.Close()
	blob := saBlob(state)
	if old, typ, err := k.GetBinaryValue(name); err == nil && typ == registry.BINARY && len(old) >= 12 {
		copy(old[0:4], blob[0:4])
		blob = old
	}
	return k.SetBinaryValue(name, blob)
}

// saBlob 12 字节审批块：前 4 字节状态，后 8 字节留给 Windows 的时间戳（全 0 它也认）。
func saBlob(state uint32) []byte {
	b := make([]byte, 12)
	binary.LittleEndian.PutUint32(b[0:4], state)
	return b
}

// saDelete 删除审批记录（彻底删除启动项时用，不留残骸）。
func saDelete(hive registry.Key, sub, name string) {
	k, err := registry.OpenKey(hive, saBase+`\`+sub, registry.SET_VALUE)
	if err != nil {
		return
	}
	defer k.Close()
	_ = k.DeleteValue(name)
}

// 启动文件夹的审批记录：用户级在 HKCU、公共级在 HKLM，两边都写 / 两边都读才不会漏。
func saReadFolder(p string) (uint32, bool) {
	if st, ok := saRead(registry.CURRENT_USER, "StartupFolder", p); ok {
		return st, true
	}
	return saRead(registry.LOCAL_MACHINE, "StartupFolder", p)
}

func saWriteFolder(p string, state uint32) error {
	e1 := saWrite(registry.CURRENT_USER, "StartupFolder", p, state)
	e2 := saWrite(registry.LOCAL_MACHINE, "StartupFolder", p, state)
	if e1 != nil && e2 != nil {
		return e1
	}
	return nil
}

// ---------- 启动项（Run / RunOnce 键 + 启动文件夹） ----------

type runLoc struct {
	reg   registry.Key
	sub   string
	tag   string
	saSub string // 对应的 StartupApproved 子键；"" = 不参与审批状态（RunOnce）
}

// suBackupDir 启动文件夹快捷方式的临时存放目录（StartupApproved 写不进去时的兜底）。
func suBackupDir() string {
	d := filepath.Join(appDataDir(), "startup_backup")
	_ = os.MkdirAll(d, 0o755)
	return d
}

// setRunValue 按原值类型写回。
//
// ⚠️ Run 键里 REG_EXPAND_SZ 非常常见（形如 "%ProgramFiles%\X\Y.exe"）。
// 一律用 SetStringValue 恢复会把它降级成 REG_SZ —— Windows 不再展开环境变量，
// 这一项从此永久失效，而且**不报任何错**（2026-10-19 修）。
func setRunValue(k registry.Key, vn string, typ uint32, v string) error {
	switch typ {
	case registry.EXPAND_SZ:
		return k.SetExpandStringValue(vn, v)
	default:
		return k.SetStringValue(vn, v)
	}
}

// runValueType 决定恢复时该用什么值类型。
//
// 2026-10-19 之前的备份里没有 type 字段（老版本不存）。值里带 %xxx% 的几乎肯定
// 原本就是 REG_EXPAND_SZ —— 按 REG_SZ 写回去，Windows 不再展开环境变量，
// 这条启动项会直接失效。宁可按 EXPAND_SZ 写：它对不含变量的值同样正确。
func runValueType(p runBackup) uint32 {
	if p.Type != 0 {
		return p.Type
	}
	if strings.Contains(p.Value, "%") {
		return registry.EXPAND_SZ
	}
	return registry.SZ
}

// suStartupFolders 取「启动文件夹」的真实路径（用户级 + 公共级）。
//
// ⚠️ 换机器就会踩：原来直接用 APPDATA / ProgramData 两个环境变量拼路径。
// 这两个变量在不少环境里取不到 —— SYSTEM 账户、被改过的用户环境、
// 组策略把「开始菜单」重定向到别处、或者干脆是非中文 / 非英文的定制镜像。
// 一取不到就拼成相对路径 `Microsoft\Windows\Start Menu\...`，ReadDir 直接失败，
// 于是启动文件夹里的启动项**整片消失**，界面还表现成「这台机没有」。
//
// 先问系统要「已知文件夹」（FOLDERID_Startup / FOLDERID_CommonStartup），
// 拿不到再退回环境变量，两条路都试、按路径去重。
func suStartupFolders() []string {
	type cand struct {
		id  *windows.KNOWNFOLDERID
		env string
		sub string
	}
	list := []cand{
		{windows.FOLDERID_Startup, "APPDATA", `Microsoft\Windows\Start Menu\Programs\Startup`},
		{windows.FOLDERID_CommonStartup, "ProgramData", `Microsoft\Windows\Start Menu\Programs\Startup`},
	}
	var out []string
	seen := map[string]bool{}
	add := func(p string) {
		if p == "" || !dirExists(p) {
			return
		}
		k := strings.ToLower(filepath.Clean(p))
		if seen[k] {
			return
		}
		seen[k] = true
		out = append(out, p)
	}
	for _, c := range list {
		if p, err := windows.KnownFolderPath(c.id, 0); err == nil && p != "" {
			add(p)
		}
		if e := os.Getenv(c.env); e != "" {
			add(filepath.Join(e, c.sub))
		}
	}
	return out
}

func listRunItems(out *[]StartupItem, moved map[string]string) {
	locs := []runLoc{
		{registry.CURRENT_USER, `Software\Microsoft\Windows\CurrentVersion\Run`, "注册表 · 当前用户 Run", "Run"},
		{registry.LOCAL_MACHINE, `Software\Microsoft\Windows\CurrentVersion\Run`, "注册表 · 全机 Run", "Run"},
		{registry.LOCAL_MACHINE, `Software\Wow6432Node\Microsoft\Windows\CurrentVersion\Run`, "注册表 · 全机 Run (32位)", "Run32"},
		// RunOnce：属于启动项的常规扫描面。它只在下一次登录后跑一次，
		// Windows 不给它 StartupApproved（没有可恢复的"禁用"位），所以单独一类处理。
		{registry.CURRENT_USER, `Software\Microsoft\Windows\CurrentVersion\RunOnce`, "注册表 · 当前用户 RunOnce（下次登录后自动清除）", ""},
		{registry.LOCAL_MACHINE, `Software\Microsoft\Windows\CurrentVersion\RunOnce`, "注册表 · 全机 RunOnce（下次登录后自动清除）", ""},
		{registry.LOCAL_MACHINE, `Software\Wow6432Node\Microsoft\Windows\CurrentVersion\RunOnce`, "注册表 · 全机 RunOnce (32位)", ""},
	}
	for _, lc := range locs {
		k, err := registry.OpenKey(lc.reg, lc.sub, registry.QUERY_VALUE)
		if err != nil {
			continue
		}
		names, _ := k.ReadValueNames(-1)
		for _, vn := range names {
			v, _, err := k.GetStringValue(vn)
			if err != nil {
				continue // 非字符串值（DWORD 等）在 Run 键里没有意义，跳过
			}
			// ID 前缀决定写回哪个 hive：HKCU → run|，HKLM → runm|，
			// 否则禁用时一律开 CURRENT_USER 会报「找不到文件」。
			pfx := "run"
			if lc.reg == registry.LOCAL_MACHINE {
				pfx = "runm"
			}
			if lc.saSub == "" {
				pfx += "o" // RunOnce → runo / runmo
			}
			id := pfx + "|" + lc.sub + "|" + vn

			// 双状态：Run 值在 ≠ 已启用，还得看 StartupApproved
			enabled := true
			saKnown := false
			if lc.saSub != "" {
				if st, ok := saRead(lc.reg, lc.saSub, vn); ok {
					enabled = st == saStateEnabled
					saKnown = true
				}
			}
			it := makeRunItem(id, vn, v, lc.tag, cmdToExe(v), enabled)
			it.SaKnown = saKnown
			// 空命令不按"系统项"处理：解析不出程序的启动项宁可列出来让用户自己看
			it.IsSys = strings.TrimSpace(v) != "" && !suIsThirdParty(v)
			*out = append(*out, it)
		}
		k.Close()
	}
	// 启动文件夹（用户 + 公共）—— lnk 目标批量解析
	var lnks []string
	for _, dir := range suStartupFolders() {
		ents, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, en := range ents {
			if en.IsDir() || !strings.EqualFold(filepath.Ext(en.Name()), ".lnk") {
				continue
			}
			lnks = append(lnks, filepath.Join(dir, en.Name()))
		}
	}
	targets := lnkTargets(lnks)
	for _, full := range lnks {
		exe := targets[full]
		// 双状态：先看文件在不在（被移走 = 本工具兜底禁用），再看 StartupApproved
		enabled := fileExists(full)
		saKnown := false
		if enabled {
			if st, ok := saReadFolder(full); ok {
				enabled = st == saStateEnabled
				saKnown = true
			}
		}
		// cmd 传解析出的目标 exe（原来传空串 → 「复制命令行」按钮是灰的、中文名库也匹配不上）
		it := makeRunItem("runf|"+full, strings.TrimSuffix(filepath.Base(full), ".lnk"), exe, "启动文件夹", exe, enabled)
		it.IsSys = exe != "" && !suIsThirdParty(exe)
		// lnk 目标解析不出来时（UWP 快捷方式等）exe 为空 → IconKey 空 → 这一行没有图标。
		// 拿 lnk 本身当图标来源：ExtractIconEx 不认 lnk，GetAppIcon 会兜到 SHGetFileInfo，系统认。
		if it.IconKey == "" && fileExists(full) {
			it.IconKey = "w|" + full + "|0"
		}
		it.SaKnown = saKnown
		*out = append(*out, it)
	}
	// StartupApproved 写不进去、快捷方式被移到备份目录的那些项也要列出来 ——
	// 否则用户禁用完这一项直接从界面消失，就再也开不回来了（禁用的项始终留在列表里）。
	// moved 是 suEnumAll 在锁内做的快照：id → 原 lnk 路径。
	for id, orig := range moved {
		it := makeRunItem(id, strings.TrimSuffix(filepath.Base(orig), ".lnk"), "", "启动文件夹（已禁用）", "", false)
		it.Why = "本工具已禁用（快捷方式已移入备份目录），可随时恢复"
		*out = append(*out, it)
	}
}

// lnkTargets 批量解析快捷方式的目标 exe。
//
// ⚠️ 2026-10-12 重写：原实现走 PowerShell（New-Object -ComObject WScript.Shell），
// 在 PowerShell 组件缺失（本机实测 0x80070002）、执行策略受限、WSH 被禁的机器上直接失败 ——
// 启动文件夹里的快捷方式全部拿不到目标：没有图标、「打开所在位置」和「复制命令行」都是空的，
// 界面还把「读不到」演成「本来没有」。现在直接读 .lnk 二进制，只要文件能读就一定出结果。
func lnkTargets(paths []string) map[string]string {
	out := map[string]string{}
	for _, p := range paths {
		if t := lnkTarget(p); t != "" {
			out[p] = cmdToExe(t)
		}
	}
	return out
}

// lnkTarget 解析单个 .lnk 的目标路径（[MS-SHLLINK] 2.1~2.3）。
// 目标路径只认 LinkInfo 的 LocalBasePath + CommonPathSuffix —— 这是规范里唯一确定的
// 「本地目标完整路径」。RELATIVE_PATH 是相对路径、IDList 可能是壳命名空间，
// UWP 快捷方式根本没有 exe（这两种情况一律返回空，由调用方兜默认图标）。
func lnkTarget(path string) string {
	b, err := os.ReadFile(path)
	if err != nil || len(b) < 0x4C {
		return ""
	}
	if binary.LittleEndian.Uint32(b[0:4]) != 0x0000004C { // HeaderSize 固定 0x4C
		return ""
	}
	flags := binary.LittleEndian.Uint32(b[0x14:0x18])
	off := 0x4C
	if flags&0x01 != 0 { // 跳过 LinkTargetIDList
		if off+2 > len(b) {
			return ""
		}
		off += 2 + int(binary.LittleEndian.Uint16(b[off:off+2]))
	}
	if flags&0x02 == 0 || off+0x24 > len(b) { // 没有 LinkInfo 就无从谈本地路径
		return ""
	}
	// LinkInfo: Size(4) HeaderSize(4) Flags(4) VolIDOff(4) BasePathOff(4) NetRelOff(4) SuffixOff(4)
	//           [BasePathOffUnicode(4) SuffixOffUnicode(4)] —— 后两个仅 HeaderSize >= 0x24 时存在
	hdrSize := int(binary.LittleEndian.Uint32(b[off+4 : off+8]))
	if binary.LittleEndian.Uint32(b[off+8:off+0x0C])&0x01 == 0 { // VolumeIDAndLocalBasePath 未设置
		return ""
	}
	baseOff := int(binary.LittleEndian.Uint32(b[off+0x10 : off+0x14]))
	sufOff := int(binary.LittleEndian.Uint32(b[off+0x18 : off+0x1C]))
	if hdrSize >= 0x24 { // Unicode 偏移优先：中文路径也走这条，不存在再回退 ANSI
		if u := int(binary.LittleEndian.Uint32(b[off+0x1C : off+0x20])); u > 0 {
			baseOff = u
		}
		if u := int(binary.LittleEndian.Uint32(b[off+0x20 : off+0x24])); u > 0 {
			sufOff = u
		}
	}
	uni := hdrSize >= 0x24
	read := func(o int) string {
		if o <= 0 || off+o >= len(b) {
			return ""
		}
		i := off + o
		if uni {
			var u []uint16
			for i+1 < len(b) {
				c := binary.LittleEndian.Uint16(b[i:])
				i += 2
				if c == 0 {
					break
				}
				u = append(u, c)
			}
			return string(utf16.Decode(u))
		}
		j := i
		for j < len(b) && b[j] != 0 {
			j++
		}
		return decodeANSI(b[i:j])
	}
	base := read(baseOff)
	if base == "" {
		return ""
	}
	return base + read(sufOff)
}

// decodeANSI .lnk 里的 ANSI 字符串是系统代码页（简中 = GBK），直接当 UTF-8 读会出乱码
func decodeANSI(b []byte) string {
	if s, err := simplifiedchinese.GBK.NewDecoder().String(string(b)); err == nil {
		return s
	}
	return string(b)
}

// ---------- 常见启动项中文名库（新手友好：本地内置常见项） ----------

type suFriendlyEnt struct{ name, desc string }

// key：exe 文件名（小写，含 .exe）或名称关键词（不含 .exe，子串匹配）
var suFriendlyDB = map[string]suFriendlyEnt{
	"googleupdate.exe":      {"Google 更新", "Google 软件的自动更新组件，禁用后 Chrome 照常使用"},
	"edgeupdate.exe":        {"Edge 浏览器更新", "Microsoft Edge 的自动更新组件，禁用不影响浏览器使用"},
	"onedrive.exe":          {"OneDrive 云同步", "微软云盘同步工具，不用 OneDrive 可放心禁用"},
	"steam.exe":             {"Steam 游戏平台", "Steam 客户端开机自启，禁用后不影响游戏"},
	"dingtalk.exe":          {"钉钉", "钉钉办公通讯，禁用后手动打开即可"},
	"wechat.exe":            {"微信", "微信开机自启，禁用后需手动打开"},
	"weixin.exe":            {"微信", "微信开机自启，禁用后需手动打开"},
	"qq.exe":                {"QQ", "QQ 开机自启，禁用后需手动打开"},
	"todesk.exe":            {"ToDesk 远程控制", "远程控制软件，如需被随时远程连接请勿禁用"},
	"awesun.exe":            {"AweSun 远程控制", "远程控制软件，如需被随时远程连接请勿禁用"},
	"sunloginclient.exe":    {"向日葵远程控制", "远程控制软件，如需被随时远程连接请勿禁用"},
	"wallpaper32.exe":       {"Wallpaper Engine 动态壁纸", "动态壁纸引擎开机自启，禁用后壁纸恢复静态"},
	"everything.exe":        {"Everything 文件搜索", "全盘文件名秒搜工具，建议保留"},
	"snipaste.exe":          {"Snipaste 截图工具", "截图贴图工具，建议保留"},
	"utools.exe":            {"uTools 效率工具箱", "快捷启动器，建议保留"},
	"cloudmusic.exe":        {"网易云音乐", "网易云音乐开机自启，禁用不影响使用"},
	"kugou.exe":             {"酷狗音乐", "酷狗音乐开机自启，禁用不影响使用"},
	"qqmusic.exe":           {"QQ音乐", "QQ音乐开机自启，禁用不影响使用"},
	"bilibili.exe":          {"哔哩哔哩客户端", "B 站客户端开机自启，禁用不影响使用"},
	"douyin.exe":            {"抖音", "抖音桌面客户端开机自启，禁用不影响使用"},
	"baidunetdisk.exe":      {"百度网盘", "百度网盘开机自启，禁用后不影响下载"},
	"thunder.exe":           {"迅雷", "迅雷开机自启，禁用后不影响下载"},
	"hipsdaemon.exe":        {"火绒安全", "火绒安全核心服务，建议保留以维持防护"},
	"wsctrl.exe":            {"火绒安全", "火绒安全组件，建议保留以维持防护"},
	"usysdiag.exe":          {"火绒安全", "火绒安全组件，建议保留以维持防护"},
	"wpscenter.exe":         {"WPS Office", "WPS 云服务组件，禁用不影响文档编辑"},
	"wpscloudsvr.exe":       {"WPS 云服务", "WPS 云同步组件，不用云同步可禁用"},
	"spotify.exe":           {"Spotify 音乐", "Spotify 开机自启，禁用不影响使用"},
	"discord.exe":           {"Discord", "Discord 开机自启，禁用后需手动打开"},
	"telegram.exe":          {"Telegram", "Telegram 开机自启，禁用后需手动打开"},
	"code.exe":              {"VS Code 编辑器", "VS Code 开机自启，禁用后需手动打开"},
	"wegame.exe":            {"WeGame 游戏平台", "腾讯 WeGame 开机自启，禁用不影响游戏"},
	"tim.exe":               {"TIM", "腾讯 TIM 办公版开机自启，禁用后需手动打开"},
	"nvidia web helper.exe": {"NVIDIA 显卡组件", "NVIDIA 驱动辅助组件，禁用不影响显卡性能"},
	"teams.exe":             {"Microsoft Teams", "Teams 会议软件开机自启，不用可禁用"},
	"dropbox.exe":           {"Dropbox 云同步", "Dropbox 同步工具，不用可禁用"},
	// 名称关键词（服务 / 计划任务里经常没有独立 exe）
	"googleupdate":        {"Google 更新", "Google 软件的自动更新组件，禁用后 Chrome 照常使用"},
	"edgeupdate":          {"Edge 浏览器更新", "Microsoft Edge 的自动更新组件，禁用不影响浏览器使用"},
	"microsoftedgeupdate": {"Edge 浏览器更新", "Microsoft Edge 的自动更新组件，禁用不影响浏览器使用"},
	"onedrive":            {"OneDrive 云同步", "微软云盘同步工具，不用 OneDrive 可放心禁用"},
	"steam":               {"Steam 游戏平台", "Steam 客户端开机自启，禁用后不影响游戏"},
	"dingtalk":            {"钉钉", "钉钉办公通讯，禁用后手动打开即可"},
	"wechat":              {"微信", "微信开机自启，禁用后需手动打开"},
	"weixin":              {"微信", "微信开机自启，禁用后需手动打开"},
	"wallpaperengine":     {"Wallpaper Engine 动态壁纸", "动态壁纸引擎开机自启，禁用后壁纸恢复静态"},
	"cloudmusic":          {"网易云音乐", "网易云音乐开机自启，禁用不影响使用"},
	"baidunetdisk":        {"百度网盘", "百度网盘开机自启，禁用后不影响下载"},
	"sunlogin":            {"向日葵远程控制", "远程控制软件，如需被随时远程连接请勿禁用"},
	"aweray":              {"AweSun 远程控制", "远程控制软件，如需被随时远程连接请勿禁用"},
	"todesk":              {"ToDesk 远程控制", "远程控制软件，如需被随时远程连接请勿禁用"},
	"runplatform":         {"Chrome 更新辅助任务", "Google Chrome 的定时更新检查任务，禁用后浏览器仍会自动更新"},
}

// friendlyApply 用中文名库美化条目（exe 精确命中优先，其次名称关键词）
func friendlyApply(it *StartupItem) {
	base := ""
	if exe := cmdToExe(it.Cmd); exe != "" {
		base = strings.ToLower(filepath.Base(exe))
	}
	ent, ok := suFriendlyDB[base]
	if !ok {
		ln := strings.ToLower(it.Name)
		for k, f := range suFriendlyDB {
			if !strings.HasSuffix(k, ".exe") && strings.Contains(ln, k) {
				ent, ok = f, true
				break
			}
		}
	}
	if !ok {
		return
	}
	if ent.name != "" {
		it.Name = ent.name
	}
	if it.Desc == "" && ent.desc != "" {
		it.Desc = ent.desc
	}
}

// makeRunItem 组装一条启动项。enabled 由调用方判定：
// Run 键看 StartupApproved、启动文件夹看「文件在不在 + StartupApproved」。
func makeRunItem(id, name, cmd, loc, exe string, enabled bool) StartupItem {
	desc, pub := "", ""
	if exe != "" && fileExists(exe) {
		desc, pub = verInfoCached(exe)
	}
	adv, why, sys := adviceFor("run", name, exe)
	if !enabled {
		adv, why = "", ""
	}
	it := StartupItem{
		ID: id, Kind: "run", Name: name, Desc: desc, Pub: pub, Cmd: cmd, Loc: loc,
		Enabled: enabled, Advice: adv, Why: why, Sys: sys,
		IconKey: iconKeyOf(exe),
	}
	friendlyApply(&it)
	suMarkInvalid(&it)
	return it
}

// verCache PE 版本信息缓存。
//
// ⚠️ 换机器就会踩：服务项 267 条 + 计划任务 157 条，其中大批指向同一个文件
// （svchost.exe / OfficeC2RClient.exe / dllhost.exe …）。每条都跑一遍
// GetFileVersionInfoSizeW + GetFileVersionInfoW + VerQueryValueW，
// 在机械盘、网络盘、或者装了实时防护会拦截每次文件读取的机器上，一次扫描能被拖到好几秒。
// 同一个路径只问一次。
var (
	verCache   = map[string]verPair{}
	verCacheMu sync.Mutex
)

type verPair struct{ desc, company string }

// verInfoCached 带缓存的 verInfo。
// verInfo 自己会锁 verInfoMu，所以这里必须用另一把锁，且**不能持锁调用 verInfo**（否则自死锁）。
func verInfoCached(exe string) (string, string) {
	if exe == "" {
		return "", ""
	}
	k := strings.ToLower(exe)
	verCacheMu.Lock()
	p, ok := verCache[k]
	verCacheMu.Unlock()
	if ok {
		return p.desc, p.company
	}
	d, c := verInfo(exe)
	verCacheMu.Lock()
	if len(verCache) > 4096 {
		verCache = map[string]verPair{} // 兜底防无限涨：正常机器远到不了
	}
	verCache[k] = verPair{d, c}
	verCacheMu.Unlock()
	return d, c
}

// verCacheReset 强制重扫时清掉：软件刚更新过的话，文件描述不该还是旧的。
func verCacheReset() {
	verCacheMu.Lock()
	verCache = map[string]verPair{}
	verCacheMu.Unlock()
}

func iconKeyOf(exe string) string {
	if exe != "" && fileExists(exe) {
		return "w|" + exe + "|0"
	}
	return ""
}

// ---------- 服务项 ----------

// svcProtected 不建议禁用的 Windows 核心服务。
//
// ⚠️ 2026-10-19 口径变更：以前这些服务是**直接不列出**（连同所有系统目录下的服务一起被藏掉），
// 结果服务项只列得出 6 条，而实际的 Win32 服务有 267 条 —— 数量根本对不上。
// 系统项不该靠藏起来处理：列出来，用「官方建议 = 保持现状」提示，用户自己看得见、自己判断。
// 所以这里改成：照样列出来，把「别动它」写进建议里。
var svcProtected = map[string]bool{
	"schedule": true, "wuauserv": true, "bits": true, "cryptsvc": true, "winmgmt": true,
	"eventlog": true, "themes": true, "audiosrv": true, "audioendpointbuilder": true,
	"windefend": true, "wscsvc": true, "dhcp": true, "dnscache": true, "nsi": true,
	"plugplay": true, "profsvc": true, "staterepository": true, "tokenbroker": true,
	"usermanager": true, "uisvc": true, "cdpsvc": true, "cdpusersvc": true, "clipboardusersvc": true,
	"camusvc": true, "do": true, "installservice": true, "uiscriptenhancer": true,
}

func listSvcItems(out *[]StartupItem) {
	k, err := registry.OpenKey(registry.LOCAL_MACHINE, `SYSTEM\CurrentControlSet\Services`, registry.READ)
	if err != nil {
		return
	}
	names, _ := k.ReadSubKeyNames(-1)
	k.Close()
	for _, n := range names {
		sk, err := registry.OpenKey(registry.LOCAL_MACHINE, `SYSTEM\CurrentControlSet\Services\`+n, registry.READ)
		if err != nil {
			continue
		}
		typ, _, _ := sk.GetIntegerValue("Type")
		start, _, serr := sk.GetIntegerValue("Start")
		// 2=自动 3=手动 4=已禁用（0/1 是内核与系统引导驱动，不算启动项）
		// 手动的也要列：第三方服务不论启动类型都该让用户能管
		if serr != nil || typ&0x30 == 0 || start > 4 {
			sk.Close()
			continue
		}
		img, _, _ := sk.GetStringValue("ImagePath")
		// ⚠️ 2026-10-19：服务项列的是**全部 Win32 服务**（本机实测 267 条），一条不藏。
		// Windows 自己的那 261 条靠 IsSys 标记，默认口径下不显示，打开开关才列出来。
		desc, _, _ := sk.GetStringValue("Description")
		// Description / DisplayName 常写成间接字符串 "@%SystemRoot%\system32\xxx.dll,-200"，
		// 原样显示给用户就是一串天书。取不到人话就留空，让 friendlyApply / 文件名兜底。
		desc = plainSvcText(desc)
		if desc == "" {
			desc = plainSvcText(firstString(sk, "DisplayName"))
		}
		delayed := false
		if d, _, derr := sk.GetIntegerValue("DelayedAutoStart"); derr == nil && d == 1 {
			delayed = true
		}
		// 触发启动（Trigger Start）：Start 通常是 3（手动），但真正的启动条件是 TriggerInfo
		// 里注册的系统事件。不读它就只能显示"手动"，用户会误判成"开机不自启、可以随便关"。
		triggered := false
		if _, _, terr := sk.GetBinaryValue("TriggerInfo"); terr == nil {
			triggered = true
		}
		sk.Close()
		exe := cmdToExe(strings.ReplaceAll(img, `\??\`, ""))
		enabled := start != 4 // 手动(3) 也是开着，只是不自动跑
		manual := start == 3
		adv, why, sys := adviceFor("svc", n, exe)
		if !enabled {
			adv, why = "", ""
		}
		if svcProtected[strings.ToLower(n)] {
			adv, why, sys = "", "Windows 核心服务：禁用会让任务计划 / 系统更新 / 网络等功能失效，强烈建议保留", true
		}
		if triggered {
			adv = ""
			why = "触发启动：开机不自动运行，由系统事件（插入设备、网络变化等）按需唤起"
		} else if manual {
			adv = ""
			why = "手动启动：开机不会自动运行，由程序或系统在需要时唤起"
		}
		pub := ""
		if exe != "" && fileExists(exe) {
			_, pub = verInfoCached(exe)
		}
		loc := "服务 · " + n
		if delayed {
			loc += "（延迟启动）"
		}
		if triggered {
			loc += "（触发启动）"
		} else if manual {
			loc += "（手动启动）"
		}
		*out = append(*out, StartupItem{
			ID: "svc|" + n, Kind: "svc", Name: n, Desc: desc, Pub: pub,
			Cmd: expandEnv(strings.ReplaceAll(img, `\??\`, "")), Loc: loc,
			Enabled: enabled, Advice: adv, Why: why, Sys: sys,
			IconKey: iconKeyOf(exe),
		})
		it := &(*out)[len(*out)-1]
		it.IsSys = !suIsThirdParty(img)
		friendlyApply(it)
		suMarkInvalid(it)
	}
}

// firstString 取一个字符串值，取不到返回空。
func firstString(k registry.Key, name string) string {
	v, _, err := k.GetStringValue(name)
	if err != nil {
		return ""
	}
	return v
}

// plainSvcText 过滤掉间接字符串（@%SystemRoot%\...dll,-200 这类），
// 它只有资源加载器认得，直接显示给用户是没有意义的天书。
func plainSvcText(s string) string {
	s = strings.TrimSpace(s)
	if s == "" || strings.HasPrefix(s, "@") {
		return ""
	}
	return s
}

// ---------- 计划任务 ----------

// taskXML 只取需要的字段（命名空间忽略，按 local name 匹配）。
// Actions 下可能有多个 Exec（多动作计划任务），取第一个作为展示命令行。
type taskXML struct {
	RegistrationInfo struct {
		Description string `xml:"Description"`
		Author      string `xml:"Author"`
		URI         string `xml:"URI"`
	} `xml:"RegistrationInfo"`
	// ⚠️ 2026-10-19 改写：原来只声明 LogonTrigger / BootTrigger，其余触发器**解析不出来**。
	// 而 \Microsoft\ 的过滤判据恰好是「有没有 Logon/Boot」，结果把纯日历触发的 Office 任务
	// （Office ClickToRun Service Monitor 就只有 CalendarTrigger）误杀了 —— 想留的没留住。
	// 用 ,any + XMLName 把 <Triggers> 下所有子元素名原样收下来，一个不漏。
	Triggers struct {
		Any []struct {
			XMLName xml.Name
		} `xml:",any"`
	} `xml:"Triggers"`
	Settings struct {
		Enabled string `xml:"Enabled"`
		Hidden  string `xml:"Hidden"`
	} `xml:"Settings"`
	Actions struct {
		Exec []struct {
			Command   string `xml:"Command"`
			Arguments string `xml:"Arguments"`
		} `xml:"Exec"`
	} `xml:"Actions"`
}

// 触发器类型 → 中文。显示触发时机，用户靠它判断"这玩意儿什么时候跑"。
// （TASK_TRIGGER_TYPE2 的编号和这里的元素名是两套东西，别混着记：登录是 LogonTrigger=9，
//
//	而 2 是"每天"，记错就会把日常任务当成登录启动项。）
var taskTriggerCN = map[string]string{
	"BootTrigger":               "开机时",
	"LogonTrigger":              "登录时",
	"TimeTrigger":               "定时一次",
	"CalendarTrigger":           "按计划（每天/每周…）",
	"IdleTrigger":               "空闲时",
	"EventTrigger":              "事件触发",
	"RegistrationTrigger":       "任务注册时",
	"SessionStateChangeTrigger": "会话状态变化",
	"WnfStateChangeTrigger":     "系统内部通知",
}

// listTaskItems 枚举计划任务。
//
// ⚠️ 2026-10-12 重写：原实现走 PowerShell（Get-ScheduledTask | ConvertTo-Json），
// 在 PowerShell 组件缺失的机器上直接失败却被当成「这台机没有计划任务」—— 把读不到演成没有。
// 现在直接解析 %SystemRoot%\System32\Tasks 下的任务定义 XML（Task Scheduler 2.0 磁盘格式）：
// 无外部进程、无执行策略问题，且 <Enabled> 就是权威的启用状态（schtasks 的状态列还会被本地化）。
func listTaskItems(out *[]StartupItem) error {
	root := filepath.Join(suSystemRoot(), "System32", "Tasks")
	if _, err := os.Stat(root); err != nil {
		return fmt.Errorf("任务目录不可读: %w", err)
	}
	var walkErr error
	var bad, got int
	// ⚠️ 2026-10-19：原来是 `_ = filepath.WalkDir(...)` + `var walkErr error` ——
	// WalkDir 的返回值被丢掉，walkErr 从头到尾是 nil，`return walkErr` 永远返回 nil。
	// 结果「空列表不等于读成功了」这条设计（见 suEnumAll 注释）对任务分类**根本没生效**。
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			bad++
			return nil // 单个文件读不到（受保护的系统任务）不该让整个分类失败
		}
		if d.IsDir() {
			return nil
		}
		rel, e := filepath.Rel(root, p)
		if e != nil {
			return nil
		}
		parts := strings.Split(filepath.ToSlash(rel), "/")
		name := parts[len(parts)-1]
		dir := "\\"
		if len(parts) > 1 {
			dir = "\\" + strings.Join(parts[:len(parts)-1], "\\") + "\\"
		}
		var t taskXML
		if xml.Unmarshal([]byte(decodeTaskFile(p)), &t) != nil {
			bad++
			return nil
		}
		cmd := ""
		if len(t.Actions.Exec) > 0 {
			cmd = strings.TrimSpace(t.Actions.Exec[0].Command + " " + t.Actions.Exec[0].Arguments)
		}
		exe := cmdToExe(cmd)
		// ⚠️ 2026-10-19 口径变更：删掉了原来两条过滤——
		//   ① 「程序在系统目录 = Windows 内部任务，一律不列」
		//   ② 「\Microsoft\ 树只留带实质触发器的」（会把只有 CalendarTrigger 的 Office 任务误杀）
		// 计划任务不该按触发器过滤 —— 该不该显示，只应看任务自己声明的 <Hidden>。
		// 本机实测 212 个任务里 157 条非隐藏，以前的触发器过滤把 157 砍到 10 —— 对不上就是这么来的。
		//
		// <Hidden>true</Hidden>（本机 55 条）是任务**自己声明**「别在界面里显示我」，
		// Windows 自带的 taskschd.msc 同样遵守它 —— 属于客观判据，不是路径猜的。
		if strings.EqualFold(strings.TrimSpace(t.Settings.Hidden), "true") {
			return nil
		}
		enabled := true
		switch strings.TrimSpace(t.Settings.Enabled) {
		case "false":
			enabled = false
		}
		adv, why, sys := adviceFor("task", name, exe)
		if !enabled {
			adv, why = "", ""
		}
		// Windows 自带的任务照样列出来，但把「别动它」写进建议里，
		// 和服务项用的是同一套口径：系统项 = 官方建议「保持现状」，不是消失。
		// ⚠️ 只认 \Microsoft\Windows*（含 Windows Defender）：\Microsoft\Office\ 这类是
		// **用户装的软件**借系统目录注册的任务，该给建议就给建议（Office 自动更新会被判成建议禁止，
		// 不能因为它躺在 \Microsoft\ 下就一律免责。
		if strings.HasPrefix(dir, "\\Microsoft\\Windows") || suIsSystemExe(cmd) {
			adv, why, sys = "", "Windows 系统任务：由系统或自带组件注册，禁用可能影响更新、诊断、索引等功能，建议保留", true
		}
		desc, pub := t.RegistrationInfo.Description, ""
		if exe != "" && fileExists(exe) {
			vd, vp := verInfoCached(exe)
			if vd != "" {
				desc = vd
			}
			pub = vp
		}
		// 触发时机展示：用户靠它判断"这玩意儿到底什么时候会跑"
		var trigNames []string
		for _, tr := range t.Triggers.Any {
			if cn := taskTriggerCN[tr.XMLName.Local]; cn != "" {
				trigNames = append(trigNames, cn)
			}
		}
		loc := "计划任务 · " + dir
		if len(trigNames) > 0 {
			loc += " · " + strings.Join(trigNames, "/")
		}
		*out = append(*out, StartupItem{
			ID: "task|" + dir + name, Kind: "task", Name: name, Desc: desc, Pub: pub,
			Cmd: cmd, Loc: loc,
			Enabled: enabled, Advice: adv, Why: why, Sys: sys,
			IconKey: iconKeyOf(exe),
		})
		it := &(*out)[len(*out)-1]
		it.IsSys = !suIsThirdPartyTask(dir, cmd)
		friendlyApply(it)
		suMarkInvalid(it)
		got++
		return nil
	})
	if err != nil {
		walkErr = err
	}
	// 一个都没解析出来，但过程里确实有读不了的 —— 这是"没读到"，不能演成"这台机没有任务"
	if walkErr == nil && got == 0 && bad > 0 {
		walkErr = fmt.Errorf("任务目录里有 %d 个文件读不了或不是有效任务定义", bad)
	}
	return walkErr
}

// decodeTaskFile 任务定义 XML 是 UTF-16LE（带 FF FE BOM）；个别机器可能是 UTF-8。
// 不判 BOM 直接按 UTF-8 解会读出一半的 0x00 间隔字符，中文任务名全乱码。
func decodeTaskFile(p string) string {
	b, err := os.ReadFile(p)
	if err != nil {
		return ""
	}
	var s string
	if len(b) >= 2 && b[0] == 0xFF && b[1] == 0xFE {
		if len(b)%2 != 0 {
			b = b[:len(b)-1]
		}
		u := make([]uint16, (len(b)-2)/2)
		for i := range u {
			u[i] = binary.LittleEndian.Uint16(b[2+i*2:])
		}
		s = string(utf16.Decode(u))
	} else if len(b) >= 3 && b[0] == 0xEF && b[1] == 0xBB && b[2] == 0xBF {
		s = string(b[3:])
	} else {
		s = string(b)
	}
	// 声明里写的是 encoding="UTF-16"，而这里已经解成 UTF-8 字符串了。
	// 不剥掉声明，encoding/xml 会报「encoding "UTF-16" declared but CharsetReader is nil」，
	// 于是每一个任务都解析失败 —— 界面表现就是「计划任务 0 条」。
	if strings.HasPrefix(s, "<?xml") {
		if i := strings.Index(s, "?>"); i > 0 {
			s = s[i+2:]
		}
	}
	return s
}

// suSystemRoot SystemRoot 变量取不到时（极少数精简系统）退回 C:\Windows
func suSystemRoot() string {
	if w := os.Getenv("SystemRoot"); w != "" {
		return w
	}
	return `C:\Windows`
}

// suShowSys 「显示系统自带的」开关（默认关）。
//
// 2026-10-19 口径：默认只列**第三方软件装的**项 —— Windows 自己那 261 条服务、
// 153 条系统任务列出来只会把真正该管的几条埋掉（本机实测：服务 267→6，任务 157→12）。
// 想查系统服务时把它打开，全部照样列得出来，数据没有丢，只是默认不显示。
// 开关只在内存里：重开程序回到默认口径，不会被上次的状态带偏。
// ⚠️ 单独一把锁：ListStartup 全程持有 suMu，而开关要在那里面读，
// 共用 suMu 就是自己锁自己（Go 的 Mutex 不可重入）。
var (
	suShowSys   bool
	suShowSysMu sync.Mutex
)

func suShowSystem() bool {
	suShowSysMu.Lock()
	defer suShowSysMu.Unlock()
	return suShowSys
}

// SetStartupShowSystem 前端「显示系统自带的」开关。只切标记，不重扫 ——
// 缓存里三类都是全量，下一帧直接按新口径重画即可。
func (a *App) SetStartupShowSystem(on bool) {
	suShowSysMu.Lock()
	suShowSys = on
	suShowSysMu.Unlock()
}

// GetStartupShowSystem 当前是否在显示 Windows 自带的项（界面开关回显用）。
func (a *App) GetStartupShowSystem() bool { return suShowSystem() }

// suIsThirdParty 这一项是不是「第三方软件装的」。
//
// 判据（本机可复现：服务项 12、计划任务 13）：
//
//	① 程序不在系统目录 → 第三方（Office、GameViewer、AweSun 这类装在 Program Files / D 盘的）
//	② 程序就在系统目录，但它自己的「公司名」不是 Microsoft → 也算第三方。
//	   Intel 的显卡 / 芯片组服务全装在 System32 里（IntelCpHDCPSvc / IntelCpHeciSvc /
//	   jhi_service / RstMwService / WMIRegistrationService / TPMProvisioningService），
//	   只看路径会把这 6 条全漏掉 —— 只算路径是 6 条，加上它们才 12 条。
//	③ 其余（系统目录 + 微软公司名，或根本拿不到程序）→ Windows 自带。
//
// ⚠️ 为什么不用数字签名：第三方驱动普遍由 "Microsoft Windows Hardware Compatibility
// Publisher" 做 WHQL 签名，按签名判会把 Realtek / Intel 全归成微软（实测 448 个有实体的
// 注册表项里只剩 2 条「非微软」，明显失真）。公司名读的是 PE 版本资源里的 CompanyName，
// 和右键「属性 → 详细信息 → 公司」看到的是同一个字段。
func suIsThirdParty(cmdOrPath string) bool {
	if strings.TrimSpace(cmdOrPath) == "" {
		return false
	}
	if !suIsSystemExe(cmdOrPath) {
		return true
	}
	exe := cmdToExe(expandEnv(strings.ReplaceAll(strings.TrimSpace(cmdOrPath), `\??\`, "")))
	if exe == "" || !fileExists(exe) {
		return false // 文件不在，读不到公司名，按系统自带处理（不夸大第三方）
	}
	_, co := verInfoCached(exe)
	co = strings.TrimSpace(co)
	if co == "" {
		// 在系统目录、又读不到公司名（没有版本资源）→ 当系统自带。
		// 本机实测这条能剔掉 System32\OpenSSH\ssh-agent.exe 这类没有公司名的系统组件。
		return false
	}
	return !strings.Contains(strings.ToLower(co), "microsoft")
}

// suIsThirdPartyTask 计划任务版的同一判据。
// 没有可执行程序（COM 处理器类任务）时只能看它注册在谁的目录下。
func suIsThirdPartyTask(dir, cmd string) bool {
	if strings.TrimSpace(cmd) == "" {
		return !strings.HasPrefix(dir, `\Microsoft\`)
	}
	return suIsThirdParty(cmd)
}

// suIsSystemExe 判断一条命令行指向的程序是不是 Windows 自带的（位于系统目录）。
//
// 启动项管理里这类一律不列给用户：关掉可能搞坏系统，而且这些名字他根本不认识。
// 服务项与计划任务共用这条判据，口径才能一致。
//
// ⚠️ 必须 expandEnv 之后再比：注册表 ImagePath 常写成 %windir%\System32\xxx.exe，
// 任务 XML 的 Command 常写成 %windir%\system32\xxx.exe 或 \SystemRoot\System32\...。
// 不展开会把 ALG / Fax / VSS / TrustedInstaller 等一大堆系统服务误判成第三方
// （本机实测：不展开 = 21 项自动服务，展开后 = 12 项）。
func suIsSystemExe(cmdOrPath string) bool {
	s := strings.TrimSpace(cmdOrPath)
	if s == "" {
		return true // 拿不到程序 = 管不了，不列（如无 ImagePath 的虚拟服务、COM 类任务）
	}
	s = strings.ReplaceAll(s, `\??\`, "")
	s = strings.TrimSpace(expandEnv(s))
	exe := cmdToExe(s)
	if exe == "" {
		return true
	}
	lp := strings.ToLower(exe)
	sys := strings.ToLower(suSystemRoot())
	return strings.HasPrefix(lp, sys) ||
		strings.HasPrefix(lp, `\systemroot`) ||
		strings.HasPrefix(lp, `\??\`) ||
		strings.HasPrefix(lp, `system32\`) ||
		strings.HasPrefix(lp, `syswow64\`)
}

// taskSplit 把 task|<目录><名称> 拆成 COM 需要的 (目录, 名称)。
// ⚠️ COM 的 GetFolder 不接受目录末尾带反斜杠 —— 实测 GetFolder("\A\B\") 直接报「发生意外」，
// 而 XML 枚举出来的路径是带末尾反斜杠的。两边口径必须在这里统一，否则开关必然失败。
func taskSplit(key string) (folder, name string) {
	name = filepath.Base(key)
	folder = "\\" + strings.TrimSuffix(strings.TrimSuffix(strings.TrimPrefix(key, "\\"), name), "\\")
	return
}

// taskSetEnabled 用 Task Scheduler 自己的 COM 接口启用/禁用一个计划任务。
//
// ⚠️ COM 有线程亲和性：Wails 的绑定调用跑在 goroutine 上，OS 线程随时会迁移，
// 不锁线程会得到 0x800401F0（COM 未初始化）或随机失败 —— 与 wmi.go / hwspec.go 同一套写法。
// 另一个坑：go-ole 的 VARIANT 只能 Clear 一次，再 Release 一次会直接崩（实测 0xc0000005）。
func taskSetEnabled(folder, name string, enable bool) error {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	_ = ole.CoInitializeEx(0, ole.COINIT_MULTITHREADED)
	defer ole.CoUninitialize()

	unk, err := oleutil.CreateObject("Schedule.Service")
	if err != nil {
		return fmt.Errorf("无法创建计划任务组件（Task Scheduler 服务可能未运行）: %w", err)
	}
	defer unk.Release()
	svc, err := unk.QueryInterface(ole.IID_IDispatch)
	if err != nil {
		return fmt.Errorf("计划任务服务组件不可用: %w", err)
	}
	defer svc.Release()
	if _, err := oleutil.CallMethod(svc, "Connect"); err != nil {
		return fmt.Errorf("连接计划任务服务失败（服务可能被禁用）: %w", err)
	}
	fv, err := oleutil.CallMethod(svc, "GetFolder", folder)
	if err != nil {
		return fmt.Errorf("找不到计划任务目录 %s: %w", folder, err)
	}
	defer fv.Clear()
	tv, err := oleutil.CallMethod(fv.ToIDispatch(), "GetTask", name)
	if err != nil {
		return fmt.Errorf("找不到该计划任务（可能已被删除）: %w", err)
	}
	defer tv.Clear()
	if _, err := oleutil.PutProperty(tv.ToIDispatch(), "Enabled", enable); err != nil {
		return fmt.Errorf("修改计划任务失败（系统任务需要管理员权限）: %w", err)
	}
	return nil
}

// taskDelete 彻底删除一个计划任务（ITaskFolder::DeleteTask）。
//
// ⚠️ 不能只删 %SystemRoot%\System32\Tasks 下的 XML 文件：那样任务仍注册在
// Task Scheduler 服务里，会留下一个打不开也删不掉的幽灵任务，界面上还显示着。
func taskDelete(folder, name string) error {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	_ = ole.CoInitializeEx(0, ole.COINIT_MULTITHREADED)
	defer ole.CoUninitialize()

	unk, err := oleutil.CreateObject("Schedule.Service")
	if err != nil {
		return fmt.Errorf("无法创建计划任务组件（Task Scheduler 服务可能未运行）: %w", err)
	}
	defer unk.Release()
	svc, err := unk.QueryInterface(ole.IID_IDispatch)
	if err != nil {
		return fmt.Errorf("计划任务服务组件不可用: %w", err)
	}
	defer svc.Release()
	if _, err := oleutil.CallMethod(svc, "Connect"); err != nil {
		return fmt.Errorf("连接计划任务服务失败（服务可能被禁用）: %w", err)
	}
	fv, err := oleutil.CallMethod(svc, "GetFolder", folder)
	if err != nil {
		return fmt.Errorf("找不到计划任务目录 %s: %w", folder, err)
	}
	defer fv.Clear()
	if _, err := oleutil.CallMethod(fv.ToIDispatch(), "DeleteTask", name, 0); err != nil {
		return fmt.Errorf("删除计划任务失败（系统任务需要管理员权限）: %w", err)
	}
	return nil
}

// ---------- 对外接口 ----------

var (
	suCache   []StartupItem
	suCacheAt time.Time
	suDiag    = map[string]string{} // 分类 → 读取失败原因（"读不到" ≠ "本来没有"）
)

// IsElevated 当前进程是否管理员。
// 非管理员改不动服务项 / 全机启动项 / 系统计划任务，界面要提前说清楚，
// 不能等用户拨了开关才甩一句「需要管理员权限」。
func (a *App) IsElevated() bool { return isElevated() }

// GetStartupDiag 各分类的读取失败原因（空 = 三类都读成功了）。
// 界面上「0 项」必须靠它区分「这台机本来没有」和「没读到」，否则就是在骗人。
func (a *App) GetStartupDiag() map[string]string {
	suMu.Lock()
	defer suMu.Unlock()
	out := map[string]string{}
	for k, v := range suDiag {
		out[k] = v
	}
	return out
}

// suEnumAll 三源并行枚举。
// 第二个返回值是各分类的「读取失败原因」—— **空列表不等于读成功了**。
// 历史上计划任务读失败被界面显示成「计划任务 0」，用户以为自己机器上没有计划任务。
func suEnumAll() ([]StartupItem, map[string]string) {
	// 被本工具移进备份目录的启动文件夹快捷方式：id → 原路径。
	// 必须在启动 goroutine **之前**取快照 —— suDisabled 是有锁的共享状态，
	// 并发读写就是数据竞争。
	moved := map[string]string{}
	for id, raw := range suDisabled {
		if !strings.HasPrefix(id, "runf|") {
			continue
		}
		var p struct {
			Lnk string `json:"lnk"`
		}
		if json.Unmarshal(raw, &p) == nil && p.Lnk != "" && !fileExists(p.Lnk) {
			moved[id] = p.Lnk
		}
	}
	var runL, svcL, taskL []StartupItem
	var taskErr error
	var wg sync.WaitGroup
	wg.Add(3)
	go func() { defer wg.Done(); listRunItems(&runL, moved) }()
	go func() { defer wg.Done(); listSvcItems(&svcL) }()
	go func() { defer wg.Done(); taskErr = listTaskItems(&taskL) }()
	wg.Wait()
	diag := map[string]string{}
	if taskErr != nil {
		diag["task"] = taskErr.Error()
		cleanTrace("启动项：计划任务枚举失败 —— " + taskErr.Error())
	}
	return append(append(runL, svcL...), taskL...), diag
}

// ListStartup 枚举全部三类启动项（10 分钟缓存秒开；跳过已忽略项）
func (a *App) ListStartup() ([]StartupItem, error) {
	suMu.Lock()
	defer suMu.Unlock()
	suLoadState()
	if suCache == nil || time.Since(suCacheAt) >= 10*time.Minute {
		suCache, suDiag = suEnumAll()
		suCacheAt = time.Now()
	}
	ign := map[string]bool{}
	for _, g := range suIgnored {
		ign[g.ID] = true
	}
	out := make([]StartupItem, 0, len(suCache))
	seen := map[string]bool{}
	showSys := suShowSystem()
	for _, it := range suCache {
		if ign[it.ID] {
			continue
		}
		// 默认口径：Windows 自带的系统项不列出来（开关打开才列）。
		// ⚠️ 例外：本工具动过的项必须照样列 —— 禁用了却从列表消失 = 用户再也点不到「恢复」，
		// 这正是老版本"删值式禁用"的老毛病，不能在换口径时又犯一次。
		if !showSys && it.IsSys {
			if _, touched := suDisabled[it.ID]; !touched {
				continue
			}
		}
		// ⚠️ 谁说了算：系统里有 StartupApproved 记录时**听系统的**，本机备份只当恢复凭据。
		//   以前一律以 suDisabled 为准：用户在本工具里禁用后，又在「设置 → 启动」里改回启用，
		//   系统那边已经是启用了，这边还显示「已禁用」—— 又变成两套状态各说各话。
		//   只有在系统那边**没有**记录时（老版删值式禁用、Win7、RunOnce）才用本机备份兜底。
		if _, disabled := suDisabled[it.ID]; disabled && !it.SaKnown {
			it.Enabled = false
		}
		seen[it.ID] = true
		out = append(out, it)
	}
	// 本工具禁用过、但现在枚举不到的项要留在列表里（禁用项始终留在表里）：
	// 老版本是"删值"式禁用，值没了这一项就从界面消失 —— 用户再也点不到「恢复」。
	// 这里按备份补一条占位行，标成已禁用，用户随时能开回来。
	for id, raw := range suDisabled {
		if seen[id] || ign[id] {
			continue
		}
		if strings.HasPrefix(id, "svc|") || strings.HasPrefix(id, "task|") {
			continue // 服务 / 任务禁用后仍在枚举范围内，不会消失
		}
		var p runBackup
		if json.Unmarshal(raw, &p) != nil {
			continue
		}
		name := id
		if i := strings.LastIndex(id, "|"); i >= 0 {
			name = id[i+1:]
		}
		loc := "启动项（已禁用）"
		if p.Lnk != "" {
			name = strings.TrimSuffix(filepath.Base(p.Lnk), ".lnk")
			loc = "启动文件夹（已禁用）"
		}
		it := StartupItem{
			ID: id, Kind: "run", Name: name, Cmd: p.Value, Loc: loc, Enabled: false,
			Why:     "本工具已禁用，可随时恢复",
			IconKey: iconKeyOf(cmdToExe(p.Value)),
		}
		// 老版本「删值式」禁用留下的记录，如果连指向的程序都没了，这一项其实已经没用了 ——
		// 标成「无效项 / 建议删除」，用户可以一键清掉（恢复入口仍然保留）。
		if exe := cmdToExe(p.Value); exe != "" && !fileExists(exe) {
			it.Advice = "delete"
			it.Why = "这个启动项指向的程序已经不在了，留着也不会再启动，建议清理掉"
		}
		out = append(out, it)
	}
	// 排序：建议删除 > 建议禁止 > 保持现状，同级内已启用的在前（批量优化也按这个顺序挑）。
	sort.Slice(out, func(i, j int) bool {
		ri, rj := adviceRank(out[i].Advice), adviceRank(out[j].Advice)
		if ri != rj {
			return ri < rj
		}
		if out[i].Enabled != out[j].Enabled {
			return out[i].Enabled
		}
		return out[i].Name < out[j].Name
	})
	return out, nil
}

// ListStartupForce 强制重新扫描（用户点刷新按钮）
func (a *App) ListStartupForce() ([]StartupItem, error) {
	suMu.Lock()
	suCache = nil
	suMu.Unlock()
	verCacheReset() // 软件可能刚更新过，文件描述不该还是旧的
	return a.ListStartup()
}

// SetStartupEnable 启用/禁用一个启动项。disable 会先备份原状以便恢复。成功后同步缓存，前端秒开不重扫。
func (a *App) SetStartupEnable(id string, enable bool) (string, error) {
	suMu.Lock()
	defer suMu.Unlock()
	s, err := suSetEnable(id, enable)
	if err == nil {
		for i := range suCache {
			if suCache[i].ID == id {
				suCache[i].Enabled = enable
			}
		}
	}
	// 操作记录：开关开机启动项是实打实改系统，必须留痕
	name := id
	for i := range suCache {
		if suCache[i].ID == id {
			name = suCache[i].Name
			break
		}
	}
	e := OpLogEntry{
		Kind:  "startup",
		Title: "开机启动项：" + map[bool]string{true: "启用", false: "禁用"}[enable] + "「" + name + "」",
		Items: []string{name},
		OK:    1,
	}
	if err != nil {
		e.OK, e.Fail, e.Err = 0, 1, err.Error()
	}
	appendOpLog(e)
	return s, err
}

// suSplitKey 把 "<sub>|<valueName>" 拆开（valueName 里也可能有 |，所以从右往左找）。
func suSplitKey(key string) (sub, vn string, ok bool) {
	i := strings.LastIndex(key, "|")
	if i < 0 {
		return "", "", false
	}
	return key[:i], key[i+1:], true
}

// suUIKind ID 前缀 → 前端分类（SU_KIND 只认 run / svc / task）。
func suUIKind(pfx string) string {
	switch pfx {
	case "svc":
		return "svc"
	case "task":
		return "task"
	default:
		return "run"
	}
}

// runBackup 启动项的恢复载荷。带 type 是必须的：见 setRunValue 的注释。
type runBackup struct {
	Value string `json:"value"`
	Type  uint32 `json:"type"`
	Lnk   string `json:"lnk,omitempty"` // 仅 runf：快捷方式原路径
	Mode  string `json:"mode"`          // approved=改审批状态 / value=删值 / folder=移走文件
}

func suSetEnable(id string, enable bool) (string, error) {
	suLoadState()
	parts := strings.SplitN(id, "|", 2)
	if len(parts) != 2 {
		return "", fmt.Errorf("无效的启动项 ID")
	}
	pfx, key := parts[0], parts[1]
	var name string
	switch pfx {
	case "run", "runm":
		// run|<sub>|<valueName>（HKCU）/ runm|<sub>|<valueName>（HKLM）
		//
		// ⚠️ 2026-10-19 改写：禁用 = 改 StartupApproved 的审批状态，
		// Run 值原样保留 —— 可恢复、任务管理器同步显示「已禁用」。
		// 旧实现是 DeleteValue（= 删除），备份只在本机 JSON 里，丢了就永远找不回来。
		hive := registry.CURRENT_USER
		if pfx == "runm" {
			hive = registry.LOCAL_MACHINE
		}
		sub, vn, ok := suSplitKey(key)
		if !ok {
			return "", fmt.Errorf("无效的 Run 项")
		}
		name = vn
		saSub := "Run"
		if strings.Contains(strings.ToLower(sub), "wow6432node") {
			saSub = "Run32"
		}
		if enable {
			// ① 值被（老版本或其它工具）删过就先补回去，按原类型写
			if raw, has := suDisabled[id]; has {
				var p runBackup
				if json.Unmarshal(raw, &p) == nil && p.Value != "" {
					if k, err := registry.OpenKey(hive, sub, registry.QUERY_VALUE|registry.SET_VALUE); err == nil {
						if _, _, gerr := k.GetStringValue(vn); gerr != nil {
							_ = setRunValue(k, vn, runValueType(p), p.Value)
						}
						k.Close()
					}
				}
			}
			// ② 审批状态写回启用
			if err := saWrite(hive, saSub, vn, saStateEnabled); err != nil {
				return "", fmt.Errorf("恢复启动项失败（需要管理员权限）: %w", err)
			}
		} else {
			// 先把值和类型存下来（无论走哪条路径都能恢复）
			if k, err := registry.OpenKey(hive, sub, registry.QUERY_VALUE); err == nil {
				if v, vt, gerr := k.GetStringValue(vn); gerr == nil {
					suBackup(id, runBackup{Value: v, Type: vt, Mode: "approved"})
				}
				k.Close()
			}
			err := saWrite(hive, saSub, vn, saStateDisabled)
			// 兜底：写完**一定回读一次**。审批状态写不进去、或这台机根本不认它
			// （组策略限制、精简系统）时，退回删值 —— 宁可真的禁掉，
			// 也不能出现「界面显示已禁用、实际照跑」这种假安全感。
			// 这时备份里已经带上了值类型，恢复不会把 REG_EXPAND_SZ 降级成 REG_SZ。
			st, rd := saRead(hive, saSub, vn)
			if err != nil || !rd || st != saStateDisabled {
				k, oerr := registry.OpenKey(hive, sub, registry.QUERY_VALUE|registry.SET_VALUE)
				if oerr != nil {
					if err != nil {
						return "", fmt.Errorf("禁用启动项失败（需要管理员权限）: %w", err)
					}
					return "", fmt.Errorf("打开注册表失败: %w", oerr)
				}
				v, vt, gerr := k.GetStringValue(vn)
				if gerr != nil {
					k.Close()
					return "", fmt.Errorf("读取启动项失败: %w", gerr)
				}
				suBackup(id, runBackup{Value: v, Type: vt, Mode: "value"})
				derr := k.DeleteValue(vn)
				k.Close()
				if derr != nil {
					return "", fmt.Errorf("删除启动项失败: %w", derr)
				}
			}
		}
	case "runf":
		// 启动文件夹 .lnk
		//
		// ⚠️ 2026-10-19：这一段以前**根本走不到** —— 启动文件夹项的 ID 前缀是 runf，
		// 而这里只有 case "run","runm"，runf 掉进了 default 报「未知的启动项类型」。
		// 快捷方式的开关从来就没能用过。
		lnk := key
		name = strings.TrimSuffix(filepath.Base(lnk), ".lnk")
		bakDir := suBackupDir()
		if enable {
			// ① 文件被移走过就先挪回来
			bak := filepath.Join(bakDir, filepath.Base(lnk))
			if fileExists(bak) && !fileExists(lnk) {
				if err := os.Rename(bak, lnk); err != nil && !fileExists(lnk) {
					return "", fmt.Errorf("恢复启动项失败: %w", err)
				}
			}
			if !fileExists(lnk) {
				return "", fmt.Errorf("找不到该快捷方式，无法恢复")
			}
			// ② 清掉审批状态
			_ = saWriteFolder(lnk, saStateEnabled)
		} else {
			if !fileExists(lnk) {
				return "", fmt.Errorf("该快捷方式已不存在")
			}
			// Windows 原生口径：写 StartupApproved\StartupFolder，快捷方式**留在原处**
			// （界面上仍是「已禁用」，还能再开回来；删掉才会从界面消失）。
			err := saWriteFolder(lnk, saStateDisabled)
			st, rd := saReadFolder(lnk)
			if err != nil || !rd || st != saStateDisabled {
				// 兜底：审批状态这套在这台机上不生效 → 直接把快捷方式移走（一定生效）
				if e2 := os.Rename(lnk, filepath.Join(bakDir, filepath.Base(lnk))); e2 != nil {
					if err != nil {
						return "", fmt.Errorf("禁用启动项失败: %w", err)
					}
					return "", fmt.Errorf("移动启动项失败: %w", e2)
				}
				suBackup(id, runBackup{Lnk: lnk, Mode: "folder"})
			} else {
				suBackup(id, runBackup{Lnk: lnk, Mode: "approved"})
			}
		}
	case "runo", "runmo":
		// RunOnce：Windows 不给它审批状态位，没有"禁用"这个概念 ——
		// 只能删值（它本来也只在下次登录后跑一次），按原类型备份以便恢复。
		hive := registry.CURRENT_USER
		if pfx == "runmo" {
			hive = registry.LOCAL_MACHINE
		}
		sub, vn, ok := suSplitKey(key)
		if !ok {
			return "", fmt.Errorf("无效的 RunOnce 项")
		}
		name = vn
		if enable {
			raw, has := suDisabled[id]
			if !has {
				return "", fmt.Errorf("没有该项目的备份，无法恢复")
			}
			var p runBackup
			_ = json.Unmarshal(raw, &p)
			k, err := registry.OpenKey(hive, sub, registry.SET_VALUE)
			if err != nil {
				return "", fmt.Errorf("打开注册表失败: %w", err)
			}
			err = setRunValue(k, vn, runValueType(p), p.Value)
			k.Close()
			if err != nil {
				return "", fmt.Errorf("恢复启动项失败: %w", err)
			}
		} else {
			k, err := registry.OpenKey(hive, sub, registry.QUERY_VALUE|registry.SET_VALUE)
			if err != nil {
				return "", fmt.Errorf("打开注册表失败: %w", err)
			}
			v, vt, gerr := k.GetStringValue(vn)
			if gerr != nil {
				k.Close()
				return "", fmt.Errorf("读取启动项失败: %w", gerr)
			}
			suBackup(id, runBackup{Value: v, Type: vt, Mode: "value"})
			derr := k.DeleteValue(vn)
			k.Close()
			if derr != nil {
				return "", fmt.Errorf("删除启动项失败: %w", derr)
			}
		}
	case "svc":
		name = key
		sk, err := registry.OpenKey(registry.LOCAL_MACHINE, `SYSTEM\CurrentControlSet\Services\`+key, registry.QUERY_VALUE|registry.SET_VALUE)
		if err != nil {
			return "", fmt.Errorf("打开服务失败（需要管理员权限）: %w", err)
		}
		defer sk.Close()
		if enable {
			raw, ok := suDisabled[id]
			if !ok {
				return "", fmt.Errorf("没有该服务的备份，无法恢复")
			}
			var p struct {
				Start   uint64 `json:"start"`
				Delayed uint64 `json:"delayed"`
			}
			_ = json.Unmarshal(raw, &p)
			if err := sk.SetDWordValue("Start", uint32(p.Start)); err != nil {
				return "", fmt.Errorf("恢复服务失败: %w", err)
			}
			if p.Delayed == 1 {
				sk.SetDWordValue("DelayedAutoStart", 1)
			} else {
				sk.DeleteValue("DelayedAutoStart")
			}
		} else {
			start, _, _ := sk.GetIntegerValue("Start")
			delayed, _, _ := sk.GetIntegerValue("DelayedAutoStart")
			suBackup(id, map[string]uint64{"start": start, "delayed": delayed})
			if err := sk.SetDWordValue("Start", 4); err != nil {
				return "", fmt.Errorf("禁用服务失败（需要管理员权限）: %w", err)
			}
		}
	case "task":
		// ⚠️ 2026-10-12 改写：原实现用 PowerShell 的 Enable/Disable-ScheduledTask，
		// 在 PowerShell 组件缺失或执行策略受限的机器上必然失败（且错误信息是 GBK 乱码）。
		// 现在走 Task Scheduler 自己的 COM 接口（Schedule.Service），不依赖任何脚本宿主。
		tp, tn := taskSplit(key)
		name = tn
		if err := taskSetEnabled(tp, tn, enable); err != nil {
			return "", err
		}
	default:
		return "", fmt.Errorf("未知的启动项类型（%s）—— 这一项无法启用/禁用", pfx)
	}
	if enable {
		delete(suDisabled, id)
		suLogAdd("enable", suUIKind(pfx), name, id)
	} else {
		suLogAdd("disable", suUIKind(pfx), name, id)
	}
	suSaveState()
	return name, nil
}

// DeleteStartup 彻底删除一个启动项 —— **不可恢复**，与「禁用」是两个不同的动作。
//
// 两者分开：禁用改状态、可恢复；删除动真格、UI 上明示不可恢复。
// 混在一起的话，用户点一下开关就可能永久丢掉一条启动项。
//
// 服务不支持删除：删错一个服务可能让系统组件直接失效，只能禁用。
func (a *App) DeleteStartup(id string) (string, error) {
	suMu.Lock()
	defer suMu.Unlock()
	name, err := suDelete(id)
	e := OpLogEntry{
		Kind:  "startup",
		Title: "开机启动项：删除「" + name + "」（不可恢复）",
		Items: []string{name},
		OK:    1,
	}
	if err != nil {
		if name == "" {
			name = id
		}
		e.OK, e.Fail, e.Err = 0, 1, err.Error()
	}
	appendOpLog(e)
	return name, err
}

func suDelete(id string) (string, error) {
	suLoadState()
	parts := strings.SplitN(id, "|", 2)
	if len(parts) != 2 {
		return "", fmt.Errorf("无效的启动项 ID")
	}
	pfx, key := parts[0], parts[1]
	var name string
	switch pfx {
	case "run", "runm", "runo", "runmo":
		hive := registry.CURRENT_USER
		if pfx == "runm" || pfx == "runmo" {
			hive = registry.LOCAL_MACHINE
		}
		sub, vn, ok := suSplitKey(key)
		if !ok {
			return "", fmt.Errorf("无效的启动项")
		}
		name = vn
		saSub := ""
		if pfx == "run" || pfx == "runm" {
			saSub = "Run"
			if strings.Contains(strings.ToLower(sub), "wow6432node") {
				saSub = "Run32"
			}
		}
		k, err := registry.OpenKey(hive, sub, registry.QUERY_VALUE|registry.SET_VALUE)
		if err != nil {
			return name, fmt.Errorf("打开注册表失败: %w", err)
		}
		derr := k.DeleteValue(vn)
		k.Close()
		if derr != nil {
			return name, fmt.Errorf("删除启动项失败: %w", derr)
		}
		if saSub != "" {
			saDelete(hive, saSub, vn) // 不留审批状态残骸
		}
		delete(suDisabled, id)
	case "runf":
		lnk := key
		name = strings.TrimSuffix(filepath.Base(lnk), ".lnk")
		if err := os.Remove(lnk); err != nil && !os.IsNotExist(err) {
			return name, fmt.Errorf("删除快捷方式失败: %w", err)
		}
		// 备份目录里可能还留着被移走的那一份，一并清掉
		_ = os.Remove(filepath.Join(suBackupDir(), filepath.Base(lnk)))
		saDelete(registry.CURRENT_USER, "StartupFolder", lnk)
		saDelete(registry.LOCAL_MACHINE, "StartupFolder", lnk)
		delete(suDisabled, id)
	case "task":
		// 计划任务用 COM 删（直接删磁盘 XML 不会从计划任务服务里注销，会留下幽灵任务）
		folder, tn := taskSplit(key)
		name = tn
		if err := taskDelete(folder, tn); err != nil {
			return name, err
		}
		delete(suDisabled, id)
	case "svc":
		return "", fmt.Errorf("服务不支持删除 —— 删错服务可能导致系统组件失效，只能禁用")
	default:
		return "", fmt.Errorf("未知的启动项类型（%s）", pfx)
	}
	suLogAdd("delete", suUIKind(pfx), name, id)
	suSaveState()
	return name, nil
}
