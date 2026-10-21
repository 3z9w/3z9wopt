package main

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"golang.org/x/text/encoding/unicode"
	"golang.org/x/text/transform"
)

// ============================================================
// 工具库 —— 核心引擎（工具定义见 tools_*.go，按分类分文件）
//
// 设计原则（2026-09-21 与用户确认）：
//   1. 全部走 Windows 原生命令（sfc/dism/netsh/netstat/bcdedit/WMI…），
//      **一个都不依赖 PowerShell** —— 目标用户群大量使用精简版系统，
//      PowerShell 托管引擎被精简是常态（本机实测报 80070002）；
//   2. 长任务实时输出、随时可中断（复用项目既有 emit 事件机制）；
//   3. 高风险操作（引导修复/更新组件重建）执行前二次确认 + 自动备份。
//
// ------------------------------------------------------------
// 【怎么加一个工具】—— 扩展手册，照着做不会错
//   1. 找分类文件（tools_fix / tools_net / tools_disk / tools_info / tools_util .go），
//      在 init() 里加一条 regTool(ToolItem{...}, toolImpl{...})；
//   2. 执行方式三选一（优先级：Run > Plan > Steps）：
//        Steps: []toolStep{{exe: "xxx.exe", args: []string{...}}}   固定命令序列
//        Plan:  func(arg string) []toolStep                        命令随参数变化
//        Run:   func(tc *toolCtx) (int, string)                    要读 WMI / 解析输出 / 多步交互
//   3. 元数据别偷懒：Timeout 必填（不填会被测试拦下）、改系统设置的给 Danger、
//      运行后需重启才生效的给 Reboot（会自动进"待重启"提醒并引导重启）、
//      sfc 这类"进度不走普通输出"的命令给 silent: "sfc"；
//   4. 跑 `go test .`：目录完整性、危险项守卫、规格基线三层把关；
//      确认改动是你要的之后，用 `GOLDEN_UPDATE=1 go test -run TestToolGolden .` 刷新基线。
//
// 【怎么加一个分类】
//   toolCats 加一条 + 新建 tools_<key>.go 写 regTool(...) 即可。
//   前端页签、筛选、计数全部由 GetToolCats/GetToolCatalog 驱动，无需改前端。
//   注意：分类内的工具顺序 = 该文件里 regTool 的书写顺序；分类之间互不影响。
// ------------------------------------------------------------
// ============================================================

// ToolItem 工具元数据（前端渲染 + 执行参数）
type ToolItem struct {
	ID       string    `json:"id"`
	Cat      string    `json:"cat"`
	Name     string    `json:"name"`
	Desc     string    `json:"desc"`
	Cmd      string    `json:"cmd"`
	Risk     string    `json:"risk"` // safe | readonly | caution | reboot
	Long     bool      `json:"long"` // 长任务
	Danger   bool      `json:"danger"`
	Backup   bool      `json:"backup"`
	OutText  bool      `json:"outText"`
	Note     string    `json:"note"`
	ArgKind  string    `json:"argKind"` // "" | text | drive | shutdown | opentool | updatelist
	ArgLabel string    `json:"argLabel"`
	ArgHint  string    `json:"argHint"`
	ArgDef   string    `json:"argDef"`
	ArgReq   bool      `json:"argReq"`
	Opts     []ToolOpt `json:"opts,omitempty"`
	Timeout  int       `json:"timeout"`
	// Reboot 运行成功后会写进「待重启」清单（复用优化模块的机制）：
	// 工具页顶部常驻提醒条 + 完成后引导重启，重启后提醒自动消失。
	Reboot    bool   `json:"reboot"`
	RebootMsg string `json:"rebootMsg,omitempty"` // 自定义重启说明（留空用默认文案）
	// Recommend 推荐标记：卡片挂醒目金色角标，全库建议 ≤5 个（挂多了等于没挂）。
	Recommend bool `json:"recommend"`
}

// ToolOpt 下拉选项
type ToolOpt struct {
	V string `json:"v"`
	T string `json:"t"`
}

// ToolCat 分类
type ToolCat struct {
	Key  string `json:"key"`
	Name string `json:"name"`
	Desc string `json:"desc"`
}

// toolCats 分类定义（顺序 = 界面页签顺序）。新增分类在这里加一条即可。
var toolCats = []ToolCat{
	{Key: "fix", Name: "系统修复", Desc: "修复系统文件、组件与引导问题"},
	{Key: "net", Name: "网络工具", Desc: "网络重置、连通性诊断与配置排查"},
	{Key: "disk", Name: "磁盘工具", Desc: "硬盘健康、读写速度与碎片分析"},
	{Key: "info", Name: "系统信息", Desc: "硬件、补丁与运行状态一览"},
	{Key: "util", Name: "实用工具", Desc: "日常高频的小功能"},
}

// ============================================================
// 注册表：一个工具 = 一处声明（元数据 + 执行方式），见文件顶部的扩展手册
// ============================================================

// toolImpl 一个工具的执行方式。三选一，优先级 Run > Plan > Steps。
type toolImpl struct {
	Run   func(tc *toolCtx) (int, string) // 定制实现
	Plan  func(arg string) []toolStep     // 命令随参数变化
	Steps []toolStep                      // 固定命令序列
}

var (
	toolsByCat  = map[string][]ToolItem{} // 分类文件各自往里放，顺序 = 该文件书写顺序
	toolImpls   = map[string]toolImpl{}
	toolCatalog []ToolItem
	catalogOnce sync.Once
)

// buildCatalog 按 toolCats 的顺序拼出目录。
// 为什么不直接 append 到 toolCatalog：分类文件是分开的，init() 执行顺序由文件名决定，
// 直接 append 会让"全部"页签里的分类顺序随文件名字母序漂移 —— 这里显式定序，一劳永逸。
func buildCatalog() {
	catalogOnce.Do(func() {
		toolCatalog = nil
		for _, c := range toolCats {
			toolCatalog = append(toolCatalog, toolsByCat[c.Key]...)
		}
	})
}

// registerTool 注册一个工具（写进给定的集合，便于测试）。
// 返回 error 而不是 panic，是为了让测试能直接验证各种填错的情况。
func registerTool(cats []ToolCat, byCat map[string][]ToolItem, impls map[string]toolImpl, it ToolItem, impl toolImpl) error {
	if strings.TrimSpace(it.ID) == "" {
		return fmt.Errorf("工具 ID 不能为空（%q）", it.Name)
	}
	if it.Name == "" || it.Desc == "" || it.Cmd == "" {
		return fmt.Errorf("工具 %s 的 Name/Desc/Cmd 不能为空", it.ID)
	}
	if _, dup := impls[it.ID]; dup {
		return fmt.Errorf("工具 ID 重复：%s（换个 ID，或删掉多余的 regTool）", it.ID)
	}
	known := map[string]bool{}
	for _, c := range cats {
		known[c.Key] = true
	}
	if !known[it.Cat] {
		return fmt.Errorf("工具 %s 的分类 %q 不在 toolCats 里（先加分类，或改分类键）", it.ID, it.Cat)
	}
	switch it.Risk {
	case "safe", "readonly", "caution", "reboot":
	default:
		return fmt.Errorf("工具 %s 的风险等级非法：%q（只能是 safe/readonly/caution/reboot）", it.ID, it.Risk)
	}
	switch it.ArgKind {
	case "", "text", "drive", "shutdown", "opentool", "updatelist", "netpanel", "speedtest":
	default:
		return fmt.Errorf("工具 %s 的参数类型非法：%q", it.ID, it.ArgKind)
	}
	// updatelist / netpanel 自带专属弹窗 UI（列表、功能面板），不走通用参数表单，无需 ArgLabel
	if it.ArgKind != "" && it.ArgKind != "updatelist" && it.ArgKind != "netpanel" && it.ArgKind != "speedtest" && it.ArgLabel == "" {
		return fmt.Errorf("工具 %s 有参数但没写 ArgLabel（界面渲染不出来）", it.ID)
	}
	if it.ArgKind == "opentool" && len(it.Opts) == 0 {
		return fmt.Errorf("工具 %s 是下拉选择但没给选项 Opts", it.ID)
	}
	if it.Timeout <= 0 {
		return fmt.Errorf("工具 %s 没设 Timeout（会挂死）", it.ID)
	}
	if impl.Run == nil && impl.Plan == nil && len(impl.Steps) == 0 {
		return fmt.Errorf("工具 %s 没有执行实现（Run/Plan/Steps 三选一，否则点了没反应）", it.ID)
	}
	if impl.Run != nil && (impl.Plan != nil || len(impl.Steps) > 0) {
		return fmt.Errorf("工具 %s 同时写了 Run 和 Plan/Steps，请只留一种（Run 优先，容易看漏）", it.ID)
	}
	impls[it.ID] = impl
	byCat[it.Cat] = append(byCat[it.Cat], it)
	return nil
}

// regTool 供各分类文件的 init() 调用；填错直接 panic —— 启动即暴露，别等到用户点下去才发现。
func regTool(it ToolItem, impl toolImpl) {
	if err := registerTool(toolCats, toolsByCat, toolImpls, it, impl); err != nil {
		panic("工具库注册失败：" + err.Error())
	}
}

// driveOrSys 盘符参数：留空回落到系统盘，自动补冒号
func driveOrSys(arg string) string {
	d := strings.TrimSpace(arg)
	if d == "" {
		d = os.Getenv("SystemDrive")
		if d == "" {
			d = "C:"
		}
	}
	if !strings.HasSuffix(d, ":") {
		d += ":"
	}
	return d
}

// ============================================================
// 绑定方法（前端调用）
// ============================================================

var (
	toolMu      sync.Mutex
	toolCancel  context.CancelFunc
	toolRunning bool
)

// GetToolCats 分类定义
func (a *App) GetToolCats() []ToolCat { return toolCats }

// GetDrives 固定盘符列表（工具库「盘符」参数用）
func (a *App) GetDrives() []string {
	var out []string
	for _, d := range fixedDrives() {
		out = append(out, strings.TrimSuffix(d, `\`))
	}
	return out
}

// GetLanShareInfo 本机局域网共享信息（IP / 机器名 / SMB 访问地址），局域网共享面板头部用
func (a *App) GetLanShareInfo() map[string]string {
	host, _ := os.Hostname()
	ip := ""
	// UDP dial 不真正发包，只为拿到出口网卡的本地地址
	if c, err := net.Dial("udp", "223.5.5.5:53"); err == nil {
		if addr, ok := c.LocalAddr().(*net.UDPAddr); ok {
			ip = addr.IP.String()
		}
		c.Close()
	}
	if ip == "" { // 回退：枚举网卡取第一个 IPv4
		if addrs, err := net.InterfaceAddrs(); err == nil {
			for _, a := range addrs {
				if ipn, ok := a.(*net.IPNet); ok && !ipn.IP.IsLoopback() && ipn.IP.To4() != nil {
					ip = ipn.IP.String()
					break
				}
			}
		}
	}
	smb := "smb://" + host
	if ip != "" {
		smb += " 或 smb://" + ip
	}
	return map[string]string{"ip": ip, "hostname": host, "smb": smb}
}

// GetToolCatalog 工具目录（首次访问时按分类定序组装）
func (a *App) GetToolCatalog() []ToolItem {
	buildCatalog()
	return toolCatalog
}

// StopTool 中断当前工具
func (a *App) StopTool() {
	toolMu.Lock()
	if toolCancel != nil {
		toolCancel()
	}
	toolMu.Unlock()
}

// RunTool 启动工具。返回空串表示已启动，非空为错误文案。
func (a *App) RunTool(id, arg string) string {
	def := findTool(id)
	if def == nil {
		return "未知工具：" + id
	}
	arg = strings.TrimSpace(arg)
	if def.ArgReq && arg == "" {
		return "请先填写「" + def.ArgLabel + "」"
	}
	if def.ArgDef != "" && arg == "" {
		arg = def.ArgDef
	}

	toolMu.Lock()
	if toolRunning {
		toolMu.Unlock()
		return "已有工具正在执行，请先等它跑完或点「停止」"
	}
	appendOpLog(OpLogEntry{
		Kind: "tool", Title: "执行工具「" + def.Name + "」",
		Items: []string{func() string {
			if arg != "" {
				return def.Name + " · 参数：" + arg
			}
			return def.Name
		}()},
		OK: 1,
	})
	to := def.Timeout
	if to <= 0 {
		to = 300
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(to)*time.Second)
	toolCancel = cancel
	toolRunning = true
	toolMu.Unlock()

	go func() {
		defer func() {
			toolMu.Lock()
			toolRunning = false
			toolCancel = nil
			toolMu.Unlock()
			cancel()
		}()
		a.execTool(def, arg, ctx)
	}()
	return ""
}

// findTool 按 ID 找工具定义
func findTool(id string) *ToolItem {
	buildCatalog()
	for i := range toolCatalog {
		if toolCatalog[i].ID == id {
			return &toolCatalog[i]
		}
	}
	return nil
}

type toolCtx struct {
	a   *App
	arg string
	ctx context.Context
	log func(kind, text string)
}

func (a *App) execTool(def *ToolItem, arg string, ctx context.Context) {
	start := time.Now()
	tc := &toolCtx{
		a:   a,
		arg: arg,
		ctx: ctx,
		log: func(kind, text string) { a.emit("tool:log", kind, text) },
	}
	tc.log("head", "$ "+def.Cmd)
	if arg != "" && def.ArgKind != "" {
		tc.log("dim", "参数："+arg)
	}

	// 高风险工具：先备份，再执行
	if def.Backup {
		if !backupBCD(ctx, tc.log) {
			a.emit("tool:done", -1, int(time.Since(start).Seconds()), "引导配置备份失败，已中止（未做任何修改）")
			return
		}
	}

	impl := toolImpls[def.ID]
	var code int
	var msg string
	switch {
	case impl.Run != nil:
		code, msg = impl.Run(tc)
	case impl.Plan != nil:
		code, msg = runSteps(tc, impl.Plan(arg))
	default:
		code, msg = runSteps(tc, impl.Steps)
	}

	// 需重启才生效：提示 + 落进待重启清单（复用优化模块机制，重启后自动消失）
	if code == 0 && def.Reboot {
		txt := def.RebootMsg
		if txt == "" {
			txt = "该操作需要重启电脑才能完全生效"
		}
		tc.log("warn", "[需重启生效] "+txt+"。重启前界面会一直保留这条提醒，重启后自动消失。")
		addPendingReboot([]string{def.Name})
		if msg != "" {
			msg += " · "
		}
		msg += "需重启生效"
	}
	a.emit("tool:done", code, int(time.Since(start).Seconds()), msg)
}

// ------------------------------------------------------------
// 步骤执行
// ------------------------------------------------------------

type toolStep struct {
	title string
	exe   string
	args  []string
	stdin string
	// silent 标记"进度不走普通输出通道"的命令（sfc 是典型：输出是 UTF-16
	// 且全程用 \r 刷新，用户容易误判卡死）。值为说明文案的关键字：
	// "sfc" = 开跑前提示 + 结束后读 CBS.log 补结果摘要。
	silent string
	// fn 非空时直接执行它，不启动外部进程。用于替代 reg.exe 一类外部命令：
	// 精简版系统常把这类 exe 移除掉，同样的改动改用原生 API 完成更稳。
	fn func() error
}

func runSteps(tc *toolCtx, steps []toolStep) (int, string) {
	if len(steps) == 0 {
		return -1, "该工具没有可执行的命令"
	}
	stepsOK := 0
	for _, st := range steps {
		if tc.ctx.Err() != nil {
			return -1, "已停止"
		}
		if st.title != "" {
			tc.log("head", "▶ "+st.title)
		}
		// sfc 扫描耗时 5-15 分钟，开头阶段可能停顿较久 —— 先打预防针，
		// 避免用户把"还没出进度"当成卡死（2026-09-21 用户实测反馈）
		if st.silent != "" {
			tc.log("dim", "提示：扫描通常需要 5–15 分钟，期间会持续刷新进度百分比；刚启动或阶段切换时可能停顿片刻，属正常现象，不是卡死。可随时点「停止」。")
		}
		stepStart := time.Now()
		var code int
		var err error
		if st.fn != nil {
			// 原生步骤：成功 code=0，失败 code=1（与外部命令的口径保持一致）
			if e := st.fn(); e != nil {
				code, err = 1, e
			}
		} else {
			code, err = runStepStream(tc.ctx, tc.log, st.exe, st.args, st.stdin)
		}
		if tc.ctx.Err() != nil {
			return -1, "已停止"
		}
		if err != nil {
			// 继续后续步骤：例如 /fixboot 失败不应阻断 /rebuildbcd
			tc.log("err", fmt.Sprintf("退出码 %d —— %s", code, shortErr(err)))
		} else {
			stepsOK++
			tc.log("ok", "完成")
		}
		if st.silent == "sfc" {
			logSfcSummary(tc.log, stepStart)
		}
	}
	if stepsOK == 0 {
		return 1, "所有命令都返回了错误，请查看上方输出"
	}
	if stepsOK < len(steps) {
		return 0, fmt.Sprintf("部分完成（%d/%d 步成功）", stepsOK, len(steps))
	}
	return 0, ""
}

// sfcResult sfc 扫描结果摘要（解析自 CBS.log 的 [SR] 行）
type sfcResult struct {
	Repaired int      // 已自动修复的文件数
	Cannot   []string // 无法修复的成员文件（前几条）
	Detail   int      // [SR] 行总数（说明确有扫描活动）
}

// parseSfcSummary 从 CBS.log 内容中提取 since 之后的 sfc 结果。
// 标准输出只给一句结论（"未发现完整性冲突"/"找到完整性冲突，无法修复其中部分"），
// CBS.log 能给出修复明细数量，两者互补。
func parseSfcSummary(content string, since time.Time) sfcResult {
	var res sfcResult
	seen := map[string]bool{}
	for _, line := range strings.Split(content, "\n") {
		line = strings.TrimRight(line, "\r")
		// CBS.log 行首格式：2026-09-21 14:40:45, Info  CSI  ...
		if len(line) < 19 || line[4] != '-' {
			continue
		}
		// 必须用 ParseInLocation：CBS.log 是本地时间，time.Parse 会按 UTC
		// 解析，导致时区差 8 小时、过滤时段完全错位
		t, err := time.ParseInLocation("2006-01-02 15:04:05", line[:19], time.Local)
		if err != nil || t.Before(since.Add(-time.Second)) {
			continue
		}
		i := strings.Index(line, "[SR]")
		if i < 0 {
			continue
		}
		res.Detail++
		msg := line[i:]
		switch {
		case strings.Contains(msg, "Repairing corrupted file"),
			strings.Contains(msg, "Repairing file"):
			res.Repaired++
		case strings.Contains(msg, "Cannot repair member file"):
			// 取文件名（通常在最后一个 \ 后）
			if j := strings.LastIndex(msg, `\`); j >= 0 {
				rest := msg[j+1:]
				if k := strings.IndexAny(rest, " \t("); k > 0 {
					rest = rest[:k]
				}
				if rest != "" && !seen[rest] {
					seen[rest] = true
					if len(res.Cannot) < 5 {
						res.Cannot = append(res.Cannot, rest)
					}
				}
			}
		}
	}
	return res
}

// logSfcSummary 把 sfc 结果摘要写进日志区；读不到 CBS.log 时保持安静（不影响主流程）。
func logSfcSummary(log func(kind, text string), since time.Time) {
	root := os.Getenv("SystemRoot")
	if root == "" {
		root = `C:\Windows`
	}
	f, err := os.Open(filepath.Join(root, `Logs\CBS\CBS.log`))
	if err != nil {
		return
	}
	defer f.Close()
	// CBS.log 可能上百 MB，只读尾部 32MB 足够覆盖一次 sfc 扫描的 [SR] 记录
	if st, err := f.Stat(); err == nil && st.Size() > 32<<20 {
		f.Seek(-32<<20, io.SeekEnd)
	}
	data, err := io.ReadAll(bufio.NewReaderSize(f, 1<<16))
	if err != nil {
		return
	}
	res := parseSfcSummary(string(data), since)
	if res.Detail == 0 {
		return // 没有本次运行痕迹（可能刚被轮转），不乱下结论
	}
	log("head", "── sfc 结果摘要（CBS.log）──")
	if res.Repaired > 0 {
		log("ok", fmt.Sprintf("发现损坏文件并已自动修复 %d 处", res.Repaired))
	}
	if len(res.Cannot) > 0 {
		log("err", fmt.Sprintf("存在 %d 个无法自动修复的文件，建议先跑「DISM 组件存储修复」，完成后再回来重跑一次 sfc：", len(res.Cannot)))
		for _, n := range res.Cannot {
			log("err", "  · "+n)
		}
	}
	if res.Repaired == 0 && len(res.Cannot) == 0 {
		log("ok", "未发现完整性冲突，系统文件完好")
	}
}

// resolveExe 把裸命令名解析成 System32 下的绝对路径。
// 为什么必须这么做：
//  1. 精简版系统 / PATH 残缺的进程里，exec.Command 走 PATH 查找会直接失败
//     （实测报 executable file not found in %PATH%，工具点了没反应）；
//  2. 本程序以管理员身份运行，裸命令名存在 PATH 劫持风险 —— 一旦被劫持，
//     等于把管理员权限交给别人。
// 带路径分隔符的、或 System32 里确实没有的，原样返回交给系统兜底。

// nativeSystem32 拿到**真正的（原生位数的）System32** 路径，绕开 32 位进程的 WOW64 文件重定向。
// 32 位程序访问 %SystemRoot%\System32 会被系统重定向到 SysWOW64，那里缺少部分系统工具
// （或行为不一致），而 DISM++ 之类工具用的是原生 64 位版本。用 Sysnative 这个只有 32 位进程
// 可见的别名，可以直达真实 System32。64 位进程不需要重定向，Sysnative 也不存在，直接用 System32。

// resolveSystemExe 解析 Windows 自带命令行工具（pnputil / powershell / dism 等）的完整路径，
// 优先用原生 System32，确保 32 位编译的程序也能调到正确的原生（64 位）版本。
// 带路径分隔符或 System32 里确实没有的，原样返回交给系统兜底。

// runStepStream 执行单条命令，输出按 \r / \n 切分实时推送。
// kind：line = 新行；cr = 覆盖当前行（进度刷新）；head = 步骤标题；ok/err = 状态
func runStepStream(ctx context.Context, log func(kind, text string), exe string, args []string, stdin string) (int, error) {
	exe = resolveExe(exe)
	cmd := exec.CommandContext(ctx, exe, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: 0x08000000}
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return -1, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return -1, err
	}
	if err := cmd.Start(); err != nil {
		return -1, err
	}
	// 心跳：超过 25 秒没有任何输出就推一行"仍在执行"，避免长任务看起来像卡死
	//（部分命令开头/阶段切换时长时间不吐进度，是真实场景）。
	var lastAct int64 // 输出活跃时刻（unix nano），atomic
	atomic.StoreInt64(&lastAct, time.Now().UnixNano())
	hbLog := func(kind, text string) {
		atomic.StoreInt64(&lastAct, time.Now().UnixNano())
		log(kind, text)
	}
	hbDone := make(chan struct{})
	hbStart := time.Now()
	go func() {
		tk := time.NewTicker(15 * time.Second)
		defer tk.Stop()
		for {
			select {
			case <-hbDone:
				return
			case <-tk.C:
				idle := time.Since(time.Unix(0, atomic.LoadInt64(&lastAct)))
				if idle > 25*time.Second {
					el := time.Since(hbStart).Truncate(time.Second)
					hs := fmtDuration(el)
					if el < time.Hour { // 不足 1 小时带上秒，避免出现"已用时 0 分钟"
						hs = fmt.Sprintf("%d 分 %02d 秒", int(el.Minutes()), int(el.Seconds())%60)
					}
					log("dim", fmt.Sprintf("…仍在执行中，已用时 %s（静默期不产生输出属正常，不是卡死）", hs))
					atomic.StoreInt64(&lastAct, time.Now().UnixNano())
				}
			}
		}
	}()
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); pumpLines(consoleReader(stdout), hbLog) }()
	go func() { defer wg.Done(); pumpLines(consoleReader(stderr), hbLog) }()
	wg.Wait() // 必须先读完管道，再 Wait（否则管道关闭会丢输出）
	werr := cmd.Wait()
	close(hbDone)
	code := 0
	if werr != nil {
		if ee, ok := werr.(*exec.ExitError); ok {
			code = ee.ExitCode()
		} else {
			code = -1
		}
	}
	return code, werr
}

// consoleReader 识别子进程输出的文本编码，统一规范成 UTF-8 字节流再交给 pumpLines 切行。
// 为什么：sfc 的重定向输出是 UTF-16LE（实测"验证 97%"被直读成乱码"解瑞把l0"、
// ASCII 被 0x00 隔成"9 7 %"），不转就全乱；其余命令是 ANSI/UTF-8 字节，原样放行，
// 由 decodeConsole 按 UTF-8/GBK 兜底。
func consoleReader(r io.Reader) io.Reader {
	br := bufio.NewReaderSize(r, 4096)
	first := make([]byte, 4096)
	n, _ := br.Read(first) // 阻塞到有首块为止（至少 1 字节），拿到多少算多少
	if n <= 0 {
		return br
	}
	chunk := first[:n]
	if isUtf16(chunk) {
		dec := unicode.UTF16(unicode.LittleEndian, unicode.UseBOM).NewDecoder()
		return transform.NewReader(io.MultiReader(bytes.NewReader(chunk), br), dec)
	}
	return io.MultiReader(bytes.NewReader(chunk), br)
}

// isUtf16 判断输出块是否 UTF-16 文本：BOM 直接判定；
// 否则看 0x00 密度 —— UTF-16LE 的 ASCII 字符（含 \r\n）高位必是 0x00，
// 中文字符里低位为 0x00 的也不少（如"开"U+5F00）。
// 实测 sfc 全文密度 22%~25%、纯中文行也有 ~10%；而 ANSI/UTF-8/GBK 文本
// 实际不会出现 0x00，所以 8% 是安全分界（阈值定 25% 会漏判真实 sfc 输出，踩过）。
func isUtf16(chunk []byte) bool {
	if len(chunk) >= 2 && chunk[0] == 0xFF && chunk[1] == 0xFE {
		return true
	}
	if len(chunk) < 8 {
		return false
	}
	nuls := 0
	for _, b := range chunk {
		if b == 0 {
			nuls++
		}
	}
	return nuls*12 >= len(chunk)
}

// pumpLines 逐字节读取，按 \r / \n 切分并推送。
// 为什么不用 Scanner：dism/sfc 的进度条用 \r 覆盖同一行，Scanner 按 \n 切会一直等不到输出。
func pumpLines(r io.Reader, log func(kind, text string)) {
	br := bufio.NewReaderSize(r, 8192)
	var sb []byte
	// cr 压帧：80ms 窗口内只保留最新一帧（pendingCR），旧帧丢弃。
	// 关键：**最新一帧绝不丢** —— 直接丢弃会把结论行吞掉（实测 sfc 的
	// "未找到完整性冲突"紧跟在"验证 100%"后几十毫秒到达，被旧版节流直接扔了）。
	var pendingCR string
	lastCR := time.Time{}
	flush := func(kind string) {
		text := decodeConsole(sb)
		sb = sb[:0]
		if text == "" {
			return
		}
		if kind == "cr" {
			if time.Since(lastCR) < 80*time.Millisecond {
				pendingCR = text
				return
			}
			lastCR = time.Now()
			log("cr", text)
			return
		}
		if pendingCR != "" { // 出新行了，先把压着的进度帧按序放出去
			log("cr", pendingCR)
			pendingCR = ""
		}
		log("line", text)
	}
	for {
		b, err := br.ReadByte()
		if err != nil {
			break
		}
		switch b {
		case '\n':
			flush("line")
		case '\r':
			if next, err := br.Peek(1); err == nil && next[0] == '\n' {
				_, _ = br.Discard(1)
				flush("line")
			} else {
				flush("cr")
			}
		default:
			sb = append(sb, b)
			if len(sb) >= 4000 {
				flush("line")
			}
		}
	}
	flush("line")
	if pendingCR != "" { // 收尾：最后压着的那帧（往往就是结论）必须放出去
		log("cr", pendingCR)
	}
}

// decodeConsole 控制台输出解码：优先按 UTF-8，失败再按 GBK。
// 中文系统的 sfc/dism/netstat 等原生输出是 GBK，直接当 UTF-8 读会全是乱码。

// ------------------------------------------------------------
// 通用小工具
// ------------------------------------------------------------




// parseWmiDate 解析 WMI 日期。可能是 CIM_DATETIME（20240115000000.000000+480）、
// 纯日期（20240115），也可能是美式 M/D/YYYY（Win32_QuickFixEngineering.InstalledOn 常见）。

// batteryReportPath 电池报告输出路径
func batteryReportPath() string {
	dir := filepath.Join(os.Getenv("LOCALAPPDATA"), "3z9wOpt", "reports")
	_ = os.MkdirAll(dir, 0755)
	return filepath.Join(dir, "battery-report.html")
}

// backupBCD 导出当前引导配置。返回 false 表示备份失败（调用方必须中止后续写入操作）。
func backupBCD(ctx context.Context, log func(kind, text string)) bool {
	dir := filepath.Join(os.Getenv("LOCALAPPDATA"), "3z9wOpt", "reports")
	if err := os.MkdirAll(dir, 0755); err != nil {
		log("err", "无法创建备份目录："+err.Error())
		return false
	}
	p := filepath.Join(dir, "bcd_backup_"+time.Now().Format("20060102_150405")+".bcd")
	log("head", "▶ 导出当前引导配置（安全备份）")
	_, err := runStepStream(ctx, log, "bcdedit.exe", []string{"/export", p, "/y"}, "")
	if err != nil {
		log("err", "备份失败，已中止，未做任何修改")
		return false
	}
	log("ok", "引导配置已备份："+p)
	return true
}
