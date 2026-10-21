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
	"fmt"
	"strings"
	"time"
)

// deletableProbeCap 单项目标数上限：超过就不再实测，整项按"都能删"报。
// 理由：异常机器上 deepjunk 这类项可能吐出几万条目标，全量试开会把扫描拖慢到不能忍。
// 不做估算 → 超了就老实承认"没测"，不许按前若干条的比例外推。
const deletableProbeCap = 6000

// probeDeletable 扫描收尾调用：把这一项的目标按「能不能删」分类，体积逐条精确累加。
// 结果只用于界面结算与提示，**不影响删除范围**（勾了就照删，见 CleanJunk）。
func probeDeletable(it *JunkItem) {
	if it == nil {
		return
	}
	it.Cleanable, it.LockedBytes, it.LockedFiles = 0, 0, 0
	it.DeniedBytes, it.DeniedFiles = 0, 0

	// 纯命令项（回收站 / 还原点 / 事件日志 / 注册表 / 快捷方式）没有可枚举目标，
	// 试开无从下手 —— 它们的释放量本来就是按磁盘空闲差倒推的，按"整项可清"报。
	if len(it.targets) == 0 {
		it.Cleanable = it.Bytes
		return
	}

	if len(it.targets) > deletableProbeCap {
		cleanTrace(fmt.Sprintf("deletable probe skip %s: 目标 %d 条超过上限 %d，本次不实测（按全部可清报）",
			it.ID, len(it.targets), deletableProbeCap))
		it.Cleanable = it.Bytes
		return
	}

	t0 := time.Now()
	var gone int
	/* 删除特权要在扫描这会儿就拿到手：清理端同样会用它（removeNow），
	   两头不一致就会出现"扫描说能删、清理说没权限"自己打自己脸的结果。 */
	deletePrivilegesReady()
	for _, p := range it.targets {
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
			// 扫描过程中它自己没了：这部分体积释放不出来，不计入能清的部分
			gone++
		default:
			// 判不出来就按能删算 —— 不许因为"我们没认出来"就扣掉用户的空间
			it.Cleanable += b
		}
	}

	ms := time.Since(t0).Milliseconds()
	if ms >= 200 || it.LockedBytes > 0 || it.DeniedBytes > 0 || gone > 0 {
		cleanTrace(fmt.Sprintf("deletable %s: %d 条 %dms 能删 %.1fMB 被占 %.1fMB(%d) 受保护 %.1fMB(%d) 已没了 %d 条",
			it.ID, len(it.targets), ms,
			float64(it.Cleanable)/1048576,
			float64(it.LockedBytes)/1048576, it.LockedFiles,
			float64(it.DeniedBytes)/1048576, it.DeniedFiles, gone))
	}
}
