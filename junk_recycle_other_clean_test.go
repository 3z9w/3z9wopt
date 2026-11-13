package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ============================================================================
// 「其他账户回收站」删除引擎的实验室验证（REALCLEAN=1 才跑）。
//
// 🔴 绝不碰真实数据：测试自建一个假 SID 目录（本机账户列表里查无此人的合法孤儿 SID），
//    放几个假文件，验证护栏与删除行为，最后整个删掉测试目录。
//    真实存在的孤儿残留（本机 D 盘 577 MB）由用户在界面上自己决定清不清。
// ============================================================================

func TestLabRecycleOtherClean(t *testing.T) {
	if os.Getenv("REALCLEAN") != "1" {
		t.Skip("设 REALCLEAN=1 才跑（会真删文件，但只删本测试自建的假目录）")
	}
	const fakeSID = "S-1-5-21-1111111111-2222222222-3333333333-9999"
	bin := `D:\$Recycle.Bin`
	fake := filepath.Join(bin, fakeSID)

	// 开场自检：这个 SID 必须真的是孤儿（不在本机账户列表里），否则测试前提不成立
	prof := profileSIDs()
	if prof == nil {
		t.Fatal("读不到 ProfileList，无法验证")
	}
	if prof[fakeSID] {
		t.Fatalf("%s 竟然在本机账户列表里，测试前提不成立", fakeSID)
	}
	if !looksLikeUserSID(fakeSID) {
		t.Fatalf("%s 没通过标准用户 SID 校验", fakeSID)
	}
	if _, err := os.Stat(fake); err == nil {
		t.Fatalf("%s 已经存在，先手动确认再跑", fake)
	}

	// 造数据：$R 主文件、$I 元数据、desktop.ini（应保留）、子目录里的文件
	if err := os.MkdirAll(fake, 0777); err != nil {
		t.Fatalf("建假目录失败: %v", err)
	}
	defer os.RemoveAll(fake) // 测试结束把整个假目录拿走，不留垃圾
	const mainSize = 1 << 20
	if err := os.WriteFile(filepath.Join(fake, `$R000001.txt`), make([]byte, mainSize), 0644); err != nil {
		t.Fatalf("写假文件失败: %v", err)
	}
	if err := os.WriteFile(filepath.Join(fake, `$I000001.txt`), make([]byte, 544), 0644); err != nil {
		t.Fatalf("写元数据失败: %v", err)
	}
	if err := os.WriteFile(filepath.Join(fake, `desktop.ini`), []byte("[.ShellClassInfo]\r\n"), 0644); err != nil {
		t.Fatalf("写 desktop.ini 失败: %v", err)
	}
	sub := filepath.Join(fake, `$R000002.dir`)
	if err := os.MkdirAll(sub, 0777); err != nil {
		t.Fatalf("建子目录失败: %v", err)
	}
	if err := os.WriteFile(filepath.Join(sub, `inner.txt`), []byte("hello"), 0644); err != nil {
		t.Fatalf("写子文件失败: %v", err)
	}

	// ① 扫描侧：fake 必须被归进「孤儿」清单
	o := recycleOtherStats()
	if o.sizes[strings.ToLower(fake)] <= 0 {
		t.Fatalf("孤儿清单里没有假目录（sizes=%v）——孤儿判定或动态盘枚举有问题", o.sizes)
	}

	// ② 清理侧护栏：orphanRecycleDirs 也要列出来
	dirs, liveBytes, ok := orphanRecycleDirs()
	if !ok {
		t.Fatal("orphanRecycleDirs 报「账户列表读不到」，按设计一个都不许删")
	}
	found := false
	for _, d := range dirs {
		if strings.EqualFold(d, fake) {
			found = true
		}
		if filepath.VolumeName(d)+`\$Recycle.Bin` != filepath.Dir(d) {
			// 父目录必须是其所在盘自己的 $Recycle.Bin（盘符动态，不写死某个盘）
			t.Errorf("孤儿目录 %s 的父目录不是其所在盘的 $Recycle.Bin，路径护栏失效", d)
		}
	}
	if !found {
		t.Fatalf("orphanRecycleDirs 没列出假目录（dirs=%v）", dirs)
	}
	_ = liveBytes // 只用于日志，真实活账户残留不参与断言

	// ③ 删除引擎：文件该删的删掉，desktop.ini 和目录本身留下
	freed, files, skipped, err := clearRecycleSIDDir(fake)
	if err != nil {
		t.Fatalf("clearRecycleSIDDir 返回错误: %v（freed=%d files=%d skipped=%d）", err, freed, files, skipped)
	}
	if files != 3 {
		t.Errorf("删除文件数 = %d，期望 3（$R + $I + 子目录里的文件）", files)
	}
	if freed != mainSize+544+5 {
		t.Errorf("释放字节数 = %d，期望 %d", freed, mainSize+544+5)
	}
	if skipped != 0 {
		t.Errorf("跳过数 = %d，期望 0", skipped)
	}
	if _, err := os.Stat(fake); err != nil {
		t.Errorf("SID 目录本身被删掉了，违反「保留目录本身」的设计")
	}
	if _, err := os.Stat(filepath.Join(fake, `desktop.ini`)); err != nil {
		t.Errorf("desktop.ini 被删掉了，违反「保留 desktop.ini」的设计")
	}
	entries, _ := os.ReadDir(fake)
	if len(entries) != 1 {
		names := []string{}
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("删完目录里还剩 %d 项（%v），期望只剩 desktop.ini", len(entries), names)
	}
	t.Logf("实验室验证通过：freed=%d files=%d skipped=%d，目录与 desktop.ini 保留", freed, files, skipped)
}
