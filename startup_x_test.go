package main

import (
	"testing"
)

// TestStartupListRealX 真实枚举三类启动项（只读，不做任何修改）
func TestStartupListRealX(t *testing.T) {
	a := &App{}
	items, err := a.ListStartup()
	if err != nil {
		t.Fatalf("ListStartup 失败: %v", err)
	}
	n := map[string]int{}
	adv := 0
	for _, it := range items {
		n[it.Kind]++
		if it.Advice == "disable" {
			adv++
			t.Logf("建议禁止 [%s] %s — %s", it.Kind, it.Name, it.Why)
		}
		if it.Kind == "run" && it.IconKey == "" && it.Enabled {
			t.Logf("注意：已启用但无图标 %s (cmd=%s)", it.Name, it.Cmd)
		}
	}
	t.Logf("启动项 %d / 服务项 %d / 计划任务 %d，共 %d，建议禁止 %d", n["run"], n["svc"], n["task"], len(items), adv)
	if n["run"] == 0 {
		t.Fatal("启动项枚举为空（异常）")
	}
}
