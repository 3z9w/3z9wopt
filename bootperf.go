package main

/* ══════════════════════════════════════════════════════════════════════════
   开机耗时（bootperf）—— 2026-10-26 第一期：只做实测展示，不做任何提速预测。
   ---------------------------------------------------------------------------
   数据不是本软件掐表掐出来的 —— 软件自己就是开机后期才启动的，根本测不到
   内核加载那一段。真正的来源是 Windows 自己记的账：
     Microsoft-Windows-Diagnostics-Performance/Operational
       Event 100       每次开机一条（BootTime = MainPath + PostBoot）
       Event 101/102/103  被点名拖慢的组件（101 应用 / 102 驱动 / 103 服务）

   ⚠️ 口径三条，界面上必须照这个讲，否则用户拿秒表一对就穿帮：
     1) 起点是**内核加载**，不是按下电源键 —— BIOS/POST/UEFI 测不到。
     2) "进桌面" 是 MainPathBootTime，不是 BootTime。BootTime 里还含
        PostBoot 结束前那 10 秒的 idle 判定，拿它当"开机多久"会被质疑。
     3) 开了快速启动（HiberbootEnabled=1）时走的是休眠恢复路径，与冷启动
        不可比 —— 事件里没有 Hiberboot 字段，只能从注册表单独读出来提示用户。

   ⚠️ 本期刻意不做的事（别在这里加）：
     · 不预测"优化后能快几秒" —— 实测多项启动项耗时相加会超过 PostBoot 实际
       总时长（并行重叠），相加模型在数学上就不成立。
     · 不做"目标值"，只给实测值和"谁在拖后腿"。
       （记录：10-24 那次点名项合计 96.8s > PostBoot 实际 87.6s。）
   ══════════════════════════════════════════════════════════════════════════ */

import (
	"context"
	"encoding/xml"
	"errors"
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
	"unicode/utf8"

	"golang.org/x/sys/windows/registry"
)

const bootLogName = "Microsoft-Windows-Diagnostics-Performance/Operational"

/* ── 失败原因码 ──────────────────────────────────────────────────────────
   为什么不能只回一句"读取失败"：本机永远是管理员、日志永远开着，跑一次就通；
   换台机器失败的原因可能是权限、日志被关、系统精简、记录为空、超时……
   这些的处理方式完全不同（有的要提权、有的要点一下开启、有的只能等它开机几次），
   全说成"需要管理员"会把用户引到错误方向上，还显得软件不专业。
   前端按 code 显示对应引导，不要自己猜。 */
type bootErrCode string

const (
	bootErrNone     bootErrCode = ""
	bootErrNoAdmin  bootErrCode = "noadmin"  // 权限不足 → 引导以管理员重开
	bootErrNoLog    bootErrCode = "nolog"    // 通道不存在（精简系统/Server Core）→ 此功能不可用
	bootErrDisabled bootErrCode = "disabled" // 通道被关（不少优化软件会关）→ 可一键开启
	bootErrEmpty    bootErrCode = "empty"    // 还没记录 → 等几次开机
	bootErrTimeout  bootErrCode = "timeout"  // 读超时 → 重试
	bootErrNoTool   bootErrCode = "notool"   // 找不到 wevtutil → 系统异常
	bootErrParse    bootErrCode = "parse"    // 解析失败 → 记录原始错误供排查
)

// bootErrText 把原因码翻成人话。注意：不提"管理员"除非真的是权限问题。
func bootErrText(c bootErrCode, detail string) string {
	switch c {
	case bootErrNoAdmin:
		return "读取开机记录需要管理员权限，请退出后「以管理员身份运行」本程序"
	case bootErrNoLog:
		return "当前系统没有开机性能日志通道，此功能不可用"
	case bootErrDisabled:
		return "Windows 的开机性能日志处于关闭状态，开启后需重启一次才会开始记录"
	case bootErrEmpty:
		return "Windows 还没有记录到开机数据，正常开关机几次后会自动出现"
	case bootErrTimeout:
		return "读取开机记录超时，可稍后重试"
	case bootErrNoTool:
		return "系统缺少 wevtutil 组件，无法读取开机记录"
	case bootErrParse:
		if detail != "" {
			return "开机记录解析失败：" + detail
		}
		return "开机记录解析失败"
	}
	return detail
}

/* bootWevtutil 定位 wevtutil。
   不能只写 "wevtutil.exe" 依赖 PATH：PATH 被改过的机器（尤其是装了各种
   开发环境的）上会找不到或找到错的那个。优先 %SystemRoot%\System32。 */
func bootWevtutil() string {
	if p := sysExe("wevtutil.exe", ""); p != "wevtutil.exe" {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	if p, err := exec.LookPath("wevtutil.exe"); err == nil {
		return p
	}
	return ""
}

/* bootChannelState 查日志通道状态：0=不存在 1=存在但被关 2=存在且启用。
   用 `wevtutil gl /f:xml` 读 XML 里的 enabled 属性 —— 关键是**不能靠错误文本**
   判断：wevtutil 的错误信息是跟随系统语言的（中文机器上是「找不到指定的信道。」），
   拿文本做匹配在非中文系统上必然失效。退出码和 XML 属性才是跨语言稳定的。 */
func bootChannelState() int { return bootChannelStateOf(bootLogName) }

// bootChannelStateOf 带通道名参数 —— 测试要靠它拿一个"存在但关闭"的通道来
// 验证 disabled 分支，这样不必去改用户自己的日志设置。
func bootChannelStateOf(name string) int {
	exe := bootWevtutil()
	if exe == "" {
		return 0
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := bootCmd(ctx, exe, "gl", name, "/f:xml")
	b, err := cmd.Output()
	if err != nil {
		return 0 // 读不到配置 = 通道不存在（wevtutil 退出码 15007）
	}
	var ch struct {
		Enabled string `xml:"enabled,attr"`
	}
	if xml.Unmarshal(b, &ch) != nil {
		return 0
	}
	if strings.EqualFold(strings.TrimSpace(ch.Enabled), "true") {
		return 2
	}
	return 1
}

// EnableBootLog 打开开机性能日志。通道被关时给用户一个"点一下就好"的出口，
// 而不是让他自己去搜注册表。需要管理员；开启后要重启才会在日志里有记录。
func (a *App) EnableBootLog() bool {
	if !isElevated() {
		return false
	}
	exe := bootWevtutil()
	if exe == "" {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	err := bootCmd(ctx, exe, "sl", bootLogName, "/e:true").Run()
	return err == nil
}

/* bootCmd 起 wevtutil 进程。
   ⚠️ HideWindow / CREATE_NO_WINDOW 不是可选项：本程序是 -H windowsgui 编译的
   GUI 进程，自身没有控制台，子进程默认会被分配一个新控制台 —— 在开发者自己的
   机器上（从终端启动、或已附着控制台）看不出来，到普通用户桌面上就是每次
   进这个页面闪一个黑窗。必须显式关掉。 */
func bootCmd(ctx context.Context, exe string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, exe, args...)
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: 0x08000000}
	}
	return cmd
}

/* bootDecode 把 wevtutil 的输出转成字符串。
   wevtutil /f:xml 按规范输出 UTF-8，但程序名里带中文、且系统处在非 UTF-8
   代码页时，实际见过输出不是合法 UTF-8 的情况 —— 直接 string(b) 会把中文
   名字变成一堆 U+FFFD（界面上显示为"???"）。这里做一次 GBK 兜底。 */
func bootDecode(b []byte) string {
	if utf8.Valid(b) {
		return string(b)
	}
	return decodeANSI(b)
}

// 每次开机最多带几个"最慢组件"回前端。列表就那么高，带多了也没地方放。
const bootTopSlow = 3

// 往回读几次开机记录。够画趋势，又不至于让 wevtutil 跑太久。
const bootHistory = 6

type BootComponent struct {
	Name     string `json:"name"`
	Path     string `json:"path"`
	TotalMs  int    `json:"totalMs"`
	DegradMs int    `json:"degradMs"`
	Kind     string `json:"kind"` // app / driver / service

	// 下面四个是"能不能对它动手"的关键：Windows 点名的东西有一大半不在启动项列表里
	// （svchost、浏览器会话恢复、别的软件的子进程），那些只能陈述、不能给按钮。
	// 匹配上了才填，ItemID 为空 = 我们管不到它。
	ItemID      string `json:"itemId"`
	ItemName    string `json:"itemName"`
	ItemKind    string `json:"itemKind"`    // run / svc / task
	ItemEnabled bool   `json:"itemEnabled"` // 已禁用的不再给"禁用"按钮

	// NameBroken：事件里记的名字本身就是残缺的（实测遇到过 NameLength=7 却只有
	// "??exe" 五个字符的驱动记录，两条读取路径都一样，说明是源头坏数据）。
	// 前端拿到 true 就显示"名称无法识别"，不要把 "??exe" 直接甩给用户看。
	NameBroken bool `json:"nameBroken"`
}

type BootRecord struct {
	BootStart   string          `json:"bootStart"`   // 本次开机起始时刻（UTC，ISO8601）
	MainPathMs  int             `json:"mainPathMs"`  // 到桌面 —— 界面主数字用这个
	PostBootMs  int             `json:"postBootMs"`  // 桌面出来后后台全部就绪
	TotalMs     int             `json:"totalMs"`     // 两者之和（含 idle 判定，仅供参考）
	StartupApps int             `json:"startupApps"` // Windows 当时数到的启动项个数
	AfterUpdate bool            `json:"afterUpdate"` // 更新后首次开机：噪声样本，别拿来做对比
	Degraded    bool            `json:"degraded"`    // Windows 自己判定这次是否退化
	Slowest     []BootComponent `json:"slowest"`     // 本次被点名最慢的几个
}

type BootPerf struct {
	Ok        bool         `json:"ok"`
	Err       string       `json:"err"`       // 读不到时给人话原因，前端照着显示
	ErrCode   bootErrCode  `json:"errCode"`   // 机器可读的原因码，前端据此决定给什么引导（别靠 Err 文本猜）
	Hiberboot bool         `json:"hiberboot"` // 快速启动开关（Event 100 里没有，单独读注册表）
	Records   []BootRecord `json:"records"`   // 新的在前
}

/* ── wevtutil 查询 ────────────────────────────────────────────────────────
   先走 wevtutil 而不是直接 syscall wevtapi：项目里 junk_clean.go 已有调用先例，
   出效果快。真要抠性能再换 EvtQuery —— 接口不变，只换这一层。            */

type xmlData struct {
	Name  string `xml:"Name,attr"`
	Value string `xml:",chardata"`
}

type xmlEvent struct {
	System struct {
		EventID int `xml:"EventID"`
	} `xml:"System"`
	EventData struct {
		Data []xmlData `xml:"Data"`
	} `xml:"EventData"`
}

type xmlEvents struct {
	Events []xmlEvent `xml:"Event"`
}

// wevtutil 输出的每个 <Event> 都带 xmlns，而 Go 的 xml 包只匹配无命名空间
// 的字段名，直接解析会全部落空 —— 所以先把所有 xmlns 声明抹掉。
var reXmlns = regexp.MustCompile(`\s+xmlns(:\w+)?=["'][^"']*["']`)

// bootSanitize 清掉 XML 1.0 不允许的字符再解析。
//
// 这不是洁癖，是踩过的坑：本机 10-17 那条驱动记录的名字里带一个 0x03 控制字符
// （PowerShell 读同一条也报「十六进制值 0x03 是无效的字符」）。Go 的 xml 包遇到
// 它会报 invalid UTF-8 并让**整批**解析失败 —— 结果不是少一条，而是一条都拿不到，
// 界面上表现为"最慢项"永远空白。所以必须先洗一遍。
// 顺带 strings.Map 会把非法 UTF-8 字节折成 U+FFFD，解析就不会再中断。
func bootSanitize(s string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r == 0x09 || r == 0x0A || r == 0x0D: // tab / LF / CR 是合法的
			return r
		case r < 0x20: // 其余 C0 控制字符一律丢掉
			return -1
		case r == 0xFFFE || r == 0xFFFF:
			return -1
		}
		return r
	}, s)
}

/* bootQuery 统一入口 —— 实现在 bootperf_evtapi.go：
   优先 wevtapi.dll 原生查询（UTF-16 无损、不起进程、不依赖 PATH），
   失败才回退 wevtutil。调用方只管拿结果，不用关心走了哪条路。 */

var errBootNoTool = errors.New("wevtutil not found")

func bootMap(ev xmlEvent) map[string]string {
	m := make(map[string]string, len(ev.EventData.Data))
	for _, d := range ev.EventData.Data {
		m[d.Name] = d.Value
	}
	return m
}

func bootAtoi(s string) int {
	n, _ := strconv.Atoi(strings.TrimSpace(s))
	return n
}

func bootBool(s string) bool {
	s = strings.ToLower(strings.TrimSpace(s))
	return s == "true" || s == "1"
}

// hiberbootEnabled 读快速启动开关。Event 100 的 45 个字段里没有它，
// 只能从注册表拿；读不到就当没开（不因此判定整体失败）。
func hiberbootEnabled() bool {
	k, err := registry.OpenKey(registry.LOCAL_MACHINE,
		`SYSTEM\CurrentControlSet\Control\Session Manager\Power`,
		registry.QUERY_VALUE)
	if err != nil {
		return false
	}
	defer k.Close()
	v, _, err := k.GetIntegerValue("HiberbootEnabled")
	if err != nil {
		return false
	}
	return v != 0
}

/* bootNameBroken 判断事件里记的名字是不是残缺的。
   判据用 NameLength 与 Name 实际字符数对不上 —— 这是事件自带的长度字段，
   比"名字里有没有问号"可靠（合法文件名也可能含 ?）。
   实测样本：NameLength=7 但 Name="??exe"，原生 API 和 wevtutil 读出来都一样，
   说明是 Windows 写入时就坏了，我们无力还原，只能诚实地标出来。 */
func bootNameBroken(d map[string]string) bool {
	want := bootAtoi(d["NameLength"])
	if want <= 0 {
		return false // 没有长度字段可比对，不乱猜
	}
	return utf8.RuneCountInString(d["Name"]) < want
}

/* bootCache 开机数据的短缓存。
   每次进页面都查两次事件日志太重（慢机器上几百毫秒到几秒），而这份数据本身
   变化极慢 —— 一次开机才更新一次。60 秒足够，做完批量操作后前端会调
   RefreshBootPerf 强制刷新。 */
var (
	bootMu      sync.Mutex
	bootCache   *BootPerf
	bootCacheAt time.Time
)

// GetBootPerf 给前端的开机实测数据。读不到就返回 ok:false + 原因码 + 人话说明，
// 前端据此整个卡片不渲染 —— 宁可不显示，也不显示一个没底的数字。
func (a *App) GetBootPerf() BootPerf {
	bootMu.Lock()
	if bootCache != nil && time.Since(bootCacheAt) < 60*time.Second {
		out := *bootCache
		bootMu.Unlock()
		return out
	}
	bootMu.Unlock()

	out := a.readBootPerf()
	bootMu.Lock()
	bootCache = &out
	bootCacheAt = time.Now()
	bootMu.Unlock()
	return out
}

// RefreshBootPerf 强制重读（批量禁用/清理之后用，让卡片立刻反映最新状态）。
func (a *App) RefreshBootPerf() BootPerf {
	out := a.readBootPerf()
	bootMu.Lock()
	bootCache = &out
	bootCacheAt = time.Now()
	bootMu.Unlock()
	return out
}

func (a *App) readBootPerf() BootPerf {
	out := BootPerf{Hiberboot: hiberbootEnabled()}

	exe := bootWevtutil()
	if exe == "" {
		out.ErrCode = bootErrNoTool
		out.Err = bootErrText(bootErrNoTool, "")
		return out
	}

	evs, timedOut, err := bootQueryWithFallback("*[System[EventID=100]]", bootHistory)
	if err != nil {
		out.ErrCode, out.Err = bootClassifyErr(err, timedOut)
		return out
	}
	if len(evs) == 0 {
		/* 空结果有两种完全不同的成因，处理方式天差地别：
		   ① 通道被关（不少优化软件会关它）→ 可以一键开启
		   ② 通道开着但确实还没记录（刚启用/刚重装）→ 只能等
		   本机永远命中不到这两条分支，所以必须写死判据而不是拍脑袋。 */
		switch bootChannelState() {
		case 0:
			out.ErrCode, out.Err = bootErrNoLog, bootErrText(bootErrNoLog, "")
			return out
		case 1:
			out.ErrCode, out.Err = bootErrDisabled, bootErrText(bootErrDisabled, "")
			return out
		}
		out.ErrCode, out.Err = bootErrEmpty, bootErrText(bootErrEmpty, "")
		return out
	}

	// 组件事件一次多读些：一次开机可能点名好几项。
	comps, _, cerr := bootQueryWithFallback("*[System[(EventID=101 or EventID=102 or EventID=103)]]", bootHistory*12)
	if cerr != nil {
		comps = nil // 组件拿不到不影响主数据，只是"谁在拖后腿"那栏空着
	}
	// 按 StartTime 归到各次开机下
	byBoot := map[string][]BootComponent{}
	for _, c := range comps {
		d := bootMap(c)
		kind := ""
		switch c.System.EventID {
		case 101:
			kind = "app"
		case 102:
			kind = "driver"
		case 103:
			kind = "service"
		}
		byBoot[d["StartTime"]] = append(byBoot[d["StartTime"]], BootComponent{
			Name:       d["Name"],
			Path:       d["Path"],
			TotalMs:    bootAtoi(d["TotalTime"]),
			DegradMs:   bootAtoi(d["DegradationTime"]),
			Kind:       kind,
			NameBroken: bootNameBroken(d),
		})
	}
	for k := range byBoot {
		list := byBoot[k]
		sort.SliceStable(list, func(i, j int) bool { return list[i].TotalMs > list[j].TotalMs })
		if len(list) > bootTopSlow {
			list = list[:bootTopSlow]
		}
		byBoot[k] = list
	}

	for _, ev := range evs {
		d := bootMap(ev)
		rec := BootRecord{
			BootStart:   d["BootStartTime"],
			MainPathMs:  bootAtoi(d["MainPathBootTime"]),
			PostBootMs:  bootAtoi(d["BootPostBootTime"]),
			TotalMs:     bootAtoi(d["BootTime"]),
			StartupApps: bootAtoi(d["BootNumStartupApps"]),
			AfterUpdate: bootBool(d["BootIsRebootAfterInstall"]),
			Degraded:    bootBool(d["BootIsDegradation"]),
			Slowest:     byBoot[d["BootStartTime"]],
		}
		/* 字段缺失兜底：不同 Windows 版本（Win7/10/11、LTSC、Server）写进
		   Event 100 的字段并不完全一致，某些版本上 MainPathBootTime 会缺席。
		   这时用 BootTime - PostBoot 反推，而不是显示 0 —— 显示 0 秒会让
		   用户以为"我开机 0 秒"，一眼就看出是 bug。 */
		if rec.MainPathMs == 0 && rec.TotalMs > 0 && rec.PostBootMs > 0 {
			rec.MainPathMs = rec.TotalMs - rec.PostBootMs
		}
		if rec.MainPathMs < 0 {
			rec.MainPathMs = 0
		}
		if rec.Slowest == nil {
			rec.Slowest = []BootComponent{}
		}
		// 全是 0 说明这条记录读到的是空壳，别往界面上送
		if rec.MainPathMs == 0 && rec.PostBootMs == 0 {
			continue
		}
		out.Records = append(out.Records, rec)
	}

	if len(out.Records) == 0 {
		out.ErrCode, out.Err = bootErrEmpty, bootErrText(bootErrEmpty, "")
		return out
	}
	out.Ok = true
	a.linkBootItems(&out)
	return out
}

/* bootClassifyErr 把 wevtutil 的失败翻译成原因码。
   三种成因必须分开：权限不足要引导提权、超时建议重试、其余是解析/环境问题。
   非管理员时 wevtutil 会以 ACCESS_DENIED 退出 —— 这条最容易在别人机器上踩到，
   因为开发者自己几乎总是开着管理员终端。 */
func bootClassifyErr(err error, timedOut bool) (bootErrCode, string) {
	if timedOut {
		return bootErrTimeout, bootErrText(bootErrTimeout, "")
	}
	if err == errBootNoTool {
		return bootErrNoTool, bootErrText(bootErrNoTool, "")
	}
	if !isElevated() {
		return bootErrNoAdmin, bootErrText(bootErrNoAdmin, "")
	}
	// 已提权仍失败：多半是通道没开或被精简掉了，查一次状态比猜准
	switch bootChannelState() {
	case 0:
		return bootErrNoLog, bootErrText(bootErrNoLog, "")
	case 1:
		return bootErrDisabled, bootErrText(bootErrDisabled, "")
	}
	return bootErrParse, bootErrText(bootErrParse, err.Error())
}

// linkBootItems 把 Windows 点名的"拖慢项"对回启动项列表。
//
// 为什么必须在后端做：匹配要用 cmdToExe 解析命令行（引号、参数、环境变量），
// 前端手上只有 Cmd 原始字符串，自己切会切错。
//
// 匹配不上的占多数（实测 6 次开机命中率 0~50%），那些项前端只陈述事实、不给按钮 ——
// 给一个点了也没用的按钮，比不给更伤信任。
func (a *App) linkBootItems(out *BootPerf) {
	items, err := a.ListStartup()
	if err != nil || len(items) == 0 {
		return
	}
	byExe := map[string][]StartupItem{}
	byBase := map[string][]StartupItem{}
	for _, it := range items {
		ex := strings.ToLower(cmdToExe(it.Cmd))
		if ex == "" {
			continue
		}
		byExe[ex] = append(byExe[ex], it)
		byBase[strings.ToLower(filepath.Base(ex))] = append(byBase[strings.ToLower(filepath.Base(ex))], it)
	}
	pick := func(m []StartupItem) StartupItem {
		// 同一个 exe 可能同时在启动项和计划任务里；优先挑"还启用着的"，
		// 因为只有启用中的项才值得给按钮。
		for _, x := range m {
			if x.Enabled {
				return x
			}
		}
		return m[0]
	}
	for i := range out.Records {
		for j := range out.Records[i].Slowest {
			c := &out.Records[i].Slowest[j]
			p := strings.ToLower(strings.Trim(c.Path, `"`))
			base := strings.ToLower(filepath.Base(p))
			m := byExe[p]
			if len(m) == 0 {
				m = byBase[base]
			}
			if len(m) == 0 {
				continue // 管不到：svchost、msedge 会话恢复、别的软件的子进程……
			}
			it := pick(m)
			c.ItemID = it.ID
			c.ItemName = it.Name
			c.ItemKind = it.Kind
			c.ItemEnabled = it.Enabled
		}
	}
}
