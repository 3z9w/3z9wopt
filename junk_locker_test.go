package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"
	"unsafe"
)

// 锁住 RM_PROCESS_INFO 的大小：结构体算错会**静默读错进程名**（不报错、不崩溃，
// 只是拿到的名字是错的），这类 bug 只能靠这个断言提前拦住。
func TestRmProcessInfoSize(t *testing.T) {
	const want = 668 // 12(PID+FILETIME) + 512(AppName) + 128(ServiceShortName) + 4*4
	if got := int(unsafe.Sizeof(rmProcessInfo{})); got != want {
		t.Fatalf("RM_PROCESS_INFO 大小变了: got=%d want=%d（改结构体必须重新核对 Windows SDK 定义）", got, want)
	}
}

// 保护名单：安全软件一律不关，普通进程可以关
func TestProtectReason(t *testing.T) {
	sec := []string{
		`C:\Program Files\360\360Safe\safemon\360Safe.exe`,
		`C:\Program Files\Huorong\Sysdiag\bin\hipsdaemon.exe`,
		`C:\Program Files\Tencent\QQPCMgr\QQPCMgr.exe`,
		`C:\ProgramData\Microsoft\Windows Defender\platform\MsMpEng.exe`,
		`C:\Program Files\ESET\ekrn.exe`,
	}
	for _, p := range sec {
		why, ok := protectReason(p)
		if !ok {
			t.Errorf("安全软件没被保护: %s", p)
			continue
		}
		t.Logf("  已保护 %s → %s", filepath.Base(p), why)
	}
	normal := []string{
		`C:\Users\a\AppData\Local\DingTalk\DingTalk.exe`,
		`C:\Program Files\Tencent\WeChat\WeChat.exe`,
		`C:\Program Files (x86)\Microsoft\Edge\Application\msedge.exe`,
	}
	for _, p := range normal {
		if why, ok := protectReason(p); ok {
			t.Errorf("普通进程被误保护: %s → %s", filepath.Base(p), why)
		}
	}
	// 本程序自己
	self := filepath.Base(os.Args[0])
	if _, ok := protectReason(self); !ok {
		t.Errorf("没认出本程序自己: %s", self)
	}
}

/* Windows 自己的核心进程必须标成"关不掉"（2026-10-15）。
   实测它们都跑在 svchost.exe 里（DiagTrack / WSearch…），以前跟 QQ 一样摆在能勾的框里，
   用户勾了关不掉，只会以为软件坏了。谁把它们从保护名单里拿掉，这条就红。 */
/* Windows 自己的核心进程必须标成"关不掉"（2026-10-15）。
   实测它们都跑在 svchost.exe 里（DiagTrack / WSearch…），以前跟 QQ 一样摆在能勾的框里，
   用户勾了关不掉，只会以为软件坏了。谁把它们从保护名单里拿掉，这条就红。

   2026-10-15 二次修订：explorer.exe 也进来了（不是它"关不掉"，是**我们拉不回来**）。
   真机的黑屏就是这么来的：TerminateProcess 之后指望系统自己重开 shell，
   可多数机器上它就是一片黑摆在那儿，用户只能手动重启资源管理器。
   要放出来，必须先兑现"关完自己拉回"（复制用户令牌 + CreateProcessAsUser）。 */
func TestProtectSystemProcs(t *testing.T) {
	sys := []string{"System", "svchost.exe", "csrss.exe", "smss.exe", "lsass.exe",
		"wininit.exe", "services.exe", "dwm.exe", "winlogon.exe", "DiagTrack",
		"explorer.exe", "SearchApp.exe", "SearchHost.exe", "RuntimeBroker.exe",
		"StartMenuExperienceHost.exe", "ApplicationFrameHost.exe"}
	for _, p := range sys {
		if why, ok := protectReasonAny(p, p); !ok {
			t.Errorf("系统进程没被保护: %s", p)
		} else {
			t.Logf("  已保护 %s → %s", p, why)
		}
	}
	// 反过来：用户自己的软件必须还能关，别一刀切
	for _, p := range []string{"QQ.exe", "WeChat.exe", "DingTalk.exe", "msedge.exe", "chrome.exe"} {
		if why, ok := protectReasonAny(p, p); ok {
			t.Errorf("用户程序被误保护: %s → %s", p, why)
		}
	}
}

// 本程序进程树（含 WebView2 界面子进程）必须被认出来：
// 本程序是 Wails + WebView2，界面进程占着我们自己的缓存，
// 把它当普通占用者关掉 = 清理没跑完程序先没了（2026-10-13 用户发现）。
func TestSelfTree(t *testing.T) {
	set := selfTree()
	if !set[uint32(os.Getpid())] {
		t.Fatal("进程树里没有自己")
	}
	if len(processParentMap()) == 0 {
		t.Log("拿不到进程父子关系（快照失败），此时只保护自己 —— 可接受，不许报错")
	}
	t.Logf("本程序进程树 %d 个进程", len(set))
	// 树里的进程一律不许关：whoLocks 与 killPids 两条路都要拦住
	pids := make([]int, 0, len(set))
	for pid := range set {
		pids = append(pids, int(pid))
	}
	for _, r := range killPids(pids) {
		if r.Ok {
			t.Errorf("把自己的进程关掉了: %+v", r)
		}
		if r.Err == "" {
			t.Errorf("跳过却没写原因: %+v", r)
		}
	}
}

// 真起一个子进程，验证 selfTree 能把"自己的后代"认出来 —— 这是本程序与它自己的
// WebView2 界面进程之间的**唯一**关系判据（认不出 = 真机上会把自己的界面进程关掉）。
func TestSelfTreeSpotsChild(t *testing.T) {
	cmd := exec.Command("cmd", "/c", "ping -n 8 127.0.0.1 > NUL")
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: 0x08000000}
	if err := cmd.Start(); err != nil {
		t.Skipf("起不了子进程，跳过: %v", err)
	}
	defer func() { _ = cmd.Process.Kill(); _, _ = cmd.Process.Wait() }()
	child := uint32(cmd.Process.Pid)
	for i := 0; i < 25; i++ {
		if selfTree()[child] {
			res := killPids([]int{int(child)})
			if len(res) != 1 || res[0].Ok {
				t.Fatalf("把自己的子进程关掉了: %+v", res)
			}
			t.Logf("  子进程 %d 已认出属于自己进程树，拒绝关闭: %s", child, res[0].Err)
			return
		}
		time.Sleep(60 * time.Millisecond)
	}
	t.Fatalf("selfTree 没认出自己的子进程（真机上 WebView2 界面进程就会保护不住）")
}

// WebView2 界面进程（别人家的）也不关：只杀它不杀宿主，软件还在界面黑掉，比整个关掉更难看
func TestProtectWebView2(t *testing.T) {
	why, ok := protectReason(`C:\Program Files (x86)\Microsoft\EdgeWebView\Application\120.0.0.0\msedgewebview2.exe`)
	if !ok {
		t.Fatal("WebView2 界面进程没被保护")
	}
	t.Logf("  WebView2: %s", why)
}

// killPids 绝不能杀本程序自己，也不能杀保护名单里的进程
func TestKillPidsNeverSelf(t *testing.T) {
	self := os.Getpid()
	res := killPids([]int{self, -1, 0})
	if len(res) == 0 {
		t.Fatal("应该有返回（跳过也要有记录，前端要照播）")
	}
	if len(res) != 1 {
		t.Fatalf("只传了自己 + 两个无效 PID，应只剩自己那条: %+v", res)
	}
	r := res[0]
	if r.Pid != self {
		t.Errorf("记录错了 PID: %+v", r)
	}
	if r.Ok {
		t.Errorf("把自己杀了: %+v", r)
	}
	if r.Err == "" {
		t.Errorf("跳过了却没说原因（前端会显示成空白）: %+v", r)
	}
	t.Logf("  自己被正确跳过: %s", r.Err)
}

// whoLocks 对不存在的路径必须返回空而不是报错（前端据此决定弹不弹窗）
func TestWhoLocksEmpty(t *testing.T) {
	got, err := whoLocks(nil)
	if err != nil {
		t.Fatalf("空输入不该报错: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("空输入应返回空: %+v", got)
	}
	// 不存在的路径会被 cleanPathList 过滤掉，同样不给错
	got, err = whoLocks([]string{filepath.Join(t.TempDir(), "definitely-not-here.bin")})
	if err != nil {
		t.Fatalf("不存在路径不该报错: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("不存在路径应返回空: %+v", got)
	}
}

// 端到端：拿一个本进程确实占着的文件去问，应该能问回来（至少不报错、不崩）
func TestWhoLocksOnRealFile(t *testing.T) {
	f := filepath.Join(t.TempDir(), "probe.bin")
	fh, err := os.Create(f)
	if err != nil {
		t.Skip("建不了临时文件")
	}
	defer fh.Close()
	fh.WriteString("x")

	got, err := whoLocks([]string{f})
	if err != nil {
		t.Fatalf("whoLocks 失败: %v", err)
	}
	// 本进程打开的文件，RM 未必报告（自己占自己常报不出来）—— 只要求不崩、不报错
	t.Logf("探查结果 %d 条: %+v", len(got), got)
}
