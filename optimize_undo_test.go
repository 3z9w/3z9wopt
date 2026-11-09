package main

// 操作前快照 / 回滚单测。
// 只碰 HKCU\Software\3z9wOptV2Test 临时键；快照文件写到 t.TempDir()，不碰用户真实记录。

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

// useTempOptUndo 让单测走临时文件，跳过 ProgramData 的 ACL 收紧。
// 单测通常不是管理员，不能把临时目录改成仅管理员可访问。
func useTempOptUndo(t *testing.T) {
	t.Helper()
	p := filepath.Join(t.TempDir(), "optundo.json")
	oldPath, oldKey := optUndoPathOverride, optUndoHmacKeyForTest
	optUndoPathOverride = p
	optUndoHmacKeyForTest = []byte("0123456789abcdef0123456789abcdef")
	t.Cleanup(func() {
		optUndoPathOverride = oldPath
		optUndoHmacKeyForTest = oldKey
	})
}

func cleanupOptTestKeys() {
	_ = deleteRegKeyTree(registry.CURRENT_USER, optTestKey)
}

func testReadDword(t *testing.T, keyPath, name string) (uint32, bool) {
	t.Helper()
	root, sub, err := regRoot(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	k, err := registry.OpenKey(root, sub, registry.QUERY_VALUE)
	if err == registry.ErrNotExist {
		return 0, false
	}
	if err != nil {
		t.Fatal(err)
	}
	defer k.Close()
	v, _, err := k.GetIntegerValue(name)
	if err == registry.ErrNotExist {
		return 0, false
	}
	if err != nil {
		t.Fatal(err)
	}
	return uint32(v), true
}

func testReadString(t *testing.T, keyPath, name string) (string, bool) {
	t.Helper()
	root, sub, err := regRoot(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	k, err := registry.OpenKey(root, sub, registry.QUERY_VALUE)
	if err == registry.ErrNotExist {
		return "", false
	}
	if err != nil {
		t.Fatal(err)
	}
	defer k.Close()
	v, _, err := k.GetStringValue(name)
	if err == registry.ErrNotExist {
		return "", false
	}
	if err != nil {
		t.Fatal(err)
	}
	return v, true
}

// 原值 2 → 优化写成 0 → 还原必须回到 2，而不是静态 Restore 的 1 或删除。
func TestOptUndoRestoresOriginalValue(t *testing.T) {
	useTempOptUndo(t)
	cleanupOptTestKeys()
	defer cleanupOptTestKeys()

	key := `HKEY_CURRENT_USER\` + optTestKey
	if err := execRegWrite(OptAction{Kind: "regwrite", Key: key, Value: "UndoDword", Vtype: "REG_DWORD", Data: "2"}); err != nil {
		t.Fatal(err)
	}
	it := &OptItem{
		ID: "undo-value", Name: "undo value",
		Optimize: []OptAction{{Kind: "regwrite", Key: key, Value: "UndoDword", Vtype: "REG_DWORD", Data: "0"}},
		Restore:  []OptAction{{Kind: "regwrite", Key: key, Value: "UndoDword", Vtype: "REG_DWORD", Data: "1"}},
	}

	if res := applyOptItem(context.Background(), it); !res.OK {
		t.Fatalf("应用失败：%s", res.Err)
	}
	if v, ok := testReadDword(t, key, "UndoDword"); !ok || v != 0 {
		t.Fatalf("应用后应为 0，实际 %d exists=%v", v, ok)
	}
	if res := restoreOptItem(context.Background(), it); !res.OK {
		t.Fatalf("还原失败：%s", res.Err)
	}
	if v, ok := testReadDword(t, key, "UndoDword"); !ok || v != 2 {
		t.Fatalf("还原后应回到原值 2，实际 %d exists=%v", v, ok)
	}
	if _, ok, err := loadOptUndoEntry(it.ID); err != nil || ok {
		t.Fatalf("还原成功后快照应删除，ok=%v err=%v", ok, err)
	}
}

// 优化前值不存在：还原后必须重新变成不存在，不能留下 0。
func TestOptUndoRestoresAbsentValue(t *testing.T) {
	useTempOptUndo(t)
	cleanupOptTestKeys()
	defer cleanupOptTestKeys()

	key := `HKEY_CURRENT_USER\` + optTestKey
	_ = execRegDelete(OptAction{Kind: "regdelete", Key: key, Value: "UndoAbsent"})
	it := &OptItem{
		ID: "undo-absent", Name: "undo absent",
		Optimize: []OptAction{{Kind: "regwrite", Key: key, Value: "UndoAbsent", Vtype: "REG_DWORD", Data: "0"}},
	}
	if res := applyOptItem(context.Background(), it); !res.OK {
		t.Fatalf("应用失败：%s", res.Err)
	}
	if _, ok := testReadDword(t, key, "UndoAbsent"); !ok {
		t.Fatal("应用后值应存在")
	}
	if res := restoreOptItem(context.Background(), it); !res.OK {
		t.Fatalf("还原失败：%s", res.Err)
	}
	if _, ok := testReadDword(t, key, "UndoAbsent"); ok {
		t.Fatal("还原后值应重新变为不存在")
	}
	if _, err := registry.OpenKey(registry.CURRENT_USER, optTestKey, registry.QUERY_VALUE); err != registry.ErrNotExist {
		t.Fatalf("还原后本次创建的空键应被删除，err=%v", err)
	}
}

// regdelete Value 为空 = 删整键：走真实 applyOptItem → execRegDelete → restoreOptItem。
// 同时验证：优化后其它程序新写入的值在还原时不会被删掉。
func TestOptUndoRestoresDeletedKeyTree(t *testing.T) {
	if !isElevated() {
		t.Skip("整键还原会写回安全描述符，需要管理员权限")
	}
	useTempOptUndo(t)
	cleanupOptTestKeys()
	defer cleanupOptTestKeys()

	parent := optTestKey + `\UndoTree`
	key := `HKEY_CURRENT_USER\` + parent
	k, _, err := registry.CreateKey(registry.CURRENT_USER, parent, registry.SET_VALUE|registry.CREATE_SUB_KEY)
	if err != nil {
		t.Fatal(err)
	}
	if err := k.SetDWordValue("A", 7); err != nil {
		k.Close()
		t.Fatal(err)
	}
	sk, _, err := registry.CreateKey(k, "Child", registry.SET_VALUE)
	k.Close()
	if err != nil {
		t.Fatal(err)
	}
	if err := sk.SetStringValue("B", "hi"); err != nil {
		sk.Close()
		t.Fatal(err)
	}
	sk.Close()

	it := &OptItem{
		ID: "undo-keytree", Name: "undo keytree",
		Optimize: []OptAction{{Kind: "regdelete", Key: key, Value: ""}},
	}
	if res := applyOptItem(context.Background(), it); !res.OK {
		t.Fatalf("应用失败：%s", res.Err)
	}
	if _, err := registry.OpenKey(registry.CURRENT_USER, parent, registry.QUERY_VALUE); err != registry.ErrNotExist {
		t.Fatalf("应用后整键应被删除，err=%v", err)
	}
	// 模拟优化之后其它程序又写入了新数据。
	k, _, err = registry.CreateKey(registry.CURRENT_USER, parent, registry.SET_VALUE)
	if err != nil {
		t.Fatal(err)
	}
	if err := k.SetDWordValue("NewAfterApply", 9); err != nil {
		k.Close()
		t.Fatal(err)
	}
	k.Close()

	if res := restoreOptItem(context.Background(), it); !res.OK {
		t.Fatalf("还原失败：%s", res.Err)
	}
	if v, ok := testReadDword(t, key, "A"); !ok || v != 7 {
		t.Fatalf("子键值 A 应恢复为 7，实际 %d exists=%v", v, ok)
	}
	if v, ok := testReadString(t, key+`\Child`, "B"); !ok || v != "hi" {
		t.Fatalf("孙键值 B 应恢复为 hi，实际 %q exists=%v", v, ok)
	}
	if v, ok := testReadDword(t, key, "NewAfterApply"); !ok || v != 9 {
		t.Fatalf("优化后新写入的值不应被还原删除，实际 %d exists=%v", v, ok)
	}
	// 原键允许继承父键权限，还原后不能变成 protected DACL。
	k, err = registry.OpenKey(registry.CURRENT_USER, parent, registry.READ|regReadControl)
	if err != nil {
		t.Fatal(err)
	}
	sd, err := windows.GetSecurityInfo(windows.Handle(k), windows.SE_REGISTRY_KEY, windows.DACL_SECURITY_INFORMATION)
	k.Close()
	if err != nil {
		t.Fatal(err)
	}
	control, _, err := sd.Control()
	if err != nil {
		t.Fatal(err)
	}
	if control&windows.SE_DACL_PROTECTED != 0 {
		t.Fatal("原本允许继承的键，还原后不应变成受保护 DACL")
	}
}

// 一项里第一条已改、第二条失败：必须自动把第一条退回去，不能停在半路。
func TestOptUndoCompensatesPartialFailure(t *testing.T) {
	useTempOptUndo(t)
	cleanupOptTestKeys()
	defer cleanupOptTestKeys()

	key := `HKEY_CURRENT_USER\` + optTestKey
	if err := execRegWrite(OptAction{Kind: "regwrite", Key: key, Value: "UndoDword", Vtype: "REG_DWORD", Data: "2"}); err != nil {
		t.Fatal(err)
	}
	it := &OptItem{
		ID: "undo-fail", Name: "undo fail",
		Optimize: []OptAction{
			{Kind: "regwrite", Key: key, Value: "UndoDword", Vtype: "REG_DWORD", Data: "0"},
			{Kind: "service", Name: "3z9wOptNoSuchService", Start: "4"},
		},
	}
	res := applyOptItem(context.Background(), it)
	if res.OK {
		t.Fatal("第二条动作应失败")
	}
	if v, ok := testReadDword(t, key, "UndoDword"); !ok || v != 2 {
		t.Fatalf("失败后应自动回到原值 2，实际 %d exists=%v（err=%s）", v, ok, res.Err)
	}
	if _, ok, err := loadOptUndoEntry(it.ID); err != nil || ok {
		t.Fatalf("补偿成功后不应留下快照，ok=%v err=%v", ok, err)
	}
}

// 旧版本应用过的项没有快照：只能按默认值还原，但必须通过 Warn 明确说出来。
func TestOptRestoreWithoutSnapshotWarns(t *testing.T) {
	useTempOptUndo(t)
	cleanupOptTestKeys()
	defer cleanupOptTestKeys()

	key := `HKEY_CURRENT_USER\` + optTestKey
	if err := execRegWrite(OptAction{Kind: "regwrite", Key: key, Value: "UndoDword", Vtype: "REG_DWORD", Data: "2"}); err != nil {
		t.Fatal(err)
	}
	it := &OptItem{
		ID: "undo-legacy", Name: "undo legacy",
		Optimize: []OptAction{{Kind: "regwrite", Key: key, Value: "UndoDword", Vtype: "REG_DWORD", Data: "0"}},
		Restore:  []OptAction{{Kind: "regwrite", Key: key, Value: "UndoDword", Vtype: "REG_DWORD", Data: "1"}},
	}
	if err := execOptAction(context.Background(), it.Optimize[0]); err != nil {
		t.Fatal(err)
	}
	res := restoreOptItem(context.Background(), it)
	if !res.OK {
		t.Fatalf("静态还原应成功：%s", res.Err)
	}
	if res.Warn == "" {
		t.Fatal("没有操作前快照时必须给出 Warn，不能假装恢复了原状态")
	}
	if v, ok := testReadDword(t, key, "UndoDword"); !ok || v != 1 {
		t.Fatalf("静态还原应写默认值 1，实际 %d exists=%v", v, ok)
	}
}

// 源数据里有命令没有配还原命令（如 net stop SmsRouter）：不能假装完整还原，
// 但也不该把整项判失败；其他状态已还原，用 Warn 如实说明。
func TestOptUndoMissingCmdRestoreWarns(t *testing.T) {
	useTempOptUndo(t)

	it := &OptItem{
		ID: "undo-nocmd", Name: "nocmd",
		Optimize: []OptAction{{Kind: "cmd", Cmd: "net stop SomeSvc"}},
	}
	e := &optUndoEntry{
		ID: it.ID, Name: it.Name,
		Steps: []optUndoStep{{Kind: "cmd"}},
	}
	if _, err := saveOptUndoIfAbsent(it, e); err != nil {
		t.Fatal(err)
	}
	res := restoreOptItem(context.Background(), it)
	if !res.OK {
		t.Fatalf("只有无法还原的命令时不应判失败：%s", res.Err)
	}
	if res.Warn == "" {
		t.Fatal("缺还原命令时必须给出 Warn")
	}
	if _, ok, err := loadOptUndoEntry(e.ID); err != nil || ok {
		t.Fatalf("状态已还原后快照应删除，ok=%v err=%v", ok, err)
	}
}

func TestOptUndoJournalRoundTrip(t *testing.T) {
	useTempOptUndo(t)

	it := &OptItem{
		ID: "journal-1", Name: "journal",
		Optimize: []OptAction{{Kind: "regwrite", Key: `HKEY_CURRENT_USER\Software\X`, Value: "Y", Vtype: "REG_DWORD", Data: "1"}},
	}
	e := &optUndoEntry{
		ID: it.ID, Name: it.Name,
		Steps: []optUndoStep{{
			Kind: "regvalue", Key: `HKEY_CURRENT_USER\Software\X`, Value: "Y",
			RegExists: true, RegType: uint32(registry.DWORD), RegData: []byte{1, 0, 0, 0},
			KeyExisted: true,
		}},
	}
	added, err := saveOptUndoIfAbsent(it, e)
	if err != nil || !added {
		t.Fatalf("首次应新增：added=%v err=%v", added, err)
	}
	added, err = saveOptUndoIfAbsent(it, e)
	if err != nil || added {
		t.Fatalf("重复保存不应覆盖：added=%v err=%v", added, err)
	}
	got, ok, err := loadOptUndoEntry(e.ID)
	if err != nil || !ok || len(got.Steps) != 1 || got.Steps[0].Value != "Y" {
		t.Fatalf("读取快照失败：ok=%v err=%v got=%+v", ok, err, got)
	}
	if err := deleteOptUndo(e.ID); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := loadOptUndoEntry(e.ID); err != nil || ok {
		t.Fatalf("删除后不应再读到：ok=%v err=%v", ok, err)
	}
}

// 安全回归：cmd 还原命令必须来自内存里的 OptItem（内置目录），
// 不能来自任何快照数据。
func TestOptUndoCmdRestoreComesFromCatalog(t *testing.T) {
	it := &OptItem{
		ID: "cmd-src", Name: "cmd src",
		Optimize: []OptAction{{Kind: "cmd", Cmd: "attacker controlled"}},
		Restore:  []OptAction{{Kind: "cmd", Cmd: "trusted restore command"}},
	}
	act, ok := optUndoCmdRestore(it, 0)
	if !ok || act.Cmd != "trusted restore command" {
		t.Fatalf("cmd 还原命令必须来自内置目录，实际 ok=%v cmd=%q", ok, act.Cmd)
	}
}

// 安全回归：篡改快照里的注册表路径后，校验必须拒绝执行，且不能碰任何键。
func TestOptUndoRejectsTamperedRegistryPath(t *testing.T) {
	useTempOptUndo(t)
	cleanupOptTestKeys()
	defer cleanupOptTestKeys()

	key := `HKEY_CURRENT_USER\` + optTestKey
	if err := execRegWrite(OptAction{Kind: "regwrite", Key: key, Value: "UndoDword", Vtype: "REG_DWORD", Data: "0"}); err != nil {
		t.Fatal(err)
	}
	it := &OptItem{
		ID: "undo-tamper", Name: "undo tamper",
		Optimize: []OptAction{{Kind: "regwrite", Key: key, Value: "UndoDword", Vtype: "REG_DWORD", Data: "0"}},
	}
	tampered := &optUndoEntry{
		ID: it.ID, Name: it.Name,
		Steps: []optUndoStep{{
			Kind: "regvalue", Key: key + `\Evil`, Value: "UndoDword",
			RegExists: true, RegType: uint32(registry.DWORD), RegData: []byte{9, 0, 0, 0},
		}},
	}
	// 模拟攻击者直接写文件：绕过正常保存路径，确保校验层能拦住。
	if err := writeOptUndoFile(&optUndoFile{Version: optUndoVersion, Items: []*optUndoEntry{tampered}}); err != nil {
		t.Fatal(err)
	}
	res := restoreOptItem(context.Background(), it)
	if res.OK {
		t.Fatal("被篡改路径的快照必须被拒绝")
	}
	if v, ok := testReadDword(t, key, "UndoDword"); !ok || v != 0 {
		t.Fatalf("校验失败时不能改动原值，实际 %d exists=%v", v, ok)
	}
	if _, ok := testReadDword(t, key+`\Evil`, "UndoDword"); ok {
		t.Fatal("校验失败时不能写入篡改后的路径")
	}
}

// 安全回归：快照里的未知字段（例如旧漏洞形态的 cmd 字符串）必须直接拒绝。
func TestOptUndoRejectsUnknownJSONField(t *testing.T) {
	useTempOptUndo(t)
	p, err := optUndoPath()
	if err != nil {
		t.Fatal(err)
	}
	body := fmt.Sprintf(`{"version":%d,"items":[{"id":"x","name":"x","at":"","steps":[`, optUndoVersion) +
		`{"kind":"cmd","cmd":{"cmd":"calc.exe"}}]}]}`
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadOptUndoFile(); err == nil {
		t.Fatal("含未知字段的快照必须拒绝加载")
	}
}

// 安全回归：文件内容被改动但 HMAC 没重算时，必须拒绝加载。
func TestOptUndoRejectsTamperedHMAC(t *testing.T) {
	useTempOptUndo(t)
	p, err := optUndoPath()
	if err != nil {
		t.Fatal(err)
	}
	e := &optUndoEntry{
		ID: "hmac-1", Name: "hmac",
		Steps: []optUndoStep{{Kind: "cmd"}},
	}
	if err := writeOptUndoFile(&optUndoFile{Version: optUndoVersion, Items: []*optUndoEntry{e}}); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	tampered := bytes.Replace(b, []byte(`"name": "hmac"`), []byte(`"name": "evil"`), 1)
	if bytes.Equal(b, tampered) {
		t.Fatal("测试没有改到文件内容")
	}
	if err := os.WriteFile(p, tampered, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadOptUndoFile(); err == nil {
		t.Fatal("HMAC 不匹配时必须拒绝加载")
	}
}

func TestOptUndoCmdCoveredByService(t *testing.T) {
	it := &OptItem{Optimize: []OptAction{
		{Kind: "service", Name: "hpet", Start: "4"},
		{Kind: "cmd", Cmd: "net stop hpet"},
	}}
	if !optUndoCmdCoveredByService(it, 1) {
		t.Fatal("同一项里已有 hpet 服务快照时，net stop hpet 应被覆盖")
	}
	if optUndoCmdCoveredByService(it, 0) {
		t.Fatal("服务动作本身不应被判为被覆盖")
	}
	other := &OptItem{Optimize: []OptAction{{Kind: "cmd", Cmd: "net stop other"}}}
	if optUndoCmdCoveredByService(other, 0) {
		t.Fatal("没有对应服务快照的 net stop 不应被跳过")
	}
}

func TestServiceStateVerdict(t *testing.T) {
	cases := []struct {
		name      string
		state     uint32
		running   bool
		stable    bool
		isPending bool
	}{
		{"running", svcStateRunning, true, true, false},
		{"stopped", svcStateStopped, false, true, false},
		{"start pending", svcStateStartPending, false, false, true},
		{"stop pending", svcStateStopPending, false, false, true},
		{"continue pending", svcStateContinuePending, false, false, true},
		{"pause pending", svcStatePausePending, false, false, true},
		{"paused", svcStatePaused, false, false, false},
		{"unknown", 99, false, false, false},
	}
	for _, c := range cases {
		running, stable, err := serviceStateVerdict(c.state)
		if running != c.running || stable != c.stable {
			t.Fatalf("%s: got running=%v stable=%v, want running=%v stable=%v",
				c.name, running, stable, c.running, c.stable)
		}
		if c.stable && err != nil {
			t.Fatalf("%s: 稳定状态不应返回错误：%v", c.name, err)
		}
		if !c.stable && err == nil {
			t.Fatalf("%s: 非稳定状态必须返回错误", c.name)
		}
		if c.isPending && !errors.Is(err, errServicePending) {
			t.Fatalf("%s: 应是可等待的 PENDING，实际 %v", c.name, err)
		}
	}
}

func TestOptUndoRejectsBadServiceRunningState(t *testing.T) {
	it := &OptItem{
		ID: "svc-run", Name: "svc run",
		Optimize: []OptAction{{Kind: "service", Name: "hpet", Start: "4"}},
	}
	e := &optUndoEntry{ID: it.ID, Steps: []optUndoStep{{
		Kind: "service",
		Svc:  &optUndoService{Name: "hpet", Existed: true, HasStart: true, Start: 4, Running: true},
	}}}
	if err := validateOptUndoEntry(e, it); err == nil {
		t.Fatal("RunningKnown=false 却带 Running=true 必须拒绝")
	}
	e.Steps[0].Svc.RunningKnown = true
	if err := validateOptUndoEntry(e, it); err != nil {
		t.Fatalf("合法的运行状态快照不应被拒绝：%v", err)
	}
}

// 回归：中间版本生成过 version=2 但没有 HMAC 的快照。
// 不能把它当成可信数据，也不能静默失败；应给出明确的不兼容提示，
// 并在有静态 Restore 时退回「恢复默认值」。
func TestOptUndoRejectsOldV2WithoutHMAC(t *testing.T) {
	useTempOptUndo(t)
	cleanupOptTestKeys()
	defer cleanupOptTestKeys()

	p, err := optUndoPath()
	if err != nil {
		t.Fatal(err)
	}
	old := []byte(`{"version":2,"items":[]}`)
	if err := os.WriteFile(p, old, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadOptUndoFile(); !errors.Is(err, errOptUndoIncompatible) {
		t.Fatalf("v2 无 HMAC 快照应报版本不兼容，实际 %v", err)
	}

	key := `HKEY_CURRENT_USER\` + optTestKey
	if err := execRegWrite(OptAction{Kind: "regwrite", Key: key, Value: "UndoDword", Vtype: "REG_DWORD", Data: "0"}); err != nil {
		t.Fatal(err)
	}
	it := &OptItem{
		ID: "undo-v2", Name: "undo v2",
		Optimize: []OptAction{{Kind: "regwrite", Key: key, Value: "UndoDword", Vtype: "REG_DWORD", Data: "0"}},
		Restore:  []OptAction{{Kind: "regwrite", Key: key, Value: "UndoDword", Vtype: "REG_DWORD", Data: "1"}},
	}
	res := restoreOptItem(context.Background(), it)
	if !res.OK {
		t.Fatalf("旧快照不兼容时应退回静态还原，实际失败：%s", res.Err)
	}
	if res.Warn == "" {
		t.Fatal("退回静态还原时必须说明旧快照不兼容")
	}
	if v, ok := testReadDword(t, key, "UndoDword"); !ok || v != 1 {
		t.Fatalf("静态还原应写默认值 1，实际 %d exists=%v", v, ok)
	}
}

func TestOptUndoRejectsBadStepCountAndServiceStart(t *testing.T) {
	it := &OptItem{
		ID: "undo-count", Name: "undo count",
		Optimize: []OptAction{
			{Kind: "service", Name: "WSearch", Start: "4"},
			{Kind: "service", Name: "SysMain", Start: "4"},
		},
	}
	// 动作数量不足
	e := &optUndoEntry{ID: it.ID, Steps: []optUndoStep{{
		Kind: "service", Svc: &optUndoService{Name: "WSearch", Existed: true, HasStart: true, Start: 4},
	}}}
	if err := validateOptUndoEntry(e, it); err == nil {
		t.Fatal("动作数量不匹配必须拒绝")
	}
	// 数量对上了，但服务启动类型超出 0..4
	e.Steps = append(e.Steps, optUndoStep{
		Kind: "service", Svc: &optUndoService{Name: "SysMain", Existed: true, HasStart: true, Start: 99},
	})
	if err := validateOptUndoEntry(e, it); err == nil {
		t.Fatal("非法服务启动类型必须拒绝")
	}
}
