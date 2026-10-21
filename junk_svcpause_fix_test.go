package main

import (
	"testing"
	"time"
)

// 紧急：把 WSearch 拉回 Running，并打印每一步的真实报错（不许猜）
func TestResumeWSearch(t *testing.T) {
	t.Logf("起点：svcRunning=%v", svcRunning("WSearch"))
	for i := 0; i < 5; i++ {
		if svcRunning("WSearch") {
			t.Logf("✅ 已在运行（第 %d 轮）", i)
			return
		}
		err := svcStart("WSearch")
		t.Logf("第 %d 轮 svcStart err=%v", i, err)
		time.Sleep(2 * time.Second)
		if svcRunning("WSearch") {
			t.Logf("✅ 已恢复运行（第 %d 轮）", i)
			return
		}
	}
	t.Errorf("⛔ 5 轮都起不来 WSearch，需要人工处理：设置 → 服务 → Windows Search → 启动")
}
