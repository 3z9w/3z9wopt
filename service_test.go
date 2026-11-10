package main

import (
	"strings"
	"syscall"
	"testing"
	"unsafe"
)

// 外链守卫：SvcLinks 是对外链接的唯一事实来源，外链必须是本站 https
func TestSvcLinks(t *testing.T) {
	for k, v := range (&App{}).SvcLinks() {
		if v == "" || k == "qr" || k == "reward" || k == "wxid" {
			continue // 素材文件名与微信号不是外链
		}
		if !strings.HasPrefix(v, "https://www.3z9w.com") {
			t.Errorf("外链 %s=%q 必须是本站 https 地址", k, v)
		}
	}
	ls := (&App{}).SvcLinks()
	for _, need := range []string{"site", "order", "tools", "bsod", "docs"} {
		if ls[need] == "" {
			t.Errorf("缺少外链 %s", need)
		}
	}
	if ls["wxid"] == "" {
		t.Error("缺少微信号 wxid")
	}
}

// ---------- 剪贴板 ----------

var (
	procGetClipboardData = svcU32.NewProc("GetClipboardData")
)

// clipboardRead 读回剪贴板文本（仅测试用，验证 CopyText 真的写进去了）
func clipboardRead(t *testing.T) string {
	t.Helper()
	ok := false
	for i := 0; i < 8; i++ {
		if r, _, _ := procOpenClipboard.Call(0); r != 0 {
			ok = true
			break
		}
	}
	if !ok {
		t.Fatal("打不开剪贴板")
	}
	defer procCloseClipboard.Call()
	h, _, _ := procGetClipboardData.Call(cfUnicodeText)
	if h == 0 {
		return ""
	}
	ptr, _, _ := procGlobalLock.Call(h)
	if ptr == 0 {
		return ""
	}
	defer procGlobalUnlock.Call(h)
	/* GlobalLock 把内存指针当返回值交出来（uintptr）。直接 uintptr→unsafe.Pointer 会被 go vet
	   判为误用；这里用一次类型重解释把指针取回来（二进制语义与转换完全等价）。 */
	p := *(*unsafe.Pointer)(unsafe.Pointer(&ptr))
	return syscall.UTF16ToString(unsafe.Slice((*uint16)(p), 4096))
}

func TestCopyTextRoundTrip(t *testing.T) {
	want := "3z9wOptV2 剪贴板自检 —— 中文与符号 0x000000D1 OK"
	if err := (&App{}).CopyText(want); err != "" {
		t.Fatalf("写入剪贴板失败：%s", err)
	}
	if got := clipboardRead(t); got != want {
		t.Errorf("剪贴板读回不一致：\n want=%q\n got =%q", want, got)
	}
	if err := (&App{}).CopyText(""); err == "" {
		t.Error("空内容应该返回错误提示")
	}
}

// 关于页数据守卫：版本号 / 群号 / 外链都得齐全 —— 关于页是软件的门面，缺一项就像半成品
func TestAboutInfo(t *testing.T) {
	ab := (&App{}).GetAboutInfo()
	if !strings.HasPrefix(ab.Version, "v") {
		t.Errorf("版本号 %q 应以 v 开头（侧栏与关于页都读它）", ab.Version)
	}
	if !strings.HasPrefix(ab.Site, "https://www.3z9w.com") {
		t.Errorf("官网地址必须是本站 https，实际 %q", ab.Site)
	}
	if !strings.HasPrefix(ab.BugURL, "https://") {
		t.Errorf("Bug 反馈入口必须是 https 地址，实际 %q", ab.BugURL)
	}
	if strings.TrimSpace(ab.WxID) == "" {
		t.Error("作者微信号不能为空")
	}
	if strings.TrimSpace(ab.QQGroup) == "" {
		t.Error("QQ 群号不能为空")
	}
	for _, r := range ab.QQGroup {
		if r < '0' || r > '9' {
			t.Errorf("QQ 群号 %q 里有非数字字符 —— 复制过去搜不到群", ab.QQGroup)
			break
		}
	}
}
