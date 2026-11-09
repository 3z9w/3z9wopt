package main

// 操作前快照 / 回滚单测。
// 只碰 HKCU\Software\3z9wOptV2Test 临时键；快照文件写到 t.TempDir()，不碰用户真实记录。

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/windows/registry"
)

// useTempOptUndo 让单测走临时文件，跳过 ProgramData 的 ACL 收紧。
// 单测通常不是管理员，不能把临时目录改成仅管理员可访问。
func useTempOptUndo(t *testing.T) {
	t.Helper()
	p := filepath.Join(t.TempDir(), "optundo.json")
	old := optUndoPathOverride
	optUndoPathOverride = p
	t.Cleanup(func() { optUndoPathOverride = old })
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
}

// regdelete Value 为空 = 删整键：快照必须能连子键和值一起重建。
// 注意：execRegDelete 走 RegDeleteKey，带子键的键删不掉（源目录里的空值删除项
// 都是无子键的占位键）。这里直接验证「快照 → 删除 → 还原」这条链路。
func TestOptUndoRestoresDeletedKeyTree(t *testing.T) {
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
	snap, err := captureOptUndo(it)
	if err != nil {
		t.Fatal(err)
	}
	if err := validateOptUndoEntry(snap, it); err != nil {
		t.Fatalf("整键快照应通过校验：%v", err)
	}
	if err := deleteRegKeyTree(registry.CURRENT_USER, parent); err != nil {
		t.Fatal(err)
	}
	if _, err := registry.OpenKey(registry.CURRENT_USER, parent, registry.QUERY_VALUE); err != registry.ErrNotExist {
		t.Fatalf("删除后整键应不存在，err=%v", err)
	}
	for i := len(snap.Steps) - 1; i >= 0; i-- {
		if err := restoreUndoStep(snap.Steps[i], it, i); err != nil {
			t.Fatalf("还原失败：%v", err)
		}
	}
	if v, ok := testReadDword(t, key, "A"); !ok || v != 7 {
		t.Fatalf("子键值 A 应恢复为 7，实际 %d exists=%v", v, ok)
	}
	if v, ok := testReadString(t, key+`\Child`, "B"); !ok || v != "hi" {
		t.Fatalf("孙键值 B 应恢复为 hi，实际 %q exists=%v", v, ok)
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
	body := `{"version":1,"items":[{"id":"x","name":"x","at":"","steps":[` +
		`{"kind":"cmd","cmd":{"cmd":"calc.exe"}}]}]}`
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadOptUndoFile(); err == nil {
		t.Fatal("含未知字段的快照必须拒绝加载")
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
