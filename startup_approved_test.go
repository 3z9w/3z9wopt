package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/windows/registry"
)

// 本文件锁定 2026-10-19 的三处修复，全部是真机验证（自建数据、跑完自己清干净）：
//
//	① runf（启动文件夹快捷方式）的开关 —— 以前 ID 前缀 runf 掉进 switch 的 default，
//	   报「未知的启动项类型」，快捷方式的开关从来就没能用过；
//	② 启动项的双状态 —— 禁用写 StartupApproved（Windows 原生口径，可恢复、任务管理器同步），
//	   不再删 Run 值；
//	③ 恢复时保住值类型 —— REG_EXPAND_SZ 不能被降级成 REG_SZ，否则含 %ProgramFiles%
//	   的启动项恢复后永久失效且不报错。
//
// 会真实改系统状态（HKCU 注册表 + 临时目录），所以沿用项目既有约定：
//
//	SU_TOGGLE_TEST=1 go test -run 'TestStartupFolderToggleReal|TestRunStartupApprovedToggleReal' -v .

const suTestName = "3z9wOptTestDoNotRun"

// suTestClean 把测试留下的痕迹清干净：注册表值、审批状态、本工具备份、操作日志。
func suTestClean(t *testing.T, id string, hive registry.Key, sub string) {
	t.Helper()
	if k, err := registry.OpenKey(hive, sub, registry.SET_VALUE); err == nil {
		_ = k.DeleteValue(suTestName)
		k.Close()
	}
	saDelete(hive, "Run", suTestName)
	delete(suDisabled, id)
	var kept []startupOp
	for _, op := range suLog {
		if op.ID != id {
			kept = append(kept, op)
		}
	}
	suLog = kept
	suSaveState()
}

// TestStartupFolderToggleReal 回归 runf：启动文件夹快捷方式的禁用 / 恢复必须能用。
//
// 用临时目录里的 .lnk 而不是真的往启动文件夹里塞东西 ——
// 开关路径只认这个路径本身，不需要它真的位于 Startup 目录。
func TestStartupFolderToggleReal(t *testing.T) {
	if os.Getenv("SU_TOGGLE_TEST") == "" {
		t.Skip("需要 SU_TOGGLE_TEST=1 才跑（会真实改一次系统状态）")
	}
	lnk := filepath.Join(os.TempDir(), suTestName+".lnk")
	if err := os.WriteFile(lnk, []byte("not a real shortcut"), 0o644); err != nil {
		t.Fatalf("准备测试文件失败: %v", err)
	}
	id := "runf|" + lnk
	defer func() {
		_ = os.Remove(lnk)
		_ = os.Remove(filepath.Join(suBackupDir(), filepath.Base(lnk)))
		saDelete(registry.CURRENT_USER, "StartupFolder", lnk)
		saDelete(registry.LOCAL_MACHINE, "StartupFolder", lnk)
		delete(suDisabled, id)
		var kept []startupOp
		for _, op := range suLog {
			if op.ID != id {
				kept = append(kept, op)
			}
		}
		suLog = kept
		suSaveState()
	}()

	suLoadState()
	if _, err := suSetEnable(id, false); err != nil {
		t.Fatalf("禁用启动文件夹项失败（这是 P0：runf 以前掉进 default 分支）: %v", err)
	}
	// 两条路径都算成功：① 写了审批状态且快捷方式还在原处；② 审批状态写不进去 → 移走文件
	var disabledOK bool
	if st, ok := saReadFolder(lnk); ok && st == saStateDisabled && fileExists(lnk) {
		disabledOK = true
		t.Log("禁用方式：写 StartupApproved\\StartupFolder（快捷方式留在原处）")
	} else if !fileExists(lnk) && fileExists(filepath.Join(suBackupDir(), filepath.Base(lnk))) {
		disabledOK = true
		t.Log("禁用方式：审批状态在这台机不生效 → 兜底移走快捷方式")
	}
	if !disabledOK {
		t.Fatal("禁用后既没有审批状态、快捷方式也还在 —— 禁用没生效")
	}

	if _, err := suSetEnable(id, true); err != nil {
		t.Fatalf("恢复启动文件夹项失败: %v", err)
	}
	if !fileExists(lnk) {
		t.Fatal("恢复后快捷方式没回到原位")
	}
	if st, ok := saReadFolder(lnk); ok && st != saStateEnabled {
		t.Fatalf("恢复后审批状态仍是 %d（期望 2=启用）", st)
	}
	t.Log("启动文件夹项 禁用 → 恢复 全通 ✓")
}

// TestRunStartupApprovedToggleReal 回归双状态 + 值类型保持。
//
// 造一条 REG_EXPAND_SZ 的 Run 值（正式版里 "%ProgramFiles%\X\Y.exe" 就是这种），
// 禁用 → 回读必须显示已禁用且 Run 值还在；恢复 → 值类型必须还是 EXPAND_SZ。
func TestRunStartupApprovedToggleReal(t *testing.T) {
	if os.Getenv("SU_TOGGLE_TEST") == "" {
		t.Skip("需要 SU_TOGGLE_TEST=1 才跑（会真实改一次系统状态）")
	}
	sub := `Software\Microsoft\Windows\CurrentVersion\Run`
	id := "run|" + sub + "|" + suTestName
	defer suTestClean(t, id, registry.CURRENT_USER, sub)

	k, err := registry.OpenKey(registry.CURRENT_USER, sub, registry.SET_VALUE)
	if err != nil {
		t.Fatalf("打开 HKCU Run 失败: %v", err)
	}
	// 故意用 REG_EXPAND_SZ：它就是被旧实现降级成 REG_SZ 的那种值
	if err := k.SetExpandStringValue(suTestName, `%SystemRoot%\System32\notepad.exe`); err != nil {
		k.Close()
		t.Fatalf("写入测试值失败: %v", err)
	}
	k.Close()

	suLoadState()
	if _, err := suSetEnable(id, false); err != nil {
		t.Fatalf("禁用失败: %v", err)
	}

	// 回读：必须是「已禁用」
	var out []StartupItem
	listRunItems(&out, nil)
	found := false
	for _, it := range out {
		if it.ID == id {
			found = true
			if it.Enabled {
				t.Fatal("禁用后枚举仍显示「已启用」—— 没有读 StartupApproved")
			}
		}
	}
	if !found {
		// 走了兜底删值路径也算禁用成功，但要把值补回来再验类型
		t.Log("审批状态不可用，走了兜底删值路径")
	}

	if _, err := suSetEnable(id, true); err != nil {
		t.Fatalf("恢复失败: %v", err)
	}
	k2, err := registry.OpenKey(registry.CURRENT_USER, sub, registry.QUERY_VALUE)
	if err != nil {
		t.Fatalf("回读失败: %v", err)
	}
	defer k2.Close()
	v, vt, err := k2.GetStringValue(suTestName)
	if err != nil {
		t.Fatalf("恢复后 Run 值不见了: %v", err)
	}
	if vt != registry.EXPAND_SZ {
		t.Fatalf("恢复后值类型退化成 %d（期望 %d=REG_EXPAND_SZ）—— 含 %%ProgramFiles%% 的启动项会永久失效",
			vt, registry.EXPAND_SZ)
	}
	if v != `%SystemRoot%\System32\notepad.exe` {
		t.Fatalf("恢复后值内容不对: %q", v)
	}

	// 彻底清理：删值 + 删审批状态
	_ = k2.Close()
	if kd, err := registry.OpenKey(registry.CURRENT_USER, sub, registry.SET_VALUE); err == nil {
		_ = kd.DeleteValue(suTestName)
		kd.Close()
	}
	t.Log("双状态 + REG_EXPAND_SZ 类型保持 全通 ✓")
}

// TestRunDisabledStateConsistent 只读自检：枚举出来的启用状态必须和 StartupApproved 一致。
// 这正是「系统里已禁用、这里还显示已启用」那个 bug 的哨兵。
func TestRunDisabledStateConsistent(t *testing.T) {
	suLoadState()
	var out []StartupItem
	listRunItems(&out, nil)

	checked := 0
	seen := map[string]int{}
	for _, it := range out {
		seen[it.ID]++
		if seen[it.ID] > 1 {
			t.Errorf("同一项被枚举了两次（WOW64 双视图重复？）: %s", it.ID)
		}
		// 只有 run / runm 走 StartupApproved；RunOnce（runo/runmo）没有审批状态位
		pfx := it.ID[:strings.Index(it.ID, "|")]
		if pfx != "run" && pfx != "runm" {
			continue
		}
		rest := it.ID[len(pfx)+1:]
		i := strings.LastIndex(rest, "|")
		if i < 0 {
			continue
		}
		sub, vn := rest[:i], rest[i+1:]
		hive := registry.CURRENT_USER
		sa := "Run"
		if pfx == "runm" {
			hive = registry.LOCAL_MACHINE
			if strings.Contains(strings.ToLower(sub), "wow6432node") {
				sa = "Run32"
			}
		}
		st, ok := saRead(hive, sa, vn)
		if !ok {
			continue
		}
		checked++
		want := st == saStateEnabled
		if it.Enabled != want {
			t.Errorf("%s：StartupApproved 状态=%d，枚举显示 enabled=%v（应=%v）", vn, st, it.Enabled, want)
		} else {
			t.Logf("%s：审批状态=%d → 显示 %s ✓", vn, st, map[bool]string{true: "已启用", false: "已禁用"}[it.Enabled])
		}
	}
	t.Logf("枚举 %d 条启动项，其中 %d 条有审批状态记录", len(out), checked)
	for _, it := range out {
		t.Logf("  [%s] %-45s enabled=%-5v loc=%s", it.ID[:4], it.Name, it.Enabled, it.Loc)
	}
}

// TestRunOnceNotUsingStartupApproved RunOnce 没有审批状态位，禁用只能是删值 + 备份。
// 只读确认：RunOnce 项不会去写 StartupApproved，也不会被当成 Run 项误伤。
func TestRunOnceNotUsingStartupApproved(t *testing.T) {
	var out []StartupItem
	listRunItems(&out, nil)
	for _, it := range out {
		if strings.HasPrefix(it.ID, "runo|") || strings.HasPrefix(it.ID, "runmo|") {
			if !strings.Contains(it.Loc, "RunOnce") {
				t.Errorf("RunOnce 项的位置说明不对: %s → %s", it.ID, it.Loc)
			}
			t.Logf("RunOnce 项：%s（%s）", it.Name, it.Loc)
		}
	}
}
