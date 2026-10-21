package main

// ============================================================================
// 3z9wOpt v1.1 · 垃圾清理「算账」逻辑单测
//
// 为什么要单独测这些：本次修的三件事（路径级去重、口径分层、清理后实测残留）全是"数字"
// 逻辑，而它们最容易在改动中悄悄错掉 —— 错了用户看到的就是"总量虚高 / 清完没变化"。
// 本文件全部是纯逻辑或 t.TempDir() 里的临时文件，不碰真实磁盘、不删任何东西，随时可跑：
//
//	go test .
//
// ⚠️ 契约：TotalUniqueSplit() 的口径必须与前端 design/ui-prototype.html 里的 sumSplit()
//    完全一致（同路径只算一次 / 父目录覆盖子路径 / 同一 dup 组只取最大 / 无路径项直接累加）。
//    改这里就要同步改那边，两处任一改动都要跑一遍本文件。
// ============================================================================

import (
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/windows"
)

// junkItemWithPaths 造一个只带「路径 -> 体积」的项（等价于扫描后的 tsize）
func junkItemWithPaths(id string, heavy bool, tsize map[string]int64) JunkItem {
	return JunkItem{ID: id, Heavy: heavy, tsize: tsize}
}

// ---------- 一、路径级去重 ----------

// 父目录已计入 → 子路径不再算（deepjunk 的全盘文件与系统日志目录重叠的典型场景）
func TestTotalUniqueSplitParentCoversChild(t *testing.T) {
	items := []JunkItem{
		junkItemWithPaths("syslogs", false, map[string]int64{`c:\windows\logs`: 1000}),
		junkItemWithPaths("deepjunk", true, map[string]int64{
			`c:\windows\logs\cbs.log`: 400, // 落在上面那个目录里 → 必须被父目录吃掉
			`d:\x\a.tmp`:              500,
		}),
	}
	all, heavy, targets := TotalUniqueSplit(items, nil)
	if all != 1500 {
		t.Fatalf("去重后总量 = %d，期望 1500（1000 + 500，cbs.log 被父目录覆盖）", all)
	}
	if heavy != 500 {
		t.Fatalf("「你的文件/整包」= %d，期望 500（只有 d:\\x\\a.tmp 来自重型项）", heavy)
	}
	if targets != 2 {
		t.Fatalf("目标数 = %d，期望 2", targets)
	}
}

// 路径完全相同（两个项测到同一个目标）→ 只算一次
func TestTotalUniqueSplitSamePathOnce(t *testing.T) {
	items := []JunkItem{
		junkItemWithPaths("usertemp", false, map[string]int64{`c:\windows\temp`: 700}),
		junkItemWithPaths("systemp", false, map[string]int64{`c:\windows\temp`: 700}),
	}
	all, _, targets := TotalUniqueSplit(items, nil)
	if all != 700 {
		t.Fatalf("同路径被算了两次：总量 = %d，期望 700", all)
	}
	if targets != 1 {
		t.Fatalf("目标数 = %d，期望 1", targets)
	}
}

// 同一 dup 组（winsxstmp / dismreset）→ 只取组内最大者，且不参与路径去重
func TestTotalUniqueSplitDupGroupTakesLargest(t *testing.T) {
	a := junkItemWithPaths("winsxstmp", false, map[string]int64{`c:\windows\winsxs\temp`: 90})
	a.dup = "winsxs"
	a.Bytes = 90
	b := JunkItem{ID: "dismreset", dup: "winsxs", Bytes: 300}

	all, _, _ := TotalUniqueSplit([]JunkItem{a, b}, nil)
	if all != 300 {
		t.Fatalf("dup 组总量 = %d，期望 300（组内只取最大者，不是 90+300）", all)
	}
}

// dup 组的重型归属跟着"赢"的那一项走
func TestTotalUniqueSplitDupGroupHeavyAttribution(t *testing.T) {
	heavyWin := JunkItem{ID: "h", dup: "g", Bytes: 100, Heavy: true}
	lightLose := JunkItem{ID: "l", dup: "g", Bytes: 50}

	all, heavy, _ := TotalUniqueSplit([]JunkItem{heavyWin, lightLose}, nil)
	if all != 100 || heavy != 100 {
		t.Fatalf("all=%d heavy=%d，期望 100/100", all, heavy)
	}
}

// 只勾选一部分（sel）时，未勾选项必须完全不计入
func TestTotalUniqueSplitSelection(t *testing.T) {
	items := []JunkItem{
		junkItemWithPaths("a", false, map[string]int64{`c:\a`: 100}),
		junkItemWithPaths("b", true, map[string]int64{`c:\b`: 200}),
	}
	all, heavy, _ := TotalUniqueSplit(items, map[string]bool{"b": true})
	if all != 200 || heavy != 200 {
		t.Fatalf("只勾 b 时 all=%d heavy=%d，期望 200/200", all, heavy)
	}
}

// TotalUnique（老接口）必须与 TotalUniqueSplit 的总量一致
func TestTotalUniqueMatchesSplit(t *testing.T) {
	items := []JunkItem{
		junkItemWithPaths("a", false, map[string]int64{`c:\a`: 100}),
		junkItemWithPaths("b", true, map[string]int64{`c:\a\z`: 30, `c:\b`: 200}),
	}
	all, _, _ := TotalUniqueSplit(items, nil)
	total, _ := TotalUnique(items, nil)
	if total != all {
		t.Fatalf("TotalUnique=%d 与 TotalUniqueSplit=%d 不一致", total, all)
	}
}

// ---------- 二、给前端的 paths 下发 ----------

func TestSyncPathsSortedAndNil(t *testing.T) {
	it := JunkItem{tsize: map[string]int64{"b": 2, "a": 1, "c": 3}}
	it.syncPaths()
	if len(it.Paths) != 3 {
		t.Fatalf("Paths 条数 = %d，期望 3", len(it.Paths))
	}
	if it.Paths[0].Path != "a" || it.Paths[2].Path != "c" {
		t.Fatalf("Paths 未按路径排序：%v", it.Paths)
	}
	if it.Paths[1].Bytes != 2 {
		t.Fatalf("Paths 体积对应错了：%v", it.Paths)
	}

	var empty JunkItem
	empty.syncPaths()
	if empty.Paths != nil {
		t.Fatalf("没有目标时应为 nil（JSON 里省略），实际 %v", empty.Paths)
	}
}

// ---------- 三、junction / 软链接同项去重 ----------

func TestDedupeByTargetKeepsMissingPaths(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "a.tmp")
	if err := os.WriteFile(f, []byte("x"), 0644); err != nil {
		t.Fatalf("准备临时文件失败：%v", err)
	}
	missing := filepath.Join(dir, "missing.tmp")

	got := dedupeByTarget([]string{f, f, missing})
	if len(got) != 2 {
		t.Fatalf("去重后 = %v，期望 2 条（同路径去重、不存在的保留）", got)
	}
	if got[0] != f {
		t.Fatalf("保留的第一条 = %q，期望 %q", got[0], f)
	}
}

// ---------- 四、口径分层名单 ----------

func TestHeavyIDsExistInCatalog(t *testing.T) {
	known := map[string]bool{}
	for _, it := range junkItems() {
		known[it.ID] = true
	}
	for id := range heavyIDs {
		if !known[id] {
			t.Errorf("heavyIDs 里的 %q 在清单里不存在（拼错了？改了 ID？）", id)
		}
	}
	// 反向：这几个必须分层正确（改名单时若误改，这里会红）
	mustHeavy := []string{"deepjunk", "driverres", "wechatrecv", "qqrecv", "aimodels", "msocache", "recycle"}
	mustSafe := []string{"usertemp", "prefetch", "edgecache", "thumb"}
	byID := map[string]JunkItem{}
	for _, it := range junkItems() {
		byID[it.ID] = it
	}
	for _, id := range mustHeavy {
		it, ok := byID[id]
		if !ok {
			t.Errorf("清单里找不到 %s（ID 改了？）", id)
			continue
		}
		if !it.Heavy {
			t.Errorf("%s 应属于「你的文件 / 整包」那一层", id)
		}
	}
	for _, id := range mustSafe {
		it, ok := byID[id]
		if !ok {
			t.Errorf("清单里找不到 %s（ID 改了？）", id)
			continue
		}
		if it.Heavy {
			t.Errorf("%s 不该属于重型层", id)
		}
	}
}

// 自适应门槛：require 全不存在 → Empty=true（前端隐藏该行）
func TestApplyRequireEmpty(t *testing.T) {
	dir := t.TempDir()
	missing := filepath.Join(dir, "nope")

	it := JunkItem{require: []string{missing}}
	applyRequireEmpty(&it)
	if !it.Empty {
		t.Error("require 全不存在时应 Empty=true（未安装的软件不占位）")
	}

	it2 := JunkItem{require: []string{missing, dir}}
	applyRequireEmpty(&it2)
	if it2.Empty {
		t.Error("require 里只要有一个存在，就不该隐藏该行")
	}

	it3 := JunkItem{}
	applyRequireEmpty(&it3)
	if it3.Empty {
		t.Error("没有 require 门槛的项不该被隐藏")
	}
}

// ---------- 五、清理执行顺序与并发归类 ----------

func TestCleanExecutionOrderRanksByWeight(t *testing.T) {
	got := cleanExecutionOrder([]string{"iconrefresh", "dismreset", "deepjunk", "reg:mui"})
	want := []string{"reg:mui", "deepjunk", "dismreset", "iconrefresh"}
	if len(got) != len(want) {
		t.Fatalf("长度不对：%v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("执行顺序 = %v，期望 %v（未登记权重的命令项最先、刷新动作收尾）", got, want)
		}
	}
}

// 命令型项绝不能被当成「目录快删项」扔进并发阶段（v1.1.0 修的回归）
func TestIsDirCleanItem(t *testing.T) {
	if !isDirCleanItem(&JunkItem{ID: "usertemp"}) {
		t.Error("没有 Cmd 的目录项应算目录型")
	}
	for _, cmd := range []string{
		"recycle", "vss", "evtx", "winold", "cleanmgr", "brokenappx", "iconrefresh",
		"reg:mui", "reg:apppath", "reg:uninst", "reg:clsid", "reg:shared",
		"reg:runmru", "reg:typedpaths", "reg:opensave", "reg:officemru", "badlnk",
	} {
		if isDirCleanItem(&JunkItem{ID: "x", Cmd: cmd}) {
			t.Errorf("命令型项 %q 被判成了目录型（会被并发跑，抢盘/起 PowerShell）", cmd)
		}
	}
}

// ---------- 六、纯工具函数 ----------

func TestDedupeAndExcluded(t *testing.T) {
	got := dedupe([]string{"A", "a", "b"})
	if len(got) != 2 || got[0] != "A" || got[1] != "b" {
		t.Fatalf("dedupe 大小写不敏感去重失败：%v", got)
	}
	if !excluded(`C:\Users\x\Google\Chrome\Cache`, []string{`\google\`}) {
		t.Error("excluded 未命中排除子串")
	}
	if excluded(`C:\Users\x\Brave\Cache`, []string{`\google\`}) {
		t.Error("excluded 误伤未排除路径")
	}
}

// ---------- 七、扫描顺序（2026-10-10 加） ----------
// 界面把 10 个小类合成 3 个大类显示，用户看到的顺序 = 后端扫描顺序 = 清理顺序。
// 后端原本按 junkItems() 内部顺序扫（不管前端传什么），所以这里把「按传入顺序」锁住。

func TestOrderByGroupsFollowsGivenOrder(t *testing.T) {
	list := []JunkItem{
		{ID: "a", Group: "深度清理", Seq: 1},
		{ID: "b", Group: "临时文件", Seq: 2},
		{ID: "c", Group: "浏览器缓存", Seq: 3},
		{ID: "d", Group: "临时文件", Seq: 4},
	}
	orderByGroups(list, []string{"浏览器缓存", "临时文件", "深度清理"})
	want := []string{"c", "b", "d", "a"} // 组内保持 Seq 顺序
	for i, id := range want {
		if list[i].ID != id {
			t.Fatalf("第 %d 项应为 %s，实际 %s（整体顺序：%+v）", i, id, list[i].ID, list)
		}
	}
}

func TestOrderByGroupsEmptyKeepsOriginal(t *testing.T) {
	list := []JunkItem{{ID: "x", Group: "A", Seq: 1}, {ID: "y", Group: "B", Seq: 2}}
	orderByGroups(list, nil)
	if list[0].ID != "x" || list[1].ID != "y" {
		t.Fatalf("groups 为空时不应改变顺序：%+v", list)
	}
	// 传入里没提到的组排在最后，但不打乱彼此
	list2 := []JunkItem{{ID: "p", Group: "A", Seq: 1}, {ID: "q", Group: "Z", Seq: 2}}
	orderByGroups(list2, []string{"Z"})
	if list2[0].ID != "q" || list2[1].ID != "p" {
		t.Fatalf("未登记的组应排在后面：%+v", list2)
	}
}

// ---------- 六、残留原因（2026-10-10：清完的差额必须有交代） ----------

// whyLeft 判的是"系统怎么回话"，不是我们猜的。三种确定情形必须分清：
//   · 文件正被占着（别人以 share=0 打开的）→ locked
//   · 文件好好的、谁都没占            → new（清完又被写出来的活缓存）
//   · 文件已经不在了                  → gone
// 注意：Go 的 os.OpenFile 自带 FILE_SHARE_READ|WRITE，占不住文件，
// 所以"造占用"必须也用 windows.CreateFile 走 share=0，否则这条用例会假通过。
func TestWhyLeftDetectsLocked(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "locked.tmp")
	if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := whyLeft(p); got != "new" {
		t.Fatalf("没人占用时应为 new（说明文件是活的缓存），实际 %s", got)
	}
	ptr, err := windows.UTF16PtrFromString(p)
	if err != nil {
		t.Fatal(err)
	}
	h, err := windows.CreateFile(ptr, windows.GENERIC_READ, 0, nil,
		windows.OPEN_EXISTING, windows.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer windows.CloseHandle(h)
	if got := whyLeft(p); got != "locked" {
		t.Fatalf("文件被独占打开时应为 locked，实际 %s", got)
	}
}

func TestWhyLeftGone(t *testing.T) {
	p := filepath.Join(t.TempDir(), "nope.tmp")
	if got := whyLeft(p); got != "gone" {
		t.Fatalf("文件不存在时应为 gone，实际 %s", got)
	}
}

// classifyLeft：只给前 leftReasonMax 条定性，其余不试（残留上万条时不逐个开文件）
func TestClassifyLeftCapsAtLimit(t *testing.T) {
	dir := t.TempDir()
	var paths []PathBytes
	for i := 0; i < leftReasonMax+20; i++ {
		paths = append(paths, PathBytes{Path: filepath.Join(dir, "f") + string(rune('a'+i%26)), Bytes: 1})
	}
	out, _ := classifyLeft(paths, leftReasonMax)
	if len(out) > leftReasonMax {
		t.Fatalf("最多只该定性 %d 条，实际 %d", leftReasonMax, len(out))
	}
	if out2, _ := classifyLeft(nil, leftReasonMax); len(out2) != 0 {
		t.Fatal("没有残留时应返回空")
	}
}

// ---------- 七、"会打扰用户"的项必须由后端标记（前端不许按 ID 写死） ----------

func TestImpactMarksAreSet(t *testing.T) {
	byID := map[string]string{}
	for _, it := range junkItems() {
		if it.Impact != "" {
			byID[it.ID] = it.Impact
		}
	}
	// 2026-10-14：thumb **不再**标 explorer —— 它已经不结束资源管理器了
	// （改走普通清理链：当场删 + 排队到重启删，见 junk_items.go 该项注释）。
	// 这条断言反过来看：一旦有人又把杀 explorer 的逻辑加回来，这里就会红。
	if byID["thumb"] == "explorer" {
		t.Fatalf("thumb 不应再标 explorer（它已经不结束资源管理器了），实际 %q（清单：%+v）", byID["thumb"], byID)
	}
	// 标记值只能是这三个之一，写错前端就取不到文案
	for id, v := range byID {
		if v != "explorer" && v != "service" && v != "slow" {
			t.Fatalf("%s 的 impact=%q 不在 explorer/service/slow 之内", id, v)
		}
	}
}

// 回收站必须能问出真实大小（2026-10-10：以前界面永远显示「—」，清没清掉没人知道）。
// 这条用例在真机上跑会打出实际数字，扫出来的数应当与资源管理器里看到的对得上。
func TestRecycleBinTotalReadable(t *testing.T) {
	total, n := recycleBinTotal()
	if total < 0 || n < 0 {
		t.Fatalf("回收站查询结果不该是负数：bytes=%d items=%d", total, n)
	}
	t.Logf("真机回收站：%.1f MB / %d 个", float64(total)/1048576, n)
	// 有文件就必须有体积、有体积就必须有文件 —— 两者对不上说明接口用法错了
	if (total > 0) != (n > 0) {
		t.Fatalf("体积与数量自相矛盾：bytes=%d items=%d（多半是 SHQUERYRBINFO 的 cbSize 没填）", total, n)
	}
}
