package main

// 待重启清单的共享入口测试。
// 注意：清单文件落在 %LOCALAPPDATA%\3z9wOpt\pending_reboot.json（用户的真实状态），
// 所以测试必须"先备份 → 断言 → 还原"，绝不能把测试数据留在用户机器上。

import (
	"os"
	"testing"
)

func TestAddPendingRebootShared(t *testing.T) {
	p := rebootStatePath()
	orig, err := os.ReadFile(p)
	had := err == nil
	defer func() {
		if had {
			_ = os.WriteFile(p, orig, 0o644)
		} else {
			_ = os.Remove(p)
		}
	}()

	// 清空后写入两项，再补一项重复的与一项新的
	_ = os.Remove(p)
	addPendingReboot([]string{"工具甲", "工具乙"})
	st := readPendingReboot()
	if st.N != 2 {
		t.Fatalf("应记录 2 项，实际 %d（%v）", st.N, st.Items)
	}
	addPendingReboot([]string{"工具乙", "工具丙"})
	st = readPendingReboot()
	if st.N != 3 {
		t.Fatalf("重复项应被去重，应记录 3 项，实际 %d（%v）", st.N, st.Items)
	}
	if st.At == "" || st.Uptime == 0 {
		t.Fatalf("记录时间/开机时长没写上：%+v", st)
	}
	// 空/空白名不应产生记录
	addPendingReboot([]string{"", "   "})
	if got := readPendingReboot(); got.N != 3 {
		t.Fatalf("空白项不应入清单，实际 %d", got.N)
	}

	// GetPendingReboot 与清单同源（前端提醒条读的就是它）
	got := (&App{}).GetPendingReboot()
	if got.N != 3 {
		t.Fatalf("GetPendingReboot 应返回 3 项，实际 %d", got.N)
	}
}

// 需重启的工具必须同时给出说明文案（前端确认弹窗与完成提示都用它）
func TestRebootToolsHaveMessage(t *testing.T) {
	buildCatalog()
	n := 0
	for _, it := range toolCatalog {
		if !it.Reboot {
			continue
		}
		n++
		if len(it.RebootMsg) < 8 {
			t.Fatalf("工具 %s 标了需重启，但没有写清楚「为什么要重启」的说明（RebootMsg）", it.ID)
		}
		// 危险操作 + 重启 同时出现时，必须已经做了二次确认
		if it.Risk == "caution" && !it.Danger {
			t.Fatalf("需重启的谨慎级工具 %s 必须带二次确认", it.ID)
		}
	}
	if n == 0 {
		t.Fatal("至少要有一个需重启的工具（net-reset / fix-update / fix-boot）")
	}
	t.Logf("需重启工具 %d 个", n)
}
