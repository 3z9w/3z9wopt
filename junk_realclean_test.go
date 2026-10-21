package main

// ============================================================================
// 真机清理实测：拿到**清掉率**（实际释放 / 扫描到的量）
//
// 背景（2026-10-14 用户质疑）：我们一直用"扫描项比别人多"当能力证明，
// 但「扫到多少 ≠ 能删掉多少」。这个测试就是来补这块证据的：
//   扫描 → 清理 → 复扫，逐项对比 before / freed / after / failed。
//
// ⚠️ 破坏性：默认 skip，必须显式 REALCLEAN=1 才执行。
//    免得任何人随手 go test ./... 就把自己机器清一遍。
//
//	REALCLEAN=1 go test -run TestRealCleanAudit -v -timeout 30m .
//
// 结果除 stdout 外另写 %TEMP%\3z9wOpt_realclean.tsv，便于逐项对照。
// ============================================================================

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

// fmtSize 本测试自带的体积格式化（生产代码里没有这个函数，不为了打印去动它们）。
func fmtSize(b int64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := int64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(b)/float64(div), "KMGTPE"[exp])
}

// realCleanSkip 这一轮**不清理**的项。
// 判据两条：① 删了不可逆或会打断用户 ② 清了会破坏当前这次会话/系统可用性。
func realCleanSkip() map[string]bool {
	skip := map[string]bool{}
	// ① 不可逆 / 用户自己的东西（naIDs 本来就是"默认不勾"的）
	for k := range naIDs {
		skip[k] = true
	}
	// ② 会打断桌面、暂停服务、或者清掉正在运行的本程序自己的缓存
	for _, k := range []string{
		"workbuddy",   // 本程序（AI 助手）正在运行，清它的缓存可能打断当前这次会话
		// 2026-10-14 起 thumb 不再结束资源管理器（只当场删 + 排队到重启删），
		// 不再打断桌面 → 从跳过名单里放出来，参与实测清掉率。
		// 2026-10-14 起 wucache 清完会把服务启回来（原来的问题是清完不启，
		// 见 junk_clean.go 里 wucachedeep 的注释）→ 不再属于"破坏可用性"，放出实测。
		"recycle",     // 清空回收站不可逆，需用户单独点头
		"dismreset",   // 分钟级，且清完无法卸载已装更新
		"iconrefresh", // 不释放空间，只是刷新
	} {
		skip[k] = true
	}
	return skip
}

func TestRealCleanAudit(t *testing.T) {
	// DIAG_LOCKS=path1|path2 只读诊断优先于一切：查这些路径被谁占着，**不删任何东西**，
	// 也不需要 REALCLEAN 开关。用途：验证"某项清不掉是不是被占用"、以及"占用者是谁"——不许靠猜。
	if diag := os.Getenv("DIAG_LOCKS"); diag != "" {
		var paths []string
		for _, s := range strings.Split(diag, "|") {
			if s = strings.TrimSpace(s); s != "" {
				paths = append(paths, s)
			}
		}
		fmt.Printf("\n=== 占用诊断（只读，不删除）===\n")
		for _, p := range paths {
			// whyLeft 直接问系统：locked(被占) / denied(权限不够或系统保护) / gone / new(能打开=没人占)
			fmt.Printf("  %-62s whyLeft=%s\n", p, whyLeft(p))
		}
		lks, err := whoLocks(paths)
		if err != nil {
			fmt.Printf("  查询失败：%v\n", err)
		} else if len(lks) == 0 {
			fmt.Println("  没查到占用者（可能文件没人占，或 Restart Manager 查不到）")
		} else {
			for _, l := range lks {
				protect := ""
				if l.Protect {
					protect = "  [不允许关闭：" + l.Why + "]"
				}
				fmt.Printf("  → %-22s pid=%-8d 占住 %d 个文件%s\n", l.Name, l.Pid, l.Files, protect)
			}
		}
		return
	}

	if os.Getenv("REALCLEAN") != "1" {
		t.Skip("真机清理实测是破坏性的；设 REALCLEAN=1 才会执行")
	}

	skip := realCleanSkip()
	// 计数类（注册表/快捷方式/使用痕迹）不占磁盘空间，对"清掉率"没有意义，
	// 且要动注册表——这轮不参与实测。
	skipGroup := map[string]bool{"注册表垃圾": true, "快捷方式": true, "使用痕迹": true}

	// ---------- 1. 基线扫描 ----------
	before, _, _, _ := ScanJunkSync(nil, "deepjunk") // 跳过 deepjunk 全盘遍历，快
	beforeBytes := map[string]int64{}
	beforeFiles := map[string]int{}
	nameOf := map[string]string{}
	// 自我保护：自动发现的应用缓存若命中本程序 / 当前会话自身，整项跳过。
	// 铁律五：清出来的东西绝不能让机器（或这次会话）不能用。
	for i := range before {
		if before[i].ID != "appcacheauto" {
			continue
		}
		for _, p := range before[i].Paths {
			lp := strings.ToLower(p.Path)
			if strings.Contains(lp, "workbuddy") || strings.Contains(lp, "3z9wopt") {
				skip["appcacheauto"] = true
				fmt.Printf("自我保护：appcacheauto 命中本程序目录 %s → 整项跳过\n", p.Path)
				break
			}
		}
	}

	var ids []string
	for i := range before {
		it := &before[i]
		beforeBytes[it.ID] = it.Bytes
		beforeFiles[it.ID] = it.Files
		nameOf[it.ID] = it.Name
		if skip[it.ID] || skipGroup[it.Group] || it.Bytes <= 0 {
			continue
		}
		ids = append(ids, it.ID)
	}
	// ONLY=id1,id2 只清理指定项 —— 做单变量对照实验用。
	// 例：停掉 WSearch 服务后 ONLY=searchidx 跑一遍，验证"占用是不是唯一障碍"。
	if only := os.Getenv("ONLY"); only != "" {
		want := map[string]bool{}
		for _, s := range strings.Split(only, ",") {
			if s = strings.TrimSpace(s); s != "" {
				want[s] = true
			}
		}
		kept := ids[:0]
		for _, id := range ids {
			if want[id] {
				kept = append(kept, id)
			}
		}
		ids = kept
	}

	sort.Strings(ids)

	var scanTotal, planTotal int64
	for id, b := range beforeBytes {
		scanTotal += b
		if !skip[id] && !skipGroup[groupOf(before, id)] && b > 0 {
			planTotal += b
		}
	}

	fmt.Printf("\n=== 真机清理实测 %s ===\n", time.Now().Format("15:04:05"))
	fmt.Printf("扫描到（全项去重前合计）：%s\n", fmtSize(scanTotal))
	fmt.Printf("本轮计划清理：%d 项 / %s\n", len(ids), fmtSize(planTotal))
	fmt.Printf("跳过：不可逆 %d 项 + 会打断会话/服务 %d 类 + 计数类（注册表/快捷方式/痕迹）\n\n",
		len(naIDs), 7)

	// ---------- 2. 清理 ----------
	t0 := time.Now()
	a := &App{} // ctx 为 nil → a.emit 静默返回，不会因 runtime 未初始化而崩
	results := a.CleanJunkSafe(ids)
	cleanMs := time.Since(t0).Milliseconds()

	freedOf := map[string]int64{}
	failedOf := map[string]int{}
	rebootNOf := map[string]int{}
	rebootBOf := map[string]int64{}
	errOf := map[string]string{}
	var freedTotal int64
	var failedTotal, rebootNTotal int
	var rebootBTotal int64
	for _, r := range results {
		freedOf[r.ID] = r.Bytes
		failedOf[r.ID] = r.Failed
		rebootNOf[r.ID] = r.Reboot
		rebootBOf[r.ID] = r.RebootBytes
		if r.Err != "" {
			errOf[r.ID] = r.Err
		}
		freedTotal += r.Bytes
		failedTotal += r.Failed
		rebootNTotal += r.Reboot
		rebootBTotal += r.RebootBytes
	}

	// ---------- 3. 复扫（实测残留，不许用"选中量 − 已释放"估算）----------
	after, _, _, _ := ScanJunkSync(nil, "deepjunk")
	afterBytes := map[string]int64{}
	afterFiles := map[string]int{}
	for i := range after {
		afterBytes[after[i].ID] = after[i].Bytes
		afterFiles[after[i].ID] = after[i].Files
	}

	// ---------- 4. 出报告 ----------
	var leftTotal int64
	for _, id := range ids {
		leftTotal += afterBytes[id]
	}

	fmt.Printf("清理耗时：%d ms\n", cleanMs)
	fmt.Printf("当场释放：%s\n", fmtSize(freedTotal))
	fmt.Printf("重启后释放：%s（%d 个文件已排进系统重启删除队列，现在还没腾出空间）\n",
		fmtSize(rebootBTotal), rebootNTotal)
	fmt.Printf("复扫残留：%s（当场删不掉也没排上队的 %d 个文件）\n", fmtSize(leftTotal), failedTotal)
	if planTotal > 0 {
		rate := float64(freedTotal) / float64(planTotal) * 100
		rate2 := float64(freedTotal+rebootBTotal) / float64(planTotal) * 100
		fmt.Printf("★ 当场清掉率：%.1f%%　★ 含重启的真实清掉率：%.1f%%（分母=计划清理量 %s）\n",
			rate, rate2, fmtSize(planTotal))
	}
	fmt.Println()

	// 逐项明细（按释放量降序）
	type row struct {
		id, name       string
		before, freed  int64
		after          int64
		failed         int
		err            string
	}
	var rows []row
	for _, id := range ids {
		rows = append(rows, row{id, nameOf[id], beforeBytes[id], freedOf[id], afterBytes[id], failedOf[id], errOf[id]})
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].freed > rows[j].freed })

	var sb strings.Builder
	sb.WriteString("id\t名称\t扫描到\t释放\t复扫残留\t未删文件数\t错误\n")
	fmt.Println("逐项明细（Top 20）：")
	for i, r := range rows {
		if i < 20 {
			pct := ""
			if r.before > 0 {
				pct = fmt.Sprintf("  (%.0f%%)", float64(r.freed)/float64(r.before)*100)
			}
			fmt.Printf("  %-14s %-22s 扫 %-10s 释放 %-10s 残留 %-10s%s\n",
				r.id, r.name, fmtSize(r.before), fmtSize(r.freed), fmtSize(r.after), pct)
		}
		sb.WriteString(fmt.Sprintf("%s\t%s\t%d\t%d\t%d\t%d\t%s\n",
			r.id, r.name, r.before, r.freed, r.after, r.failed, r.err))
	}

	// 清得最不干净的项（释放 0 但扫到很多）—— 这些就是"扫得到却删不掉"的窟窿
	fmt.Println("\n扫得到但没清掉的（扫到 >1MB 且释放为 0）：")
	for _, r := range rows {
		if r.before > 1024*1024 && r.freed == 0 {
			why := r.err
			if why == "" && r.failed > 0 {
				why = fmt.Sprintf("%d 个文件被占用", r.failed)
			}
			if why == "" {
				why = "（未报原因）"
			}
			fmt.Printf("  %-14s %-22s 扫到 %-10s %s\n", r.id, r.name, fmtSize(r.before), why)
		}
	}

	out := filepath.Join(os.TempDir(), "3z9wOpt_realclean.tsv")
	if err := os.WriteFile(out, []byte(sb.String()), 0o644); err == nil {
		fmt.Printf("\n完整逐项明细：%s\n", out)
	}
}

// groupOf 从一次扫描结果里取某项所属分组（用于跳过计数类）。
func groupOf(items []JunkItem, id string) string {
	for i := range items {
		if items[i].ID == id {
			return items[i].Group
		}
	}
	return ""
}
