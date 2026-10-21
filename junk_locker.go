package main

// 清理前的「谁占着文件」查询 + 经用户确认后强制结束进程（2026-10-13 用户拍板）。
//
// 为什么要这一层：扫描报 2G、实际只清掉 1G，用户会认为软件不行。
// 大头是文件被程序占着（locked）+ 清完程序立刻重建（new），两者同源 —— 把占着的进程关掉，
// 清理就能真正删干净，复扫也不会再看到"又长出来"的东西。
//
// 用什么查：Windows 官方 Restart Manager（rstrtmgr.dll，Vista+ 自带，
// Windows Installer 判断"谁占着文件"用的就是它）。不是自己枚举句柄。
//
// 铁律对齐：
//   - 判不出来就说判不出来，不编进程名（RmGetList 拿不到就是没人占，或系统不让查）
//   - 关进程只关用户确认过的 PID，且安全软件主进程一律不动（关掉 = 解除防护，属"影响正常运行"）
//   - 强制结束是破坏性动作，调用方必须先让用户确认，本文件不自己决定关谁

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// ---------------------------------------------------------------------------
// Restart Manager 的 P/Invoke
// ---------------------------------------------------------------------------

var (
	rmDLL         = syscall.NewLazyDLL("rstrtmgr.dll")
	rmStartSess   = rmDLL.NewProc("RmStartSession")
	rmRegister    = rmDLL.NewProc("RmRegisterResources")
	rmGetList     = rmDLL.NewProc("RmGetList")
	rmEndSession  = rmDLL.NewProc("RmEndSession")
	kernel32DLL   = syscall.NewLazyDLL("kernel32.dll")
	procQueryName = kernel32DLL.NewProc("QueryFullProcessImageNameW")
)

// RM_UNIQUE_PROCESS：PID(4) + FILETIME(8) = 12 字节，对齐 4
type rmUniqueProcess struct {
	Pid       uint32
	StartTime syscall.Filetime
}

// RM_PROCESS_INFO：实测 sizeof = 668（已在本机验证：12 + 512 + 128 + 4*4）
// 结构体大小算错会静默读错进程名，改这里必须重跑验证。
type rmProcessInfo struct {
	Process          rmUniqueProcess
	AppName          [256]uint16 // CCH_RM_MAX_APP_NAME
	ServiceShortName [64]uint16  // CCH_RM_MAX_SVC_NAME
	ApplicationType  uint32
	AppStatus        uint32
	TSSessionId      uint32
	Restartable      int32
}

// RmStartSession 的会话 key 缓冲区长度（CCH_RM_SESSION_KEY = sizeof(GUID)*2 + 1 = 65）
const rmSessionKeyLen = 65

// ---------------------------------------------------------------------------
// 对外数据结构
// ---------------------------------------------------------------------------

// Locker 一个占着待清理文件的**应用**（不是单个进程）。
// 为什么要按应用聚合：实测一台机器上钉钉有 8 个进程、Edge 4 个、微信 4 个，
// 逐个列出来 UI 就是 8 行「钉钉」，用户根本没法选。按 exe 聚成一条，关的时候把它的进程全关掉。
type Locker struct {
	Pids    []int  `json:"pids"`    // 该应用的全部进程，关闭时一起关
	Pid     int    `json:"pid"`     // 代表 PID（= Pids[0]），只为兼容显示
	Name    string `json:"name"`    // 系统报告的应用名（如「钉钉」），拿不到时回落到 exe 名
	Exe     string `json:"exe"`     // 映像文件名（如 DingTalk.exe），拿不到为空
	Files   int    `json:"files"`   // 抽样里被它占住的文件数（抽样，不是全量）
	Protect bool   `json:"protect"` // true = 不允许关闭（自身 / 安全软件）
	Why     string `json:"why"`     // Protect 的原因，前端照播
}

// ---------------------------------------------------------------------------
// 查占用者
// ---------------------------------------------------------------------------

// whoLocks 用 Restart Manager 查「哪些进程占着这些路径」。
// 分批注册是因为一次注册过多路径会失败；拿不到就说拿不到，不猜。
func whoLocks(paths []string) ([]Locker, error) {
	paths = cleanPathList(paths)
	if len(paths) == 0 {
		return nil, nil
	}
	tree := selfTree()   // 本程序进程树：这里命中的一律不许关（见 selfTree 注释）
	hits := map[uint32]int{}    // pid -> 占住的文件数
	apps := map[uint32]string{} // pid -> 系统报告的应用显示名（如「钉钉」）
	const batch = 60
	for i := 0; i < len(paths); i += batch {
		end := i + batch
		if end > len(paths) {
			end = len(paths)
		}
		got, err := rmWhoLocksBatch(paths[i:end])
		if err != nil {
			// 单批失败不拖垮整体：这一批的信息放弃，其余照实返回
			cleanTrace(fmt.Sprintf("whoLocks 批次 %d-%d 失败: %v", i, end, err))
			continue
		}
		for _, h := range got {
			hits[h.Pid]++
			if h.App != "" && apps[h.Pid] == "" {
				apps[h.Pid] = h.App
			}
		}
	}
	if len(hits) == 0 {
		return nil, nil
	}
	// 按 exe 聚合成「应用」：同一个 exe 的多个进程并成一条，关闭时一起关
	type agg struct {
		pids   []int
		files  int
		name   string
		exe    string
		isSelf bool
	}
	groups := map[string]*agg{}
	order := []string{}
	for pid, n := range hits {
		exe := processImageName(pid)
		key := strings.ToLower(strings.TrimSpace(exe))
		if key == "" {
			key = fmt.Sprintf("pid:%d", pid) // 拿不到 exe 名的进程各自算一条，不硬凑
		}
		g := groups[key]
		if g == nil {
			g = &agg{exe: exe, name: strings.TrimSpace(exe), isSelf: tree[pid]}
			groups[key] = g
			order = append(order, key)
		}
		g.pids = append(g.pids, int(pid))
		g.files += n
		if tree[pid] {
			g.isSelf = true // 同名的进程里只要有一个是自己的进程树，整组都不许关
		}
		if g.isSelf {
			continue // 自己的显示名保持 exe 名
		}
		if a, ok := apps[pid]; ok && a != "" {
			g.name = a // 系统报的应用名更贴近任务管理器里看到的
		}
	}
	out := make([]Locker, 0, len(groups))
	for _, k := range order {
		g := groups[k]
		lk := Locker{Pids: g.pids, Pid: g.pids[0], Name: g.name, Exe: g.exe, Files: g.files}
		if lk.Name == "" {
			lk.Name = fmt.Sprintf("PID %d", lk.Pid)
		}
		if g.isSelf {
			lk.Protect = true
			lk.Why = "本程序自己（关掉它清理就中断了）"
		} else if w, ok := protectReasonAny(g.exe, g.name); ok {
			lk.Protect = true
			lk.Why = w
		}
		out = append(out, lk)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Protect != out[j].Protect {
			return !out[i].Protect // 可关的排前面
		}
		if out[i].Files != out[j].Files {
			return out[i].Files > out[j].Files
		}
		return out[i].Name < out[j].Name
	})
	return out, nil
}

// rmHit 一个占用者：PID + 系统报告的应用显示名
type rmHit struct {
	Pid uint32
	App string
}

// rmWhoLocksBatch 一批路径走一次完整 RM 会话，返回占用者。
func rmWhoLocksBatch(paths []string) ([]rmHit, error) {
	var session uint32
	key := make([]uint16, rmSessionKeyLen)
	if r, _, _ := rmStartSess.Call(uintptr(unsafe.Pointer(&session)), 0,
		uintptr(unsafe.Pointer(&key[0]))); r != 0 {
		return nil, fmt.Errorf("RmStartSession 返回 %d", r)
	}
	defer rmEndSession.Call(uintptr(session))

	ptrs := make([]uintptr, len(paths))
	for i, p := range paths {
		w, err := syscall.UTF16PtrFromString(p)
		if err != nil {
			continue
		}
		ptrs[i] = uintptr(unsafe.Pointer(w))
	}
	if r, _, _ := rmRegister.Call(uintptr(session), uintptr(len(ptrs)),
		uintptr(unsafe.Pointer(&ptrs[0])), 0, 0, 0, 0); r != 0 {
		return nil, fmt.Errorf("RmRegisterResources 返回 %d", r)
	}

	var needed, n, reboot uint32
	// 第一次调用只为取数量，返回 234(ERROR_MORE_DATA) 是正常的
	r, _, _ := rmGetList.Call(uintptr(session), uintptr(unsafe.Pointer(&needed)),
		uintptr(unsafe.Pointer(&n)), 0, uintptr(unsafe.Pointer(&reboot)))
	if r != 0 && r != 234 {
		return nil, fmt.Errorf("RmGetList 返回 %d", r)
	}
	if needed == 0 {
		return nil, nil
	}
	buf := make([]rmProcessInfo, needed)
	n = needed
	if r, _, _ := rmGetList.Call(uintptr(session), uintptr(unsafe.Pointer(&needed)),
		uintptr(unsafe.Pointer(&n)),
		uintptr(unsafe.Pointer(&buf[0])), uintptr(unsafe.Pointer(&reboot))); r != 0 {
		return nil, fmt.Errorf("RmGetList(2) 返回 %d", r)
	}
	out := make([]rmHit, 0, n)
	for i := uint32(0); i < n; i++ {
		if buf[i].Process.Pid == 0 {
			continue
		}
		h := rmHit{Pid: buf[i].Process.Pid}
		// 服务类进程的名字在 ServiceShortName 里，普通应用在 AppName 里
		if s := syscall.UTF16ToString(buf[i].ServiceShortName[:]); s != "" {
			h.App = s
		} else {
			h.App = syscall.UTF16ToString(buf[i].AppName[:])
		}
		out = append(out, h)
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// 进程信息 / 强制结束
// ---------------------------------------------------------------------------

// processImageName 拿进程的映像文件名（只要文件名，不要全路径）。
func processImageName(pid uint32) string {
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, pid)
	if err != nil {
		return ""
	}
	defer windows.CloseHandle(h)
	buf := make([]uint16, 1024)
	size := uint32(len(buf))
	r, _, _ := procQueryName.Call(uintptr(h), 0,
		uintptr(unsafe.Pointer(&buf[0])), uintptr(unsafe.Pointer(&size)))
	if r == 0 || size == 0 {
		return ""
	}
	full := windows.UTF16ToString(buf[:size])
	if full == "" {
		return ""
	}
	return filepath.Base(full)
}

// KillPidResult 一个 PID 的结束结果（前端照播，失败原因必须是系统说的话）
type KillPidResult struct {
	Pid  int    `json:"pid"`
	Name string `json:"name"`
	Ok   bool   `json:"ok"`
	Err  string `json:"err,omitempty"`
}

// killPids 强制结束一批进程。只关调用方给进来的 PID，且这里再做一次保护名单复核。
// 已被保护名单命中 / 已退出的进程不算失败。
//
// 2026-10-15 修一个真机事故：用户勾了 ToDesk，界面上"没报错"，可 ToDesk 的界面又弹回来了。
// 真相不是我们关不掉，而是**只关了一个**：ToDesk 这类软件同时跑着多个同名的实例
// （一个在服务会话里随系统启动、一个在桌面会话里是用户看得见的窗口），
// 杀掉其中一个，另一个（或它背后的守护）立刻把界面拉回来。
// → 关进程要按"这个程序"关，不是按"这个 PID"关。
func killPids(pids []int) []KillPidResult {
	out := make([]KillPidResult, 0, len(pids))
	self := os.Getpid()
	tree := selfTree()
	peers := pidsByImage() // 一次快照：进程名 → 它的所有 PID
	done := map[uint32]bool{}
	killed := map[string]bool{} // 真正关掉过的程序名，供下面复查"有没有复活"
	for _, pid := range pids {
		if pid <= 0 || done[uint32(pid)] {
			continue // 无效 PID / 同一个程序被勾了两次，不重复动手
		}
		res := KillPidResult{Pid: pid, Name: processImageName(uint32(pid))}
		if res.Name == "" {
			res.Name = fmt.Sprintf("PID %d", pid)
		}
		// 跳过的也必须留一条记录 —— 前端要照播，静默丢掉会让用户以为"没处理"
		if pid == self || tree[uint32(pid)] {
			res.Err = "已跳过：本程序自己"
			out = append(out, res)
			continue
		}
		if w, ok := protectReason(res.Name); ok {
			res.Err = "已跳过：" + w
			out = append(out, res)
			continue
		}
		/* 同名实例一起收拾：用户勾的是"ToDesk"这个程序，不是一个 PID。
		   只杀其中一个，剩下的那个会把它拉回来 —— 那才是"关不掉"的真相。 */
		group := []uint32{uint32(pid)}
		if key := strings.ToLower(res.Name); key != "" {
			for _, q := range peers[key] {
				if q == uint32(pid) || q == uint32(self) || tree[q] || done[q] {
					continue
				}
				group = append(group, q)
			}
		}
		for _, g := range group {
			done[g] = true
			r := KillPidResult{Pid: int(g), Name: res.Name}
			if err := terminateProcess(g); err != nil {
				r.Err = classifyKillErr(err, res.Name)
			} else {
				r.Ok = true
				killed[strings.ToLower(res.Name)] = true
			}
			out = append(out, r)
		}
	}
	out = append(out, respawnedResults(killed, done)...)
	return out
}

// respawnedResults 复查「关掉了却又自己起来」的那些。
//
// 有些软件被它的后台守护重新拉起来（ToDesk、向日葵、TeamViewer 都是这个路数）：
// 我们明明 TerminateProcess 成功了，用户的屏幕上它却还在。
// 这时候**必须照实说** —— 闷声不说，用户看到的是"这工具连个进程都关不掉"；
// 说出来，他就知道那批文件只能等重启删，不会再去点第二遍。
// 返回一个「已跳过」语义的结果，让前端聚成一句话，不逐条弹窗。
func respawnedResults(killed map[string]bool, done map[uint32]bool) []KillPidResult {
	if len(killed) == 0 {
		return nil // 一个都没关掉就没必要等这 900 毫秒
	}
	time.Sleep(900 * time.Millisecond)
	alive := pidsByImage()
	names := make([]string, 0, len(killed))
	for n := range killed {
		names = append(names, n)
	}
	sort.Strings(names) // 顺序固定，日志和界面上的说法才不会每次都不一样
	out := make([]KillPidResult, 0, len(names))
	for _, n := range names {
		newPid := uint32(0)
		for _, p := range alive[n] {
			if !done[p] {
				newPid = p
				break
			}
		}
		if newPid == 0 {
			continue // 没复活，正常
		}
		out = append(out, KillPidResult{
			Pid:  int(newPid),
			Name: n,
			Err:  "已跳过：关掉了但它自己又起来了（后台服务在重新拉它）",
		})
	}
	return out
}

// classifyKillErr 把「没关掉」说成人话。
//
// 为什么必须分（2026-10-15 真机）：用户勾了一批进程，界面上刷出一串「无法关闭 XXX」，
// 看着像软件没本事。实打实的真相是几件完全不同的事：
//   1. Windows 不允许关（受保护进程 / 系统宿主，OpenProcess 直接 ACCESS_DENIED）
//      —— 谁来了都关不掉，而且**不影响这次清理**，不该按失败报；
//   2. 普通软件却 ACCESS_DENIED：它跑在比本工具更高的权限里（本工具这次没以管理员身份启动）
//      —— 这是**我们**权限不够，不是它受保护。说成"受保护"会误导用户以为谁来了都没辙，
//         而他的真实处境只是"右键用管理员身份再开一次就好"；
//   3. 真没关掉（句柄竞争、进程刚好在退出…）—— 这才是我们的问题。
// 前缀「已跳过：」是给前端的暗号：跳过的不计失败、聚成一句话说完，别一条一个弹窗。
func classifyKillErr(err error, name string) string {
	var en syscall.Errno
	if errors.As(err, &en) && (en == 5 || en == 1314) {
		if _, protected := protectReason(name); protected {
			return "已跳过：Windows 保护这个进程，谁都关不掉"
		}
		return "没关掉：它的权限比本工具高（试试用管理员身份运行本工具）"
	}
	return "没关掉：" + err.Error()
}

func terminateProcess(pid uint32) error {
	h, err := windows.OpenProcess(windows.PROCESS_TERMINATE|windows.SYNCHRONIZE, false, pid)
	if err != nil {
		// 系统关键进程（csrss / smss / System …）在这里就被拒绝 —— 这正是我们要的天然防线
		return fmt.Errorf("打不开进程：%v", err)
	}
	defer windows.CloseHandle(h)
	if err := windows.TerminateProcess(h, 1); err != nil {
		return fmt.Errorf("结束失败：%v", err)
	}
	// 等一下让句柄真正释放（不等待会导致清理时仍报占用）
	windows.WaitForSingleObject(h, 3000)
	return nil
}

// ---------------------------------------------------------------------------
// 本程序进程树：自己 + 自己的子进程（含 WebView2 界面进程）
// ---------------------------------------------------------------------------

// selfTree 返回「本程序自己 + 它的所有后代进程」的 PID 集合。
//
// 为什么必须有这一层（2026-10-13 用户发现）：本程序是 Wails + WebView2，
// 界面跑在 msedgewebview2.exe 子进程里，而它通常正占着我们自己的缓存目录
// （%APPDATA%\<本程序>\EBWebView\Default\Cache 会被通用应用缓存规则扫到）。
// Restart Manager 于是把「Microsoft Edge WebView2」报成占用者，
// 若当成普通进程关掉 —— 清理还没跑完，本程序自己先没了。
//
// 判不出来（快照失败）时只返回自己：宁可少保护，也不许把别的进程误当成自己。
func selfTree() map[uint32]bool {
	set := map[uint32]bool{uint32(os.Getpid()): true}
	parents := processParentMap()
	// 多跑几轮：子进程还会再派生子进程（WebView2 自身就有渲染/Utility 子进程）
	for i := 0; i < 8 && len(parents) > 0; i++ {
		grew := false
		for pid, ppid := range parents {
			if set[pid] {
				continue
			}
			if _, ok := set[ppid]; ok {
				set[pid] = true
				grew = true
			}
		}
		if !grew {
			break
		}
	}
	return set
}

// processParentMap 用进程快照拿一次 pid -> 父 pid（比逐个 NtQuery 快得多，一次够用）。
func processParentMap() map[uint32]uint32 {
	snap, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return nil
	}
	defer windows.CloseHandle(snap)
	var e windows.ProcessEntry32
	e.Size = uint32(unsafe.Sizeof(e))
	if err := windows.Process32First(snap, &e); err != nil {
		return nil
	}
	out := map[uint32]uint32{}
	for {
		out[e.ProcessID] = e.ParentProcessID
		if err := windows.Process32Next(snap, &e); err != nil {
			break // ERROR_NO_MORE_FILES = 走完了
		}
	}
	return out
}

// pidsByImage 一次快照列出「进程名（小写）→ 它的所有 PID」。
//
// 为什么要这一层（2026-10-15 真机）：ToDesk 勾了却像没关掉 —— 界面又弹了回来。
// 实测 `tasklist` 一眼就看见了：**它同时跑着两个实例**，
// 一个在服务会话（session 0，随系统启动的那个），一个在桌面会话（用户看得见的窗口）。
// 杀掉其中一个，另一个立刻把界面拉回来，看上去就是"这工具关不掉进程"。
// 向日葵 / TeamViewer / 带守护的远控软件都是这个路数。
// 只取一次快照是为了让"关之前查名单"和"关之后复查"用的是同一套口径。
func pidsByImage() map[string][]uint32 {
	snap, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return nil
	}
	defer windows.CloseHandle(snap)
	var e windows.ProcessEntry32
	e.Size = uint32(unsafe.Sizeof(e))
	if err := windows.Process32First(snap, &e); err != nil {
		return nil
	}
	out := map[string][]uint32{}
	for {
		n := strings.ToLower(windows.UTF16ToString(e.ExeFile[:]))
		if n != "" && e.ProcessID != 0 {
			out[n] = append(out[n], e.ProcessID)
		}
		if err := windows.Process32Next(snap, &e); err != nil {
			break // ERROR_NO_MORE_FILES = 走完了
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// 保护名单：这些进程一律不关
// ---------------------------------------------------------------------------

// protectReason 返回该进程为什么不能关。ok=false 表示可以关。
// 依据：关掉它会让用户电脑进入不安全状态，或就是本程序自己。
// 系统关键进程（csrss/smss/lsass…）不在名单里 —— Windows 自己会拒绝 OpenProcess，
// 那道防线比名单更可靠（实测 TerminateProcess 对它们必然失败）。
func protectReason(exe string) (string, bool) {
	if exe == "" {
		return "", false
	}
	n := strings.ToLower(filepath.Base(exe))
	switch {
	case n == strings.ToLower(filepath.Base(os.Args[0])):
		return "本程序自己", true
	case n == "msedgewebview2.exe":
		// WebView2 是**别的软件的界面进程**（钉钉/微信/各类 Electron 替代品都在用）。
		// 只杀它不杀宿主，结果是软件还在、界面黑掉 —— 比整个关掉更难看，统一不关。
		// 本程序自己的那个已被 selfTree 拦在更前面，这里兜的是别人家的。
		return "WebView2 界面进程（关掉只会让软件界面黑掉，软件本体还在）", true
	case n == "explorer.exe":
		/* Windows 外壳：2026-10-15 真机事故后从这里封死（用户原话「桌面黑了，一直没回来」）。
		   原因很简单：**我们关得掉，但拉不回来** —— TerminateProcess 之后原本指望系统自己重开，
		   可只有少数情况下 Windows 才会重建 shell，多数时候就是一片黑屏摆在那儿，
		   用户只能手动重启资源管理器。为了多清十几 MB 缩略图缓存换来一个黑屏，这笔账怎么算都亏。
		   thumb 清理链早就改成「不杀 explorer、删不动的走 MoveFileEx 排到重启删」，
		   这条只是在补另一个口子：**用户自己勾的那条路也通向 killPids**（两头口径不一致）。
		   🔴 要把它放出来，必须先兑现"关完自己拉回来"（复制 explorer 原用户令牌后 CreateProcessAsUser），
		      否则就是 2026-10-15 那个黑屏再来一遍。 */
		return "Windows 桌面和任务栏（关掉后桌面会黑，本工具不会把它拉回来）", true
	}
	// 安全软件主进程：关掉 = 临时解除防护，属"影响正常运行"
	sec := []struct {
		name string
		who  string
	}{
		{"360safe.exe", "360安全卫士"}, {"360tray.exe", "360安全卫士"},
		{"qhsafetray.exe", "360安全卫士"}, {"360sd.exe", "360杀毒"},
		{"qqpcmgr.exe", "QQ电脑管家"}, {"qqpctray.exe", "QQ电脑管家"},
		{"qqpcexternal.exe", "QQ电脑管家"}, {"qqpcrealtimespeedup.exe", "QQ电脑管家"},
		{"hipsdaemon.exe", "火绒安全"}, {"usysdiag.exe", "火绒安全"},
		{"sysdiag.exe", "火绒安全"}, {"hipstray.exe", "火绒安全"},
		{"msmpeng.exe", "Windows Defender"}, {"nissrv.exe", "Windows Defender"},
		{"securityhealthservice.exe", "Windows 安全中心"},
		{"securityhealthsystray.exe", "Windows 安全中心"},
		{"avp.exe", "卡巴斯基"}, {"klnagent.exe", "卡巴斯基"},
		{"mcafee", "McAfee"}, {"mfeann.exe", "McAfee"},
		{"symantec", "赛门铁克"}, {"ccsvchst.exe", "赛门铁克"},
		{"avg", "AVG"}, {"avast", "Avast"}, {"aswidsagent.exe", "Avast"},
		{"bdagent.exe", "Bitdefender"}, {"vsserv.exe", "Bitdefender"},
		{"ekrn.exe", "ESET"}, {"egui.exe", "ESET"},
		{"avguard.exe", "Avira"}, {"savservice.exe", "Sophos"},
		{"psanhost.exe", "趋势科技"}, {"tmccsf.exe", "趋势科技"},
		{"drweb", "Dr.Web"}, {"f-secure", "F-Secure"},
		{"sandboxie", "Sandboxie"}, {"kismain.exe", "金山毒霸"},
		{"kxetray.exe", "金山毒霸"}, {"ksafe.exe", "金山卫士"},
	}
	for _, s := range sec {
		if strings.HasPrefix(n, s.name) {
			return s.who + "（安全软件，关掉会解除防护）", true
		}
	}
	/* Windows 自己的核心进程（2026-10-15 补）：系统不允许任何软件结束它们，
	   TerminateProcess 对它们必然失败 —— 以前这些跟 QQ 一样摆在"能勾的框"里，
	   用户勾了没反应，只会以为软件坏了。现在直接标成关不掉，界面上不给勾。
	   ⚠️ svchost.exe 一定要在里面：DiagTrack / WSearch 这类服务都跑在它里面，
	   为了清几十 MB 去杀一个服务宿主进程，是把机器往不可用里推。 */
	for _, s := range []string{
		"system", "registry", "memory compression", "secure system", "idle",
		"svchost.exe", "csrss.exe", "smss.exe", "lsass.exe", "wininit.exe",
		"services.exe", "dwm.exe", "winlogon.exe", "fontdrvhost.exe",
		"ctfmon.exe", "taskhostw.exe", "sihost.exe", "shellexperiencehost.exe",
		// 下面是 Restart Manager 报的**服务名**（不是 exe 名），走 protectReasonAny 那条路匹配
		"diagtrack", "trustedinstaller", "wsearch", "sysmain", "cryptsvc", "bits",
		/* ↓ 2026-10-15 补：这批是「壳」和各类系统宿主（受保护进程 PPL / 服务宿主），
		   TerminateProcess 对它们必然回 ACCESS_DENIED。以前它们跟 QQ 一起摆在能勾的框里，
		   用户勾了没反应还弹一堆「无法关闭 XXX」—— 表面像是软件坏了，
		   实际是 Windows 压根不允许。现在一律标成关不掉，界面上不给勾。 */
		"startmenuexperiencehost.exe", "searchhost.exe", "searchapp.exe",
		"searchindexer.exe", "searchprotocolhost.exe", "runtimebroker.exe",
		"applicationframehost.exe", "textinputhost.exe", "lockapp.exe",
		"backgroundtaskhost.exe", "conhost.exe", "audiodg.exe", "dllhost.exe",
		"wudfhost.exe", "wmiprvse.exe", "unsecapp.exe", "spoolsv.exe",
		"trustedinstaller.exe", "tiworker.exe", "lsaiso.exe",
	} {
		if n == s {
			return "Windows 系统进程（系统不允许关闭）", true
		}
	}
	// 系统外壳：关掉会让桌面/任务栏消失，虽然能拉回来但属于明显干扰，交给用户明确勾选
	// （explorer 不在自动保护里，但前端会单独标「关闭后桌面会闪一下」）
	return "", false
}

// protectReasonAny 同时按「映像文件名」和「系统报的应用名」判定。
// 为什么要有这个（2026-10-15）：有些进程拿不到 exe 名（PID 4 只能报 "System"），
// 只查 exe 就会漏判 —— 它会被当成"能关的程序"摆给用户勾，勾了却关不掉。
func protectReasonAny(exe, appName string) (string, bool) {
	if w, ok := protectReason(exe); ok {
		return w, true
	}
	return protectReason(appName)
}

// ---------------------------------------------------------------------------
// 抽样：从待清理项里挑出拿去问 Restart Manager 的路径
// ---------------------------------------------------------------------------

// collectProbePaths 收集「拿去问谁占着」的抽样路径。
// 全量注册会拖慢清理前的等待（有的项上万文件），抽样足够反映"是谁占着"。
//   - 目录型项才抽样，命令型项（recycle / vss / dism …）没有具体文件可问
//   - 每项最多 perItem 条，总数封顶 total 条
func collectProbePaths(ctx context.Context, items []JunkItem, perItem, total int) []string {
	var out []string
	seen := map[string]bool{}
	add := func(p string) {
		if len(out) >= total || p == "" || seen[p] {
			return
		}
		seen[p] = true
		out = append(out, p)
	}
	for i := range items {
		if len(out) >= total {
			break
		}
		it := items[i]
		if !isDirCleanItem(&it) {
			continue
		}
		dirs, files := resolveTargets(ctx, &it)
		got := 0
		for _, f := range files {
			add(f)
			got++
			if got >= perItem {
				break
			}
			if len(out) >= total {
				break
			}
		}
		// 目录里没直接拿到文件时，从目录里现取几个
		if got < perItem {
			for _, d := range dirs {
				for _, f := range topFilesUnder(d, perItem-got) {
					add(f)
					got++
					if got >= perItem {
						break
					}
				}
				if got >= perItem {
					break
				}
			}
		}
	}
	return out
}

// topFilesUnder 取目录下的前 n 个文件（浅层即可，只为抽样）
func topFilesUnder(dir string, n int) []string {
	if n <= 0 {
		return nil
	}
	var out []string
	var walk func(d string, depth int)
	walk = func(d string, depth int) {
		if len(out) >= n || depth > 3 {
			return
		}
		es, err := os.ReadDir(d)
		if err != nil {
			return
		}
		for _, e := range es {
			if len(out) >= n {
				return
			}
			p := filepath.Join(d, e.Name())
			if e.IsDir() {
				walk(p, depth+1)
				continue
			}
			out = append(out, p)
		}
	}
	walk(dir, 0)
	return out
}

// cleanPathList 去掉重复与空串，RM 对不存在/不可读路径会直接让整批失败
func cleanPathList(in []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(in))
	for _, p := range in {
		if p == "" || seen[p] {
			continue
		}
		if _, err := os.Stat(p); err != nil {
			continue
		}
		seen[p] = true
		out = append(out, p)
	}
	return out
}

// 并发保护：whoLocks 同一时刻只跑一次（锁本体与缓存都在 junk_api.go 的
// 占用者探测缓存那一块 —— 分两处定义会 redeclared，别再搬回来）
