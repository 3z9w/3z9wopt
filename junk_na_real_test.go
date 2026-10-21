package main

import (
	"os"
	"sort"
	"testing"
)

// TestRealNaCheck 真机实证：后端 naIDs 名单（「默认不勾选」）在界面上到底生效没有。
//
// 判据：界面 ui-prototype.html:3575 是 `na: !!it.empty, on: !it.empty` —— **不读后端 na**，
// 只看 empty（本机没扫到 / 扫到 0 字节）。所以只要这项扫出了体积，它就是打勾的。
//
// 只读：调 ScanJunkSync 真扫盘，**不清理任何东西**。
// 跑法：REALJUNK=1 go test -run TestRealNaCheck -v
func TestRealNaCheck(t *testing.T) {
	if os.Getenv("REALJUNK") != "1" {
		t.Skip("设 REALJUNK=1 才跑（会真扫盘，几秒到几十秒）")
	}

	list, total, files, ms := ScanJunkSync(nil, "")
	t.Logf("真机扫描完成：%d 项 / %.1f MB / %d 个文件 / %d ms", len(list), float64(total)/1048576, files, ms)

	type row struct {
		id, group string
		bytes     int64
		empty     bool
	}
	var naRows, normalRows []row
	for _, it := range list {
		r := row{it.ID, it.Group, it.Bytes, it.Empty}
		switch {
		case it.Na:
			naRows = append(naRows, r)
		case it.Bytes > 0:
			normalRows = append(normalRows, r)
		}
	}
	sort.Slice(normalRows, func(i, j int) bool { return normalRows[i].bytes > normalRows[j].bytes })

	t.Log("")
	t.Log("=== 后端标了「默认不勾」(Na=true) 的项：界面上到底是空框还是打勾？ ===")
	t.Log("（界面判据 on = !empty，empty = 本机没扫到东西 / 扫到 0 字节）")
	bad := 0
	for _, r := range naRows {
		on := !r.empty
		mark := "空框（符合预期）"
		if on {
			mark = "打勾（默认就会删！）"
			bad++
		}
		t.Logf("  %-12s %-14s %10.1f MB  empty=%-5v → %s", r.id, r.group, float64(r.bytes)/1048576, r.empty, mark)
	}
	t.Logf("  ---- Na 名单共 %d 项，其中 %d 项在界面上默认【打勾】", len(naRows), bad)

	t.Log("")
	t.Log("=== 对照组：没标 Na 的普通项（本来就该默认勾）===")
	for i, r := range normalRows {
		if i >= 6 {
			break
		}
		s := "空框"
		if !r.empty {
			s = "打勾"
		}
		t.Logf("  %-12s %-14s %10.1f MB  empty=%-5v → %s", r.id, r.group, float64(r.bytes)/1048576, r.empty, s)
	}
}
