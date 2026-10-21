package main

import (
	"fmt"
	"os"
	"testing"
	"time"
)

// 一键清理「点不动」的修复验证（2026-10-14）
//
// 取证：%TEMP%\3z9wOptV11_clean_trace.log 里 JunkWhoLocks 抽 112 条路径走
// Restart Manager，单次 5~9 秒；前端 15 秒超时把它判死，后端算完的结果没人接。
//
// 本测证明两件事：
//   ① 第一次真算要多久（这是用户在没预热时会干等的时长）
//   ② 同一组 id 第二次是命中缓存的（预热方案成立 → 点下去秒回）
//   ③ 两个调用方同时来不会排队重算（第二个等第一个的结果）
//
// 跑法：WHOLOCKS=1 go test -run TestDiagWhoLocksCache -v -timeout 5m .
func TestDiagWhoLocksCache(t *testing.T) {
	if os.Getenv("WHOLOCKS") != "1" {
		t.Skip("设 WHOLOCKS=1 才跑（要走 Restart Manager，慢）")
	}
	a := &App{}
	ids := []string{
		"usertemp", "systemp", "chromecache", "edgecache", "thumb",
		"recent", "prefetch", "syslogs", "svclogs", "dingtalk",
		"wechatcache", "appcacheauto", "fontcache", "searchidx",
	}
	lockProbeReset()

	t0 := time.Now()
	first := a.JunkWhoLocks(ids)
	d1 := time.Since(t0)
	t.Logf("① 首次真算：%v，查到 %d 个占用进程", d1.Round(time.Millisecond), len(first))

	t1 := time.Now()
	second := a.JunkWhoLocks(ids)
	d2 := time.Since(t1)
	t.Logf("② 同 id 再问一次：%v，查到 %d 个占用进程", d2.Round(time.Millisecond), len(second))

	if d2 > 200*time.Millisecond {
		t.Errorf("第二次没有命中缓存：%v（预热方案不成立，用户点下去还是要干等）", d2)
	}
	if len(second) != len(first) {
		t.Errorf("缓存返回的数量对不上：首次 %d / 缓存 %d", len(first), len(second))
	}

	// ③ 并发：两个调用方同时来，第二个应该等第一个的结果，而不是排队重算
	lockProbeReset()
	doneCh := make(chan time.Duration, 2)
	start := time.Now()
	for i := 0; i < 2; i++ {
		go func() {
			s := time.Now()
			a.JunkWhoLocks(ids)
			doneCh <- time.Since(s)
		}()
	}
	var ds []time.Duration
	for i := 0; i < 2; i++ {
		ds = append(ds, <-doneCh)
	}
	total := time.Since(start)
	t.Logf("③ 并发两次：各耗时 %v / %v，总共 %v",
		ds[0].Round(time.Millisecond), ds[1].Round(time.Millisecond), total.Round(time.Millisecond))
	// 排队重算的话总时长会接近 2 倍单次；等待方案应当接近 1 倍
	if d1 > 2*time.Second && total > d1*3/2 {
		t.Errorf("并发时仍在排队重算：单次 %v，两次共 %v", d1, total)
	}

	fmt.Printf("\n结论：首次 %v → 预热后 %v（快 %.0f 倍）\n",
		d1.Round(time.Millisecond), d2.Round(time.Millisecond),
		float64(d1)/float64(max64(int64(d2), int64(time.Millisecond))))
}

func max64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}
