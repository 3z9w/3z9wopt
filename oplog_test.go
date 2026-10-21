package main

import "testing"

// 操作记录守卫：这是「改过什么」的唯一证据链，写不进、读不出、清不掉都算事故
func TestOpLogRoundTrip(t *testing.T) {
	t.Setenv("LOCALAPPDATA", t.TempDir()) // 别碰用户真实记录

	if got := (&App{}).GetOpLog(); len(got) != 0 {
		t.Fatalf("全新环境操作记录应为空，实际 %d 条", len(got))
	}

	appendOpLog(OpLogEntry{Kind: "optimize", Title: "应用 2 项系统优化", Items: []string{"A", "B"}, OK: 2, Reboot: true})
	appendOpLog(OpLogEntry{Kind: "startup", Title: "开机启动项：禁用「X」", Items: []string{"X"}, OK: 1})
	appendOpLog(OpLogEntry{Kind: "tool", Title: "执行工具「SFC」", Items: []string{"SFC"}, Fail: 1, Err: "boom"})

	list := (&App{}).GetOpLog()
	if len(list) != 3 {
		t.Fatalf("应有 3 条记录，实际 %d", len(list))
	}
	if list[0].Kind != "tool" {
		t.Errorf("最新的应排最前，实际首条是 %q", list[0].Kind)
	}
	if list[0].Ts == "" {
		t.Error("时间戳不能为空（用户要按时间回看）")
	}
	if list[2].Title != "应用 2 项系统优化" || !list[2].Reboot {
		t.Errorf("最早那条内容不对：%+v", list[2])
	}
	if list[0].Fail != 1 || list[0].Err != "boom" {
		t.Errorf("失败条目要记原因，实际 %+v", list[0])
	}

	if !(&App{}).ClearOpLog() {
		t.Error("清空失败")
	}
	if n := len((&App{}).GetOpLog()); n != 0 {
		t.Errorf("清空后应为空，实际 %d 条", n)
	}
}

// 上限守卫：只留最近 300 条，别把本机文件撑成几百 KB
func TestOpLogCap(t *testing.T) {
	t.Setenv("LOCALAPPDATA", t.TempDir())
	for i := 0; i < 320; i++ {
		appendOpLog(OpLogEntry{Kind: "tool", Title: "执行工具", OK: 1})
	}
	if n := len((&App{}).GetOpLog()); n != opLogMax {
		t.Errorf("记录上限应为 %d 条，实际 %d", opLogMax, n)
	}
}
