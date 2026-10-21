package main

// ============================================================================
// 只读探针：扫描期「这个文件能不能删」的实测成本与结论分布
//
// 目的：判断能不能把 whyLeft（CreateFile share=0 试开一次）从「清理之后」提前到
//       「扫描阶段」——用实测顶替 blocked.json 黑名单 / renew 压制的记忆机制。
//       全量跑一遍要多久、结论分布如何，必须实测，不许估。
//
// 守门：SCANPROBE=1（不设就跳过，免得拖慢日常 go test）
// 只读：全程只 Stat + CreateFile 试开，**不删任何东西**。
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

type reasonAgg struct {
	n     int
	bytes int64
}

type itemCost struct {
	id    string
	n     int
	ms    int64
	bytes int64
}

// TestProbeDeletableFields 验证改完之后 JunkItem 上的实测字段是否自洽：
// Cleanable + Locked + Denied + 已消失的部分 应该正好等于扫到的 Bytes（不许凭空多也不许凭空少）。
func TestProbeDeletableFields(t *testing.T) {
	if os.Getenv("SCANPROBE") != "1" {
		t.Skip("守门未开：请设 SCANPROBE=1")
	}
	list, _, _, ms := ScanJunkSync(nil, "")
	fmt.Printf("\n=== 扫描 %d 项 / %dms，核对实测字段 ===\n", len(list), ms)

	type row struct {
		id     string
		bytes  int64
		can    int64
		locked int64
		denied int64
	}
	rows := []row{}
	var totalB, totalC, totalL, totalD int64
	bad := 0
	for i := range list {
		it := &list[i]
		if it.Bytes == 0 {
			continue
		}
		totalB += it.Bytes
		totalC += it.Cleanable
		totalL += it.LockedBytes
		totalD += it.DeniedBytes
		sum := it.Cleanable + it.LockedBytes + it.DeniedBytes
		if sum > it.Bytes {
			bad++
			fmt.Printf("  ❌ %s: 实测分类合计 %.1fMB > 扫到 %.1fMB\n",
				it.ID, float64(sum)/1048576, float64(it.Bytes)/1048576)
		}
		if it.DeniedBytes > 0 || it.LockedBytes > 0 {
			rows = append(rows, row{it.ID, it.Bytes, it.Cleanable, it.LockedBytes, it.DeniedBytes})
		}
	}
	fmt.Printf("\n全场: 扫到 %.1fMB = 实测能删 %.1fMB + 被占 %.1fMB + 受保护 %.1fMB + 扫描中已消失 %.1fMB\n",
		float64(totalB)/1048576, float64(totalC)/1048576, float64(totalL)/1048576, float64(totalD)/1048576,
		float64(totalB-totalC-totalL-totalD)/1048576)
	fmt.Printf("超出的项数（分类合计 > 扫到）: %d（必须为 0）\n", bad)

	fmt.Printf("\n=== 有「删不动」部分的项 ===\n")
	if len(rows) == 0 {
		fmt.Printf("  无 —— 本机这一轮扫描出来的东西全能删\n")
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].locked+rows[i].denied > rows[j].locked+rows[j].denied })
	for _, r := range rows {
		fmt.Printf("  %-14s 扫到 %8.1fMB  能删 %8.1fMB  被占 %6.1fMB  受保护 %6.1fMB\n",
			r.id, float64(r.bytes)/1048576, float64(r.can)/1048576,
			float64(r.locked)/1048576, float64(r.denied)/1048576)
	}
	if bad > 0 {
		t.Fatalf("有 %d 项的实测分类超过了扫到的体积", bad)
	}

	/* 守住用户那条最终口径：**任何一项都不许被"藏起来"** ——
	   黑名单（blocked.json）与压制（clean_hist.json）那一套已于 2026-10-15 整套移除，
	   扫描结果必须与清单项数一一对应；少一项就说明有人又把过滤机制加回来了。 */
	catalog := junkItems()
	if len(list) != len(catalog) {
		t.Fatalf("扫描结果被过滤：清单 %d 项，实际只列出 %d 项", len(catalog), len(list))
	}
	fmt.Printf("\n被过滤掉（藏起来）的项数: %d（必须是 0）\n", len(catalog)-len(list))
	fmt.Printf("✅ 全部 %d 项照常列出，没有任何一项被历史机制藏起来\n", len(list))
}

func TestProbeDeletableCost(t *testing.T) {
	if os.Getenv("SCANPROBE") != "1" {
		t.Skip("守门未开：请设 SCANPROBE=1")
	}

	list, _, _, ms := ScanJunkSync(nil, "")
	fmt.Printf("\n=== 真机扫描完成：%d 项，耗时 %dms ===\n", len(list), ms)

	var targets int
	agg := map[string]*reasonAgg{}
	costs := []itemCost{}

	t0 := time.Now()
	withTargets := 0
	for i := range list {
		it := &list[i]
		if len(it.targets) == 0 {
			continue // 纯命令项（回收站 / 还原点 / 事件日志）没有可枚举目标
		}
		withTargets++
		ti := time.Now()
		for _, p := range it.targets {
			targets++
			r := whyLeft(p)
			a, ok := agg[r]
			if !ok {
				a = &reasonAgg{}
				agg[r] = a
			}
			a.n++
			a.bytes += it.tsize[strings.ToLower(p)]
		}
		costs = append(costs, itemCost{id: it.ID, n: len(it.targets), ms: time.Since(ti).Milliseconds(), bytes: it.Bytes})
	}
	full := time.Since(t0).Milliseconds()

	fmt.Printf("\n=== 全量实测成本 ===\n")
	fmt.Printf("有可枚举目标的项: %d / %d\n", withTargets, len(list))
	fmt.Printf("目标总数: %d\n", targets)
	fmt.Printf("全量 whyLeft 总耗时: %d ms\n", full)
	if targets > 0 {
		fmt.Printf("平均每条: %.3f ms（1000 条约 %.1f 秒）\n",
			float64(full)/float64(targets), float64(full)/float64(targets))
	}

	fmt.Printf("\n=== 结论分布（Windows 自己回答的）===\n")
	order := []string{"new", "locked", "denied", "gone", "other"}
	var tot int64
	for _, a := range agg {
		tot += a.bytes
	}
	for _, r := range order {
		a, ok := agg[r]
		if !ok {
			continue
		}
		pct := 0.0
		if tot > 0 {
			pct = float64(a.bytes) / float64(tot) * 100
		}
		fmt.Printf("  %-7s %6d 条  %9.1f MB  (%.1f%%)\n", r, a.n, float64(a.bytes)/1048576, pct)
	}
	names := map[string]string{
		"new":    "能删（现在没被占着）",
		"locked": "被程序占着（关掉程序就能删）",
		"denied": "系统保护（谁都删不掉）",
		"gone":   "已经不在了",
		"other":  "没判出来",
	}
	for _, r := range order {
		if _, ok := agg[r]; ok {
			fmt.Printf("    %-7s = %s\n", r, names[r])
		}
	}

	fmt.Printf("\n=== 最耗时的 12 项（判断要不要限制每条项的目标数）===\n")
	sort.Slice(costs, func(i, j int) bool { return costs[i].ms > costs[j].ms })
	var sumMs int64
	for i, c := range costs {
		if i >= 12 {
			break
		}
		fmt.Printf("  %-14s 目标 %5d 条  耗时 %5d ms  %.1f MB\n", c.id, c.n, c.ms, float64(c.bytes)/1048576)
	}
	for _, c := range costs {
		sumMs += c.ms
	}

	// 抽样上限方案：每项最多试 8 条 → 省下来的时间够不够值得
	const probeMax = 8
	t1 := time.Now()
	sampled := 0
	for i := range list {
		it := &list[i]
		n := len(it.targets)
		if n == 0 {
			continue
		}
		if n > probeMax {
			n = probeMax
		}
		for k := 0; k < n; k++ {
			whyLeft(it.targets[k])
			sampled++
		}
	}
	smpMs := time.Since(t1).Milliseconds()
	fmt.Printf("\n=== 抽样方案（每项最多 %d 条）===\n", probeMax)
	fmt.Printf("抽样总数: %d 条，耗时 %d ms\n", sampled, smpMs)

	fmt.Printf("\n=== 结论提示 ===\n")
	fmt.Printf("全量 %dms vs 抽样 %dms；全量占一次扫描(约 %dms)的 %.1f%%\n",
		full, smpMs, ms, float64(full)/float64(ms)*100)
	fmt.Printf("（本探针只读，没有删除任何文件）\n")
}

/* TestDeepJunkSample 只读探针：把「全盘零散垃圾文件」这一项今天实际抓到哪些文件列出来。
   守门 DEEPSAMPLE=1。目的：这项删的是全盘 5 类扩展名的文件，光看定义说不清风险，
   要看真实路径才知道里面有没有用户自己的东西。绝不删任何东西。 */
func TestDeepJunkSample(t *testing.T) {
	if os.Getenv("DEEPSAMPLE") != "1" {
		t.Skip("探针未开：请设 DEEPSAMPLE=1")
	}
	list, _, _, ms := ScanJunkSync(nil, "")
	fmt.Printf("\n=== 扫描 %d 项 / %dms，取 deepjunk ===\n", len(list), ms)

	var total int64
	for _, it := range list {
		if it.ID != "deepjunk" {
			continue
		}
		m := map[string]struct{ n int; b int64 }{}
		for _, p := range it.Paths {
			ext := strings.ToLower(filepath.Ext(p.Path))
			e := m[ext]
			e.n++
			e.b += p.Bytes
			m[ext] = e
			total += p.Bytes
		}
		fmt.Printf("合计 %d 个文件 / %.1f MB\n\n=== 按扩展名 ===\n", len(it.Paths), float64(total)/1048576)
		type row struct {
			k string
			n int
			b int64
		}
		var rows []row
		for k, v := range m {
			rows = append(rows, row{k, v.n, v.b})
		}
		sort.Slice(rows, func(i, j int) bool { return rows[i].b > rows[j].b })
		for _, r := range rows {
			fmt.Printf("  %-8s %5d 个  %8.1f MB\n", r.k, r.n, float64(r.b)/1048576)
		}

		fmt.Printf("\n=== 体积最大的 15 个（真实路径）===\n")
		ps := append([]PathBytes{}, it.Paths...)
		sort.Slice(ps, func(i, j int) bool { return ps[i].Bytes > ps[j].Bytes })
		for i := 0; i < len(ps) && i < 15; i++ {
			fmt.Printf("  %7.1f MB  %s\n", float64(ps[i].Bytes)/1048576, ps[i].Path)
		}

		/* 有没有伸进系统 / 程序目录 —— 这是判断风险的关键：
		   清 \Windows\*.log 和清自己目录下的日志，性质完全不同。 */
		sd := strings.ToLower(filepath.Clean(systemDrive())) // 形如 c:\（末尾自带分隔符，别再拼一个）
		var sysN int
		var sysB int64
		perExt := map[string][]string{}
		for _, p := range it.Paths {
			lp := strings.ToLower(p.Path)
			ext := strings.ToLower(filepath.Ext(p.Path))
			if len(perExt[ext]) < 2 {
				perExt[ext] = append(perExt[ext], p.Path)
			}
			if strings.HasPrefix(lp, sd+`windows`) || strings.Contains(lp, `\program files`) ||
				strings.HasPrefix(lp, sd+`programdata`) {
				sysN++
				sysB += p.Bytes
			}
		}
		fmt.Printf("\n=== 落在系统 / 程序目录里的 ===\n")
		fmt.Printf("  %d 个 / %.1f MB（占合计 %.1f%%）\n",
			sysN, float64(sysB)/1048576, float64(sysB)/float64(total)*100)
		fmt.Printf("\n=== 每类扩展名取样 ===\n")
		for _, r := range rows {
			for _, s := range perExt[r.k] {
				fmt.Printf("  %-6s %s\n", r.k, s)
			}
		}
	}
}
