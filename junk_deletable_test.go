package main

// 「删不删得动」实测的回归测试（2026-11-12 修两个口径 bug 时补）：
//   ① walkSizeFast 读不动的目录不许再静默按 0 计（Unread 计数上报）；
//   ② 目录型目标必须走进去逐文件实测，不许只试开目录本身就整项报「能删」；
//   ③ 实测覆盖不到的部分（预算用完 / 文件中途消失）按「能删」兜底 —— 铁律：
//      宁可让用户多勾一点，也不许凭空把他能清的空间扣掉。

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// mkProbeFiles 在 dir 下写 count 个固定 10 字节的文件，返回总字节数。
func mkProbeFiles(t *testing.T, dir string, count int) int64 {
	t.Helper()
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	var total int64
	for i := 0; i < count; i++ {
		p := filepath.Join(dir, "f"+string(rune('a'+i))+".tmp")
		if err := os.WriteFile(p, []byte("0123456789"), 0644); err != nil {
			t.Fatal(err)
		}
		total += 10
	}
	return total
}

func TestWalkSizeFastNormalTreeZeroUnread(t *testing.T) {
	base := t.TempDir()
	total := mkProbeFiles(t, filepath.Join(base, "sub"), 3)
	if err := os.WriteFile(filepath.Join(base, "top.tmp"), []byte("0123456789"), 0644); err != nil {
		t.Fatal(err)
	}
	total += 10
	b, f, un := walkSizeFast(context.Background(), base)
	if un != 0 {
		t.Fatalf("普通可读目录树不许记未量到: unread=%d", un)
	}
	if b != total || f != 4 {
		t.Fatalf("体积/文件数不对: bytes=%d want=%d files=%d want=4", b, total, f)
	}
}

func TestWalkSizeFastMissingRootNotUnread(t *testing.T) {
	// 目标本来就没落上（规则解析后软件卸载了之类）：不算「未量到」，就是没有
	b, f, un := walkSizeFast(context.Background(), filepath.Join(t.TempDir(), "nope"))
	if b != 0 || f != 0 || un != 0 {
		t.Fatalf("不存在的根应返回三个 0: %d %d %d", b, f, un)
	}
}

func TestProbeDeletableDirTargetFilesAccounted(t *testing.T) {
	// 🔴 这就是「452 MB 混进可清数字」的回归锚：目录型目标旧版只试开目录本身，
	//    Edge 运行中整目录 440 MB 全被计入「能删」。现在必须逐文件入账，
	//    未被占的临时文件应全部计入 Cleanable。
	dir := filepath.Join(t.TempDir(), "cache")
	total := mkProbeFiles(t, dir, 4)
	it := &JunkItem{ID: "t_dir", Bytes: total, Files: 4}
	it.targets = []string{dir}
	it.tsize = map[string]int64{strings.ToLower(dir): total}
	probeDeletable(context.Background(), it)
	if it.Cleanable != total {
		t.Fatalf("未被占的目录内容应全额计入能清: cleanable=%d want=%d", it.Cleanable, total)
	}
	if it.LockedBytes != 0 || it.DeniedBytes != 0 {
		t.Fatalf("无人占用的临时目录不该有被占/受保护: locked=%d denied=%d", it.LockedBytes, it.DeniedBytes)
	}
}

func TestProbeDeletableVanishedFileConservative(t *testing.T) {
	// 铁律回归锚：扫描后文件自己没了（walk 记了体积、实测时已不存在），
	// 差额按「能删」兜底 —— 不许凭空扣掉用户能清的空间。
	dir := filepath.Join(t.TempDir(), "cache")
	total := mkProbeFiles(t, dir, 3)
	it := &JunkItem{ID: "t_gone", Bytes: total, Files: 3}
	it.targets = []string{dir}
	it.tsize = map[string]int64{strings.ToLower(dir): total}
	if err := os.Remove(filepath.Join(dir, "fa.tmp")); err != nil {
		t.Fatal(err)
	}
	probeDeletable(context.Background(), it)
	accounted := it.Cleanable + it.LockedBytes + it.DeniedBytes
	if accounted != total {
		t.Fatalf("实测覆盖不到的差额应兜底计入能清: accounted=%d want=%d", accounted, total)
	}
}

func TestProbeDirTargetBudgetStops(t *testing.T) {
	// 预算用完必须停（防几万文件的目录把扫描拖死），停了之后由外层对账兜底。
	dir := filepath.Join(t.TempDir(), "big")
	mkProbeFiles(t, dir, 5)
	it := &JunkItem{ID: "t_budget"}
	budget := 2
	gone, goneBytes := probeDirTarget(context.Background(), it, dir, &budget)
	if gone != 0 || goneBytes != 0 {
		t.Fatalf("临时目录不该有 gone: %d %d", gone, goneBytes)
	}
	if it.Cleanable != 20 {
		t.Fatalf("预算 2 条只许实测 2 个文件(20B): cleanable=%d", it.Cleanable)
	}
	if budget != 0 {
		t.Fatalf("预算应耗尽: %d", budget)
	}
}
