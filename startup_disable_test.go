package main

/* 禁用 → 验证 → 恢复 的完整循环测试（2026-10-26）
   用户问："关闭掉之后，下次开机还会不会重启？"
   真重启测不了，但可以把"Windows 下次开机凭什么不启动它"这条证据链跑一遍：
     ① Run 值是否还在（在 = 没有粗暴删值，可恢复）
     ② StartupApproved 是否写成 03（03 = Windows 自己认的"已禁用"，任务管理器同款）
     ③ ListStartup 是否读回 enabled=false（界面与系统状态一致）
     ④ 恢复后是否全部还原
   样本怎么挑：不能写死某一个程序名 —— 写死了就变成"只有装了 Realtek 的那台机器
   能跑"，换台机器直接 skip，等于没测。这里按安全性排优先级自动挑：
     ① 已知无害的（Realtek 音频管理器这类，开关都不影响功能）
     ② 其它非系统项，但必须避开安全软件和输入法（禁了会出事）
   挑不到就 skip 并说明原因，不硬选一个可能有害的样本。 */

import (
	"fmt"
	"strings"
	"testing"

	"golang.org/x/sys/windows/registry"
)

func TestDisablePersist(t *testing.T) {
	a := &App{}
	items, err := a.ListStartup()
	if err != nil {
		t.Fatal(err)
	}
	/* 禁用这些会真出事（安全软件/输入法），测试绝不能拿它们当样本。
	   与 startup.go 的 reNeverAdvice 同一个思路，但这里更保守 —— 测试只需要
	   一个"关掉无所谓"的东西。 */
	unsafeWord := []string{
		"kaspersky", "mcafee", "norton", "avast", "avg", "defender", "security",
		"huorong", "360", "tencent", "qqpinyin", "baiduinput", "iflyime",
		"ctfmon", "inputmethod", "sogou", "driver", "synaptics", "touchpad",
	}
	// ① 白名单：已知开关都无副作用的
	safeWord := []string{"rthdvcpl", "realtek", "gameviewer"}
	var target StartupItem
	pickOne := func(words []string) StartupItem {
		for _, it := range items {
			if it.Kind != "run" || !it.Enabled || it.Sys {
				continue
			}
			l := strings.ToLower(it.Name + " " + it.Cmd)
			for _, w := range words {
				if strings.Contains(l, w) {
					return it
				}
			}
		}
		return StartupItem{}
	}
	target = pickOne(safeWord)
	if target.ID == "" {
		// ② 兜底：任取一个非系统项，但必须通过危险词过滤
		for _, it := range items {
			if it.Kind != "run" || !it.Enabled || it.Sys || it.ID == "" {
				continue
			}
			l := strings.ToLower(it.Name + " " + it.Cmd)
			bad := false
			for _, w := range unsafeWord {
				if strings.Contains(l, w) {
					bad = true
					break
				}
			}
			if !bad {
				target = it
				break
			}
		}
	}
	if target.ID == "" {
		t.Skip("本机找不到可安全试手动的 run 项（都带保护词或已禁用），跳过 —— 不影响其它测试")
	}
	fmt.Println("样本:", target.Name, "| ID:", target.ID)

	parts := strings.SplitN(target.ID, "|", 2)
	sub, vn, _ := suSplitKey(parts[1])
	hive := registry.CURRENT_USER
	if strings.HasPrefix(target.ID, "runm|") {
		hive = registry.LOCAL_MACHINE
	}
	readState := func() (string, string) {
		val, appr := "(空)", "(无记录)"
		if k, e := registry.OpenKey(hive, sub, registry.QUERY_VALUE); e == nil {
			if v, _, ge := k.GetStringValue(vn); ge == nil {
				val = v
			}
			k.Close()
		}
		saSub := "Run"
		if strings.Contains(strings.ToLower(sub), "wow6432node") {
			saSub = "Run32"
		}
		st, rd := saRead(hive, saSub, vn)
		if rd {
			appr = fmt.Sprintf("%02X", st)
		}
		return val, appr
	}

	fmt.Println("\n--- ① 禁用前 ---")
	v0, a0 := readState()
	fmt.Printf("  Run 值         : %s\n  StartupApproved: %s\n", v0, a0)

	fmt.Println("\n--- ② 执行禁用 ---")
	if _, err := a.SetStartupEnable(target.ID, false); err != nil {
		t.Fatalf("禁用失败: %v", err)
	}
	v1, a1 := readState()
	fmt.Printf("  Run 值         : %s\n  StartupApproved: %s\n", v1, a1)

	ok1 := v1 == v0 && v1 != "(空)"
	ok2 := a1 == "03"
	fmt.Printf("  ✓ Run 值保留（可恢复）: %v\n", ok1)
	fmt.Printf("  ✓ 审批状态=03（Windows 认的禁用）: %v\n", ok2)

	fmt.Println("\n--- ③ 重新读列表，界面状态是否跟着变 ---")
	var after []StartupItem
	if items2, e := a.ListStartupForce(); e == nil {
		after = items2
	}
	got := false
	for _, it := range after {
		if it.ID == target.ID {
			got = true
			fmt.Printf("  列表读回 enabled=%v（期望 false）\n", it.Enabled)
			if it.Enabled {
				t.Error("列表仍显示启用，界面与系统状态不一致")
			}
		}
	}
	if !got {
		fmt.Println("  该项已从列表消失（不应发生）")
	}

	fmt.Println("\n--- ④ 恢复 ---")
	if _, err := a.SetStartupEnable(target.ID, true); err != nil {
		t.Fatalf("恢复失败（需手工处理）: %v", err)
	}
	v2, a2 := readState()
	fmt.Printf("  Run 值         : %s\n  StartupApproved: %s\n", v2, a2)
	fmt.Printf("  ✓ 值与禁用前一致: %v\n", v2 == v0)
	fmt.Printf("  ✓ 审批状态已清回启用: %v\n", a2 != "03")

	if !ok1 || !ok2 {
		t.Error("禁用机制验证未通过")
	}
}
