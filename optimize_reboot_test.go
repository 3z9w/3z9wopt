package main

// 待重启清单单测（2026-09-21）
// 覆盖：记录条件（只记成功 + 需重启的项）、重复不累加、还原摘除、已重启自动清空。
// 注意：会临时接管 %LOCALAPPDATA%\3z9wOpt\pending_reboot.json —— 先备份、跑完还原，绝不动用户真实数据。

import (
	"os"
	"testing"
)

// rebootStateSandbox 备份并清空待重启状态文件，返回还原函数
func rebootStateSandbox(t *testing.T) func() {
	t.Helper()
	p := rebootStatePath()
	backup, err := os.ReadFile(p)
	had := err == nil
	_ = os.Remove(p)
	return func() {
		_ = os.Remove(p)
		if had {
			_ = os.WriteFile(p, backup, 0o644)
		}
	}
}

func TestRebootPendingLifecycle(t *testing.T) {
	defer rebootStateSandbox(t)()

	var rbID, safeID string
	for i := range optCatalog {
		if itemNeedsReboot(&optCatalog[i]) {
			if rbID == "" {
				rbID = optCatalog[i].ID
			}
		} else if safeID == "" {
			safeID = optCatalog[i].ID
		}
	}
	if rbID == "" || safeID == "" {
		t.Fatal("目录里找不到样本项（需重启项 / 即时生效项都要有）")
	}

	// 1) 即时生效的项不该进清单
	recordPendingReboot([]OptResult{{ID: safeID, OK: true}})
	if st := readPendingReboot(); st.N != 0 {
		t.Fatalf("即时生效项不该记入待重启清单，实际 N=%d", st.N)
	}

	// 2) 执行失败的项不该进清单（没写成功，自然不用重启）
	recordPendingReboot([]OptResult{{ID: rbID, OK: false}})
	if st := readPendingReboot(); st.N != 0 {
		t.Fatalf("失败项不该记入待重启清单，实际 N=%d", st.N)
	}

	// 3) 成功 + 需重启 → 记 1 项，且带开机时长与时间戳
	recordPendingReboot([]OptResult{{ID: rbID, OK: true}})
	st := readPendingReboot()
	if st.N != 1 || len(st.Items) != 1 {
		t.Fatalf("应为 1 项待重启，实际 N=%d items=%v", st.N, st.Items)
	}
	if st.Uptime <= 0 || st.At == "" {
		t.Fatalf("应记录开机时长与时间戳，实际 uptime=%d at=%q", st.Uptime, st.At)
	}

	// 4) 同项重复记录不累加（避免用户反复点应用把清单撑大）
	recordPendingReboot([]OptResult{{ID: rbID, OK: true}})
	if st = readPendingReboot(); st.N != 1 {
		t.Fatalf("重复记录不应累加，实际 N=%d", st.N)
	}

	// 5) 未重启时 GetPendingReboot 如实返回（提醒条要显示）
	if got := (&App{}).GetPendingReboot(); got.N != 1 {
		t.Fatalf("未重启时应返回 1 项，实际 N=%d", got.N)
	}

	// 6) 识别到已重启（开机时长回退）→ 自动清空并删除状态文件
	st = readPendingReboot()
	st.Uptime = systemUptimeMs() + 3600*1000 // 伪造：记录时的开机时长比现在大 → 中间重启过
	writePendingReboot(st)
	if got := (&App{}).GetPendingReboot(); got.N != 0 {
		t.Fatalf("识别到已重启应自动清空，实际 N=%d", got.N)
	}
	if _, err := os.Stat(rebootStatePath()); err == nil {
		t.Fatal("清空后状态文件应被删除")
	}

	// 7) 还原方向：把需重启的项还原回默认 → 从清单摘除，空了就删文件
	recordPendingReboot([]OptResult{{ID: rbID, OK: true}})
	clearPendingReboot([]OptResult{{ID: rbID, OK: true}})
	if st = readPendingReboot(); st.N != 0 {
		t.Fatalf("还原后应从清单摘除，实际 N=%d", st.N)
	}
	if _, err := os.Stat(rebootStatePath()); err == nil {
		t.Fatal("清单清空后状态文件应被删除")
	}
}

// 重启识别信号：BootId 变化要能判定为已重启（本机读不到 BootId 时跳过）
func TestRebootedSinceBootID(t *testing.T) {
	cur := systemBootID()
	if cur == "" {
		t.Skip("本机读不到 BootId，跳过该信号校验")
	}
	// 开机时长与现在一致（排除时长信号），只有 BootId 不同 → 判定为已重启
	st := RebootPending{N: 1, Uptime: systemUptimeMs(), BootID: "stale-" + cur}
	if !rebootedSince(st) {
		t.Fatal("BootId 变化应判定为已重启")
	}
	// BootId 一致且时长一致 → 未重启
	st.BootID = cur
	if rebootedSince(st) {
		t.Fatal("BootId 与开机时长都一致时不应判定为已重启")
	}
	// BootId 双方都为空 → 不能凭它判定（避免误清提醒）
	if rebootedSince(RebootPending{N: 1, Uptime: systemUptimeMs()}) {
		t.Fatal("BootId 缺失时不应凭该信号判定已重启")
	}
}
