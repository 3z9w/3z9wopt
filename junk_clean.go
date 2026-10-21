package main

// ============================================================================
// 3z9wOpt v1.1 · 垃圾清理执行引擎
// 来源：v1.0（D:\3z9wopt(v2)\3z9wOptV2\app.go）按行切片复制，清理行为与参数保持 1:1，
//       只改了一处：诊断日志路径从 v1.0 的 D:\3z9wopt\_proto 改到 %TEMP%，两边不互相覆盖。
// 说明：扫描引擎在 junk_scan.go（1.1 重写，并发版），本文件只负责「执行清理」。
// ============================================================================

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"github.com/wailsapp/wails/v2/pkg/runtime"
	"golang.org/x/sys/windows"
)

type CleanResult struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Bytes  int64  `json:"bytes"`
	Err    string `json:"err"`
	Failed int    `json:"failed"` // 被占用未能删除的文件数（2026-09-21 加：清后如实提示用户"还有多少没释放"）

	// —— v1.1.0：清理后复扫的实测残留 ——
	// 以前「还剩多少」是前端拿「已选中 − 已释放」估算的（而"已释放"对回收站/cleanmgr 这类
	// 命令型项是按磁盘差值倒推的），所以复扫数字对不上、看着像"没清掉"。
	// 现在后端在清理收尾时对被清理项复扫一遍，这三个字段是实测值：
	// LeftBytes=0 且 LeftDetail 为空 才是真的清干净了。
	// ⚠️ 这里**不再下发逐文件路径清单**（原 LeftPaths 字段已删）：没人读它，
	// 而 WinSxS 这类项一次能复扫出上万条路径，全量塞进返回值纯属白送几百 KB
	// 给前端——绑定调用的返回值是一次性回传的，越大越容易回不来。
	// 要给人看具体是哪几个文件，用下面这个有上限的 LeftDetail。
	LeftBytes int64 `json:"leftBytes"`
	LeftFiles int   `json:"leftFiles"`

	// —— v1.1.0：残留文件"为什么没删掉"（2026-10-10 用户要求）——
	// 用户扫到 500M、只清掉 450M 就会怀疑软件没干完活。光说"还有 50M"不够，
	// 得告诉他这几文件现在动不了的原因，以及什么情况下才能清掉 —— 否则他只会再点一次，
	// 结果还是一样。LeftDetail 每条带一个 Reason，前端据此分组 + 给对应建议。
	// Reason 只写**能确定的**：locked=正被别的程序占着 / denied=权限或系统保护 /
	// new=清完又被程序重新生成了 / gone=现在已经不在了 / other=这次判断不出来。
	// 判不出来就如实写 other，**不许猜一个看起来像原因的原因**。
	LeftDetail []LeftEntry `json:"leftDetail,omitempty"`

	// —— v1.1.0：排进「重启后删除」队列的量（2026-10-14 加）——
	// 被占用删不掉的文件，现在会交给系统在下次启动时删（见 junk_rebootdel.go）。
	// ⚠️ 这部分**现在还没释放**，绝不能并进 Bytes（并进去就是虚报释放量，
	// 用户一复扫发现还在，只会更不信任）。单独回传，前端单列一行「重启后释放」。
	Reboot       int   `json:"reboot,omitempty"`
	RebootBytes  int64 `json:"rebootBytes,omitempty"`

	// —— v1.1.0：这一项被放到后台跑（2026-10-11 用户定）——
	// 数分钟级的项不该拖着整个清理不放：其它项清完就出结果、让用户返回，
	// 它自己在后台接着跑完。Async=true 时 Bytes 必然是 0（还没跑完，不知道释放了多少），
	// 前端据此显示"它还在跑，别关机"的提示，不要写成"清理失败"或"0 字节"。
	Async bool `json:"async,omitempty"`
}

// asyncCmds 放到后台跑的命令（判据：要跑好几分钟、不阻塞其它项、用户看得到系统自己的窗口）。
// 2026-10-14：cleanmgr 整项删除后这张表暂时是空的 —— 机制留着，以后有分钟级项加一行即可
// （前端 Async 字段与"它还在后台跑"的提示都由它派生，别在别处另写一套）。
var asyncCmds = map[string]bool{}

// LeftEntry 一条残留文件的实测情况
type LeftEntry struct {
	Path   string `json:"path"`
	Bytes  int64  `json:"bytes"`
	Reason string `json:"reason"`
}

// ---------- 垃圾项白名单（清理只允许清单内的 ID，前端无法传任意路径） ----------

// errScanAborted 全盘扫描被取消（v1.0 app.go:1154）
var errScanAborted = fmt.Errorf("scan aborted")

// ---------- 服务控制常量与 DLL 入口（直接调 SCM，不依赖 sc.exe / net.exe） ----------

const (
	scManagerConnect = 0x0001
	svcQueryStatus   = 0x0004
	svcStartAccess   = 0x0010
	svcStopAccess    = 0x0020
	svcControlStop   = 0x00000001
	svcStateRunning  = 4
)

var (
	advapi32      = syscall.NewLazyDLL("advapi32.dll")
	procOpenSCM   = advapi32.NewProc("OpenSCManagerW")
	procOpenSvc   = advapi32.NewProc("OpenServiceW")
	procQuerySvc  = advapi32.NewProc("QueryServiceStatus")
	procCtrlSvc   = advapi32.NewProc("ControlService")
	procStartSvc  = advapi32.NewProc("StartServiceW")
	procCloseSvcH = advapi32.NewProc("CloseServiceHandle")
)

// ============================================================================
// 应用对象（Wails 绑定入口）

type App struct {
	ctx context.Context
	// 清理停止（2026-09-19 用户裁决：只做清理停止，扫描不做取消）
	cleanMu     sync.Mutex
	cleanCancel context.CancelFunc
}

func NewApp() *App { return &App{} }

func (a *App) startup(ctx context.Context) { a.ctx = ctx }

// emit 统一发事件。ctx 未就绪（如无头单测里直接调后端）时静默跳过，
// 避免 wails runtime 抛 "invalid context" 噪音日志，同时让后端逻辑可脱离 UI 测试。
func (a *App) emit(name string, data ...interface{}) {
	if a.ctx == nil {
		return
	}
	runtime.EventsEmit(a.ctx, name, data...)
}

// ============================================================================
// 清理执行编排（2026-10-09 从 v1.0 app.go 按行切片复制，路径/参数/行为未改）

var cleanExecWeight = map[string]int{
	"deepjunk": 1,                                   // 全盘遍历，慢但不打扰用户
	"recycle":  2, "evtx": 2, "vss": 2, "winold": 2, // 秒级系统命令
	"wucachedeep": 3, // 停更新服务（清完会启回来），影响面稍大
	"dismreset":   4, // 数分钟
	// cleanmgr 已整项删除（2026-10-14），权重跟着去掉
	// thumbdeep 已随 thumb 合并删除（2026-10-14）：thumb 改走目录型清理链（当场删 + 排队重启删），
	// 不再杀资源管理器。谁再把"杀 explorer"加回来，junk_test.go:TestImpactMarksAreSet 会红。
	"iconrefresh": 7, // 纯图标刷新动作，必须收尾（清完缓存刷一下才生效，脚本 final 阶段同款）
}

// cleanExecutionOrder 按权重稳定排序，返回执行顺序（不改传入切片）。
func cleanExecutionOrder(ids []string) []string {
	out := make([]string, len(ids))
	copy(out, ids)
	sort.SliceStable(out, func(i, j int) bool {
		return cleanExecWeight[out[i]] < cleanExecWeight[out[j]]
	})
	return out
}

// isDirCleanItem 目录型清理项（Cmd 为空 = 清一个目录/文件的条目）。
// 命令型项（recycle / vss / evtx / winold / cleanmgr / reg:xxx / badlnk / brokenappx …）
// 一律走串行阶段：它们要么需要重扫、要么要停服务/杀进程/起 PowerShell，
// 不能跟目录删除并发抢盘（v1.1.0 修：原来按「权重未登记 = 0」判断，把它们当成了秒级快删项）。
// 单独抽成函数是为了能写单测把这条规则锁住（见 junk_test.go）。
func isDirCleanItem(it *JunkItem) bool { return it.Cmd == "" }

func (a *App) CleanJunk(ids []string) []CleanResult {
	trace := cleanTrace
	trace("CleanJunk start ids=" + strings.Join(ids, ","))
	known := map[string]JunkItem{}
	for _, it := range junkItems() {
		known[it.ID] = it
	}
	// 清理停止通道：StopClean 取消这个 ctx，各阶段就地中断
	ctx, cancel := context.WithCancel(context.Background())
	a.cleanMu.Lock()
	a.cleanCancel = cancel
	a.cleanMu.Unlock()
	defer func() {
		a.cleanMu.Lock()
		a.cleanCancel = nil
		a.cleanMu.Unlock()
		cancel()
	}()
	results := []CleanResult{} // 必须初始化为空切片，否则空结果序列化成 null 把前端 .reduce 打崩
	total := len(ids)
	done := 0
	var mu sync.Mutex
	ids = cleanExecutionOrder(ids) // 执行顺序编排：快的先跑出结果，耗时/干扰大的排最后，刷新动作收尾

	// 阶段一：目录型快删项 4 路并发（2026-09-19 提速：相互独立、都是秒级，
	// 串行要排队等最慢的和，并发后总耗时 ≈ 最慢的那一项）
	// v1.1.0：判据从「权重为 0」改成「是不是目录型项」。reg:* / badlnk / brokenappx 这些
	// 命令项没登记权重 → 原来权重 0 被当成"秒级目录快删项"扔进这里并发（badlnk 还要起 PowerShell），
	// 与 4 路目录删除抢盘。命令型一律进串行阶段，按权重排序执行。
	var fast, slow []string
	for _, id := range ids {
		it, ok := known[id]
		if ok && isDirCleanItem(&it) {
			fast = append(fast, id)
		} else {
			slow = append(slow, id)
		}
	}
	var wg sync.WaitGroup
	sem := make(chan struct{}, 4)
	for _, id := range fast {
		if ctx.Err() != nil {
			break
		}
		it, ok := known[id]
		if !ok {
			continue
		}
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			mu.Lock()
			d := done
			mu.Unlock()
			a.emit("clean:progress", d, total, it.Name, it.ID)
			var res CleanResult
			if it.Cmd != "" {
				res = CleanResult{ID: it.ID, Name: it.Name}
				res.Bytes, res.Err = a.runCmdCleanCtx(ctx, &it)
			} else {
				res = cleanDirItem(ctx, &it)
			}
			mu.Lock()
			results = append(results, res)
			done++
			d = done
			mu.Unlock()
			a.emit("clean:progress", d, total, "", "")
		}()
	}
	wg.Wait()

	// 阶段二：耗时/系统项严格按权重串行（deepjunk → 秒级命令 → 停服务 → dism → cleanmgr → explorer → 收尾）
	for _, id := range slow {
		if ctx.Err() != nil {
			trace("CleanJunk stopped by user at " + id)
			break // 用户停止：后续项不再执行，已清结果照实返回
		}
		it, ok := known[id]
		if !ok {
			continue // 白名单之外的 ID 直接拒绝
		}
		mu.Lock()
		d := done
		mu.Unlock()
		a.emit("clean:progress", d, total, it.Name, it.ID) // 开始处理前先报当前项名，前端显示"正在清理：xxx"
		res := CleanResult{ID: it.ID, Name: it.Name}
		if it.Cmd != "" {
			res.Bytes, res.Err = a.runCmdCleanCtx(ctx, &it)
		} else {
			res = cleanDirItem(ctx, &it)
		}
		if ctx.Err() != nil && res.Err == "" {
			res.Err = "已停止"
		}
		/* 放到后台的项：主流程不等它，Bytes=0 是"还没跑完"，不是"没清掉" ——
		   打上标记，前端据此提示"它还在跑"，不许显示成失败或 0 字节。 */
		if asyncCmds[it.Cmd] && res.Err == "" {
			res.Async = true
		}
		mu.Lock()
		results = append(results, res)
		done++
		d = done
		mu.Unlock()
		trace(fmt.Sprintf("  %s done freed=%d err=%q", it.ID, res.Bytes, res.Err))
		a.emit("clean:progress", d, total, "", "")
	}
	// v1.1.0：清理收尾复扫 —— 把「还剩多少」从估算换成实测（见 WorkBuddy 排查报告·候选修复 3）。
	// 只复扫「有可枚举目标」的项 + deepjunk；纯命令项（回收站 / 还原点 / 事件日志 / cleanmgr /
	// 注册表 / 快捷方式）没有可枚举目标，复扫没意义 —— 它们的释放量本来就是按磁盘差值倒推的。
	// 代价：勾了 deepjunk 时这里会再走一遍全盘扫描（十几秒），换来的是"实测残留"而不是估算。
	leftBudget := leftReasonMax // 「定性原因」是全局配额：回传的包必须有上限
	for i := range results {
		if ctx.Err() != nil {
			break // 用户中途停止：不再复核，剩余项照旧按估算走
		}
		it, ok := known[results[i].ID]
		if !ok {
			continue
		}
		if results[i].Bytes == 0 && results[i].Failed == 0 {
			continue // 这项既没删到东西、也没报失败 → 没有可复核的残留，省一轮遍历
		}
		// recycle 也复核：它是"命令型"没错，但能用系统接口问出还剩多少
		// （2026-10-10：不清它的话，D 盘那 160 MB 清不掉也永远不会有人告诉用户）
		measurable := len(it.dirs) > 0 || len(it.globs) > 0 || len(it.files) > 0 ||
			it.Cmd == "deepjunk" || it.Cmd == "recycle"
		if !measurable {
			continue
		}
		a.emit("clean:progress", total, total, "复核残留："+it.Name, it.ID)
		left := it
		scanOne(ctx, &left) // 复用扫描引擎：重新解析目标 + 重新量体积（含 paths）
		results[i].LeftBytes, results[i].LeftFiles = left.Bytes, left.Files
		results[i].LeftDetail, leftBudget = classifyLeft(left.Paths, leftBudget)
		trace(fmt.Sprintf("  %s 复核残留 bytes=%d files=%d", it.ID, left.Bytes, left.Files))
	}
	sort.Slice(results, func(i, j int) bool { return results[i].Bytes > results[j].Bytes })
	trace("CleanJunk end")
	return results
}

// cleanDirItem 目录/文件型清理项：单遍「计量+删除」（removeTreeSizedCtx），逐项间检查停止信号

// leftReasonMax 是**全局预算**：所有项加起来最多定性这么多条，不是每项这么多。
// 两个理由：① 每条都要试一次"独占打开"，上万个文件逐个试太慢；
// ② 这些路径是跟着清理结果**一次性回传给前端**的 —— 2026-10-10 那次"清理失败"
// 就是回传的包太大（一项复扫出 6798 条路径，几十项叠起来上 MB）。
// 超出的部分前端照旧显示体积，原因写 other —— 不编，但也不许把回传撑爆。
const leftReasonMax = 300

// classifyLeft 给复扫残留的每一条定一个"为什么还在"，返回剩余预算。
// 铁律：判不出来就写 other，不许编；原因来自系统自己返回的错，不是我们猜的。
func classifyLeft(paths []PathBytes, budget int) ([]LeftEntry, int) {
	if len(paths) == 0 || budget <= 0 {
		return nil, budget
	}
	n := len(paths)
	if n > budget {
		n = budget
	}
	out := make([]LeftEntry, 0, n)
	for i := 0; i < n; i++ {
		p := paths[i].Path
		if p == "" {
			continue
		}
		reason := whyLeft(p)
		// 已经排进重启队列的：不是"没清掉"，是"重启才删"。
		// 写成 locked 用户只会再点一次清理，而再点一次结果一模一样（2026-10-14）。
		if reason == "locked" && isRebootPlanned(p) {
			reason = "reboot"
		}
		out = append(out, LeftEntry{Path: p, Bytes: paths[i].Bytes, Reason: reason})
	}
	return out, budget - len(out)
}

// whyLeft 试一次"独占打开"看系统怎么说。
//   - 打得开            → 文件现在没被占着，能删。多半是清完又被程序重新写出来的（new）
//   - ERROR_SHARING_VIOLATION(32) → 正被某个程序占着（locked）
//   - ERROR_ACCESS_DENIED(5)      → 权限不够 / 系统保护（denied）
//   - 文件已经不在了              → gone
// 注意：Go 的 os.OpenFile 在 Windows 上带 FILE_SHARE_READ|WRITE，占用**不会**让它失败，
// 所以这里必须自己按 share=0 打开，否则会漏判掉所有"被占用"的情况。
func whyLeft(p string) string {
	if _, err := os.Stat(p); err != nil {
		return "gone"
	}
	ptr, err := windows.UTF16PtrFromString(p)
	if err != nil {
		return "other"
	}
	// FILE_FLAG_BACKUP_SEMANTICS：残留清单里可能混着目录，没这个 flag 打开目录会失败
	h, err := windows.CreateFile(ptr, windows.GENERIC_READ, 0, nil,
		windows.OPEN_EXISTING, windows.FILE_FLAG_BACKUP_SEMANTICS, 0)
	if err == nil {
		windows.CloseHandle(h)
		return "new"
	}
	var en syscall.Errno
	if errors.As(err, &en) {
		switch en {
		case 32: // ERROR_SHARING_VIOLATION
			return "locked"
		case 5: // ERROR_ACCESS_DENIED
			return "denied"
		case 2, 3: // ERROR_FILE_NOT_FOUND / ERROR_PATH_NOT_FOUND
			return "gone"
		}
	}
	return "other"
}

// protectedRuntimePaths 运行时 / 系统组件目录硬保护清单：命中即跳过删除。
//
// 2026-10-13 事故后加。当时「.NET 程序集缓存 (GAC)」项把 C:\Windows\Microsoft.NET\assembly\GAC_*
// 当缓存清掉了 —— 但 GAC 不是缓存，是 .NET Framework 强命名程序集的安装区，删了不会自动重建，
// 结果所有依赖 GAC 的 .NET 程序启动即抛 0xE0434352 静默崩溃（用户表现为"双击 exe 没反应"）。
// 用户原话：「如果清理之后影响机器的正常使用，那是不可饶恕的！」
//
// 口径：只保护「删了会让程序打不开 / 系统坏掉」的组件区，**不保护日志与缓存子目录**，
// 所以 System32\LogFiles（svclogs）、WinSxS\Temp（winsxstmp）这类照常能清。
// 同理不保护 C:\Windows\assembly\NativeImages_*（netnative 项要清的 NGen 映像，是能重建的真缓存）。
//
// ⚠️ 铁律一：不写死盘符 —— 统一小写后按「含分隔符的路径片段」匹配，系统盘不在 C 盘也生效。
var protectedRuntimePaths = []string{
	`\windows\microsoft.net\assembly\gac_32`,
	`\windows\microsoft.net\assembly\gac_64`,
	`\windows\microsoft.net\assembly\gac_msil`,
	`\windows\microsoft.net\framework`,
	`\windows\microsoft.net\framework64`,
	`\windows\assembly\gac`,
	`\windows\assembly\gac_32`,
	`\windows\assembly\gac_64`,
	`\windows\assembly\gac_msil`,
	`\windows\driverstore`,
	`\windows\system32\drivers`,
	`\windows\inf`,
}

// isProtectedRuntimePath 目标是否落在运行时保护清单内（小写比对，不写死盘符）
func isProtectedRuntimePath(p string) bool {
	lp := strings.ToLower(filepath.Clean(p)) + string(os.PathSeparator)
	for _, s := range protectedRuntimePaths {
		if strings.Contains(lp, s+string(os.PathSeparator)) {
			return true
		}
	}
	return false
}

func cleanDirItem(ctx context.Context, it *JunkItem) CleanResult {
	res := CleanResult{ID: it.ID, Name: it.Name}
	// 删除链 L3：先停掉占着它的服务，清完立刻恢复（junk_svcpause.go）
	dirs, files := resolveTargets(ctx, it)
	restore := pauseServicesFor(it.ID, files)
	defer restore()
	cleanTrace(fmt.Sprintf("  %s targets dirs=%d files=%d", it.ID, len(dirs), len(files)))
	var st removeStat
	failTraced := 0 // 删除失败留证的计数（见 traceRemoveFail）
	stopped := false
	for _, d := range dirs {
		if ctx.Err() != nil {
			stopped = true
			break
		}
		// 硬保护：运行时/组件目录一律不删，宁可少清也不能弄坏系统
		if isProtectedRuntimePath(d) {
			cleanTrace(fmt.Sprintf("clean skip(protected) %s 运行时组件目录，禁止删除", d))
			continue
		}
		entries, err := os.ReadDir(d)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if ctx.Err() != nil {
				stopped = true
				break
			}
			st = st.add(removeTreeSizedCtx(ctx, filepath.Join(d, e.Name())))
		}
	}
	if !stopped {
		for _, f := range files {
			if ctx.Err() != nil {
				stopped = true
				break
			}
			if isProtectedRuntimePath(f) {
				cleanTrace(fmt.Sprintf("clean skip(protected) %s 运行时组件目录，禁止删除", f))
				continue
			}
			if info, err := os.Stat(f); err == nil {
				ok, rerr := removeNow(f)
				if ok {
					st.freed += info.Size()
					continue
				}
				// 2026-10-15：删不掉必须留下「系统说的原话」，否则事后只能靠复扫反推
				traceRemoveFail(f, rerr, &failTraced)
				if markRebootDelete(f) == nil {
					st.pending++
					st.pendingBytes += info.Size()
					continue
				}
				st.failed++
			}
		}
	}
	res.Bytes = st.freed
	res.Failed = st.failed
	res.Reboot = st.pending
	res.RebootBytes = st.pendingBytes
	switch {
	case stopped:
		res.Err = "已停止"
	case st.pending > 0 && st.failed > 0:
		/* 两件事都要说清楚：一部分重启后自动删，另一部分这次真没辙。
		   合成一句"部分文件被占用"用户只会再点一次，而再点一次结果一模一样。 */
		res.Err = fmt.Sprintf("%d 个已排到重启后删除，另有 %d 个这次没清掉", st.pending, st.failed)
	case st.pending > 0:
		res.Err = fmt.Sprintf("%d 个已排到重启后删除", st.pending)
	case st.failed > 0:
		res.Err = "部分文件被占用"
	}
	return res
}

// removeStat 一次删除动作的实测结果。
// 2026-10-14：把「已排进重启队列」从 freed / failed 里拆出来。
//   以前删不掉只有 failed 一个筐，于是「等重启就能删掉」和「永远删不掉」混在一起，
//   既低估了真实能力（明明能删的被算成失败），也没法给用户任何下一步指引。
type removeStat struct {
	freed        int64 // 当场删掉、当场释放
	failed       int   // 删不掉，也没排上重启队列
	pending      int   // 删不掉，但已排进重启队列
	pendingBytes int64 // 这部分要等重启才释放，现在不算数
}

func (s removeStat) add(o removeStat) removeStat {
	return removeStat{freed: s.freed + o.freed, failed: s.failed + o.failed,
		pending: s.pending + o.pending, pendingBytes: s.pendingBytes + o.pendingBytes}
}

func removeTreeSizedCtx(ctx context.Context, root string) removeStat {
	var st removeStat
	info, err := os.Stat(root)
	if err != nil {
		return st
	}
	if !info.IsDir() {
		if ok, rerr := removeNow(root); !ok {
			cleanTrace(fmt.Sprintf("clean skip(单文件) %s err=%v", root, rerr))
			if markRebootDelete(root) == nil {
				st.pending, st.pendingBytes = 1, info.Size()
				return st
			}
			st.failed = 1
			return st
		}
		st.freed = info.Size()
		return st
	}
	type fileJob struct {
		p string
		s int64
	}
	jobs := make(chan fileJob, 256)
	var dirs []string
	var wg sync.WaitGroup
	var mu sync.Mutex
	skips := 0 // 2026-10-09：清理被跳过的目标数（只记录前 20 条的明细）
	var freed, pendingBytes int64
	var failed, pending int
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range jobs {
				ok, rerr := removeNow(j.p)
				if ok {
					mu.Lock()
					freed += j.s
					mu.Unlock()
					continue
				}
				mu.Lock()
				if skips < 20 {
					skips++
					cleanTrace(fmt.Sprintf("clean skip %s err=%v", j.p, rerr))
				}
				mu.Unlock()
				/* 2026-10-14：删不掉先试着排进重启队列，排上了就不算失败。
				   实测 134 个占用文件里绝大多数属于这一类（junk_rebootdel.go）。
				   2026-10-15：removeNow 拿不到手 → 这一轮手里多了一把钥匙（备份/还原特权），
				   WinSxS\Temp 那类 TrustedInstaller 文件就是靠它才删得动的。 */
				if markRebootDelete(j.p) == nil {
					mu.Lock()
					pending++
					pendingBytes += j.s
					mu.Unlock()
					continue
				}
				mu.Lock()
				failed++
				mu.Unlock()
			}
		}()
	}
	filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if ctx.Err() != nil {
			return errScanAborted
		}
		if err != nil {
			// 2026-10-09 修：原来这里一律 return nil —— 没删掉却不计 failed，
			// 界面上完全看不出「这块其实没清掉」，用户只会觉得「清完再扫还是有」。
			// 权限不足 / 路径 >260 字符 / 文件被句柄占用都会走到这里，现在如实计数 + 留 trace。
			if isReparsePoint(p) {
				return filepath.SkipDir
			}
			mu.Lock()
			failed++
			if skips < 20 {
				skips++
				cleanTrace(fmt.Sprintf("clean skip %s err=%v", p, err))
			}
			mu.Unlock()
			if d != nil && d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			if p != root && isReparsePoint(p) {
				return filepath.SkipDir
			}
			if p != root {
				dirs = append(dirs, p)
			}
			return nil
		}
		var sz int64
		if fi, err := d.Info(); err == nil {
			sz = fi.Size()
		}
		jobs <- fileJob{p, sz}
		return nil
	})
	close(jobs)
	wg.Wait()
	// 目录最深的先删（此时绝大多数已空）；删不掉的无视 —— 里面有被占用文件残留，上面已计入 failed
	sort.Slice(dirs, func(i, j int) bool { return len(dirs[i]) > len(dirs[j]) })
	for _, d := range dirs {
		_ = os.Remove(d)
	}
	_ = os.Remove(root)
	return removeStat{freed: freed, failed: failed, pending: pending, pendingBytes: pendingBytes}
}

// deepJunkFilesUnder 同上，但可指定根目录（单测用，避免测试碰真实 C 盘）

func deepJunkFilesUnder(root string) []string {
	root = filepath.Clean(root)
	var out []string
	filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			if d != nil && d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			if p == root {
				return nil
			}
			if isReparsePoint(p) {
				return filepath.SkipDir
			}
			return nil
		}
		name := strings.ToLower(d.Name())
		for _, e := range deepJunkExts {
			if strings.HasSuffix(name, e) {
				out = append(out, p)
				break
			}
		}
		return nil
	})
	return out
}

// clearDirContents 删除目录内全部条目，保留目录本身（与脚本 `del` + `rd`/`md` 语义一致）

func clearDirContents(dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	var firstErr error
	for _, e := range entries {
		if err := os.RemoveAll(filepath.Join(dir, e.Name())); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

/* thumbCacheTargets 随 thumbdeep 一并删除（2026-10-14）：清图标/缩略图缓存不再杀 explorer，
   改由 thumb 项的目录型清理链处理，这个函数已无调用点。 */

// diskFreeBytes 读取盘符可用空间（GetDiskFreeSpaceExW）

// ============================================================================
// 系统服务控制（停/启更新服务、资源管理器）

func openServiceHandle(name string, access uint32) (uintptr, bool) {
	scm, _, _ := procOpenSCM.Call(0, 0, scManagerConnect)
	if scm == 0 {
		return 0, false
	}
	defer procCloseSvcH.Call(scm)
	n16, err := syscall.UTF16PtrFromString(name)
	if err != nil {
		return 0, false
	}
	s, _, _ := procOpenSvc.Call(scm, uintptr(unsafe.Pointer(n16)), uintptr(access))
	if s == 0 {
		return 0, false
	}
	return s, true
}

// svcRunning 查询服务当前是否 RUNNING
func svcRunning(name string) bool {
	s, ok := openServiceHandle(name, svcQueryStatus)
	if !ok {
		return false
	}
	defer procCloseSvcH.Call(s)
	var st [7]uint32 // SERVICE_STATUS
	if r, _, _ := procQuerySvc.Call(s, uintptr(unsafe.Pointer(&st[0]))); r == 0 {
		return false
	}
	return st[1] == svcStateRunning
}

// svcStop 停止服务（已停止时返回 nil）
func svcStop(name string) error {
	s, ok := openServiceHandle(name, svcStopAccess|svcQueryStatus)
	if !ok {
		return fmt.Errorf("无法打开服务 %s", name)
	}
	defer procCloseSvcH.Call(s)
	var st [7]uint32
	if r, _, err := procCtrlSvc.Call(s, svcControlStop, uintptr(unsafe.Pointer(&st[0]))); r == 0 {
		return fmt.Errorf("停止 %s 失败: %v", name, err)
	}
	return nil
}

// svcStart 启动服务
func svcStart(name string) error {
	s, ok := openServiceHandle(name, svcStartAccess|svcQueryStatus)
	if !ok {
		return fmt.Errorf("无法打开服务 %s", name)
	}
	defer procCloseSvcH.Call(s)
	if r, _, err := procStartSvc.Call(s, 0, 0); r == 0 {
		return fmt.Errorf("启动 %s 失败: %v", name, err)
	}
	return nil
}

// ---------- 进程枚举（直接调 Toolhelp 快照，不依赖 tasklist.exe） ----------

type processEntry32 struct {
	dwSize              uint32
	cntUsage            uint32
	th32ProcessID       uint32
	th32DefaultHeapID   uintptr
	th32ModuleID        uint32
	cntThreads          uint32
	th32ParentProcessID uint32
	pcPriClassBase      int32
	dwFlags             uint32
	szExeFile           [syscall.MAX_PATH]uint16
}

// findProcess 按可执行名查找进程 PID（大小写不敏感）
func findProcess(exeName string) []uint32 {
	k32 := syscall.NewLazyDLL("kernel32.dll")
	create := k32.NewProc("CreateToolhelp32Snapshot")
	first := k32.NewProc("Process32FirstW")
	next := k32.NewProc("Process32NextW")
	closeH := k32.NewProc("CloseHandle")

	const th32csSnapProcess = 0x00000002
	h, _, _ := create.Call(th32csSnapProcess, 0)
	if h == 0 || h == uintptr(syscall.InvalidHandle) {
		return nil
	}
	defer closeH.Call(h)

	var pe processEntry32
	pe.dwSize = uint32(unsafe.Sizeof(pe))
	var out []uint32
	r, _, _ := first.Call(h, uintptr(unsafe.Pointer(&pe)))
	for r != 0 {
		if strings.EqualFold(syscall.UTF16ToString(pe.szExeFile[:]), exeName) {
			out = append(out, pe.th32ProcessID)
		}
		r, _, _ = next.Call(h, uintptr(unsafe.Pointer(&pe)))
	}
	return out
}

/* 2026-10-14 删除：explorerRunning / launchExplorerScriptWay / killExplorer
   这三个是 thumbdeep（杀 explorer → 清图标缓存 → 重启 explorer）的随葬品。
   thumb 已改走目录型清理链（当场删 + MoveFileEx 排队到重启删），不再结束资源管理器，
   它们从此没有任何调用点（铁律九：为十几 MB 黑一次屏、关掉用户所有文件夹窗口，不值）。
   想恢复"重启后仍是标准权限桌面"的话另写，别再从别处翻回来。 */

// ---------- 命令型清理（隐藏窗口执行，带超时） ----------

// systemDrive 本机系统盘根目录（带尾反斜杠），例如 `C:\`。
// 必要性：Windows.old / 卷影副本 / 组件存储都长在系统盘上，而**系统盘不一定是 C:** ——
// 盘符被改过的品牌机、PE/WinToGo、多系统机器上写死 C:\ 会让这几项恒报 0 且报"成功"（谎报）。
func systemDrive() string {
	d := strings.TrimSpace(os.Getenv("SystemDrive"))
	if d == "" {
		d = `C:` // 环境变量被裁剪时的兜底
	}
	return strings.TrimSuffix(d, `\`) + `\`
}

// sysExe 返回系统自带命令行工具的绝对路径；查不到就原样返回名字（退回走 PATH）。
// 必要性：精简版系统、服务上下文、企业锁 PATH 的机器上 PATH 可能被裁剪，
// 裸命令名会得到 exec: not found，而旧代码把所有失败一律说成「可能需要管理员权限」——
// 那是错误归因，用户照着去提权也没用（2026-10-11 换机器审查）。
// subdir 用于不在 System32 根目录的工具（如 WindowsPowerShell\v1.0）。
func sysExe(name, subdir string) string {
	if w := os.Getenv("SystemRoot"); w != "" {
		p := filepath.Join(w, "System32", subdir, name)
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return name
}

func runHidden(timeout time.Duration, name string, args ...string) error {
	return runHiddenCtx(context.Background(), timeout, name, args...)
}

// runHiddenCtx 同 runHidden，但挂在父 ctx 上 —— 清理停止时子进程（cleanmgr/dism 等）会被直接杀掉
func runHiddenCtx(parent context.Context, timeout time.Duration, name string, args ...string) error {
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: 0x08000000} // CREATE_NO_WINDOW
	return cmd.Run()
}

// runHiddenOut 同 runHidden，但捕获输出（用于需要判断“本来就没有”而非“失败”的场景）
func runHiddenOut(timeout time.Duration, name string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: 0x08000000}
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// hasShadowCopies 判断系统当前是否存在卷影副本/系统还原点。
// 必要性：vssadmin delete shadows 在“没有可删除的项目”时同样返回非 0 退出码，
// 若不区分就会把“本来就没有还原点”误报成“执行失败（可能需要管理员权限）”。
//
// 返回 (有无还原点, 能否判断)。**第二个返回值是 2026-10-11 加的**：
// 原来命令执行失败（vssadmin 不存在 / 超时）时也返回 false，于是把"没跑成"当成"本来就没有"，
// 界面报"清理成功"—— 这是谎报。现在判不出来就是判不出来，不许当成"没有"。
func hasShadowCopies() (have, known bool) {
	out, err := runHiddenOut(30*time.Second, sysExe("vssadmin.exe", ""), "list", "shadows")
	if err != nil {
		return false, false
	}
	for _, k := range []string{"找不到满足查询的项目", "没有找到", "No items found", "no items found"} {
		if strings.Contains(out, k) {
			return false, true
		}
	}
	for _, k := range []string{"卷影副本 ID", "Shadow Copy ID"} {
		if strings.Contains(out, k) {
			return true, true
		}
	}
	return false, false // 德/日/韩等其它语言的输出认不出来：判不出来，不猜
}

// ============================================================================
// 命令型清理项执行（deepjunk / cleanmgr / dismreset / wucachedeep / thumbdeep ...）

func (a *App) runCmdCleanCtx(ctx context.Context, it *JunkItem) (int64, string) {
	var err error
	// 2026-09-20 补齐：注册表垃圾 / 使用痕迹清理（重扫拿新鲜清单再删，见 junkreg.go）
	if strings.HasPrefix(it.Cmd, "reg:") {
		return regItemClean(ctx, it.Cmd)
	}
	switch it.Cmd {
	case "badlnk":
		return badlnkClean(ctx)
	case "recycle":
		// 2026-09-21 修复：清的是全盘回收站（SHEmptyRecycleBinW root=NULL），账也要记全盘 ——
		// 原来只量 C:\$Recycle.Bin，D 盘清掉 323MB 却报 freed=8.8MB，用户以为没删
		before := recycleTotalSize()
		err = clearRecycleBin()
		if err == nil {
			return before, ""
		}
		/* 2026-10-15 修正：**回收站本来就是空的时候，SHEmptyRecycleBinW 照样返回非 0**，
		   而那时代码一路 fall through 到函数末尾的统一兜底，给用户看的是
		   「执行失败（可能需要管理员权限）」—— 用户真去提权，提完还是这句话。
		   跟 2026-10-11 vssadmin 那次是同一个坑：**同一个非零返回，成因完全不同。**
		   改判据：出错不许自己猜原因，让系统自己的现状说话 —— 复核一遍全盘回收站，
		   真空了就是清掉了（含本来就没东西），还占着才是真失败。 */
		if after := recycleTotalSize(); after == 0 {
			cleanTrace(fmt.Sprintf("recycle: 系统接口返回 %v，但复核后回收站确实是空的 —— 按成功处理", err))
			return before, ""
		} else {
			return 0, fmt.Sprintf("回收站里还剩 %s 没清掉，多半是里面的文件正被占用", fmtBytes(after))
		}
	case "winold":
		// 2026-10-11：系统盘不再写死 C:（见 systemDrive），路径加引号防空格
		oldDir := filepath.Join(systemDrive(), "Windows.old")
		if _, serr := os.Stat(oldDir); serr != nil {
			return 0, "" // 不存在，无需清理
		}
		before, _ := walkSize(oldDir)
		err = runHiddenCtx(ctx, 10*time.Minute, sysExe("cmd.exe", ""), "/c",
			`takeown /f "`+oldDir+`" /r /d y >nul 2>&1 & icacls "`+oldDir+`" /grant administrators:F /t >nul 2>&1 & rmdir /s /q "`+oldDir+`" >nul 2>&1`)
		if err == nil {
			return before, ""
		}
	case "vss":
		// 2026-09-21 计量修复：删还原点真实释放空间，按系统盘空闲差记账（原来恒报 0）
		// 2026-10-11：盘符不再写死 C:（见 systemDrive）
		beforeVss := diskFreeBytes(systemDrive())
		err = runHiddenCtx(ctx, 5*time.Minute, sysExe("vssadmin.exe", ""), "delete", "shadows", "/all", "/quiet")
		if err != nil {
			// 只有**确认**本来就没有还原点，才不算失败；vssadmin 不可用 / 输出认不出来时
			// 如实报错 —— 不许把"没跑成"报成"清理成功"
			if have, known := hasShadowCopies(); known && !have {
				err = nil
			}
		}
		if err == nil {
			if after := diskFreeBytes(systemDrive()); after > beforeVss {
				return after - beforeVss, ""
			}
		}
	case "evtx":
		// 2026-09-21 计量修复：wevtutil cl 是截断日志文件，按 winevt\Logs 目录前后差记账（原来恒报 0）
		evtDir := filepath.Join(os.Getenv("windir"), `System32\winevt\Logs`)
		beforeEvt, _ := walkSize(evtDir)
		for _, lg := range []string{"Application", "System", "Security", "Setup"} {
			if ctx.Err() != nil {
				break
			}
			if e := runHiddenCtx(ctx, time.Minute, sysExe("wevtutil.exe", ""), "cl", lg); e != nil {
				err = e
			}
		}
		if err == nil {
			if after, _ := walkSize(evtDir); after < beforeEvt {
				return beforeEvt - after, ""
			}
		}
	case "dismreset":
		// 脚本第 186 行同款参数（G7 裁决：只留脚本这一种，不带 resetbase 的温和版已删）
		// 2026-09-21 计量修复：组件清理真实释放空间，按 C 盘空闲差记账（原来恒报 0）
		beforeDism := diskFreeBytes(systemDrive())
		err = runHiddenCtx(ctx, 20*time.Minute, sysExe("dism.exe", ""), "/online", "/cleanup-image", "/startcomponentcleanup", "/resetbase")
		if err == nil {
			if after := diskFreeBytes(systemDrive()); after > beforeDism {
				return after - beforeDism, ""
			}
		}
	case "iconrefresh":
		// 脚本第 361 行 ie4uinit.exe -show
		err = runHiddenCtx(ctx, time.Minute, filepath.Join(os.Getenv("windir"), "System32", "ie4uinit.exe"), "-show")
	case "wucachedeep":
		/* 🔴 2026-10-14 改：清完必须把服务启回来。
		   原来是照抄脚本（脚本自始至终没有 net start），结果 wuauserv / cryptSvc /
		   bits / msiserver 四个服务清完就一直停在原地 —— 用户当时毫无察觉，
		   但从此 Windows 更新、后台传输、加密服务、msi 安装全都废了。
		   铁律五：清出来的东西，绝不能让机器不能用。
		   只恢复「清理前本来就在跑」的：用户自己停着的，我们不去动它。 */
		W := os.Getenv("windir")
		wuSvcs := []string{"wuauserv", "cryptSvc", "bits", "msiserver"}
		wasUp := map[string]bool{}
		for _, s := range wuSvcs {
			if ctx.Err() != nil {
				return 0, "已停止"
			}
			wasUp[s] = svcRunning(s)
			/* 用带等待的版本：ControlService 返回 ≠ 文件句柄放开（searchidx 上实测
			   停完立刻删只有 36%，等够了才 99%）—— 这里同样是停服务再删。 */
			_ = svcStopWait(s, 5*time.Second)
		}
		defer func() {
			for _, s := range wuSvcs {
				if !wasUp[s] {
					continue
				}
				/* 起不回来必须留痕：静默吞掉的话，用户机器会一直少一个服务，
				   而他永远不会知道是我们干的。 */
				if !svcStartWait(s, 30*time.Second) {
					cleanTrace("wucachedeep: 服务 " + s + " 没启回来")
				} else {
					cleanTrace("wucachedeep: 服务 " + s + " 已恢复")
				}
			}
		}()
		var st removeStat
		for _, d := range []string{
			filepath.Join(W, `SoftwareDistribution\Download`),
			filepath.Join(W, `ServiceProfiles\NetworkService\AppData\Local\Microsoft\Windows\DeliveryOptimization\Cache`),
		} {
			if ctx.Err() != nil {
				return st.freed, "已停止"
			}
			entries, rerr := os.ReadDir(d)
			if rerr != nil {
				continue
			}
			for _, e := range entries { // 单遍「计量+删除」，替代旧版 walkSize+clearDirContents 两遍枚举
				if ctx.Err() != nil {
					return st.freed, "已停止"
				}
				st = st.add(removeTreeSizedCtx(ctx, filepath.Join(d, e.Name())))
			}
		}
		if st.failed > 0 {
			return st.freed, "部分文件被占用"
		}
		return st.freed, ""
	/* case "thumbdeep"（杀 explorer → 清图标缓存 → 重启 explorer）已于 2026-10-14 删除：
	   它要黑一次屏、还会关掉用户所有已打开的文件夹窗口，为十几 MB 不值（铁律九）。
	   thumb 现在走目录型清理链：当场删得动的删，删不动的 MoveFileEx 排队到重启删。 */
	case "deepjunk":
		// 脚本第 252~260 行：递归全盘清 5 类扩展名
		// 2026-09-19 提速：16 路并发遍历 + 遍历时顺带取体积（免去事后逐个 Stat），8 路并发删除
		// 2026-09-21 多盘：所有固定磁盘并行扫描（原写死 C 盘）
		files := deepJunkScanDrives(ctx)
		if ctx.Err() != nil {
			return 0, "已停止"
		}
		var freed, pendingBytes int64
		failed, pending := 0, 0
		var mu sync.Mutex
		var wg sync.WaitGroup
		sem := make(chan struct{}, 8)
		for _, f := range files {
			if ctx.Err() != nil {
				break
			}
			wg.Add(1)
			sem <- struct{}{}
			go func(f djFile) {
				defer wg.Done()
				defer func() { <-sem }()
				if err := os.Remove(f.path); err != nil {
					mu.Lock()
					if markRebootDelete(f.path) == nil {
						pending++
						pendingBytes += f.size
					} else {
						failed++
					}
					mu.Unlock()
				} else {
					mu.Lock()
					freed += f.size
					mu.Unlock()
				}
			}(f)
		}
		wg.Wait()
		if ctx.Err() != nil {
			return freed, "已停止"
		}
		if failed > 0 && pending > 0 {
			return freed, fmt.Sprintf("%d 个已排到重启后删除，另有 %d 个跳过", pending, failed)
		}
		if pending > 0 {
			return freed, fmt.Sprintf("%d 个已排到重启后删除", pending)
		}
		if failed > 0 {
			return freed, fmt.Sprintf("部分文件被占用（%d 个跳过）", failed)
		}
		return freed, ""
	case "brokenappx":
		// PowerShell 不在 System32 根目录，要带 WindowsPowerShell\v1.0（sysExe 找不到会退回 PATH）
		err = runHiddenCtx(ctx, 5*time.Minute, sysExe("powershell.exe", `WindowsPowerShell\v1.0`), "-NoProfile", "-Command",
			"Get-AppxPackage -AllUsers | Where-Object { $_.Status -eq 'Error' } | Remove-AppxPackage -ErrorAction SilentlyContinue")
	}
	if err != nil {
		if ctx.Err() != nil {
			return 0, "已停止" // 停止信号杀掉的子进程不能报成「执行失败」
		}
		if errors.Is(err, exec.ErrNotFound) || errors.Is(err, exec.ErrDot) {
			/* 2026-10-11：命令不存在和"权限不够"是两回事 ——
			   精简版系统 / 无桌面体验的 Server 上根本没有 cleanmgr、dism、ie4uinit，
			   一律说「可能需要管理员权限」是错误归因，用户照着提权也解决不了。 */
			return 0, "本机没有这个清理组件（系统里找不到它调用的系统工具）"
		}
		return 0, "执行失败（可能需要管理员权限）"
	}
	return 0, ""
}

// recycleTotalSize 全盘回收站实测体积。
// 2026-10-15：抽出来是因为同一个东西现在要量两遍 —— 清之前记一笔账，
// 清之后还要再量一次用来判定"到底清掉没有"（见上面 case "recycle" 里的注释）。
func recycleTotalSize() int64 {
	var total int64
	for _, drv := range fixedDrives() {
		b, _ := walkSize(drv + `$Recycle.Bin`)
		total += b
	}
	return total
}

func clearRecycleBin() error {
	ps := syscall.NewLazyDLL("shell32.dll")
	// SHEmptyRecycleBinW(hwnd, root=nil(所有盘), flags=SHERB_NOCONFIRMATION|SHERB_NOPROGRESSUI|SHERB_NOSOUND=7)
	p := ps.NewProc("SHEmptyRecycleBinW")
	if err := p.Find(); err != nil {
		return err
	}
	r, _, msg := p.Call(0, 0, 7)
	if r != 0 {
		return fmt.Errorf("recyclebin: %v", msg)
	}
	return nil
}

// ============================================================================
// 以下两个来自 v1.0 app.go：walkSize（清理前后核算用）与 diskFreeBytes
// （清理前后对比磁盘可用空间，得出真实释放量 —— 删的是压缩/稀疏文件时算体积会偏）。
// ============================================================================

func walkSize(root string) (int64, int) {
	if root == "" {
		return 0, 0
	}
	root = filepath.Clean(root)
	var size int64
	var count int
	filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return nil // 无权限/不存在直接跳过
		}
		if d.IsDir() {
			if p != root && isReparsePoint(p) {
				return filepath.SkipDir // 子目录里的 junction 不重复计数
			}
			return nil
		}
		if info, err := d.Info(); err == nil {
			size += info.Size()
			count++
		}
		return nil
	})
	return size, count
}

// CleanJunk 按白名单 ID 清理，返回每项释放的字节数
// cleanExecWeight 清理执行顺序权重（用户 2026-09-19 要求把耗时/干扰大的动作往后排，由 AI 负责「怎么运行」）。
// 数字越小越先执行；不在表里的（目录型快删项）默认 0，最先跑完给用户即时反馈。
// 排序用稳定排序，同权重保持用户勾选顺序。

func diskFreeBytes(drive string) int64 {
	k32 := syscall.NewLazyDLL("kernel32.dll")
	p := k32.NewProc("GetDiskFreeSpaceExW")
	if err := p.Find(); err != nil {
		return 0
	}
	d16, err := syscall.UTF16PtrFromString(drive)
	if err != nil {
		return 0
	}
	var free, total, totalFree uint64
	r, _, _ := p.Call(uintptr(unsafe.Pointer(d16)),
		uintptr(unsafe.Pointer(&free)), uintptr(unsafe.Pointer(&total)), uintptr(unsafe.Pointer(&totalFree)))
	if r == 0 {
		return 0
	}
	return int64(free)
}
