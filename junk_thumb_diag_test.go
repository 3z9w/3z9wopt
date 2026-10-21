package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 缩略图/图标缓存到底能不能「不重启资源管理器就清掉」？（2026-10-14 用户提问）
//
// 只做只读探测（share=0 试着独占打开 + 查占用进程），**一个文件都不删** ——
// 真删了用户桌面图标会错乱，而实验目的是搞清"能不能"，不是清。
//
// 跑法：THUMBDIAG=1 go test -run TestDiagThumbLock -v -timeout 3m .
func TestDiagThumbLock(t *testing.T) {
	if os.Getenv("THUMBDIAG") != "1" {
		t.Skip("设 THUMBDIAG=1 才跑")
	}
	L := os.Getenv("LOCALAPPDATA")
	dir := filepath.Join(L, "Microsoft", "Windows", "Explorer")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("读不了 %s: %v", dir, err)
	}

	type row struct {
		name string
		size int64
		why  string
	}
	var rows []row
	for _, e := range entries {
		n := e.Name()
		if !strings.HasPrefix(n, "iconcache_") && !strings.HasPrefix(n, "thumbcache_") {
			continue
		}
		p := filepath.Join(dir, n)
		fi, _ := e.Info()
		var sz int64
		if fi != nil {
			sz = fi.Size()
		}
		rows = append(rows, row{n, sz, whyLeft(p)})
	}
	if len(rows) == 0 {
		t.Fatalf("没找到 iconcache_*/thumbcache_*，目录不对？%s", dir)
	}

	freeN, lockN, otherN := 0, 0, 0
	var freeBytes, lockBytes int64
	fmt.Println("文件                        体积        独占打开结果")
	for _, r := range rows {
		fmt.Printf("%-26s %10d  %s\n", r.name, r.size, r.why)
		switch r.why {
		case "gone", "new":
			freeN++
			freeBytes += r.size
		default:
			lockN++
			lockBytes += r.size
			otherN++
		}
	}
	fmt.Printf("\n能独占打开（=现在就能删）: %d 个 / %d 字节\n", freeN, freeBytes)
	fmt.Printf("打不开（被占着）        : %d 个 / %d 字节\n", lockN, lockBytes)

	// 谁占着
	var paths []string
	for _, r := range rows {
		if r.why == "locked" || r.why == "denied" {
			paths = append(paths, filepath.Join(dir, r.name))
		}
	}
	if len(paths) > 6 {
		paths = paths[:6]
	}
	if len(paths) > 0 {
		lks, err := whoLocks(paths)
		if err != nil {
			t.Logf("查占用者失败: %v", err)
		} else {
			fmt.Println("\n占用者：")
			for _, l := range lks {
				fmt.Printf("  %s (pid %d) 占 %d 个文件\n", l.Name, l.Pid, l.Files)
			}
			if len(lks) == 0 {
				fmt.Println("  （Restart Manager 查不到 —— 系统级句柄，用户态无解）")
			}
		}
	}

	// 结论：如果全部/大部分打不开，"不重启 explorer 立刻删"就不成立
	if lockN > 0 {
		t.Logf("结论：%d 个文件被占着 → 不结束 explorer 就删不掉，"+
			"只能走 MoveFileEx 排队到下次开机删（不黑屏，但当下不释放）", lockN)
	}
}
