package main

// 2026-10-14：searchidx「停 WSearch 再删」到底有没有用？——严格 ablation（含静置变量）
//
// 前情（全部是机上一手数据，不许用记忆代替）：
//   ① 手工实验 2026-10-14：停 WSearch → 11% → 100%（当时没有严格的"实验前确认在跑"）
//   ② 全量复测 15:12：18%，日志显示 pause 没触发
//   ③ 单跑 15:17：100%，但日志显示「WSearch 未在运行」→ 100% 不是 pause 的功劳
//   ④ 严格 ablation：实验前确认在跑 → 停 → **立刻删** → 只有 38.1%，9 个文件仍删不掉
// ④ 与 ① 矛盾。唯一没控制的变量是「停服务到动手删之间有没有静置」——
// ControlService(STOP) 返回 ≠ 进程句柄全放开。本实验就补这一个变量。

import (
	"context"
	"os"
	"testing"
	"time"
)

func TestDiagSearchIdxAblation(t *testing.T) {
	if os.Getenv("ABLATION") == "" {
		t.Skip("破坏性实验：设 ABLATION=1 才跑（会删本机 Windows 搜索索引，系统会自动重建）")
	}
	// ⚠️ 关掉重启删除：本实验要量的是「当场能删掉多少」，
	// 排队成功会掩盖"停服务到底有没有让文件变得可删"这个问题。
	os.Setenv("NO_REBOOTDEL", "1")
	defer os.Unsetenv("NO_REBOOTDEL")

	settle := time.Duration(0)
	if os.Getenv("SETTLE") != "" {
		if d, err := time.ParseDuration(os.Getenv("SETTLE")); err == nil {
			settle = d
		}
	}

	if !svcStartWait("WSearch", 30*time.Second) {
		t.Fatal("前提不成立：起不来 WSearch")
	}
	t.Logf("[前提] WSearch 在跑 = %v", svcRunning("WSearch"))
	time.Sleep(1 * time.Second)

	items := junkItems()
	var it *JunkItem
	for i := range items {
		if items[i].ID == "searchidx" {
			it = &items[i]
			break
		}
	}
	if it == nil {
		t.Fatal("清单里没有 searchidx")
	}
	ctx := context.Background()
	scanOne(ctx, it)
	before := it.Bytes
	t.Logf("[扫描] %d 字节 / %d 个文件", before, it.Files)
	if before == 0 {
		t.Skip("现在没有可清的索引，实验跳过")
	}

	// 停服务（内部会轮询探测文件是否真的放开）
	var probe []string
	for _, pb := range it.Paths {
		probe = append(probe, pb.Path)
	}
	t0 := time.Now()
	restore := pauseServicesFor("searchidx", probe)
	t.Logf("[暂停] 停服务+等句柄放开耗时 %v", time.Since(t0).Round(time.Millisecond))
	if settle > 0 {
		time.Sleep(settle)
		t.Logf("[额外静置] %v", settle)
	}
	res := cleanDirItem(ctx, it)
	restore()

	t.Logf("[清理] 释放 %d 字节 / 失败 %d / 重启队列 %d / err=%q",
		res.Bytes, res.Failed, res.Reboot, res.Err)
	scanOne(ctx, it)
	t.Logf("[复扫] 残留 %d 字节 / %d 个文件", it.Bytes, it.Files)
	if before > 0 {
		t.Logf("★ 静置 %v 的清掉率 = %.1f%%", settle, float64(res.Bytes)/float64(before)*100)
	}

	// 收尾必须复原：起得慢，给足时间并如实报
	if !svcStartWait("WSearch", 60*time.Second) {
		t.Errorf("⛔ WSearch 没能恢复 —— 本程序不能给用户留一个停掉的服务")
	} else {
		t.Logf("[收尾] WSearch 已恢复运行")
	}
}
