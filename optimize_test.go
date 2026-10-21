package main

// 系统优化模块单测：目录完整性 / 数据解码 / 注册表写删检测闭环
// 测试只碰 HKCU\Software\3z9wOptV2Test 临时键，跑完即清理。

import (
	"context"
	"testing"

	"golang.org/x/sys/windows/registry"
)

func testCtx() context.Context { return context.Background() }

// TestOptDetectSmoke 对真实目录全量跑一遍检测（只读注册表）：不 panic、不卡死、状态有变化即可
func TestNeedsRebootRules(t *testing.T) {
	svc := &OptItem{ID: "x", Optimize: []OptAction{{Kind: "service", Name: "WSearch", Start: "4"}}}
	if !itemNeedsReboot(svc) {
		t.Fatal("service 动作应判为需重启")
	}
	bcd := &OptItem{ID: "y", Optimize: []OptAction{{Kind: "cmd", Cmd: "bcdedit /set nx optin"}}}
	if !itemNeedsReboot(bcd) {
		t.Fatal("bcdedit 应判为需重启")
	}
	svcReg := &OptItem{ID: "z", Optimize: []OptAction{{Kind: "regwrite", Key: `HKEY_LOCAL_MACHINE\SYSTEM\CurrentControlSet\Services\wuauserv`, Value: "Start"}}}
	if !itemNeedsReboot(svcReg) {
		t.Fatal("Services 注册表写入应判为需重启")
	}
	ui := &OptItem{ID: "w", Optimize: []OptAction{{Kind: "regwrite", Key: `HKEY_CURRENT_USER\SOFTWARE\Microsoft\Windows\CurrentVersion\Search`, Value: "SearchboxTaskbarMode"}}}
	if itemNeedsReboot(ui) {
		t.Fatal("HKCU 外观项不应判为需重启")
	}

	// ---- 2026-09-21 补全规则：内核 / 会话 / 设备防护 / 保留存储 / UAC 总开关 ----
	mustReboot := []struct{ name, key, val string }{
		{"内存管理(LargeSystemCache)", `HKEY_LOCAL_MACHINE\SYSTEM\CurrentControlSet\Control\Session Manager\Memory Management`, "LargeSystemCache"},
		{"内核缓解(MitigationOptions)", `HKLM\System\ControlSet001\Control\Session Manager\kernel`, "MitigationOptions"},
		{"设备防护(HVCI)", `HKEY_LOCAL_MACHINE\SYSTEM\CurrentControlSet\Control\DeviceGuard\Scenarios\HypervisorEnforcedCodeIntegrity`, "Enabled"},
		{"保留存储", `HKEY_LOCAL_MACHINE\SOFTWARE\Microsoft\Windows\CurrentVersion\ReserveManager`, "MiscPolicyInfo"},
		{"UAC 总开关(EnableLUA)", `HKEY_LOCAL_MACHINE\SOFTWARE\Microsoft\Windows\CurrentVersion\Policies\System`, "EnableLUA"},
		{"服务(ControlSet001 分支)", `HKLM\SYSTEM\ControlSet001\services\PcaSvc`, "Start"},
		{"Control 根级(进程共享阈值)", `HKLM\SYSTEM\CurrentControlSet\Control`, "SvcHostSplitThresholdInKB"},
	}
	for _, c := range mustReboot {
		it := &OptItem{ID: "r", Optimize: []OptAction{{Kind: "regwrite", Key: c.key, Value: c.val}}}
		if !itemNeedsReboot(it) {
			t.Fatalf("%s 应判为需重启", c.name)
		}
	}

	// ---- 明确即时生效、绝不能被误标为需重启 ----
	mustNotReboot := []struct{ name, key, val string }{
		{"应用策略(HKLM\\SOFTWARE\\Policies)", `HKLM\SOFTWARE\Policies\Microsoft\Windows\Windows Search`, "BingSearchEnabled"},
		{"电源相关注册表", `HKLM\SYSTEM\CurrentControlSet\Control\Power`, "HibernateEnabled"},
		{"资源管理器外观", `HKLM\SOFTWARE\Microsoft\Windows\CurrentVersion\Explorer`, "AlwaysUnloadDll"},
		{"远程协助", `HKLM\SYSTEM\CurrentControlSet\Control\Remote Assistance`, "fAllowToGetHelp"},
	}
	for _, c := range mustNotReboot {
		it := &OptItem{ID: "n", Optimize: []OptAction{{Kind: "regwrite", Key: c.key, Value: c.val}}}
		if itemNeedsReboot(it) {
			t.Fatalf("%s 是即时生效的，不应判为需重启", c.name)
		}
	}
	// powercfg（切电源计划 / 关休眠）即时生效；bcdedit 仍需重启
	if itemNeedsReboot(&OptItem{ID: "p", Optimize: []OptAction{{Kind: "cmd", Cmd: "powercfg.exe -setactive 8c5e7fda-e8bf-4a96-9a85-a6e23a8c635c"}}}) {
		t.Fatal("powercfg 切电源计划是即时生效，不应判为需重启")
	}
	if itemNeedsReboot(&OptItem{ID: "h", Optimize: []OptAction{{Kind: "cmd", Cmd: "powercfg -hibernate off"}}}) {
		t.Fatal("powercfg 关闭休眠是即时生效，不应判为需重启")
	}

	// ---- 目录级抽查：判定必须覆盖到真实的重点项 ----
	byID := map[string]*OptItem{}
	for i := range optCatalog {
		byID[optCatalog[i].ID] = &optCatalog[i]
	}
	for id, want := range map[string]bool{
		"xingneng-26": true,  // 大系统缓存（内存管理）
		"xingneng-32": true,  // 优化处理器性能（PriorityControl）
		"xingneng-45": true,  // GPU 硬件加速（GraphicsDrivers）
		"safe-3":      true,  // UAC 总开关 EnableLUA
		"safe-9":      true,  // 关闭内存完整 HVCI
		"safe-10":     true,  // 关闭虚拟化安全性 VBS
		"xingneng-19": true,  // 禁用 SysMain（服务）
		"xingneng-29": false, // 启用高性能电源计划（powercfg -setactive 即时）
		"system-1":    false, // 关闭休眠（即时）
		"explorer-1":  false, // 隐藏任务栏搜索框（外观）
		"explorer-30": false, // 关闭资讯和兴趣（外观）
		"xingneng-40": false, // 优化搜索与小娜（应用策略）
	} {
		it := byID[id]
		if it == nil {
			t.Fatalf("目录里找不到 %s", id)
		}
		if got := itemNeedsReboot(it); got != want {
			t.Fatalf("%s(%s) 的需重启判定应为 %v，实际 %v", id, it.Name, want, got)
		}
	}
	// 覆盖率兜底：总数不能太少（历史上只判出 17 项，明显漏了内核/设备防护类）
	rb := 0
	for i := range optCatalog {
		if itemNeedsReboot(&optCatalog[i]) {
			rb++
		}
	}
	if rb < 28 {
		t.Fatalf("需重启项仅 %d 项，判定规则疑似退化（内核/服务/设备防护类应覆盖到 30 项上下）", rb)
	}
	t.Logf("需重启判定 OK：目录 %d 项中 %d 项需重启电脑后生效", len(optCatalog), rb)
}

func TestSysSnapshotSmoke(t *testing.T) {
	app := &App{}
	s := app.GetSysSnapshot()
	if s.TotalGB <= 0 {
		t.Fatalf("TotalGB 应为正, got %v", s.TotalGB)
	}
	if s.ProcN < 10 {
		t.Fatalf("进程数应大于 10, got %d", s.ProcN)
	}
	if s.Ts == "" {
		t.Fatal("快照时间戳不能为空")
	}
}

func TestOptDetectSmoke(t *testing.T) {
	optimized := 0
	for i := range optCatalog {
		if isOptimized(&optCatalog[i]) {
			optimized++
		}
	}
	t.Logf("全量检测 OK：%d/%d 项当前已是优化态", optimized, len(optCatalog))
}

const optTestKey = `Software\3z9wOptV2Test`

func TestOptCatalogIntegrity(t *testing.T) {
	total, cats, basic, deep := OptCatalogCheck()
	// 154 = 源目录 151 项 + optimize_extra.go 追加 3 项（2026-09-21 用户点名的常用设置）
	if total != 154 {
		t.Fatalf("目录项数应为 154，实际 %d", total)
	}
	if cats != 7 {
		t.Fatalf("分类数应为 7，实际 %d", cats)
	}
	if basic != 87 {
		t.Fatalf("基本优化预设应为 87，实际 %d", basic)
	}
	if deep != 127 {
		t.Fatalf("深度优化预设应为 127，实际 %d", deep)
	}
	// 每项都必须有检测字段和至少一个动作
	for _, it := range optCatalog {
		if it.CheckKey == "" {
			t.Fatalf("%s 缺少 CheckKey", it.ID)
		}
		if len(it.Optimize) == 0 {
			t.Fatalf("%s 没有 Optimize 动作", it.ID)
		}
	}
	order := optCategoryOrder()
	if len(order) != 7 {
		t.Fatalf("分类顺序应为 7 组，实际 %d", len(order))
	}
	t.Logf("目录 OK：154 项（源 151 + 追加 3）/ 7 组 %v / 基本 87 / 深度 127", order)
}

func TestDecodeRegData(t *testing.T) {
	if dw, _, _, err := decodeRegData("REG_DWORD", "5"); err != nil || dw != 5 {
		t.Fatalf("dword 解码失败 dw=%d err=%v", dw, err)
	}
	if _, _, bin, err := decodeRegData("REG_BINARY", "00000000"); err != nil || len(bin) != 4 {
		t.Fatalf("binary hex 解码失败 len=%d err=%v", len(bin), err)
	}
	_, _, bin, err := decodeRegData("REG_BINARY", "22,22,22,00,00,02")
	if err != nil || len(bin) != 6 || bin[0] != 0x22 || bin[5] != 0x02 {
		t.Fatalf("binary 逗号解码失败 bin=%v err=%v", bin, err)
	}
	if _, s, _, err := decodeRegData("REG_SZ", "hello"); err != nil || s != "hello" {
		t.Fatalf("sz 解码失败 s=%q err=%v", s, err)
	}
}

func TestRegWriteDetectRoundTrip(t *testing.T) {
	item := OptItem{
		ID: "test-1", CheckKey: `HKEY_CURRENT_USER\` + optTestKey,
		CheckValue: "TestDword", Optimized: "1",
		Optimize: []OptAction{{Kind: "regwrite", Key: `HKEY_CURRENT_USER\` + optTestKey, Value: "TestDword", Vtype: "REG_DWORD", Data: "1"}},
		Restore:  []OptAction{{Kind: "regdelete", Key: `HKEY_CURRENT_USER\` + optTestKey, Value: "TestDword"}},
	}
	// 初始（值不存在，Optimized 非空）→ 未优化
	_ = execRegDelete(item.Restore[0])
	if isOptimized(&item) {
		t.Fatal("值不存在时不应判定为已优化")
	}
	// 优化 → 已优化
	if err := execOptAction(testCtx(), item.Optimize[0]); err != nil {
		t.Fatalf("regwrite 失败: %v", err)
	}
	if !isOptimized(&item) {
		t.Fatal("写入后应判定为已优化")
	}
	// 还原 → 未优化
	if err := execOptAction(testCtx(), item.Restore[0]); err != nil {
		t.Fatalf("regdelete 失败: %v", err)
	}
	if isOptimized(&item) {
		t.Fatal("删除后应回到未优化")
	}
	// 清理临时键
	registry.DeleteKey(registry.CURRENT_USER, optTestKey)
	t.Log("regwrite → 检测 → regdelete → 检测 闭环 OK")
}

func TestOptimizedEmptyMeansValueAbsent(t *testing.T) {
	// Optimized 为空串 = 「该值不存在才算优化态」：值不存在 → true，值存在 → false
	item := OptItem{
		ID: "test-2", CheckKey: `HKEY_CURRENT_USER\` + optTestKey,
		CheckValue: "GhostValue", Optimized: "",
	}
	_ = execRegDelete(OptAction{Kind: "regdelete", Key: item.CheckKey, Value: item.CheckValue})
	if !isOptimized(&item) {
		t.Fatal("值不存在且 Optimized 为空时应判定已优化")
	}
	_ = execOptAction(testCtx(), OptAction{Kind: "regwrite", Key: item.CheckKey, Value: "GhostValue", Vtype: "REG_DWORD", Data: "7"})
	if isOptimized(&item) {
		t.Fatal("值存在时不应判定已优化")
	}
	registry.DeleteKey(registry.CURRENT_USER, optTestKey)
}
