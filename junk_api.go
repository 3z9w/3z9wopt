package main

// ============================================================================
// 3z9wOpt v1.1 · Wails 绑定层（只暴露垃圾清理相关的接口）
//
// 事件流（前端 OnEvent 监听）：
//   scan:start  {total int, groups []string}
//   scan:item   {JunkItem}            每扫完一项推一次（流式，实时增长）
//   scan:done   {bytes, files, ms}
//   clean:progress (done, total, name, id)   与 v1.0 一致，前端可直接复用
// ============================================================================

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// orderByGroups 把待扫项按「前端传进来的 groups 先后顺序」排好（组内保持 junkItems 自带顺序）。
// 用户在界面上看到的类别顺序 = 扫描顺序 = 清理顺序，进度条上出现的项才不会跳来跳去。
// groups 为空（全扫）时保持原顺序不动。
func orderByGroups(list []JunkItem, groups []string) {
	if len(groups) == 0 {
		return
	}
	rank := make(map[string]int, len(groups))
	for i, g := range groups {
		if _, ok := rank[g]; !ok {
			rank[g] = i
		}
	}
	// 没在 groups 里出现过的组一律排最后（不靠 map 的零值，那样会排到最前面去）
	rankOf := func(g string) int {
		if r, ok := rank[g]; ok {
			return r
		}
		return len(groups)
	}
	sort.SliceStable(list, func(i, j int) bool {
		ri, rj := rankOf(list[i].Group), rankOf(list[j].Group)
		if ri != rj {
			return ri < rj
		}
		return list[i].Seq < list[j].Seq
	})
}

// ---------------------------------------------------------------------------
// 诊断日志（v1.0 写死 D:\3z9wopt\_proto，1.1 改到 %TEMP%，两边互不覆盖）
// ---------------------------------------------------------------------------

func cleanTrace(msg string) {
	f, err := os.OpenFile(filepath.Join(os.TempDir(), "3z9wOptV11_clean_trace.log"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err == nil {
		fmt.Fprintf(f, "%s %s\n", time.Now().Format("15:04:05.000"), msg)
		f.Close()
	}
}

// DebugLog 前端调试日志（写入同一 trace 文件，排查用）
func (a *App) DebugLog(s string) {
	cleanTrace("[JS] " + s)
}

// ---------------------------------------------------------------------------
// 扫描状态（1.1：只做清理停止，扫描不取消 —— 与 v1.0 的用户裁决一致）
// ---------------------------------------------------------------------------

// scanSeq 单次扫描的序号，前端据此丢弃过期回调
type scanState struct {
	mu      sync.Mutex
	running bool
}

var scanSt scanState

// StopClean 停止正在进行的清理（当前项就地中断，后续项不再执行，已清结果照实返回）
func (a *App) StopClean() {
	a.cleanMu.Lock()
	defer a.cleanMu.Unlock()
	if a.cleanCancel != nil {
		cleanTrace("StopClean: 用户请求停止清理")
		a.cleanCancel()
	}
}

// ---------------------------------------------------------------------------
// 清单
// ---------------------------------------------------------------------------

// GetJunkCatalog 返回全部清理项（未测量体积），前端进页面即列出全部项。
// Empty=true 的项（require 门槛全不存在 = 本机没装）前端隐藏该行。
func (a *App) GetJunkCatalog() []JunkItem {
	items := junkItems()
	for i := range items {
		items[i].Bytes = 0
		items[i].Files = 0
		items[i].ScanMs = 0
		// 哪些项在后台跑由 asyncCmds 一张表说了算，这里派生给前端（两处不许各写一套）
		items[i].Async = asyncCmds[items[i].Cmd]
		// 自适应：require 全不存在 → 前端隐藏
		if len(items[i].require) > 0 {
			ok := false
			for _, r := range items[i].require {
				if _, err := os.Stat(r); err == nil {
					ok = true
					break
				}
			}
			items[i].Empty = !ok
		}
	}
	return items
}

// JunkGroup 分组汇总（前端顶部用）
type JunkGroup struct {
	Name  string `json:"name"`
	Items int    `json:"items"`
}

// GetJunkGroups 返回全部组名 + 项数（供「选分类」页渲染）
func (a *App) GetJunkGroups() []JunkGroup {
	items := junkItems()
	order := []string{}
	m := map[string]int{}
	for _, it := range items {
		if _, ok := m[it.Group]; !ok {
			order = append(order, it.Group)
		}
		m[it.Group]++
	}
	out := []JunkGroup{}
	for _, g := range order {
		out = append(out, JunkGroup{Name: g, Items: m[g]})
	}
	return out
}

// ---------------------------------------------------------------------------
// 扫描（只扫勾选的组）
// ---------------------------------------------------------------------------

// ScanJunk 按组扫描。groups 为空 = 全扫。异步执行，结果通过事件推送。
// 2026-10-09 修复：原来上一轮还没扫完时直接静默 return，前端一直等 scan:done
// 永远等不到（取消扫描后再点扫描就会拿到「0 B 完成」）。现在返回 false 让前端明确提示。
func (a *App) ScanJunk(groups []string) bool {
	scanSt.mu.Lock()
	if scanSt.running {
		scanSt.mu.Unlock()
		cleanTrace("ScanJunk rejected: 上一轮扫描还没结束")
		return false
	}
	scanSt.running = true
	scanSt.mu.Unlock()
	go a.scanJunkAsync(groups)
	return true
}

func (a *App) scanJunkAsync(groups []string) {
	// v1.1：扫描可取消（前端「取消扫描」按钮）。已扫完的项照实推送，未开始的不派发。
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	scanCancelMu.Lock()
	scanCancel = cancel
	scanCancelMu.Unlock()
	defer func() {
		scanCancelMu.Lock()
		scanCancel = nil
		scanCancelMu.Unlock()
		scanSt.mu.Lock()
		scanSt.running = false
		scanSt.mu.Unlock()
	}()

	want := map[string]bool{}
	for _, g := range groups {
		want[g] = true
	}
	all := junkItems()
	var list []JunkItem
	for _, it := range all {
		if len(want) == 0 || want[it.Group] {
			list = append(list, it)
		}
	}
	orderByGroups(list, groups) // 扫描顺序 = 用户在选择页看到的顺序
	cleanTrace(fmt.Sprintf("ScanJunk start groups=%v items=%d", groups, len(list)))
	a.emit("scan:start", len(list), groups)

	t0 := time.Now()
	/* item 级并发（6 路），但**事件严格按 list 顺序推送**。
	   原来是谁先扫完谁先报 —— 界面上的项就会跳来跳去，与选择页的类别顺序对不上。
	   这里并发度不变（慢的一项只在"排队推送"上挡一下，总耗时不受影响）。 */
	par := 6
	sem := make(chan struct{}, par)
	var wg sync.WaitGroup
	var mu sync.Mutex
	cond := sync.NewCond(&mu)
	var totalBytes int64
	var totalFiles int
	var doneN int
	var aborted bool
	done := make([]bool, len(list))
	emitDone := make(chan struct{})
	go func() { // 顺序发射器：等第 i 项扫完才推第 i 条
		defer close(emitDone)
		for i := range list {
			mu.Lock()
			for !done[i] && !aborted {
				cond.Wait()
			}
			ok := done[i]
			mu.Unlock()
			if !ok {
				return // 用户取消了扫描：没扫到的不再推
			}
			a.emit("scan:item", &list[i])
		}
	}()
dispatch:
	for i := range list {
		mu.Lock()
		select {
		case <-ctx.Done():
			aborted = true
			mu.Unlock()
			cond.Broadcast()
			break dispatch
		default:
		}
		mu.Unlock()
		wg.Add(1)
		sem <- struct{}{}
		go func(it *JunkItem, idx int) {
			defer wg.Done()
			defer func() { <-sem }()
			scanOne(ctx, it) // v1.1.0：ctx 必须传进去，否则「取消扫描」中断不了正在跑的全盘扫描
			mu.Lock()
			totalBytes += it.Bytes
			totalFiles += it.Files
			doneN++
			done[idx] = true
			mu.Unlock()
			cond.Broadcast()
			if it.ScanMs > 300 {
				cleanTrace(fmt.Sprintf("scan slow %s %dms bytes=%d files=%d", it.ID, it.ScanMs, it.Bytes, it.Files))
			}
		}(&list[i], i)
	}
	wg.Wait()
	cond.Broadcast() // 兜底唤醒（正常不会卡住，留一道保险）
	<-emitDone      // 等事件推完再收尾：否则 scan:done 会抢在最后几条 item 前面
	// 最近一次真机扫描结果留档：GetTotalUnique 要靠它做路径级去重（前端拿不到 tsize）
	setLastScan(list)
	if aborted {
		cleanTrace(fmt.Sprintf("scan aborted: %d/%d items done, bytes=%d", doneN, len(list), totalBytes))
	}

	ms := time.Since(t0).Milliseconds()
	cleanTrace(fmt.Sprintf("scan done total=%dms bytes=%d files=%d", ms, totalBytes, totalFiles))
	// 2026-10-09 修复：后端其实有路径级去重（TotalUnique：父子覆盖 + 同 dup 组取最大），
	// 但前端拿不到 targets/tsize（这两个是小写字段，JSON 不序列化），只能用 sumUnique 拍脑袋，
	// 于是 deepjunk（全盘 .log/.dmp）与其它日志项天然重叠的部分被算两遍。
	// 现在把去重合计算出结果直接回传，前端不再自己加。
	uniqB, uniqF := TotalUnique(list, nil)
	cleanTrace(fmt.Sprintf("scan done unique bytes=%d files=%d", uniqB, uniqF))
	a.emit("scan:done", ms, uniqB, uniqF, totalBytes, totalFiles)
}

// ScanJunkSync 同步扫描（命令行自检 / 单测用，不依赖事件）。
// skipCmd 非空时跳过该 Cmd 的项（例：-nofull 跳掉 deepjunk 的全盘遍历，做快速迭代）。
func ScanJunkSync(groups []string, skipCmd string) ([]JunkItem, int64, int, int64) {
	want := map[string]bool{}
	for _, g := range groups {
		want[g] = true
	}
	all := junkItems()
	var list []JunkItem
	for _, it := range all {
		if skipCmd != "" && it.Cmd == skipCmd {
			continue
		}
		if len(want) == 0 || want[it.Group] {
			list = append(list, it)
		}
	}
	orderByGroups(list, groups) // 与异步扫描同一口径：顺序由传入的 groups 决定
	t0 := time.Now()
	ScanAll(context.Background(), list)
	setLastScan(list)
	var b int64
	var f int
	for i := range list {
		b += list[i].Bytes
		f += list[i].Files
	}
	return list, b, f, time.Since(t0).Milliseconds()
}

// ---------------------------------------------------------------------------
// 去重合计（勾选项的真实可释放量：同一处只算一次）
// ---------------------------------------------------------------------------

// lastScan 最近一次真机扫描的结果（含每条目标路径与体积）。
// 必要性：tsize/targets 是小写字段、不进 JSON，前端拿不到路径，
// 「同一个文件只算一次」这件事只能由后端算。
var lastScan struct {
	mu    sync.Mutex
	items []JunkItem
}

func setLastScan(items []JunkItem) {
	lastScan.mu.Lock()
	lastScan.items = items
	lastScan.mu.Unlock()
}

func getLastScan() []JunkItem {
	lastScan.mu.Lock()
	defer lastScan.mu.Unlock()
	return lastScan.items
}

// GetTotalUnique 传入勾选的 ID 列表，返回去重后的合计体积与目标数。
// ids 为空 = 全部计入。
// v1.1.0 修复：以前这里永远拿 junkItems()（tsize 全空）→ 恒返回 (0,0)，
// 前端也就没法用它核算。现在优先用最近一次真机扫描的结果，路径级去重才成立。
func (a *App) GetTotalUnique(ids []string) (int64, int) {
	items := getLastScan()
	if len(items) == 0 {
		items = junkItems() // 还没扫过：体积全是 0（前端此时也不该显示数字）
	}
	sel := map[string]bool{}
	for _, id := range ids {
		sel[id] = true
	}
	if len(ids) == 0 {
		sel = nil
	}
	return TotalUnique(items, sel)
}

// ---------------------------------------------------------------------------
// 清理（v1.0 的执行引擎，1:1 行为）
// ---------------------------------------------------------------------------

// ---------------------------------------------------------------------------
// 清理前的「谁占着」查询 + 经用户确认后关进程（2026-10-13 用户拍板）
//
// 拆成三步绑定是有意的：不改动 CleanJunkSafe 的签名（它是老接口，前端/单测都在用），
// 且让「关进程」这一步的成败能单独回传给前端 —— 用户能看见"钉钉没关掉"，
// 而不是混进清理结果里变成一个笼统的失败数。
// ---------------------------------------------------------------------------

// JunkWhoLocks 查「哪些进程占着这些清理项要删的文件」。
// 前端在点清理之后、真正清理之前调一次，据此弹**一次**窗让用户决定关不关。
// 返回空切片（不是 nil）表示没查到占用者，前端据此直接开始清理、不弹窗。
// ---------------------------------------------------------------------------
// 占用者探测缓存（2026-10-14 用户实测：「一键清理点不动」）
//
// 取证：%TEMP%\3z9wOptV11_clean_trace.log 里，JunkWhoLocks 抽 112 条路径走
// Restart Manager，单次 5~9 秒；后端 lockProbeMu 又把并发调用串成队 ——
// 用户点了 5 次就是 5 次排队，前端 15 秒超时后 reject，后端算完的结果没人接，
// 界面上什么都没发生。用户看到的就是「点不动」。
//
// 三条对策：
//   ① 同一组 id 30 秒内复用结果（前端扫描完会预热一次，点下去时基本已就绪）
//   ② 已经有同名探测在跑时**等它**，不排队重算（排队才是越点越慢的元凶）
//   ③ 前端点击立刻给反馈，不再让用户对着没变化的界面干等
// ---------------------------------------------------------------------------
const lockProbeTTL = 30 * time.Second

var (
	lockProbeMu        sync.Mutex
	lockProbeKey       string
	lockProbeAt        time.Time
	lockProbeVal       []Locker
	lockProbeRunning   string   // 正在算的 key；空表示没人在算
	lockProbeRunningAt time.Time // 防卡死：跑太久就当它已经废了
)

func lockProbeLoad(key string) ([]Locker, bool) {
	lockProbeMu.Lock()
	defer lockProbeMu.Unlock()
	if lockProbeKey == key && time.Since(lockProbeAt) < lockProbeTTL {
		out := make([]Locker, len(lockProbeVal))
		copy(out, lockProbeVal)
		return out, true
	}
	return nil, false
}

func lockProbeStore(key string, v []Locker) {
	lockProbeMu.Lock()
	lockProbeKey, lockProbeAt, lockProbeVal = key, time.Now(), v
	lockProbeMu.Unlock()
}

// lockProbeTryBegin 抢到算的资格返回 true；别人正在算返回 false。
// 上一轮算超过 60 秒还没结束（异常卡住）就强制接管，不让用户永远等下去。
func lockProbeTryBegin(key string) bool {
	lockProbeMu.Lock()
	defer lockProbeMu.Unlock()
	if lockProbeRunning != "" && time.Since(lockProbeRunningAt) < 60*time.Second {
		return false
	}
	lockProbeRunning, lockProbeRunningAt = key, time.Now()
	return true
}

func lockProbeEnd() {
	lockProbeMu.Lock()
	lockProbeRunning = ""
	lockProbeMu.Unlock()
}

// 后端真要清东西了就作废缓存：进程被关掉、文件被删掉，旧的占用结果已经不准。
func lockProbeReset() {
	lockProbeMu.Lock()
	lockProbeKey, lockProbeAt, lockProbeVal, lockProbeRunning = "", time.Time{}, nil, ""
	lockProbeMu.Unlock()
}

func (a *App) JunkWhoLocks(ids []string) []Locker {
	ids = append([]string(nil), ids...)
	sort.Strings(ids)
	key := strings.Join(ids, "|")

	if v, ok := lockProbeLoad(key); ok {
		cleanTrace(fmt.Sprintf("JunkWhoLocks: 复用缓存，%d 个占用进程", len(v)))
		return v
	}
	if !lockProbeTryBegin(key) {
		// 别排队重算（排队才是越点越慢的元凶），等正在跑的那次出结果
		cleanTrace("JunkWhoLocks: 已有同名探测在跑，等它的结果")
		deadline := time.Now().Add(30 * time.Second)
		for time.Now().Before(deadline) {
			time.Sleep(120 * time.Millisecond)
			if v, ok := lockProbeLoad(key); ok {
				return v
			}
		}
		cleanTrace("JunkWhoLocks: 等超时，本次不弹占用提示（照常清理）")
		return []Locker{} // 非 nil
	}
	defer lockProbeEnd()

	out := a.whoLocksNow(ids)
	lockProbeStore(key, out)
	return out
}

// whoLocksNow 真正的探测逻辑（调用方必须已经拿到 lockProbeTryBegin 的资格）。
func (a *App) whoLocksNow(ids []string) []Locker {
	known := map[string]JunkItem{}
	for _, it := range junkItems() {
		known[it.ID] = it
	}
	items := make([]JunkItem, 0, len(ids))
	for _, id := range ids {
		if it, ok := known[id]; ok {
			items = append(items, it)
		}
	}
	out := []Locker{} // 必须非 nil：前端直接 .map/.reduce，null 会打崩
	if len(items) == 0 {
		return out
	}

	// 硬超时：collectProbePaths 会走 resolveTargets，个别项解析慢不能把界面卡死
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()

	paths := collectProbePaths(ctx, items, 10, 200)
	if len(paths) == 0 {
		cleanTrace("JunkWhoLocks: 没有可探查的路径，跳过")
		return out
	}
	cleanTrace(fmt.Sprintf("JunkWhoLocks: 抽样 %d 条路径探查占用者", len(paths)))
	lks, err := whoLocks(paths)
	if err != nil {
		cleanTrace("JunkWhoLocks 失败: " + err.Error())
		return out
	}
	if len(lks) == 0 {
		return out
	}
	cleanTrace(fmt.Sprintf("JunkWhoLocks: 查到 %d 个占用进程", len(lks)))
	return append(out, lks...)
}

// KillLockers 强制结束用户勾选确认过的进程（破坏性动作：调用方必须先让用户确认）。
// 保护名单里的进程（本程序自己 / 安全软件）在这里再复核一次，一律不动。
func (a *App) KillLockers(pids []int) []KillPidResult {
	cleanTrace(fmt.Sprintf("KillLockers: 用户确认关闭 %d 个进程 %v", len(pids), pids))
	res := killPids(pids)
	ok := 0
	for _, r := range res {
		if r.Ok {
			ok++
		} else if r.Err != "" {
			cleanTrace(fmt.Sprintf("  KillLockers 未关闭 pid=%d %s: %s", r.Pid, r.Name, r.Err))
		}
	}
	cleanTrace(fmt.Sprintf("KillLockers 完成: 成功 %d / %d", ok, len(res)))
	if res == nil {
		return []KillPidResult{}
	}
	return res
}

// CleanJunkSafe 清理白名单内的 ID（前端只能传清单里的 ID，无法传任意路径）
func (a *App) CleanJunkSafe(ids []string) []CleanResult {
	// 真开始删了：占用结果立刻作废（进程可能被关掉、文件马上就没了）
	lockProbeReset()
	known := map[string]bool{}
	for _, it := range junkItems() {
		known[it.ID] = true
	}
	ok := []string{}
	for _, id := range ids {
		if known[id] {
			ok = append(ok, id)
		}
	}
	return a.CleanJunk(ok)
}

// ---------------------------------------------------------------------------
// 取消扫描（1.1 补：用户要求可取消；与 v1.0 不同，v1.0 只做清理停止）
// ---------------------------------------------------------------------------

var scanCancel context.CancelFunc
var scanCancelMu sync.Mutex

// StopScan 请求取消正在进行的扫描（已扫完的项照实返回）
func (a *App) StopScan() {
	scanCancelMu.Lock()
	defer scanCancelMu.Unlock()
	if scanCancel != nil {
		cleanTrace("StopScan: 用户请求停止扫描")
		scanCancel()
	}
}
