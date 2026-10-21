package main

import (
	"os"
	"strings"
	"testing"
	"time"
	"unicode/utf16"
)

func TestParseIconRef(t *testing.T) {
	cases := []struct {
		in   string
		exe  string
		idx  int
		want bool
	}{
		{`"C:\Program Files\A\a.exe",0`, `C:\Program Files\A\a.exe`, 0, true},
		{`C:\Windows\system32\shell32.dll,-152`, `C:\Windows\system32\shell32.dll`, -152, true},
		{``, ``, 0, false},
		{`notexist`, ``, 0, false},
	}
	for _, c := range cases {
		exe, idx := parseIconRef(c.in)
		if exe == "" {
			if c.want {
				t.Fatalf("parseIconRef(%q) 应有结果", c.in)
			}
			continue
		}
		if exe != c.exe || idx != c.idx {
			t.Fatalf("parseIconRef(%q) = (%q,%d)，期望 (%q,%d)", c.in, exe, idx, c.exe, c.idx)
		}
	}
	exe, _ := parseIconRef(`%SystemRoot%\system32\notepad.exe`)
	if strings.Contains(exe, "%") || !strings.HasSuffix(strings.ToLower(exe), `\system32\notepad.exe`) {
		t.Fatalf("环境变量型图标路径未展开: %s", exe)
	}
}

func TestExpandWinEnv(t *testing.T) {
	got := expandWinEnv(`%SystemRoot%\system32`)
	if strings.Contains(got, "%") {
		t.Fatalf("环境变量未展开: %s", got)
	}
	if !strings.HasSuffix(strings.ToLower(got), `\system32`) {
		t.Fatalf("展开结果异常: %s", got)
	}
}

func TestBuildUninstallCmdMsi(t *testing.T) {
	exe, args, name, ok := buildUninstallCmd(`MsiExec.exe /I{4B4B0A6F-1111-2222-3333-444444444444}`)
	if !ok || exe != "msiexec.exe" {
		t.Fatalf("MSI 卸载命令解析失败: %s %v", exe, args)
	}
	if len(args) == 0 || !strings.HasPrefix(args[0], "/X{") {
		t.Fatalf("MSI /I 应改写为 /X，实际: %v", args)
	}
	if name != "msiexec.exe" {
		t.Fatalf("进程名错误: %s", name)
	}
}

func TestBuildUninstallCmdPlain(t *testing.T) {
	exe, args, _, ok := buildUninstallCmd(`"C:\Windows\notepad.exe" /x`)
	if !ok {
		t.Fatal("普通命令解析失败")
	}
	if !strings.EqualFold(exe, `C:\Windows\notepad.exe`) {
		t.Fatalf("exe 解析错误: %s", exe)
	}
	if len(args) != 1 || args[0] != "/x" {
		t.Fatalf("参数解析错误: %v", args)
	}
}

func TestParseInstallDate(t *testing.T) {
	if v := parseInstallDate("20260920"); v == 0 {
		t.Fatal("yyyymmdd 格式解析失败")
	}
	if v := parseInstallDate("2026-09-01"); v == 0 {
		t.Fatal("yyyy-mm-dd 格式解析失败")
	}
	if v := parseInstallDate(""); v != 0 {
		t.Fatal("空值应为 0")
	}
	if v := parseInstallDate("19800101"); v != 0 {
		t.Fatal("过早日期应被丢弃")
	}
}

func TestKwCleanFilters(t *testing.T) {
	kws := kwClean("Microsoft Visual C++ 2015 Redistributable (x64) - 14.0")
	for _, k := range kws {
		if k == "microsoft" || k == "x64" || k == "the" {
			t.Fatalf("停用词未被过滤: %v", kws)
		}
	}
	has := false
	for _, k := range kws {
		if k == "redistributable" {
			has = true
		}
	}
	if !has {
		t.Fatalf("有效关键词丢失: %v", kws)
	}
	if len(kwClean("微信")) == 0 {
		t.Fatal("中文名应保留")
	}
}

func TestAppBlockReason(t *testing.T) {
	e := &AppEntry{Name: "Microsoft Visual C++ 2015-2022 Redistributable (x64)"}
	if appBlockReason(e) == "" {
		t.Fatal("运行库应被加入不可卸载黑名单")
	}
	e2 := &AppEntry{Name: "网易云音乐"}
	if appBlockReason(e2) != "" {
		t.Fatal("普通软件不应被拦截")
	}
}

func TestDirUsable(t *testing.T) {
	if dirUsable(`C:\`) {
		t.Fatal("磁盘根目录不应被视为可扫描目录")
	}
	if dirUsable(`C:\Windows`) {
		t.Fatal("系统目录不应被视为可扫描目录")
	}
	if dirUsable("") || dirUsable(`C:\a`) {
		t.Fatal("空/不存在的目录不应通过")
	}
}

func TestEnumerateWin32NoPanic(t *testing.T) {
	list := enumerateWin32()
	if len(list) == 0 {
		t.Fatal("本机应至少枚举出 1 个已安装程序")
	}
	n := 0
	for _, e := range list {
		if e.ID == "" || e.Name == "" {
			t.Fatal("枚举结果存在空 ID/名称")
		}
		if e.Kind != "win32" {
			t.Fatalf("kind 字段错误: %s", e.Kind)
		}
		if e.SizeSrc != "reg" && e.SizeSrc != "none" && e.SizeSrc != "dir" {
			t.Fatalf("sizeSrc 取值非法: %s", e.SizeSrc)
		}
		n++
		if n > 5 {
			break
		}
	}
}

func TestLoadIconPNGReal(t *testing.T) {
	b, ok := loadIconPNG(`C:\Windows\System32\notepad.exe`, 0)
	if !ok || len(b) < 100 {
		t.Fatalf("notepad.exe 图标提取失败: ok=%v len=%d", ok, len(b))
	}
	if b[0] != 0x89 || b[1] != 'P' || b[2] != 'N' || b[3] != 'G' {
		t.Fatal("产物不是 PNG")
	}
}

func TestHiconPNGReal(t *testing.T) {
	// shell32.dll 图标 0（大图标）
	b, ok := loadIconPNG(`C:\Windows\System32\shell32.dll`, 0)
	if !ok || len(b) < 100 {
		t.Fatalf("shell32.dll 图标提取失败: ok=%v len=%d", ok, len(b))
	}
}

func TestAppIconDataURL(t *testing.T) {
	a := &App{}
	// 先验证一个肯定存在的图标 key
	e := enumerateWin32()
	found := ""
	for _, x := range e {
		if x.IconKey != "" {
			found = x.IconKey
			break
		}
	}
	if found == "" {
		t.Skip("本机没有可提取图标的程序")
	}
	url := a.GetAppIcon(found)
	if url == "" || len(url) < 40 {
		t.Fatalf("GetAppIcon(%s) 返回空/过短 (len=%d)", found, len(url))
	}
	if !strings.HasPrefix(url, "data:image/png;base64,") {
		t.Fatalf("不是 data URL: %.40s", url)
	}
}

func TestResidueRiskScoring(t *testing.T) {
	strong := []string{"netease"}
	weak := []string{"cloudmusic"}
	r, _ := residueRisk(`HKCU\SOFTWARE\Netease\CloudMusic`, strong, weak)
	if r != "high" {
		t.Fatalf("强关键词应判为 high，实际 %s", r)
	}
	r2, _ := residueRisk(`C:\ProgramData\CloudMusic`, strong, weak)
	if r2 != "mid" {
		t.Fatalf("弱关键词应判为 mid，实际 %s", r2)
	}
	r3, _ := residueRisk(`C:\ProgramData\Whatever`, strong, weak)
	if r3 != "low" {
		t.Fatalf("无命中应判为 low，实际 %s", r3)
	}
}

// ---------- UWP 注册表枚举（2026-09-21：精简系统 PowerShell 挂了导致内置应用列表为空） ----------

// TestBuildUwpEntriesMarksSys PowerShell 路线产出的条目必须：① 标 Sys
// （前端「系统内置（N）」页签数的是它，曾经被写死 false 导致该页签恒为 0）；
// ② 把不可卸载的系统组件与运行库滤掉，别混进「内置应用」列表里。
func TestBuildUwpEntriesMarksSys(t *testing.T) {
	base := func() uwpRaw {
		return uwpRaw{
			Name:              "Microsoft.WindowsCalculator",
			PackageFullName:   "Microsoft.WindowsCalculator_11.2_arm64__8wekyb3d8bbwe",
			PackageFamilyName: "Microsoft.WindowsCalculator_8wekyb3d8bbwe",
			Publisher:         "CN=Microsoft Corporation, O=Microsoft Corporation, L=Redmond, S=Washington, C=US",
			Version:           "11.2.0.0",
			NonRemovable:      false,
			SignatureKind:     0,
		}
	}

	ok := base()
	got := buildUwpEntries([]uwpRaw{ok})
	if len(got) != 1 {
		t.Fatalf("普通商店应用应产出 1 条，实际 %d 条", len(got))
	}
	e := got[0]
	if !e.Sys {
		t.Fatal("Sys 未置真：前端「系统内置」页签会显示 0（A1 回归）")
	}
	if e.Kind != "uwp" || e.Name != "计算器" || e.PFN != ok.PackageFullName {
		t.Fatalf("条目字段不符：kind=%s name=%s pfn=%s", e.Kind, e.Name, e.PFN)
	}
	if !e.Removable {
		t.Fatal("可卸载的商店应用不应被标成不可卸载")
	}
	if e.Publisher != "Microsoft Corporation" {
		t.Fatalf("发布者未从 CN= 里解析出来：%s", e.Publisher)
	}
	if p := psPublisher("CN=Contoso, O=Contoso, C=CN"); p != "Contoso" {
		t.Fatalf("psPublisher 解析异常：%s", p)
	}

	// 三种"不该进列表"的包，逐个单独喂进去，确保各自被自己的那条规则滤掉
	cases := []struct {
		name string
		mut  func(*uwpRaw)
	}{
		{"不可卸载的系统组件", func(r *uwpRaw) { r.NonRemovable = true }},
		{"系统签名的包(SignatureKind=4)", func(r *uwpRaw) { r.SignatureKind = 4 }},
		{"共享框架包", func(r *uwpRaw) { r.IsFramework = true }},
		{"没有包全名", func(r *uwpRaw) { r.PackageFullName = "" }},
	}
	for _, c := range cases {
		r := base()
		c.mut(&r)
		if out := buildUwpEntries([]uwpRaw{r}); len(out) != 0 {
			t.Fatalf("%s 不应进可管理列表，实际列出 %d 条", c.name, len(out))
		}
	}
}

func TestUwpVerLess(t *testing.T) {
	if !uwpVerLess("1.29.290.0", "2026.728.1707.0") {
		t.Fatal("1.29 应小于 2026.728")
	}
	if uwpVerLess("2.0", "1.9.9") {
		t.Fatal("2.0 不应小于 1.9.9")
	}
	if uwpVerLess("1.0.0", "1.0.0") {
		t.Fatal("相等版本不应判小于")
	}
	if !uwpVerLess("1.0", "1.0.1") {
		t.Fatal("短版本号应判小于")
	}
}

// TestFetchUwpAppsRegReal 本机实跑：PS 挂了也必须能从注册表枚举出可管理的内置应用
func TestFetchUwpAppsRegReal(t *testing.T) {
	a := &App{}
	apps := fetchUwpAppsReg(a)
	if len(apps) == 0 {
		t.Fatal("注册表枚举结果为空（本机 HKCU Repository 应有已注册的商店应用）")
	}
	sysRoot := strings.ToLower(os.Getenv("SystemRoot"))
	for i, e := range apps {
		if e.Kind != "uwp" || !strings.HasPrefix(e.ID, "u.") || e.PFN == "" || e.Name == "" {
			t.Fatalf("第 %d 项字段异常: %+v", i, e)
		}
		// 2026-10-09 修 A1 的回归哨兵：前端「系统内置（N）」页签数的是 Sys，
		// 而这里曾经被写死 false，导致本机 62 个软件里内置恒为 0。两条枚举路线都必须置真。
		if !e.Sys {
			t.Fatalf("注册表枚举的 UWP 条目没标 Sys（前端「系统内置」会显示 0）: %s", e.Name)
		}
		low := strings.ToLower(e.Name)
		if strings.Contains(low, "@{") {
			t.Fatalf("显示名未解析的资源引用漏网: %s", e.Name)
		}
		for _, fw := range uwpRegFramework {
			if strings.Contains(low, fw) {
				t.Fatalf("运行库漏网: %s", e.Name)
			}
		}
		if strings.HasPrefix(strings.ToLower(e.InstallLoc), sysRoot+`\`) {
			t.Fatalf("系统组件漏网: %s -> %s", e.Name, e.InstallLoc)
		}
		if i > 50 {
			break
		}
	}
	// 同族去重：family 不应出现重复
	seen := map[string]bool{}
	for _, e := range apps {
		parts := strings.Split(e.PFN, "_")
		fam := strings.ToLower(parts[0] + "_" + parts[len(parts)-1])
		if seen[fam] {
			t.Fatalf("同族包重复出现: %s", e.PFN)
		}
		seen[fam] = true
	}
	t.Logf("注册表枚举出 %d 个可管理内置应用", len(apps))
}

// TestFetchUwpAppsFallback 端到端：PS 失败时 fetchUwpApps 必须走注册表回退并返回非空
func TestFetchUwpAppsFallback(t *testing.T) {
	apps := fetchUwpApps()
	if len(apps) == 0 {
		t.Fatal("fetchUwpApps 返回空（PS 失败后应回退注册表枚举）")
	}
	t.Logf("fetchUwpApps 共 %d 个（PS 失败时来自注册表回退）", len(apps))
}

// TestUwpIconsResolvable 实机回归：UWP 条目的图标解析成功率必须 ≥80%。
// 2026-09-21 踩坑：新版商店包用自家品牌命名（如 CalculatorAppList.*），
// 只认 Square44x44Logo 通用名时 36/53 个图标全空 —— 本测试守的就是这类大面积失败。
// 机器自适应：不同电脑装的包不同，个别桥接包解析不出属正常，不再要求 100%。
func TestUwpIconsResolvable(t *testing.T) {
	apps := fetchUwpApps()
	if len(apps) == 0 {
		t.Skip("本机无 UWP 包可测")
	}
	a := &App{}
	var failed []string
	for _, e := range apps {
		if e.IconKey == "" {
			t.Errorf("UWP 条目缺 IconKey: %s", e.Name)
			continue
		}
		if a.GetAppIcon(e.IconKey) == "" {
			failed = append(failed, e.Name+" key="+e.IconKey)
		}
	}
	rate := 1 - float64(len(failed))/float64(len(apps))
	if rate < 0.8 {
		t.Fatalf("图标解析成功率 %.0f%% 低于 80%%（共 %d 个，失败 %d 个）:\n  %s",
			rate*100, len(apps), len(failed), strings.Join(failed, "\n  "))
	}
	t.Logf("UWP 图标解析 %d/%d 成功", len(apps)-len(failed), len(apps))
	if len(failed) > 0 {
		t.Logf("未解析出的图标（正常容忍）：%v", failed)
	}
}

func TestDecodeMaybeUTF16(t *testing.T) {
	in := "Windows PowerShell 内部错误 80070002"
	// 手工编码成 UTF-16LE 字节串
	uu := utf16.Encode([]rune(in))
	b := make([]byte, 0, len(uu)*2)
	for _, r := range uu {
		b = append(b, byte(r), byte(r>>8))
	}
	if got := decodeMaybeUTF16(string(b)); got != in {
		t.Fatalf("UTF-16 解码不符: %q", got)
	}
	if got := decodeMaybeUTF16(in); got != in {
		t.Fatalf("纯 UTF-8 不应被改写: %q", got)
	}
}

// TestUwpRemoveWinRTNotFound 免 PS 移除链路回归：假包名调部署引擎，
// 必须返回 0x80073CF1（包不存在）——验证整条 WinRT 调用链，不碰任何真实应用。
// 2026-09-21 实测：精简系统 PowerShell 挂死时此路线是唯一可用移除手段。
func TestUwpRemoveWinRTNotFound(t *testing.T) {
	hr, err := removeUwpViaWinRT("Microsoft.DoesNotExist.Test_1.0.0.0_x64__zzzzzzzzzzzzz", 60*time.Second)
	if err == nil {
		t.Fatal("假包名不应移除成功")
	}
	if hr != 0x80073CF1 {
		t.Fatalf("期望 0x80073CF1（包不存在），实际 0x%08X err=%v", hr, err)
	}
	if !isDeployResultHR(hr) {
		t.Fatalf("0x%08X 应被判定为部署引擎错误（不走 PS 回退）", hr)
	}
	if isDeployResultHR(0x80040154) {
		t.Fatal("COM 激活类错误不应被判为部署错误")
	}
	t.Logf("免 PS 移除链路验证通过：0x%08X %v", hr, err)
}

// TestCalcAppSizeMarksMeasured 的回归哨兵（2026-10-09 修 A2）：
// 界面上「实测」计数恒为 0 的病根是——CalcAppSize 写好了但没人调用。
// 这里盯两件事：
//   1. 真实装着的程序 → 实测后必须把 sizeSrc 从「申报/未知」改成 dir，且 sizePending 转 false
//   2. 读不到的目录 → readable=false 且原标注保持不动（绝不把「未知」谎报成「实测 0 GB」）
func TestCalcAppSizeMarksMeasured(t *testing.T) {
	a := &App{}
	apps := a.GetWin32Apps()
	if len(apps) == 0 {
		t.Skip("本机没有枚举到桌面软件")
	}
	target := -1
	for i := range apps {
		if apps[i].InstallLoc == "" || apps[i].SizeSrc == "dir" {
			continue
		}
		if dirUsable(apps[i].InstallLoc) {
			target = i
			break
		}
	}
	if target < 0 {
		t.Skip("没有待实测的候选（都已实测或都读不到）")
	}
	id := apps[target].ID
	res := a.CalcAppSize(id)
	if !res["readable"].(bool) {
		t.Fatalf("可读目录应返回 readable=true：%s -> %v", id, res)
	}
	e := lookupApp(id)
	if e == nil {
		t.Fatal("实测后缓存里找不到该条目")
	}
	if e.SizeSrc != "dir" {
		t.Fatalf("实测完 sizeSrc 应为 dir，实际 %s（前端「实测」计数不会涨）", e.SizeSrc)
	}
	if e.SizePending {
		t.Fatal("实测完 sizePending 应转 false")
	}
	if e.Bytes <= 0 || e.Files <= 0 {
		t.Fatalf("实测结果异常：bytes=%d files=%d", e.Bytes, e.Files)
	}
	t.Logf("实测通过：%s = %.2f GB / %d 个文件", e.Name, float64(e.Bytes)/1e9, e.Files)

	// 拿不到的目录：不能把未知冒充实测（"不编造数据"红线）
	fake := AppEntry{ID: "w.fake-not-exists", Kind: "win32", Name: "不存在的应用", InstallLoc: `C:/__no_such_dir__/x`, SizeSrc: "none"}
	appMu.Lock()
	if appCache == nil {
		appCache = map[string]*AppEntry{}
	}
	appCache[fake.ID] = &fake
	appMu.Unlock()
	bad := a.CalcAppSize(fake.ID)
	if bad["readable"].(bool) {
		t.Fatal("不可读目录不应标 readable=true")
	}
	if fake.SizeSrc != "none" {
		t.Fatalf("读不到时 sizeSrc 应保持 none，不能谎报成 %s", fake.SizeSrc)
	}
}
