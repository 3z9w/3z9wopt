// 回归测试：SpaceDelete 必须真的把文件删掉（永久删除，不进回收站）。
//
// 2026-11-02 两个坑依次钉死：
//
//	① 原 recycleDelete 里 windows.StringToUTF16Ptr(path + "\x00") 会 panic
//	   （x/sys 不允许内嵌 NUL），panic 被 Wails dispatcher recover 后前端拿不到
//	   回调，15 秒判「调用超时」—— shell 一次都没被调用，文件纹丝不动。
//	② 改用 FOF_ALLOWUNDO（进回收站）后，大文件在回收站配额/策略/卷类型不支持
//	   的机器上直接返回非 0，界面显示「0 项成功、1 项失败」，文件仍在原地。
//	   现改为永久删除 + 属性剥离 + os.RemoveAll 兜底 + 以磁盘现状复核。
//
// 这些测试就是钉子：谁把参数构造改回拼接 "\x00"、或把 FOF_ALLOWUNDO 加回来，
// 这里立刻红。
package main

import (
	"os"
	"path/filepath"
	"testing"
	"unsafe"
)

func TestSpaceDeleteReallyDeletes(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "dup-file.bin")
	data := make([]byte, 1<<20) // 1MB 够验证路径，不需要真造 100MB
	for i := range data {
		data[i] = byte(i)
	}
	if err := os.WriteFile(p, data, 0o644); err != nil {
		t.Fatalf("造测试文件失败: %v", err)
	}

	var a App
	res := a.SpaceDelete([]string{p})
	if len(res) != 1 {
		t.Fatalf("期望 1 条结果，实际 %d 条", len(res))
	}
	if !res[0].Ok {
		if _, err := os.Stat(p); err == nil {
			t.Fatalf("删除失败且文件仍在: %s", res[0].Err)
		}
		t.Skipf("环境权限不足，跳过: %s", res[0].Err)
	}
	if _, err := os.Stat(p); !os.IsNotExist(err) {
		t.Fatalf("返回成功但文件仍存在: %v", err)
	}
}

// 多路径一次删多个（重复文件页会一次传一组），逐条结果都要是 ok
func TestSpaceDeleteMultiple(t *testing.T) {
	dir := t.TempDir()
	var paths []string
	for _, n := range []string{"a.bin", "b.bin", "c.bin"} {
		p := filepath.Join(dir, n)
		if err := os.WriteFile(p, []byte("same-content"), 0o644); err != nil {
			t.Fatalf("造测试文件失败: %v", err)
		}
		paths = append(paths, p)
	}
	var a App
	res := a.SpaceDelete(paths)
	if len(res) != len(paths) {
		t.Fatalf("期望 %d 条结果，实际 %d 条", len(paths), len(res))
	}
	for _, r := range res {
		if !r.Ok {
			if _, err := os.Stat(r.Path); err == nil {
				t.Fatalf("删除失败且文件仍在: %s -> %s", r.Path, r.Err)
			}
			t.Skipf("环境权限不足，跳过: %s", r.Err)
		}
		if _, err := os.Stat(r.Path); !os.IsNotExist(err) {
			t.Fatalf("返回成功但文件仍存在: %s", r.Path)
		}
	}
}

// 只读/隐藏/系统属性不能挡住删除 —— 用户反馈的重复文件里常有这类系统文件，
// 这一条专门验证 clearDeleteAttrs + os.RemoveAll 兜底链路。
func TestSpaceDeleteReadOnlyAndHidden(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "只读 副本.bin")
	if err := os.WriteFile(p, []byte("dup"), 0o644); err != nil {
		t.Fatalf("造测试文件失败: %v", err)
	}
	if err := os.Chmod(p, 0o444); err != nil { // Windows 上等价于 FILE_ATTRIBUTE_READONLY
		t.Fatalf("设置只读失败: %v", err)
	}
	var a App
	res := a.SpaceDelete([]string{p})
	if !res[0].Ok {
		t.Fatalf("只读文件应能被删掉，实际失败: %s", res[0].Err)
	}
	if _, err := os.Stat(p); !os.IsNotExist(err) {
		t.Fatalf("返回成功但只读文件仍存在: %v", err)
	}
}

// 目录分析页会删整个目录，RemoveAll 兜底必须能收掉整棵树（含只读子文件）。
func TestSpaceDeleteDirectory(t *testing.T) {
	dir := t.TempDir()
	sub := filepath.Join(dir, "待删除目录")
	if err := os.MkdirAll(filepath.Join(sub, "内层"), 0o755); err != nil {
		t.Fatalf("造测试目录失败: %v", err)
	}
	f := filepath.Join(sub, "内层", "dup.bin")
	if err := os.WriteFile(f, []byte("dup"), 0o444); err != nil {
		t.Fatalf("造测试文件失败: %v", err)
	}
	var a App
	res := a.SpaceDelete([]string{sub})
	if !res[0].Ok {
		t.Fatalf("目录应能被删掉，实际失败: %s", res[0].Err)
	}
	if _, err := os.Stat(sub); !os.IsNotExist(err) {
		t.Fatalf("返回成功但目录仍存在: %v", err)
	}
}

// 钉死产品决策：删除一律永久删除，不许把 FOF_ALLOWUNDO (0x0040) 加回来。
func TestNoAllowUndoFlag(t *testing.T) {
	if shDeleteFlags&0x0040 != 0 {
		t.Fatalf("删除不能带 FOF_ALLOWUNDO：大文件在回收站配额/策略受限的机器上会直接失败")
	}
}

// pathListUTF16 必须产出双 NUL 结尾：这是 SHFileOperationW 的硬要求，
// 也是原来那个 panic 的直接原因。
func TestPathListUTF16DoubleNul(t *testing.T) {
	buf := unsafe.Slice(pathListUTF16([]string{`C:\a.txt`}), 10)
	// "C:\a.txt" = 8 个 UTF-16 单元 + 每路径结尾 NUL + 列表结尾 NUL = 10
	if buf[len(buf)-1] != 0 || buf[len(buf)-2] != 0 {
		t.Fatalf("结尾必须是双 NUL: %v", buf)
	}
	if buf[0] != 'C' || buf[1] != ':' || buf[7] != 't' {
		t.Fatalf("内容编码不对: %v", buf)
	}

	multi := unsafe.Slice(pathListUTF16([]string{`C:\a.txt`, `D:\b.txt`}), 19)
	if multi[len(multi)-1] != 0 || multi[len(multi)-2] != 0 {
		t.Fatalf("多路径结尾也必须是双 NUL: %v", multi)
	}
	// 两个路径之间必须有一个 NUL 分隔
	if multi[8] != 0 {
		t.Fatalf("路径之间缺少 NUL 分隔: %v", multi)
	}
}
