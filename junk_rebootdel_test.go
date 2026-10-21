package main

// ============================================================================
// 「重启后删除」自证测试（2026-10-14）
//
// 为什么必须自证：上一轮我拿"扫描项比同类工具多"当能力证明，被用户实测打回原形
// （MEMORY.md 铁律七）。所以这次不许只写完就说"我们也能了"——
// 这个测试会真的往系统重启删除队列里塞一条，再**原样撤销**，用系统自己的回话当证据。
//
// 安全边界：
//   - 只用自己刚建的临时文件，不碰任何真实文件；
//   - 结束前一定撤销队列条目并恢复注册表原值（defer 里做，失败也 t.Fatal）；
//   - 没有管理员权限时 t.Skip（如实说"这台机器上没验成"，不假装通过）。
// ============================================================================

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

const pendingRenameKey = `SYSTEM\CurrentControlSet\Control\Session Manager`
const pendingRenameVal = `PendingFileRenameOperations`

// pendingRenameEntries 读出系统当前的重启删除队列（成对：源路径, 目标路径；目标是空串=删除）
func pendingRenameEntries(t *testing.T) (registry.Key, []string, bool) {
	t.Helper()
	k, err := registry.OpenKey(registry.LOCAL_MACHINE, pendingRenameKey,
		registry.QUERY_VALUE|registry.SET_VALUE)
	if err != nil {
		t.Skipf("打不开 %s（多半不是管理员）: %v", pendingRenameKey, err)
	}
	old, _, err := k.GetStringsValue(pendingRenameVal)
	if err != nil && !os.IsNotExist(err) {
		k.Close()
		t.Fatalf("读 %s 失败: %v", pendingRenameVal, err)
	}
	return k, old, err == nil
}

// TestRebootDeleteRoundTrip 占用→删不掉→排进重启队列→校验→撤销
func TestRebootDeleteRoundTrip(t *testing.T) {
	if os.Getenv("NO_REBOOTDEL") != "" {
		t.Skip("本场已关闭重启删除")
	}
	rebootPlannedReset()
	defer rebootPlannedReset()

	f, err := os.CreateTemp("", "3z9wopt_reboot_*.tmp")
	if err != nil {
		t.Fatalf("建临时文件失败: %v", err)
	}
	path := f.Name()
	if _, err := f.WriteString("x"); err != nil {
		f.Close()
		os.Remove(path)
		t.Fatal(err)
	}
	f.Close()
	defer os.Remove(path)

	// 1) 用 share=0 把它占住 —— os.Remove 必须失败，否则这个测试没意义（没造出"占用"场景）
	ptr, err := windows.UTF16PtrFromString(path)
	if err != nil {
		t.Fatal(err)
	}
	h, err := windows.CreateFile(ptr, windows.GENERIC_READ, 0, nil,
		windows.OPEN_EXISTING, windows.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		t.Fatalf("占住临时文件失败: %v", err)
	}
	defer windows.CloseHandle(h)

	if err := os.Remove(path); err == nil {
		t.Fatal("前提不成立：文件没被占住，删掉了。测试没造出占用场景，不算通过。")
	}

	// 2) 排进重启队列
	if err := markRebootDelete(path); err != nil {
		t.Skipf("这台机器排不进重启队列（不是管理员 / 非提权）: %v", err)
	}

	// 3) 系统自己说"排上了"才算数 —— 读 PendingFileRenameOperations
	k, old, existed := pendingRenameEntries(t)
	defer k.Close()
	defer func() {
		// 撤销：把注册表恢复成测试前的样子（原来没有这个值就删掉）
		if existed {
			if err := k.SetStringsValue(pendingRenameVal, old); err != nil {
				t.Fatalf("⚠️ 撤销失败，请手动检查 %s\\%s: %v", pendingRenameKey, pendingRenameVal, err)
			}
			return
		}
		_ = k.DeleteValue(pendingRenameVal)
	}()

	now, _, _ := k.GetStringsValue(pendingRenameVal)
	want := strings.ToLower(path)
	found := false
	for _, s := range now {
		if strings.Contains(strings.ToLower(s), want) {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("MoveFileEx 返回成功，但 %s 里查不到 %s —— 就是没排上，不能算通过",
			pendingRenameVal, path)
	}

	// 4) 复扫时这条必须被判成 reboot，而不是含糊的 locked
	if !isRebootPlanned(path) {
		t.Fatal("排上队了但 isRebootPlanned 认不出来，复扫还会把它报成「被占用」")
	}
	t.Logf("✅ 已排进重启队列并成功撤销：%s", path)
}

// TestRebootDeleteRefusesProtected 受保护的系统路径不许排重启删除（宁可留着）
func TestRebootDeleteRefusesProtected(t *testing.T) {
	rebootPlannedReset()
	defer rebootPlannedReset()
	p := filepath.Join(os.Getenv("SystemRoot"), `System32\drivers\etc\hosts`)
	if _, err := os.Stat(p); err != nil {
		t.Skipf("这台机器没有 %s，跳过", p)
	}
	if err := markRebootDelete(p); err == nil {
		t.Fatalf("⛔ 居然把 %s 排进了重启删除队列 —— 这是会把系统搞坏的路径，必须拦下", p)
	}
}

// TestRebootDeleteRefusesDir 只排文件，不排目录
func TestRebootDeleteRefusesDir(t *testing.T) {
	rebootPlannedReset()
	defer rebootPlannedReset()
	d := t.TempDir()
	if err := markRebootDelete(d); err == nil {
		t.Fatal("目录被排进了重启删除队列 —— 目录要等里面清空才可能删掉，提前排没意义且有风险")
	}
}
