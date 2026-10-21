package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

/* 「导出配置」的回归哨兵（2026-10-10 用户要求：复制配置改成导出配置）。

   为什么要真跑一遍写盘：导出是**有副作用**的操作（往桌面写文件），
   路径取自 desktopDir()，而 desktop 可能被 OneDrive 接管、被重定向到别的盘。
   2026-10-10 起 desktopDir 优先读注册表的 User Shell Folders（重定向机器只有那里是对的），
   所以老办法（把 USERPROFILE 指到临时目录）已经管不住它了 —— 现在直接把
   desktopDirFn 换成临时桌面，验完自动清掉，**绝不碰用户真实桌面**。

   验三件事：
     ① 真能落盘，且落在桌面里（返回的就是落盘路径）
     ② 内容像配置清单（有标题行），不是空文件
     ③ 措辞口径：缺失写「未获取到」，旧词「读不到」不许再出现；
        机箱/机型也不再出现（用户明确：机箱信息不需要读） */
func TestExportPCInfoWritesRealFile(t *testing.T) {
	tmp := t.TempDir()
	desk := filepath.Join(tmp, "Desktop")
	if err := os.MkdirAll(desk, 0o755); err != nil {
		t.Fatalf("建临时桌面失败：%v", err)
	}
	old := desktopDirFn
	desktopDirFn = func() (string, error) { return desk, nil }
	defer func() { desktopDirFn = old }()

	a := &App{}
	p := a.ExportPCInfo()
	if strings.HasPrefix(p, "ERR:") {
		t.Fatalf("导出失败：%s", p)
	}
	/* ⚠️ 不要拿返回路径跟 t.TempDir() 做前缀比较：GOTMPDIR 下测试临时目录
	   会拿到短路径（ADMINI~1），跟实际写入的长路径字符串对不上。
	   改成验「真的落在名为 Desktop 的目录里 + 文件真存在」。 */
	if filepath.Base(filepath.Dir(p)) != "Desktop" {
		t.Errorf("落盘目录应为桌面，实际 %s", filepath.Dir(p))
	}
	if st, err := os.Stat(p); err != nil || st.IsDir() {
		t.Fatalf("返回的路径上没有文件：%s（%v）", p, err)
	}
	if !strings.HasSuffix(p, ".txt") {
		t.Errorf("导出的应是 .txt 文本，实际 %s", p)
	}

	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("导出的文件读不回来：%v", err)
	}
	s := string(b)
	if !strings.Contains(s, "【本机配置】") {
		t.Errorf("导出内容不像配置清单，内容开头：%.80s", s)
	}
	if strings.Contains(s, "读不到") {
		t.Errorf("导出里还有旧措辞「读不到」，口径应统一成「未获取到」：\n%s", s)
	}
	if strings.Contains(s, "机型") || strings.Contains(s, "机箱") {
		t.Errorf("导出里还有机箱/机型信息，用户已明确不需要：\n%s", s)
	}
	// 落盘后不留垃圾（文件是临时目录里的，删掉不碰用户桌面）
	os.Remove(p)
}

/* 源码哨兵：桌面路径必须读注册表，不许再靠拼 USERPROFILE。
   桌面被重定向到 D 盘 / 被 OneDrive 接管的机器（Ghost 与精简系统里很常见），
   只有 User Shell Folders 记的是真路径；硬拼出来的会落空或落到残留空目录。 */
func TestDesktopDirUsesRegistry(t *testing.T) {
	src := stripComments(readFileT(t, "pcshare.go"), "pcshare.go")
	if !strings.Contains(src, "User Shell Folders") {
		t.Error("desktopDir 又只拼 USERPROFILE 了：桌面重定向到别的盘的机器会落空，应读 User Shell Folders")
	}
}

/* 源码哨兵：导出位置必须走「用户自选」（SaveFileDialog）。

   用户 2026-10-10 明确要求「导出配置位置由客户自行选择保存位置」——
   上面那条用例验不到对话框（a.ctx == nil 时走的是直写桌面的降级路径），
   所以这里补一条源码级断言，防止以后有人图省事改回「直接写桌面」。 */
func TestExportPCInfoUsesSaveDialog(t *testing.T) {
	src := stripComments(readFileT(t, "pcshare.go"), "pcshare.go")
	if !strings.Contains(src, "SaveFileDialog") {
		t.Error("ExportPCInfo 没走 SaveFileDialog：用户要求导出位置由自己选，不许直接写桌面")
	}
	if !strings.Contains(src, `"CANCEL"`) {
		t.Error("ExportPCInfo 没处理用户取消（CANCEL）：取消不是失败，前端要静默处理")
	}
	/* 无 UI 上下文（单测 / 无头）时不能弹窗，必须有降级分支，否则回归测试会崩 */
	if !strings.Contains(src, "a.ctx != nil") {
		t.Error("ExportPCInfo 缺少 a.ctx == nil 的降级分支：无 UI 时 SaveFileDialog 会崩")
	}
}
