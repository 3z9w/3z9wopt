package main

// 真实 MFT 扫描冒烟测试（需要管理员权限）
import (
	"fmt"
	"os"
	"testing"
	"time"
)

func TestMftScanRealX(t *testing.T) {
	if !isElevated() {
		t.Skip("需要管理员权限")
	}
	drive := "C:"
	if _, err := os.Stat(`C:\Windows`); err != nil {
		t.Skip("无 C 盘")
	}
	t0 := time.Now()
	idx, err := ScanSpaceMft(drive, func(p int) {})
	if err != nil {
		t.Fatalf("扫描失败: %v", err)
	}
	t.Logf("扫描耗时 %s：节点 %d，文件 %d，已用 %.1f GB",
		time.Since(t0).Round(time.Millisecond), len(idx.nodes), idx.total, float64(idx.bytes)/1e9)
	if len(idx.nodes) < 10000 {
		t.Fatalf("节点数异常少: %d", len(idx.nodes))
	}
	if idx.total < 5000 {
		t.Fatalf("文件数异常少: %d", idx.total)
	}
	rootKids := idx.SpaceChildren(frRoot)
	if len(rootKids) < 3 {
		t.Fatalf("根目录子项异常少: %d", len(rootKids))
	}
	foundWin := false
	for _, k := range rootKids {
		t.Logf("  根子项: %-20s %10s (files=%d)", k.Name, fmtSize_(k.Size), k.Files)
		if k.Name == "Windows" {
			foundWin = true
			if k.Size < 1<<30 {
				t.Fatalf("Windows 目录聚合大小异常: %d", k.Size)
			}
		}
	}
	if !foundWin {
		t.Fatalf("根目录未找到 Windows 目录")
	}
	// 大文件
	tops := idx.SpaceTopFiles(100<<20, 10)
	if len(tops) == 0 {
		t.Fatalf("未找到 >100MB 的文件")
	}
	t.Logf("最大文件: %s = %s", tops[0].Path, fmtSize_(tops[0].Size))
	if tops[0].Path == "" || tops[0].Path[:2] != "C:" {
		t.Fatalf("大文件路径异常: %q", tops[0].Path)
	}
	// 面包屑
	crumbs := idx.SpaceCrumbs(frRoot)
	if len(crumbs) != 1 {
		t.Fatalf("根面包屑异常: %d", len(crumbs))
	}
	t.Logf("PASS: %s", time.Since(t0).Round(time.Millisecond))
}

func fmtSize_(b int64) string {
	switch {
	case b >= 1<<30:
		return fmt.Sprintf("%0.1f GB", float64(b)/(1<<30))
	case b >= 1<<20:
		return fmt.Sprintf("%0.1f MB", float64(b)/(1<<20))
	default:
		return fmt.Sprintf("%d B", b)
	}
}
