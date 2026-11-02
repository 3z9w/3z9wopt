package main

// ============================================================================
// 「哪些文件不能删」判断链的回归测试
//
// 2026-11-02 事故：磁盘空间页只按大小 + MD5 分组，把钉钉热更新双版本目录里
// 的 libcef.dll 判成「重复」，还按路径字母序把旧版当正本，删掉的是正在用的新版。
// 用户原话：「如果用户不知道这些文件能否删除，勾选删除了，那是不是对用户
// 也是一种毁灭性的打击和影响？」
//
// 这组测试用**纯路径字符串**验证静态判断链（ctx=nil，不依赖本机装没装这些软件），
// 再用真实临时文件验证「后端复核」与「正本选择」。谁把保护规则删了、或把
// 正本选择改回路径字母序，这里立刻红。
// ============================================================================

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func riskOf(path string) FileRisk { return assessPath(path, nil) }

func wantLevel(t *testing.T, path, level string) {
	t.Helper()
	r := riskOf(path)
	if r.Level != level {
		t.Fatalf("级别不对: %s\n  期望 %s，实际 %s（%s / %s）",
			path, level, r.Level, r.Tags[0], r.Reason)
	}
}

// 系统 / 引导 / 驱动 / NTFS 元文件：一律禁止
func TestRiskSystemFilesBlocked(t *testing.T) {
	for _, p := range []string{
		`C:\Windows\System32\drivers\tcpip.sys`,
		`C:\Windows\System32\kernel32.dll`,
		`C:\Windows\Boot\EFI\bootmgfw.efi`,
		`D:\pagefile.sys`,
		`D:\hiberfil.sys`,
		`C:\$MFT`,
		`C:\System Volume Information\restorepoint`,
		`C:\Windows\Fonts\msyh.ttc`,
		`D:\driver\foo.inf`,
	} {
		wantLevel(t, p, riskBlock)
	}
}

// 程序组件 / 安装目录：即使当前没在运行也锁
func TestRiskProgramComponentsHigh(t *testing.T) {
	for _, p := range []string{
		`D:\DingDing\main\current_new\plugins\dtwebview\libcef.dll`,
		`D:\DingDing\main\current\plugins\dtwebview\libcef.dll`,
		`D:\XRK\AweSun\AweSun.exe`,
		`C:\Program Files\Common Files\System\msadc\msadco.dll`,
		`D:\proj\node_modules\pkg\index.node`,
		`C:\Users\Me\AppData\Local\Programs\Python\python.exe`,
	} {
		wantLevel(t, p, riskHigh)
	}
}

// 游戏资源 / 存档：删了下次进不去、要整包重下
func TestRiskGameFilesHigh(t *testing.T) {
	for _, p := range []string{
		`D:\Steam\steamapps\common\Foo\Content\Paks\chunk0.pak`,
		`D:\Games\Foo\Binaries\Win64\Foo-Win64-Shipping.exe`,
		`D:\Games\Foo\Saved\SaveGames\slot1.sav`,
		`D:\Games\Foo\Content\Maps\level.umap`,
		`D:\Steam\steamapps\appmanifest_123.acf`,
	} {
		wantLevel(t, p, riskHigh)
	}
}

// 云同步 / 密钥凭据 / 版本库 / 虚拟机 / 邮箱：后果不可逆，一律禁止
func TestRiskIrreplaceableDataBlocked(t *testing.T) {
	for _, p := range []string{
		`C:\Users\Me\OneDrive\工作\合同.pdf`,
		`D:\百度网盘\我的资源\照片.zip`,
		`C:\Users\Me\.ssh\id_rsa`,
		`C:\Users\Me\Documents\钱包\wallet.dat`,
		`C:\Users\Me\Documents\passwords.kdbx`,
		`D:\proj\.git\objects\ab\cdef`,
		`D:\VM\win11\disk.vmdk`,
		`C:\Users\Me\Documents\Outlook\mail.pst`,
	} {
		wantLevel(t, p, riskBlock)
	}
}

// 浏览器 / 通讯软件用户数据、备份、源码：默认锁
func TestRiskUserDataHigh(t *testing.T) {
	for _, p := range []string{
		`C:\Users\Me\AppData\Local\Google\Chrome\User Data\Default\Login Data`,
		`D:\Documents\WeChat Files\wxid_xxx\Msg\MSG0.db`,
		`D:\Backup\系统镜像.wim`,
		`D:\workspace\myproj\main.sln`,
	} {
		wantLevel(t, p, riskHigh)
	}
}

// 真缓存 / 临时文件可删；但组件型扩展名即使在缓存目录也不能降级
func TestRiskCacheRules(t *testing.T) {
	wantLevel(t, `C:\Users\Me\AppData\Local\Temp\scratch.tmp`, riskSafe)
	wantLevel(t, `D:\App\Cache\shader.log`, riskSafe)
	wantLevel(t, `D:\App\Cache\libcef.dll`, riskHigh) // 不能因为带 cache 就放行
	wantLevel(t, `D:\Game\Shaders\chunk.pak`, riskHigh)
}

// 普通个人文件可勾选，但必须是 caution（明说不可恢复）
func TestRiskPersonalFilesCaution(t *testing.T) {
	wantLevel(t, `D:\Downloads\movie.mp4`, riskCaution)
	wantLevel(t, `D:\Downloads\movie (1).mp4`, riskCaution)
	wantLevel(t, `D:\Desktop\report.docx`, riskCaution)
	wantLevel(t, `D:\Misc\unknown.xyz`, riskCaution)
}

func TestRiskDriveRootAndUNC(t *testing.T) {
	wantLevel(t, `C:\`, riskBlock)
	wantLevel(t, `\\NAS\share\file.zip`, riskHigh)
}

// 核心回归：同组里「越不能删的那份」必须被保留为正本，
// 而不是原来的「路径字母序第一份」。
func TestChooseKeepPrefersProtectedCopy(t *testing.T) {
	rs := []FileRisk{
		newRisk(`D:\Downloads\backup\movie.mp4`, riskCaution, "个人文件", "", false),
		newRisk(`D:\DingDing\main\current_new\plugins\libcef.dll`, riskHigh, "程序组件", "", false),
	}
	if k := chooseKeep(rs); k != 1 {
		t.Fatalf("应保留程序组件那份（下标 1），实际选了 %d —— 又回到路径字母序了", k)
	}
}

// 钉钉场景：两份都是组件，正在被加载的那份必须留。
func TestChooseKeepPrefersInUse(t *testing.T) {
	rs := []FileRisk{
		newRisk(`D:\DingDing\main\current\plugins\libcef.dll`, riskHigh, "程序组件", "", false),
		newRisk(`D:\DingDing\main\current_new\plugins\libcef.dll`, riskHigh, "程序组件", "", true),
	}
	if k := chooseKeep(rs); k != 1 {
		t.Fatalf("应保留正在使用的新版（下标 1），实际选了 %d", k)
	}
}

// 软件没在运行、两份都锁时，靠修改时间挑较新的版本当正本。
func TestChooseKeepPrefersNewerVersion(t *testing.T) {
	dir := t.TempDir()
	old := filepath.Join(dir, `current`, `plugins`)
	nw := filepath.Join(dir, `current_new`, `plugins`)
	if err := os.MkdirAll(old, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(nw, 0o755); err != nil {
		t.Fatal(err)
	}
	po, pn := filepath.Join(old, `libcef.dll`), filepath.Join(nw, `libcef.dll`)
	for _, p := range []string{po, pn} {
		if err := os.WriteFile(p, []byte("same"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	t0 := time.Now().Add(-48 * time.Hour)
	t1 := time.Now()
	if err := os.Chtimes(po, t0, t0); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(pn, t1, t1); err != nil {
		t.Fatal(err)
	}
	g := DupGroup{Size: 4, Paths: []string{po, pn}}
	groups := []DupGroup{g}
	assessDupGroups(groups)
	got := &groups[0]
	if len(got.Risks) != 2 {
		t.Fatalf("应产出 2 条风险，实际 %d", len(got.Risks))
	}
	if got.Keep != 1 {
		t.Fatalf("应保留较新的 current_new（下标 1），实际选了 %d", got.Keep)
	}
	if !got.Risks[got.Keep].Keep {
		t.Fatal("正本的 Keep 标记没设上")
	}
}

// 后端复核：block 永不删，high 默认拒、解锁后才放行。
// 前端的禁用勾选只是交互，绑定能被直接调用，闸门必须在后端。
func TestSpaceDeleteRiskGate(t *testing.T) {
	var a App
	blocked := `C:\Windows\System32\drivers\never-delete.sys`
	res := a.SpaceDelete([]string{blocked})
	if len(res) != 1 || res[0].Ok || !strings.Contains(res[0].Err, "已阻止删除") {
		t.Fatalf("系统文件必须被后端拒绝: %+v", res)
	}
	res2 := a.SpaceDeleteUnlocked([]string{blocked})
	if len(res2) != 1 || res2[0].Ok {
		t.Fatalf("block 即使解锁也不能删: %+v", res2)
	}

	// 造一个「看起来在程序组件目录」的真实文件，验证默认拒 / 解锁放行
	dir := t.TempDir()
	plug := filepath.Join(dir, `plugins`)
	if err := os.MkdirAll(plug, 0o755); err != nil {
		t.Fatal(err)
	}
	f := filepath.Join(plug, `component.dll`)
	if err := os.WriteFile(f, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if r := riskOf(f); r.Level != riskHigh {
		t.Fatalf("测试前提不成立：该文件应判 high，实际 %s", r.Level)
	}
	if r := a.SpaceDelete([]string{f}); r[0].Ok {
		t.Fatal("默认档不该删掉 high 文件")
	}
	if _, err := os.Stat(f); err != nil {
		t.Fatal("默认档拒绝后文件必须还在")
	}
	if r := a.SpaceDeleteUnlocked([]string{f}); !r[0].Ok {
		t.Fatalf("解锁档应能删掉 high 文件: %s", r[0].Err)
	}
	if _, err := os.Stat(f); !os.IsNotExist(err) {
		t.Fatal("解锁后文件应真的消失")
	}
}

// SpaceAssess 批量评估：条数一一对应，供目录分析 / 大文件页删除前调用。
func TestSpaceAssessBatch(t *testing.T) {
	var a App
	paths := []string{`C:\Windows\notepad.exe`, `D:\Downloads\a.mp4`, `D:\Temp\x.tmp`}
	rs := a.SpaceAssess(paths)
	if len(rs) != len(paths) {
		t.Fatalf("应返回 %d 条，实际 %d", len(paths), len(rs))
	}
	for i := range rs {
		if rs[i].Path != paths[i] || rs[i].Level == "" || len(rs[i].Tags) == 0 {
			t.Fatalf("第 %d 条结果不完整: %+v", i, rs[i])
		}
	}
}
