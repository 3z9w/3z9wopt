package main

// ============================================================================
// 扫描期「这些目标现在删不删得动」的实测
//
// 2026-10-15 用户拍板：不许再靠历史事件推断"这一项是不是清不掉"，而是**每次扫描重新问一遍系统**。
// 判据来自 Windows 自己，而且**与真正删除时用同一套动作**（junk_priv.go）：
// 以 DELETE 访问权 + 备份语义试开一次
//   - 拿到了句柄        → 现在删得动
//   - ERROR_SHARING_VIOLATION → 某个程序正占着（关掉它 / 重启就能删，**是可解的**）
//   - ERROR_ACCESS_DENIED     → 系统保护（TrustedInstaller 那类，谁都删不掉）
//
// ⚠️ 2026-10-15 换过判据：原来试的是"能不能独占打开"，结果 WinSxS\Temp 那一万个文件
//    全都"打得开" → 被判成 2.09 GB「能清」，真删的时候一个都删不动，扫描额度虚得离谱。
//    **判据必须和实际删除用的钥匙一致**，否则等于拿别的动作冒充这件事。
//
// 实测成本（本机真机跑出来的，见 junk_probe_cost_test.go）：
//   1285 个目标全量试开 = 554 ms，占整轮扫描(8840ms)的 6.3%，开销可以接受，所以**不抽样**。
//
// 🔴 铁律：体积只按 tsize 逐条精确累加，**不许按抽样比例估算**。判不出来时保守处理
//   （按"能删"计入）—— 宁可让用户多勾一点，也不许凭空把他能清的空间扣掉。
// ============================================================================

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// deletableProbeCap 单项**实测条目数**预算（文件条 + 目录里走进去的文件条一起算）。
// 理由：异常机器上目录型项可能装着几万个文件，全量试开会把扫描拖慢到不能忍。
// 超了就老实承认"没测完"，未测部分按"能删"兜底 —— 不做比例估算（铁律见下）。
const deletableProbeCap = 6000

// probeDeletable 扫描收尾调用：把这一项的目标按「能不能删」分类，体积逐条精确累加。
// 结果只用于界面结算与提示，**不影响删除范围**（勾了就照删，见 CleanJunk）。
//
// 2026-11-12 修「452 MB 混进可清数字」：旧版对目录型目标只试开**目录本身** ——
// 目录句柄拿得到 DELETE ≠ 里面的文件删得动（目录删除走的是"先清空再删空壳"，
// 占用锁全在文件上）。实测 Edge 运行中 Default\Cache 整目录 440 MB 全被计入
// 「能删」，真删时一条 Access denied。现在目录型目标**走进去逐个文件实测**，
// 文件型目标照旧逐条试开。目录本身删不删得动不影响任何字节数（空壳 = 0 字节）。
func probeDeletable(ctx context.Context, it *JunkItem) {
	if it == nil {
		return
	}
	it.Cleanable, it.LockedBytes, it.LockedFiles = 0, 0, 0
	it.DeniedBytes, it.DeniedFiles = 0, 0
	// ⚠️ it.Unread 不在这里清零：那是 walkSizeFast 阶段累计的（scanOne 开头重置）。

	// 纯命令项（回收站 / 还原点 / 事件日志 / 注册表 / 快捷方式）没有可枚举目标，
	// 试开无从下手 —— 它们的释放量本来就是按磁盘空闲差倒推的，按"整项可清"报。
	if len(it.targets) == 0 {
		it.Cleanable = it.Bytes
		return
	}

	t0 := time.Now()
	budget := deletableProbeCap
	var gone int
	var goneBytes int64
	probedDirs := 0
	/* 删除特权要在扫描这会儿就拿到手：清理端同样会用它（removeNow），
	   两头不一致就会出现"扫描说能删、清理说没权限"自己打自己脸的结果。 */
	deletePrivilegesReady()
	for _, p := range it.targets {
		if ctx.Err() != nil || budget <= 0 {
			cleanTrace(fmt.Sprintf("deletable probe skip %s: 预算 %d 条用完或扫描已取消，剩余目标按全部可清报",
				it.ID, deletableProbeCap))
			break
		}
		fi, err := os.Stat(p)
		if err != nil {
			continue // 扫描过程里它自己没了：字节进下面的对账兜底
		}
		if fi.IsDir() {
			g, gb := probeDirTarget(ctx, it, p, &budget)
			gone += g
			goneBytes += gb
			probedDirs++
			continue
		}
		budget--
		b := it.tsize[strings.ToLower(p)] // tsize 的键是小写的，targets 存原始路径
		switch probeCanDelete(p) {
		case "new":
			it.Cleanable += b
		case "locked":
			it.LockedBytes += b
			it.LockedFiles++
		case "denied":
			it.DeniedBytes += b
			it.DeniedFiles++
		case "gone":
			gone++
			goneBytes += b
		default:
			// 判不出来就按能删算 —— 不许因为"我们没认出来"就扣掉用户的空间
			it.Cleanable += b
		}
	}
	/* 对账兜底（铁律：宁可让用户多勾一点，也不许凭空把他能清的空间扣掉）：
	   实测覆盖不到的部分（预算用完 / 目标在扫描后自己没了 / 目录被重新锁上），
	   剩余体积一律计入「能删」。已知 gone 的部分除外 —— 那部分文件已经不在了，
	   删了也腾不出地方。 */
	accounted := it.Cleanable + it.LockedBytes + it.DeniedBytes + goneBytes
	if accounted < it.Bytes {
		it.Cleanable += it.Bytes - accounted
	}

	ms := time.Since(t0).Milliseconds()
	if ms >= 200 || it.LockedBytes > 0 || it.DeniedBytes > 0 || gone > 0 || it.Unread > 0 {
		cleanTrace(fmt.Sprintf("deletable %s: 目标 %d 条(目录 %d) %dms 能删 %.1fMB 被占 %.1fMB(%d) 受保护 %.1fMB(%d) 已没了 %d 条(%.1fMB) 未量到 %d 处",
			it.ID, len(it.targets), probedDirs, ms,
			float64(it.Cleanable)/1048576,
			float64(it.LockedBytes)/1048576, it.LockedFiles,
			float64(it.DeniedBytes)/1048576, it.DeniedFiles,
			gone, float64(goneBytes)/1048576, it.Unread))
	}
}

// probeDirTarget 目录型目标实测：走进去逐个文件按「同一把钥匙」问 Windows。
// 返回 (gone 条数, gone 字节)。junction / 软链接子树不深入（与 walkSizeFast 同口径，
// 防同一份缓存计两次）。读不动的子树跳过 —— 那部分体积 walkSizeFast 已经按
// it.Unread 记了「未量到」，这里不许再当它不存在或假装删得动。
func probeDirTarget(ctx context.Context, it *JunkItem, root string, budget *int) (int, int64) {
	if isReparsePoint(root) {
		return 0, 0 // 与 walkSizeFast 一致：reparse 根 = 0 字节，无需实测
	}
	var gone int
	var goneBytes int64
	_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // 读不动：跳过（体积已在 walk 阶段按未量到记账）
		}
		if d.IsDir() {
			if p != root && isReparsePoint(p) {
				return fs.SkipDir
			}
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err() // 用户取消：不再消耗预算
		}
		if *budget <= 0 {
			return errProbeBudget
		}
		(*budget)--
		var sz int64
		if fi, e := d.Info(); e == nil {
			sz = fi.Size()
		}
		switch probeCanDelete(p) {
		case "locked":
			it.LockedBytes += sz
			it.LockedFiles++
		case "denied":
			it.DeniedBytes += sz
			it.DeniedFiles++
		case "gone":
			gone++
			goneBytes += sz
		default: // new / other：判不出来按能删（铁律）
			it.Cleanable += sz
		}
		return nil
	})
	return gone, goneBytes
}

// errProbeBudget 预算用完的哨兵错误：WalkDir 收到非 nil error 停走，外层对账兜底。
var errProbeBudget = fmt.Errorf("probe budget exhausted")
