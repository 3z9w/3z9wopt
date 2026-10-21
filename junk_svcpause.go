package main

// ============================================================================
// 3z9wOpt v1.1 · 清理前「暂停占用服务」（删除链 L3，2026-10-14 新增）
//
// 【为什么只登记了一项】
// 真机单变量对照实验（见 .workbuddy/memory/2026-10-14.md）：
//   searchidx 停 WSearch   → 清掉率 11%  → 100%  ✅ 有效，收录
//   fontcache 停 FontCache → 清掉率 18%  → 1%    ❌ 无效，不收录
// 同一个"停服务"手段，一个天上一个地下。**所以这张表只能靠实测往里加，不许按
// "想当然这个服务应该占着它"来填**（MEMORY.md 铁律七：说有用之前先拿实验证伪）。
//
// 【安全红线】
// 1. 只停**当前正在跑**的服务；清理完只把原来在跑的拉起来。
//    用户自己停着的服务，我们不许顺手给他启动 —— 那是在改他机器的状态。
// 2. ControlService(STOP) 是**异步**的：发完控制码服务还在收尾，立刻删照样删不掉。
//    必须轮询到真正进入停止态（或超时）再动手，否则这个机制等于没做。
// 3. 停不下来就照常清，绝不因此中断清理。
// ============================================================================

import (
	"fmt"
	"os"
	"strings"
	"time"

	"golang.org/x/sys/windows"
)

// cleanPauseSvcs 清理项 → 清理期间需要暂停的服务。
// 收录判据：**必须有机上对照实验的清掉率数据**，没有数据的不许进。
var cleanPauseSvcs = map[string][]string{
	// 2026-10-14 实测：停 WSearch 后 Windows.edb 等索引文件可以整块删除，
	// 清掉率 11% → 100%（17.3 MB 全清，复扫残留 0）。同类工具不做这一步。
	"searchidx": {"WSearch"},
	// ⛔ fontcache **不收录**：同一天实测停 FontCache 服务后清掉率 18% → 1%，
	//    诊断显示 40 MB 集中在 3 个系统级占用的文件上，用户态无解。
	//    别再把它加回来，除非有新的实测数据。
}

// svcStopWait 停止服务并**等到它真的停下来**。
// ControlService 只是发个控制码就返回，服务收尾还要时间；不等就删 = 白停。
// 返回 true 表示确实停住了。
func svcStopWait(name string, timeout time.Duration) bool {
	_ = svcStop(name) // 已经停着的会返回错误，忽略
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if !svcRunning(name) {
			return true
		}
		time.Sleep(100 * time.Millisecond)
	}
	return !svcRunning(name)
}

// 服务停稳后还要等多久才能真的删得动？
// 2026-10-14 严格 ablation（同一台机、实验前都确认 WSearch 在跑）：
//     停完**立刻删**   → 清掉率 38.1%，9 个文件照样删不掉
//     停完**静置 3 秒**  → 失败 0 / 重启队列 0 / 残留只剩 4.2 MB
// 差别就在 ControlService(STOP) 返回 ≠ 进程把文件句柄放开。
// 但定死 3 秒太笨（用户在意清理速度），改成轮询探测：能独占打开就立刻动手。
const svcSettleMax = 5 * time.Second
const svcSettleProbe = 6 // 最多拿几个文件当探针

// pauseServicesFor 清理某项前暂停它对应的服务，返回「恢复函数」。
// 恢复函数必须被调用（defer restore()）—— 否则用户机器上会留下被我们停掉的服务。
func pauseServicesFor(id string, probe []string) (restore func()) {
	names := cleanPauseSvcs[id]
	restore = func() {}
	if len(names) == 0 {
		return restore
	}
	var toRestore []string
	cleanTrace(fmt.Sprintf("svc probe %s → 需要暂停的服务=%v", id, names))
	for _, n := range names {
		if !svcRunning(n) {
			cleanTrace(fmt.Sprintf("svc probe %s 未在运行，按用户自己的状态原样保留", n))
			continue // 用户自己停着的：不动它，恢复时也别去启动
		}
		if svcStopWait(n, 5*time.Second) {
			toRestore = append(toRestore, n)
			cleanTrace(fmt.Sprintf("svc pause %s（清理 %s 期间临时停止）", n, id))
			waitReleasable(probe)
		} else {
			cleanTrace(fmt.Sprintf("svc pause %s 超时未停稳，按原样继续清理 %s", n, id))
		}
	}
	if len(toRestore) == 0 {
		return restore
	}
	return func() {
		for _, n := range toRestore {
			/* 2026-10-14 踩坑：StartService 对「正在启动中 / 已经起来了」会直接抛
			   ERROR_SERVICE_ALREADY_RUNNING，而我们当时把它当成"恢复失败"写进日志。
			   实际上服务好好的，日志却在说失败 —— 假警报比真故障更害人（会误导排查）。
			   所以判据只有一条：**服务最终是不是在跑**，启动调用报什么错不重要。 */
			if svcStartWait(n, svcStartTimeout(n)) {
				cleanTrace(fmt.Sprintf("svc resume %s 已恢复", n))
				continue
			}
			/* 2026-10-15 修正：**「起得慢」不能被判成「起不回来」**。
			   本机 13:49 那轮日志留下了「svc resume WSearch 失败，需用户手动启动」，
			   可二十分钟后复查服务明明在跑 —— 是 10 秒的超时先下了结论。
			   索引服务要重新枚举目录才能进 RUNNING，几十秒是常态。
			   假警报比真故障更害人（会误导排查），所以第一轮超时后**再补一轮长等**，
			   这轮也判不出来，才值得写成"要用户手动启动"。 */
			if svcStartWait(n, 2*time.Minute) {
				cleanTrace(fmt.Sprintf("svc resume %s 已恢复（起得很慢，第二轮等待后才进入运行）", n))
				continue
			}
			// 真正起不回来要如实留痕：静默吞掉的话，用户机器会一直少一个服务，
			// 而他永远不会知道是我们干的。
			cleanTrace(fmt.Sprintf("svc resume %s 失败，服务现在仍未运行，需用户手动启动", n))
		}
	}
}

// svcStartTimeout 各服务"起来"要多久差着一个数量级，统一用短超时会误伤。
// 2026-10-15：WSearch 是本所示例 —— 实测远远超过 10 秒（见上面注释）。
// 没实测数据的服务一律走保守值，不许照搬 WSearch 的放宽。
func svcStartTimeout(name string) time.Duration {
	if strings.EqualFold(name, "WSearch") {
		return 90 * time.Second
	}
	return 20 * time.Second
}

// waitReleasable 等服务把文件句柄放开：拿几个目标文件当探针，能独占打开就返回。
// 判据来自系统自己的回话（share=0 打开成功/失败），不是猜时间。
// 没有可探测的文件时退回短等 1 秒 —— 总比一点不等强（立刻删实测会漏一大半）。
func waitReleasable(paths []string) {
	var probe []string
	for _, p := range paths {
		if p == "" {
			continue
		}
		if fi, err := os.Stat(p); err == nil && !fi.IsDir() {
			probe = append(probe, p)
			if len(probe) >= svcSettleProbe {
				break
			}
		}
	}
	if len(probe) == 0 {
		time.Sleep(time.Second)
		return
	}
	deadline := time.Now().Add(svcSettleMax)
	for time.Now().Before(deadline) {
		if allOpenable(probe) {
			return
		}
		time.Sleep(250 * time.Millisecond)
	}
}

// allOpenable 这批文件是不是都能以 share=0 独占打开（= 已经没人占着了）
func allOpenable(paths []string) bool {
	for _, p := range paths {
		ptr, err := windows.UTF16PtrFromString(p)
		if err != nil {
			continue
		}
		h, err := windows.CreateFile(ptr, windows.GENERIC_READ, 0, nil,
			windows.OPEN_EXISTING, windows.FILE_FLAG_BACKUP_SEMANTICS, 0)
		if err != nil {
			return false
		}
		windows.CloseHandle(h)
	}
	return true
}

// svcStartWait 启动服务并等到它真的进入 RUNNING。
// 启动本来就是异步的，且对"已经在跑/正在启动"会报错 —— 所以不看调用返回值，
// 只看最后状态是不是在跑（see 上方注释）。
func svcStartWait(name string, timeout time.Duration) bool {
	if svcRunning(name) {
		return true
	}
	_ = svcStart(name)
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if svcRunning(name) {
			return true
		}
		time.Sleep(150 * time.Millisecond)
	}
	return svcRunning(name)
}
