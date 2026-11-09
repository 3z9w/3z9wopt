package main

// ============================================================================
// 系统优化的「操作前快照」与真实回滚（2026-11-09）
//
// 旧实现的问题：
//   还原动作（OptItem.Restore）是源软件写死的「恢复系统默认值」，
//   不是「恢复应用优化之前的状态」。例如原值是 2，优化改成 0，旧还原可能
//   把值删掉或写成 1 —— 这只能叫「回到默认」，不能叫「撤销」。
//
// 现在的语义：
//   · 应用优化前，逐条记录动作会碰到的注册表值 / 整键 / 服务启动类型；
//   · 快照落在 %LOCALAPPDATA%\3z9wOpt\optundo.json，跨进程、跨重启都还在；
//   · 还原时优先按快照回写；只有找不到快照（旧版本应用过）才退回静态 Restore，
//     并在结果里带 Warn，明确告诉用户「这是默认值，不是操作前状态」；
//   · 应用某项时，如果中途失败，用刚拍的快照把这一项已经改过的部分按逆序退回，
//     不让用户停在「改了一半」的状态。
//
// 动作与快照的对应关系：
//   regwrite              → 单个注册表值（类型 + 原始字节 + 是否存在）
//   regdelete Value != "" → 单个注册表值
//   regdelete Value == "" → 整键递归快照（值 + 子键）
//   service               → 服务 Start 与 DelayedAutoStart
//   cmd                   → 无法通用快照；沿用静态 Restore 里对应的 cmd 作为补偿/还原
//   shellnotify           → 无状态，记 noop
// ============================================================================

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows/registry"
)

const (
	optUndoVersion      = 1
	optUndoMaxValueSize = 16 << 20 // 单个注册表值上限 16 MB，防止异常值撑爆日志
	optUndoMaxTreeNodes = 4096     // 整键快照最多 4096 个值/子键
	optUndoMaxTreeDepth = 16
)

// optUndoEntry 一个优化项的操作前快照。
// 同一项重复应用时保留最早的一份：它才是用户第一次动手前的状态。
type optUndoEntry struct {
	ID    string        `json:"id"`
	Name  string        `json:"name"`
	At    string        `json:"at"`
	Steps []optUndoStep `json:"steps"`
}

// optUndoStep 一条优化动作对应的操作前状态。
type optUndoStep struct {
	Kind string `json:"kind"` // regvalue / regkey / service / cmd / noop

	Key   string `json:"key,omitempty"`
	Value string `json:"value,omitempty"`

	// regvalue
	RegExists bool   `json:"regExists"`
	RegType   uint32 `json:"regType"`
	RegData   []byte `json:"regData,omitempty"`

	// regkey（regdelete 且 Value 为空）
	KeyExisted bool         `json:"keyExisted"`
	KeyTree    *optUndoTree `json:"keyTree,omitempty"`

	// service
	Svc *optUndoService `json:"svc,omitempty"`

	// cmd：原命令（仅用于报错文案）与静态 Restore 里对应的还原命令
	Orig string      `json:"orig,omitempty"`
	Cmd  *optUndoCmd `json:"cmd,omitempty"`
}

type optUndoTree struct {
	Values  []optUndoTreeValue `json:"values,omitempty"`
	Subkeys []optUndoTreeKey   `json:"subkeys,omitempty"`
}

type optUndoTreeValue struct {
	Name string `json:"name"`
	Type uint32 `json:"type"`
	Data []byte `json:"data,omitempty"`
}

type optUndoTreeKey struct {
	Name string       `json:"name"`
	Tree *optUndoTree `json:"tree"`
}

type optUndoService struct {
	Name       string `json:"name"`
	Existed    bool   `json:"existed"`
	HasStart   bool   `json:"hasStart"`
	Start      uint32 `json:"start"`
	HasDelayed bool   `json:"hasDelayed"`
	Delayed    uint32 `json:"delayed"`
}

type optUndoCmd struct {
	Cmd     string `json:"cmd"`
	Skiperr string `json:"skiperr,omitempty"`
}

type optUndoFile struct {
	Version int             `json:"version"`
	Items   []*optUndoEntry `json:"items"`
}

// ---------- 快照采集 ----------

// captureOptUndo 记录一个优化项全部 Optimize 动作的操作前状态。
func captureOptUndo(it *OptItem) (*optUndoEntry, error) {
	return captureOptUndoActs(it.ID, it.Name, it.Optimize, it.Restore)
}

// captureOptUndoActs 记录 acts 的操作前状态。
// cmdSource 用来给 cmd 动作找静态还原命令；补偿旧版静态还原时传 nil。
func captureOptUndoActs(id, name string, acts, cmdSource []OptAction) (*optUndoEntry, error) {
	e := &optUndoEntry{ID: id, Name: name, At: time.Now().Format("2006-01-02 15:04:05")}

	var cmdRestores []optUndoCmd
	for _, r := range cmdSource {
		if r.Kind == "cmd" {
			cmdRestores = append(cmdRestores, optUndoCmd{Cmd: r.Cmd, Skiperr: r.Skiperr})
		}
	}
	cmdIdx := 0

	for _, a := range acts {
		step := optUndoStep{Kind: a.Kind}
		switch a.Kind {
		case "regwrite":
			exists, typ, data, err := readRegValueRaw(a.Key, a.Value)
			if err != nil {
				return nil, fmt.Errorf("读取 %s 的原值失败：%w", a.Key, err)
			}
			step.Kind, step.Key, step.Value = "regvalue", a.Key, a.Value
			step.RegExists, step.RegType, step.RegData = exists, typ, data

		case "regdelete":
			if a.Value == "" {
				existed, tree, err := readRegKeyTree(a.Key)
				if err != nil {
					return nil, fmt.Errorf("读取 %s 的原键失败：%w", a.Key, err)
				}
				step.Kind, step.Key = "regkey", a.Key
				step.KeyExisted, step.KeyTree = existed, tree
			} else {
				exists, typ, data, err := readRegValueRaw(a.Key, a.Value)
				if err != nil {
					return nil, fmt.Errorf("读取 %s 的原值失败：%w", a.Key, err)
				}
				step.Kind, step.Key, step.Value = "regvalue", a.Key, a.Value
				step.RegExists, step.RegType, step.RegData = exists, typ, data
			}

		case "service":
			svc, err := readServiceUndo(a.Name)
			if err != nil {
				return nil, fmt.Errorf("读取服务 %s 的启动类型失败：%w", a.Name, err)
			}
			step.Kind, step.Svc = "service", svc

		case "cmd":
			step.Kind, step.Orig = "cmd", a.Cmd
			if cmdIdx < len(cmdRestores) {
				c := cmdRestores[cmdIdx]
				step.Cmd = &c
			}
			cmdIdx++

		case "shellnotify":
			step.Kind = "noop"

		default:
			return nil, fmt.Errorf("未知动作: %s", a.Kind)
		}
		e.Steps = append(e.Steps, step)
	}
	return e, nil
}

// ---------- 快照持久化 ----------

var optUndoMu sync.Mutex

func optUndoPath() string {
	d := appDataDir()
	_ = os.MkdirAll(d, 0o755)
	return filepath.Join(d, "optundo.json")
}

func loadOptUndoFile() (*optUndoFile, error) {
	b, err := os.ReadFile(optUndoPath())
	if err != nil {
		if os.IsNotExist(err) {
			return &optUndoFile{Version: optUndoVersion}, nil
		}
		return nil, err
	}
	var f optUndoFile
	if err := json.Unmarshal(b, &f); err != nil {
		return nil, err
	}
	if f.Version > optUndoVersion {
		return nil, fmt.Errorf("操作前快照版本过新（%d）", f.Version)
	}
	return &f, nil
}

func writeOptUndoFile(f *optUndoFile) error {
	if f.Version == 0 {
		f.Version = optUndoVersion
	}
	b, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return err
	}
	p := optUndoPath()
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}

// saveOptUndoIfAbsent 只在没有旧快照时写入。
// 返回 added=true 表示这次确实新增了一条，失败补偿成功后要把它删掉。
func saveOptUndoIfAbsent(e *optUndoEntry) (added bool, err error) {
	optUndoMu.Lock()
	defer optUndoMu.Unlock()

	f, err := loadOptUndoFile()
	if err != nil {
		return false, err
	}
	for _, x := range f.Items {
		if x.ID == e.ID {
			return false, nil
		}
	}
	f.Items = append(f.Items, e)
	return true, writeOptUndoFile(f)
}

func loadOptUndoEntry(id string) (*optUndoEntry, bool, error) {
	optUndoMu.Lock()
	defer optUndoMu.Unlock()

	f, err := loadOptUndoFile()
	if err != nil {
		return nil, false, err
	}
	for _, x := range f.Items {
		if x.ID == id {
			return x, true, nil
		}
	}
	return nil, false, nil
}

func deleteOptUndo(id string) error {
	optUndoMu.Lock()
	defer optUndoMu.Unlock()

	f, err := loadOptUndoFile()
	if err != nil {
		return err
	}
	out := f.Items[:0]
	for _, x := range f.Items {
		if x.ID != id {
			out = append(out, x)
		}
	}
	f.Items = out
	return writeOptUndoFile(f)
}

// ---------- 应用 / 还原骨架 ----------

// applyOptItem 拍快照 → 执行 → 失败时按快照逆序补偿。
func applyOptItem(ctx context.Context, it *OptItem) OptResult {
	res := OptResult{ID: it.ID, Name: it.Name, OK: true}
	if len(it.Optimize) == 0 {
		res.OK, res.Err = false, "该项没有定义动作"
		return res
	}

	snap, err := captureOptUndo(it)
	if err != nil {
		res.OK, res.Err = false, "记录操作前状态失败，未做任何修改："+err.Error()
		return res
	}
	added, err := saveOptUndoIfAbsent(snap)
	if err != nil {
		res.OK, res.Err = false, "记录操作前状态失败，未做任何修改："+err.Error()
		return res
	}

	last, runErr := execOptActs(ctx, it.Optimize)
	if runErr == nil {
		return res
	}

	// 失败位置的动作也可能改了一半（例如服务 Start 改完、延迟启动没写成功），
	// 所以从 last 开始一起回退；若在动作开始前就停了，last=-1，无需补偿。
	if last >= 0 {
		if cerrs := compensateOptUndo(snap.Steps, last); len(cerrs) > 0 {
			res.OK, res.Err = false, runErr.Error()+"；自动回退失败："+strings.Join(cerrs, "；")+"（可点还原重试）"
			return res
		}
		// 补偿成功 = 这一项回到了应用前的状态。刚写进去的快照已经没有价值，
		// 删掉它，避免以后还原时反而把用户后来的手动设置覆盖掉。
		if added {
			_ = deleteOptUndo(it.ID)
		}
	}
	res.OK, res.Err = false, runErr.Error()+"；已自动回退本次已执行的部分"
	return res
}

// restoreOptItem 优先按操作前快照还原；没有快照时退回旧版静态还原。
func restoreOptItem(ctx context.Context, it *OptItem) OptResult {
	res := OptResult{ID: it.ID, Name: it.Name, OK: true}

	entry, ok, err := loadOptUndoEntry(it.ID)
	if err != nil {
		res.OK, res.Err = false, "读取操作前快照失败："+err.Error()
		return res
	}
	if !ok {
		return restoreOptItemStatic(ctx, it)
	}

	var errs, warns []string
	stopped := false
	usedCmd := false
	// 逆序还原：后做的动作先退，和手工撤销的顺序一致。
	for i := len(entry.Steps) - 1; i >= 0; i-- {
		if ctx.Err() != nil {
			stopped = true
			break
		}
		if entry.Steps[i].Kind == "cmd" && entry.Steps[i].Cmd != nil {
			usedCmd = true
		}
		err := restoreUndoStep(entry.Steps[i])
		if errors.Is(err, errOptNoUndoCmd) {
			// 例如「net stop SmsRouter」源数据没有配对应的 net start。
			// 状态类改动（注册表/服务启动类型）已经还原，这条命令只能如实提示。
			warns = append(warns, undoStepLabel(entry.Steps[i])+"没有定义还原命令")
			continue
		}
		if err != nil {
			errs = append(errs, undoStepLabel(entry.Steps[i])+"："+err.Error())
		}
	}
	if stopped {
		errs = append(errs, "已停止，操作前快照已保留，可再次点还原")
	}
	if len(errs) > 0 {
		res.OK, res.Err = false, strings.Join(errs, "；")
		return res
	}
	if err := deleteOptUndo(it.ID); err != nil {
		warns = append(warns, "操作前快照删除失败："+err.Error())
	}
	if usedCmd {
		// 命令无法像注册表那样逐字节快照，只能执行源数据配的还原命令。
		// 这类动作不满足「精确回到操作前状态」，必须如实说明。
		warns = append(warns, "命令类动作按预置还原命令处理，未记录执行前状态")
	}
	if len(warns) > 0 {
		res.Warn = strings.Join(warns, "；")
	}
	return res
}

// restoreOptItemStatic 旧版行为：执行写死的 Restore。
// 失败时用刚拍的快照补偿，避免「还原到一半」。
func restoreOptItemStatic(ctx context.Context, it *OptItem) OptResult {
	res := OptResult{ID: it.ID, Name: it.Name, OK: true}
	if len(it.Restore) == 0 {
		res.OK, res.Err = false, "该项没有定义还原动作，也没有操作前快照"
		return res
	}

	snap, err := captureOptUndoActs(it.ID, it.Name, it.Restore, nil)
	if err != nil {
		res.OK, res.Err = false, "记录还原前状态失败，未做任何修改："+err.Error()
		return res
	}
	last, runErr := execOptActs(ctx, it.Restore)
	if runErr == nil {
		res.Warn = "没有找到操作前快照（这项是旧版本应用的），已按系统默认值还原，不是应用前的状态"
		return res
	}
	if last >= 0 {
		if cerrs := compensateOptUndo(snap.Steps, last); len(cerrs) > 0 {
			res.OK, res.Err = false, runErr.Error()+"；自动回退失败："+strings.Join(cerrs, "；")
			return res
		}
	}
	res.OK, res.Err = false, runErr.Error()+"；已自动回退本次已执行的部分"
	return res
}

// execOptActs 顺序执行动作，返回最后一条真正执行过的下标（-1=一条都没执行）。
// 下标用于补偿：失败动作本身也可能改了一半，所以补偿范围包含它。
func execOptActs(ctx context.Context, acts []OptAction) (last int, err error) {
	last = -1
	for i, act := range acts {
		if ctx.Err() != nil {
			return last, fmt.Errorf("已停止")
		}
		if err := execOptAction(ctx, act); err != nil {
			return i, err
		}
		last = i
	}
	return last, nil
}

// compensateOptUndo 从 last 开始逆序回退，返回无法回退的明细。
func compensateOptUndo(steps []optUndoStep, last int) []string {
	if last >= len(steps) {
		last = len(steps) - 1
	}
	var errs []string
	for i := last; i >= 0; i-- {
		if err := restoreUndoStep(steps[i]); err != nil {
			errs = append(errs, undoStepLabel(steps[i])+"："+err.Error())
		}
	}
	return errs
}

// errOptNoUndoCmd 表示这条 cmd 动作在源数据里没有对应的还原命令。
// 还原时按「如实提示、不阻塞其他项」处理；失败补偿时必须当成回退不完整。
var errOptNoUndoCmd = errors.New("没有定义还原命令")

// restoreUndoStep 按快照回写一条动作。
// cmd 用 context.Background()：用户点「停止」也不能把已经改掉的东西留在半路。
func restoreUndoStep(step optUndoStep) error {
	switch step.Kind {
	case "noop":
		return nil
	case "regvalue":
		return restoreRegValue(step.Key, step.Value, step.RegExists, step.RegType, step.RegData)
	case "regkey":
		return restoreRegKeyTree(step.Key, step.KeyExisted, step.KeyTree)
	case "service":
		return restoreServiceUndo(step.Svc)
	case "cmd":
		if step.Cmd == nil || strings.TrimSpace(step.Cmd.Cmd) == "" {
			return errOptNoUndoCmd
		}
		return execOptAction(context.Background(), OptAction{Kind: "cmd", Cmd: step.Cmd.Cmd, Skiperr: step.Cmd.Skiperr})
	}
	return fmt.Errorf("未知快照动作: %s", step.Kind)
}

func undoStepLabel(step optUndoStep) string {
	switch step.Kind {
	case "regvalue":
		name := step.Value
		if name == "" {
			name = "默认值"
		}
		return "注册表值 " + lastRegSeg(step.Key) + `\` + name
	case "regkey":
		return "注册表键 " + lastRegSeg(step.Key)
	case "service":
		if step.Svc != nil {
			return "服务 " + step.Svc.Name
		}
		return "服务"
	case "cmd":
		if step.Orig != "" {
			return "命令 " + shortCmd(step.Orig)
		}
		return "命令"
	case "noop":
		return "刷新外壳"
	}
	return step.Kind
}

// ---------- 注册表快照 ----------

// readRegValueRaw 读取单个值：是否存在、原始类型、原始字节。
// 用 GetValue(name, nil) 先问长度，再按长度取数据；不按 DWORD/SZ 猜类型。
func readRegValueRaw(keyPath, value string) (bool, uint32, []byte, error) {
	root, sub, err := regRoot(keyPath)
	if err != nil {
		return false, 0, nil, err
	}
	k, err := registry.OpenKey(root, sub, registry.QUERY_VALUE)
	if err == registry.ErrNotExist {
		return false, 0, nil, nil
	}
	if err != nil {
		return false, 0, nil, err
	}
	defer k.Close()
	return readValueRaw(k, value)
}

func readValueRaw(k registry.Key, name string) (bool, uint32, []byte, error) {
	for i := 0; i < 5; i++ {
		n, typ, err := k.GetValue(name, nil)
		if err == registry.ErrNotExist {
			return false, 0, nil, nil
		}
		if err != nil && err != registry.ErrShortBuffer {
			return false, 0, nil, err
		}
		if n < 0 || n > optUndoMaxValueSize {
			return false, 0, nil, fmt.Errorf("注册表值过大：%d 字节", n)
		}
		if n == 0 {
			return true, typ, nil, nil
		}
		buf := make([]byte, n)
		n2, typ2, err2 := k.GetValue(name, buf)
		if err2 == registry.ErrNotExist {
			return false, 0, nil, nil
		}
		if err2 == registry.ErrShortBuffer {
			continue // 两次读取之间值变大了，重试
		}
		if err2 != nil {
			return false, 0, nil, err2
		}
		return true, typ2, buf[:n2], nil
	}
	return false, 0, nil, fmt.Errorf("读取注册表值 %q 失败：值在读取过程中反复变化", name)
}

var procRegSetValueEx = advapi32.NewProc("RegSetValueExW")

// setRegRaw 按原始类型写回，保留 REG_EXPAND_SZ / REG_MULTI_SZ / REG_QWORD 等类型。
func setRegRaw(k registry.Key, name string, typ uint32, data []byte) error {
	p, err := syscall.UTF16PtrFromString(name)
	if err != nil {
		return err
	}
	var pdata *byte
	if len(data) > 0 {
		pdata = &data[0]
	}
	r, _, _ := procRegSetValueEx.Call(
		uintptr(k),
		uintptr(unsafe.Pointer(p)),
		0,
		uintptr(typ),
		uintptr(unsafe.Pointer(pdata)),
		uintptr(len(data)),
	)
	if r != 0 {
		return syscall.Errno(r)
	}
	return nil
}

func restoreRegValue(keyPath, value string, exists bool, typ uint32, data []byte) error {
	root, sub, err := regRoot(keyPath)
	if err != nil {
		return err
	}
	if !exists {
		k, err := registry.OpenKey(root, sub, registry.SET_VALUE)
		if err == registry.ErrNotExist {
			return nil
		}
		if err != nil {
			return err
		}
		defer k.Close()
		if err := k.DeleteValue(value); err != nil && err != registry.ErrNotExist {
			return err
		}
		return nil
	}
	k, _, err := registry.CreateKey(root, sub, registry.SET_VALUE)
	if err != nil {
		return err
	}
	defer k.Close()
	return setRegRaw(k, value, typ, data)
}

// readRegKeyTree 递归记录整个键（regdelete Value 为空时用）。
func readRegKeyTree(keyPath string) (bool, *optUndoTree, error) {
	root, sub, err := regRoot(keyPath)
	if err != nil {
		return false, nil, err
	}
	k, err := registry.OpenKey(root, sub, registry.QUERY_VALUE|registry.ENUMERATE_SUB_KEYS)
	if err == registry.ErrNotExist {
		return false, nil, nil
	}
	if err != nil {
		return false, nil, err
	}
	defer k.Close()
	budget := optUndoMaxTreeNodes
	tree, err := readKeyTree(k, 0, &budget)
	if err != nil {
		return false, nil, err
	}
	return true, tree, nil
}

func readKeyTree(k registry.Key, depth int, budget *int) (*optUndoTree, error) {
	if depth > optUndoMaxTreeDepth {
		return nil, fmt.Errorf("注册表键层级过深（>%d）", optUndoMaxTreeDepth)
	}
	names, err := k.ReadValueNames(-1)
	if err != nil {
		return nil, err
	}
	tree := &optUndoTree{}
	for _, name := range names {
		*budget = *budget - 1
		if *budget < 0 {
			return nil, fmt.Errorf("注册表键内容超过 %d 项", optUndoMaxTreeNodes)
		}
		exists, typ, data, err := readValueRaw(k, name)
		if err != nil {
			return nil, err
		}
		if !exists {
			continue
		}
		tree.Values = append(tree.Values, optUndoTreeValue{Name: name, Type: typ, Data: data})
	}

	subs, err := k.ReadSubKeyNames(-1)
	if err != nil {
		return nil, err
	}
	for _, name := range subs {
		*budget = *budget - 1
		if *budget < 0 {
			return nil, fmt.Errorf("注册表键内容超过 %d 项", optUndoMaxTreeNodes)
		}
		sk, err := registry.OpenKey(k, name, registry.QUERY_VALUE|registry.ENUMERATE_SUB_KEYS)
		if err != nil {
			return nil, err
		}
		child, err := readKeyTree(sk, depth+1, budget)
		sk.Close()
		if err != nil {
			return nil, err
		}
		tree.Subkeys = append(tree.Subkeys, optUndoTreeKey{Name: name, Tree: child})
	}
	return tree, nil
}

func restoreRegKeyTree(keyPath string, existed bool, tree *optUndoTree) error {
	root, sub, err := regRoot(keyPath)
	if err != nil {
		return err
	}
	// 先整键删掉，保证优化过程中新增的值/子键不会残留；再按快照重建。
	if err := deleteRegKeyTree(root, sub); err != nil {
		return err
	}
	if !existed {
		return nil
	}
	k, _, err := registry.CreateKey(root, sub, registry.SET_VALUE|registry.ENUMERATE_SUB_KEYS)
	if err != nil {
		return err
	}
	defer k.Close()
	return writeKeyTree(k, tree)
}

func writeKeyTree(k registry.Key, tree *optUndoTree) error {
	if tree == nil {
		return nil
	}
	for _, v := range tree.Values {
		if err := setRegRaw(k, v.Name, v.Type, v.Data); err != nil {
			return err
		}
	}
	for _, sc := range tree.Subkeys {
		sk, _, err := registry.CreateKey(k, sc.Name, registry.SET_VALUE|registry.ENUMERATE_SUB_KEYS)
		if err != nil {
			return err
		}
		err = writeKeyTree(sk, sc.Tree)
		sk.Close()
		if err != nil {
			return err
		}
	}
	return nil
}

func deleteRegKeyTree(root registry.Key, sub string) error {
	k, err := registry.OpenKey(root, sub, registry.ENUMERATE_SUB_KEYS|registry.SET_VALUE)
	if err == registry.ErrNotExist {
		return nil
	}
	if err != nil {
		return err
	}
	subs, err := k.ReadSubKeyNames(-1)
	k.Close()
	if err != nil {
		return err
	}
	for _, name := range subs {
		if err := deleteRegKeyTree(root, sub+`\`+name); err != nil {
			return err
		}
	}
	if i := strings.LastIndex(sub, `\`); i >= 0 {
		parent, err := registry.OpenKey(root, sub[:i], registry.SET_VALUE)
		if err != nil {
			return err
		}
		defer parent.Close()
		if err := registry.DeleteKey(parent, sub[i+1:]); err != nil && err != registry.ErrNotExist {
			return err
		}
		return nil
	}
	if err := registry.DeleteKey(root, sub); err != nil && err != registry.ErrNotExist {
		return err
	}
	return nil
}

// ---------- 服务启动类型快照 ----------

const svcRegPath = `SYSTEM\CurrentControlSet\Services\`

func readServiceUndo(name string) (*optUndoService, error) {
	s := &optUndoService{Name: name}
	k, err := registry.OpenKey(registry.LOCAL_MACHINE, svcRegPath+name, registry.QUERY_VALUE)
	if err == registry.ErrNotExist {
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	defer k.Close()
	s.Existed = true
	if v, _, err := k.GetIntegerValue("Start"); err == nil {
		s.HasStart, s.Start = true, uint32(v)
	} else if err != registry.ErrNotExist {
		return nil, err
	}
	if v, _, err := k.GetIntegerValue("DelayedAutoStart"); err == nil {
		s.HasDelayed, s.Delayed = true, uint32(v)
	} else if err != registry.ErrNotExist {
		return nil, err
	}
	return s, nil
}

func restoreServiceUndo(s *optUndoService) error {
	if s == nil || !s.Existed || !s.HasStart {
		// 优化前不存在（动作本身会失败）或没有 Start 值：没有可回写的启动类型。
		return nil
	}
	if err := setServiceStart(s.Name, s.Start); err != nil {
		return err
	}
	// setServiceStart 只保证 Start；DelayedAutoStart 按快照精确恢复（有/无都要还原）。
	k, err := registry.OpenKey(registry.LOCAL_MACHINE, svcRegPath+s.Name, registry.SET_VALUE)
	if err != nil {
		return err
	}
	defer k.Close()
	if s.HasDelayed {
		return k.SetDWordValue("DelayedAutoStart", s.Delayed)
	}
	if err := k.DeleteValue("DelayedAutoStart"); err != nil && err != registry.ErrNotExist {
		return err
	}
	return nil
}
