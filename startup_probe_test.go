package main

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	ole "github.com/go-ole/go-ole"
	"github.com/go-ole/go-ole/oleutil"
)

// TestLnkTargetReal 真机 .lnk 解析（启动文件夹里有什么就测什么）。
// 目的：lnk 目标解析曾经整个依赖 PowerShell，在 PowerShell 组件缺失 / WSH 被禁的机器上
// 全部返回空 —— 快捷方式没有图标、「打开所在位置」和「复制命令行」都是空的。
// 现在按 [MS-SHLLINK] 直读二进制，这里锁定"只要快捷方式存在就必须解出本地路径"。
func TestLnkTargetReal(t *testing.T) {
	dirs := []string{
		filepath.Join(os.Getenv("APPDATA"), `Microsoft\Windows\Start Menu\Programs\Startup`),
		filepath.Join(os.Getenv("ProgramData"), `Microsoft\Windows\Start Menu\Programs\Startup`),
	}
	var lnks []string
	for _, d := range dirs {
		ents, err := os.ReadDir(d)
		if err != nil {
			continue
		}
		for _, en := range ents {
			if !en.IsDir() && strings.EqualFold(filepath.Ext(en.Name()), ".lnk") {
				lnks = append(lnks, filepath.Join(d, en.Name()))
			}
		}
	}
	if len(lnks) == 0 {
		t.Skip("本机启动文件夹没有快捷方式，跳过")
	}
	for _, p := range lnks {
		got := lnkTarget(p)
		t.Logf("%s -> %q", filepath.Base(p), got)
		if got != "" && !strings.Contains(got, `\`) && !strings.Contains(got, `/`) {
			t.Errorf("解析出的目标不像路径: %q", got)
		}
	}
}

// TestListTaskItemsReal 计划任务枚举。
// 目的：枚举曾经走 PowerShell（Get-ScheduledTask），PowerShell 坏了就静默返回 0 条，
// 界面显示「计划任务 0」—— 把"读不到"演成"本来没有"。现在解析 Tasks 目录 XML，
// 只要系统里有非 Microsoft 的任务就必须列出来。
func TestListTaskItemsReal(t *testing.T) {
	var out []StartupItem
	err := listTaskItems(&out)
	if err != nil {
		t.Fatalf("任务目录不可读（这类机器必须报出来，不能当 0 条）: %v", err)
	}
	t.Logf("枚举到 %d 条计划任务", len(out))
	bad := 0
	for _, it := range out {
		if it.Name == "" || it.ID == "" {
			t.Errorf("任务缺少名称或 ID: %+v", it)
		}
		if !strings.HasPrefix(it.ID, "task|") {
			t.Errorf("任务 ID 前缀不对: %s", it.ID)
		}
		if strings.Contains(it.ID, "\x00") || strings.Contains(it.Name, "\x00") {
			bad++
		}
	}
	if bad > 0 {
		t.Errorf("%d 条任务的名称里含 0x00 —— UTF-16 解码没做", bad)
	}
	// 中文任务名不能被解成乱码：含 0x00 已在上面拦了，这里再确认没有"楠岃瘉"式的替换符
	for _, it := range out {
		if strings.ContainsRune(it.Name, utf8ReplacementChar) {
			t.Errorf("任务名出现替换符（编码解错）: %s", it.Name)
		}
	}
}

const utf8ReplacementChar = '\uFFFD'

// TestTaskSetEnabledRead 只做只读验证：能连上 Task Scheduler 并读到任务状态。
// 不改任何系统状态（避免测试把用户机器的任务改掉）。
func TestTaskSetEnabledRead(t *testing.T) {
	var out []StartupItem
	if err := listTaskItems(&out); err != nil || len(out) == 0 {
		t.Skip("没有可验证的计划任务")
	}
	// 取一条，验证 COM 能按 (目录, 名称) 精确定位到它
	it := out[0]
	folder, name := taskSplit(strings.TrimPrefix(it.ID, "task|"))

	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	_ = ole.CoInitializeEx(0, ole.COINIT_MULTITHREADED)
	defer ole.CoUninitialize()

	unk, err := oleutil.CreateObject("Schedule.Service")
	if err != nil {
		t.Fatalf("创建 Schedule.Service 失败（Task Scheduler 未运行？）: %v", err)
	}
	defer unk.Release()
	svc, err := unk.QueryInterface(ole.IID_IDispatch)
	if err != nil {
		t.Fatalf("QI 失败: %v", err)
	}
	defer svc.Release()
	if _, err := oleutil.CallMethod(svc, "Connect"); err != nil {
		t.Fatalf("Connect 失败（服务可能被禁用）: %v", err)
	}
	fv, err := oleutil.CallMethod(svc, "GetFolder", folder)
	if err != nil {
		t.Fatalf("GetFolder(%s) 失败 —— XML 枚举与 COM 的路径口径对不上: %v", folder, err)
	}
	defer fv.Clear()
	tv, err := oleutil.CallMethod(fv.ToIDispatch(), "GetTask", name)
	if err != nil {
		t.Fatalf("GetTask(%s) 失败 —— XML 枚举与 COM 的路径口径对不上: %v", name, err)
	}
	defer tv.Clear()
	en, err := oleutil.GetProperty(tv.ToIDispatch(), "Enabled")
	if err != nil {
		t.Fatalf("读 Enabled 失败: %v", err)
	}
	t.Logf("COM 读到 %s%s Enabled=%v（XML 判定=%v）", folder, name, en.Value(), it.Enabled)
}

// TestTaskToggleReal 真实开关一次系统计划任务（禁用 → 读回 → 恢复启用）。
//
// ⚠️ 会真改系统状态，默认跳过，用 SU_TOGGLE_TEST=1 显式开启：
//
//	SU_TOGGLE_TEST=1 go test -run TestTaskToggleReal -v .
//
// 为什么必须真跑：计划任务的开关曾经走 PowerShell（坏机器上必然失败），
// 改成 COM 之后如果不实测，等于把「开关拨不动」的风险留在原地 —— 这正是用户最初报的 bug。
// 选的是第三方任务，且最后一定改回原状态。
func TestTaskToggleReal(t *testing.T) {
	if os.Getenv("SU_TOGGLE_TEST") == "" {
		t.Skip("需要 SU_TOGGLE_TEST=1 才跑（会真实改一次计划任务状态）")
	}
	var out []StartupItem
	if err := listTaskItems(&out); err != nil {
		t.Fatalf("枚举失败: %v", err)
	}
	var pick *StartupItem
	for i := range out {
		if out[i].Enabled && strings.HasPrefix(out[i].ID, `task|\`) &&
			!strings.Contains(out[i].ID, `\Microsoft\`) {
			pick = &out[i]
			break
		}
	}
	if pick == nil {
		t.Skip("没有可安全试改的第三方计划任务")
	}
	t.Logf("选中 %s（%s）", pick.Name, pick.ID)

	enabledNow := func() bool {
		var l []StartupItem
		if err := listTaskItems(&l); err != nil {
			t.Fatalf("回读枚举失败: %v", err)
		}
		for _, it := range l {
			if it.ID == pick.ID {
				return it.Enabled
			}
		}
		t.Fatalf("回读时找不到 %s（ID 口径不一致）", pick.ID)
		return false
	}

	if _, err := suSetEnable(pick.ID, false); err != nil {
		t.Fatalf("禁用失败: %v", err)
	}
	if enabledNow() {
		t.Fatal("禁用后回读仍是「已启用」—— 开关没生效")
	}
	t.Log("禁用生效 ✓")

	if _, err := suSetEnable(pick.ID, true); err != nil {
		t.Fatalf("恢复启用失败: %v", err)
	}
	if !enabledNow() {
		t.Fatal("恢复后回读仍是「已禁用」—— 恢复没生效")
	}
	t.Log("恢复启用生效 ✓（已还原到测试前状态）")
}
