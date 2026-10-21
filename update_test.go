package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

/* 更新检查 / 校验 / 下载 的单测（2026-10-13）。
   两个必须守住的点：
     ① 版本比较不能按字符串 —— "1.10.0" < "1.9.0" 会让人永远收不到更新；
     ② 网络拉不到 ≠ 已是最新，必须回落成带原因的失败，不许把"没读到"演成"没问题"。 */

func TestVerLess(t *testing.T) {
	cases := []struct {
		a, b string
		want bool // a 比 b 旧？
	}{
		{"1.1.0", "1.2.0", true},
		{"1.2.0", "1.1.0", false},
		{"1.1.0", "1.1.0", false},
		{"1.10.0", "1.9.0", false}, // ← 字符串比较会判成 true，这条是关键
		{"1.9.0", "1.10.0", true},  // ← 字符串比较会判成 false
		{"1.1", "1.1.0", false},    // 段数不齐补 0
		{"1.1.0", "1.1", false},
		{"1.0.0", "2.0.0", true},
		{"v1.1.0", "1.2.0", true},    // v 前缀
		{"1.1.0-dev", "1.1.0", true}, // 预发布 < 正式
		{"1.1.0", "1.1.0-dev", false},
		{"1.1.0-dev", "1.2.0", true},
	}
	for _, c := range cases {
		if got := verLess(c.a, c.b); got != c.want {
			t.Errorf("verLess(%q, %q) = %v，想要 %v", c.a, c.b, got, c.want)
		}
	}
}

func TestManifestValidate(t *testing.T) {
	sha := strings.Repeat("a", 64)
	ok := updateManifest{Version: "1.2.0", URL: "https://opt.3z9w.com/download/x.exe", SHA256: sha}
	if err := ok.validate(); err != nil {
		t.Fatalf("合法清单被拒：%v", err)
	}
	bad := []updateManifest{
		{URL: ok.URL, SHA256: sha},                     // 没版本号
		{Version: "1.2.0", SHA256: sha},                // 没下载地址
		{Version: "1.2.0", URL: ok.URL},                // 没指纹
		{Version: "1.2.0", URL: ok.URL, SHA256: "abc"}, // 指纹长度不对
	}
	for i, m := range bad {
		if err := m.validate(); err == nil {
			t.Errorf("第 %d 条不合法清单居然通过了", i+1)
		}
	}
}

/*
构建号比较（2026-10-19）。

	版本号不再承担"第几次发布"，改由构建号担：同版本号重建也要能让老用户收到更新。
	两个必须守住的点：按数字比（.10 > .9 而不是字符串的 ".10" < ".9"）、
	没有构建号的一方视为最旧但**不触发**更新（老清单没有 build 字段）。
*/
func TestBuildLess(t *testing.T) {
	cases := []struct {
		a, b string
		want bool
	}{
		{"20261019.1", "20261019.2", true},
		{"20261019.2", "20261019.1", false},
		{"20261019.9", "20261019.10", true}, // ← 字符串比较会判成 false，这条是关键
		{"20261019.10", "20261019.9", false},
		{"20261018.9", "20261019.1", true}, // 昨天再多次也比今天旧
		{"20261019.1", "20261019", false},  // 只写日期 = 当天第 0 次
		{"20261019", "20261019.1", true},
		{"", "20261019.1", true},  // 自己没有构建号（老客户端）→ 视为最旧，推一次
		{"20261019.1", "", false}, // 对方没有构建号（老清单）→ 不比，别一直提示更新
		{"garbage", "20261019.1", true},
	}
	for _, c := range cases {
		if got := buildLess(c.a, c.b); got != c.want {
			t.Errorf("buildLess(%q, %q) = %v，想要 %v", c.a, c.b, got, c.want)
		}
	}
}

func TestOfficialEntries(t *testing.T) {
	top := strings.Repeat("1", 64)
	old := strings.Repeat("2", 64)
	older := strings.Repeat("3", 64)
	m := updateManifest{
		Version: "1.2.0",
		Build:   "20261101.1",
		SHA256:  top,
		History: []updateOldFile{
			{Version: "1.1.0", Build: "20261019.2", SHA256: old},
			{Version: "1.1.0", Build: "20261016.1", SHA256: older},
		},
	}
	// 最新版：顶层那一条
	if g := officialEntries(m, "1.2.0"); len(g) != 1 || g[0].SHA256 != top {
		t.Errorf("最新版应该只取顶层 sha256，拿到 %+v", g)
	}
	// 同一版本号重建过多次 → 全部保留，不是只剩一条（这是本次改动的核心）
	if g := officialEntries(m, "1.1.0"); len(g) != 2 {
		t.Fatalf("1.1.0 应该取到 2 条指纹，拿到 %d 条", len(g))
	}
	// 服务器没记这个版本 → 空，调用方必须按「无法核对」处理，不能当成校验失败
	if g := officialEntries(m, "1.0.0"); len(g) != 0 {
		t.Errorf("没有记录的版本应该返回空，拿到 %+v", g)
	}
	// 记了但指纹是坏的 → 同样算"没法核对"，不能拿去比对
	m2 := updateManifest{Version: "1.2.0", SHA256: "short", History: []updateOldFile{{Version: "1.1.0", SHA256: "x"}}}
	if g := officialEntries(m2, "1.1.0"); len(g) != 0 {
		t.Errorf("坏指纹不该被当成官方值，拿到 %+v", g)
	}
}

// 起一个假服务器当发布站，把清单和包都放上去
func fakeServer(t *testing.T, m updateManifest, pkg []byte, pkgPath string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/version.json":
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(m)
		case pkgPath:
			_, _ = w.Write(pkg)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func useFakeServer(t *testing.T, srv *httptest.Server) {
	t.Helper()
	updateManifestURLOverride = srv.URL + "/version.json"
	updateAllowInsecureForTest = true
	updateTempDirOverride = t.TempDir()
	t.Cleanup(func() {
		updateManifestURLOverride = ""
		updateAllowInsecureForTest = false
		updateTempDirOverride = ""
	})
}

func TestCheckUpdateStates(t *testing.T) {
	// 源码里 appVersion 默认是 "1.1.0-dev"，那个值会直接走 dev 分支不查更新；
	// 这里临时装成正式版，才能验真正的比对逻辑
	oldVer := appVersion
	appVersion = "1.1.0"
	defer func() { appVersion = oldVer }()

	sha := strings.Repeat("a", 64)
	pkg := make([]byte, updateMinBytes+1024)
	srv := fakeServer(t, updateManifest{
		Version: "9.9.9", BuildDate: "2026-11-01", Notes: []string{"新东西"},
		URL: "", SHA256: sha,
	}, pkg, "/dl.exe")
	useFakeServer(t, srv)

	// ① 清单缺 url → badmanifest（不能悄悄当成"已是最新"）
	a := NewApp()
	r := a.CheckUpdate()
	if r.State != "badmanifest" {
		t.Errorf("缺 url 的清单应该判 badmanifest，拿到 %q（message=%s）", r.State, r.Message)
	}
	if r.Message == "" {
		t.Error("badmanifest 必须带可读原因")
	}

	// ② 完整清单 + 服务器版本更高 → new
	srv2 := fakeServer(t, updateManifest{
		Version: "9.9.9", BuildDate: "2026-11-01", Notes: []string{"新东西"},
		URL: srv.URL + "/dl.exe", SHA256: sha,
	}, pkg, "/dl.exe")
	updateManifestURLOverride = srv2.URL + "/version.json"
	r = a.CheckUpdate()
	if r.State != "new" || r.Latest != "9.9.9" {
		t.Errorf("应该判 new / 9.9.9，拿到 %q / %q（%s）", r.State, r.Latest, r.Message)
	}
	if r.Current == "" {
		t.Error("结果里必须带本地版本，界面要显示「当前 → 最新」")
	}

	// ③ 服务器版本跟本地一样 → latest
	srv3 := fakeServer(t, updateManifest{
		Version: appVersion, URL: srv.URL + "/dl.exe", SHA256: sha,
	}, pkg, "/dl.exe")
	updateManifestURLOverride = srv3.URL + "/version.json"
	if r := a.CheckUpdate(); r.State != "latest" {
		t.Errorf("同版本应该判 latest，拿到 %q", r.State)
	}

	/* ③b 版本号一样、构建号更新 → 仍然是 new（2026-10-19 新增：
	   改个功能不必动版本号，重建发布靠构建号让老用户收到更新） */
	oldBuild := buildNum
	buildNum = "20261019.1"
	defer func() { buildNum = oldBuild }()
	srv3b := fakeServer(t, updateManifest{
		Version: appVersion, Build: "20261019.2", URL: srv.URL + "/dl.exe", SHA256: sha,
	}, pkg, "/dl.exe")
	updateManifestURLOverride = srv3b.URL + "/version.json"
	if r := a.CheckUpdate(); r.State != "new" {
		t.Errorf("同版本号但构建号更新，应该判 new，拿到 %q", r.State)
	}

	// ③c 清单没 build 字段（老格式）→ 不能因为这条误判 new
	srv3c := fakeServer(t, updateManifest{
		Version: appVersion, URL: srv.URL + "/dl.exe", SHA256: sha,
	}, pkg, "/dl.exe")
	updateManifestURLOverride = srv3c.URL + "/version.json"
	if r := a.CheckUpdate(); r.State != "latest" {
		t.Errorf("老格式清单（无 build）应该判 latest，拿到 %q", r.State)
	}

	// ③d 本机构建号比服务器新 → latest（自己发过又退回旧清单，不该让人"降级"）
	buildNum = "20261020.1"
	if r := a.CheckUpdate(); r.State != "latest" {
		t.Errorf("本机构建号更新时应该判 latest，拿到 %q", r.State)
	}

	/* ③e 本机**没有**构建号（发布构建之前的老客户端，或别人自己编的）
	   → 必须判 new。否则版本号一样、它又没有号，永远收不到更新，
	   等于把这批人永久钉在旧二进制上。 */
	buildNum = ""
	updateManifestURLOverride = srv3b.URL + "/version.json"
	if r := a.CheckUpdate(); r.State != "new" {
		t.Errorf("本机没有构建号时应该判 new，拿到 %q", r.State)
	}
	buildNum = oldBuild

	// ④ 服务器 500 / 连不上 → offline，而且不许写成"已是最新"
	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
	}))
	defer dead.Close()
	updateManifestURLOverride = dead.URL + "/version.json"
	r = a.CheckUpdate()
	if r.State != "offline" {
		t.Errorf("服务器出错应该判 offline，拿到 %q", r.State)
	}
	if r.Message == "" {
		t.Error("offline 必须说明原因，界面不能只说「检查失败」")
	}
}

/*
「我现在跑的这份是不是官方原版」——四种必须分清的结局：

	ok / mismatch / unknown / offline。
	⚠️ 尤其 unknown 不能混进 mismatch：服务器没记这个版本时，说"你这份被改过"是冤枉人。
*/
func TestVerifySelfStates(t *testing.T) {
	oldVer := appVersion
	appVersion = "1.1.0"
	defer func() { appVersion = oldVer }()

	exe, err := selfExePath()
	if err != nil {
		t.Fatalf("取不到测试程序路径：%v", err)
	}
	mine, err := sha256File(exe)
	if err != nil {
		t.Fatalf("算不出自身指纹：%v", err)
	}
	other := strings.Repeat("f", 64)
	if mine == other {
		t.Fatal("测试前提不成立")
	}
	a := NewApp()

	// ① 清单里就是这个版本、指纹也对 → ok
	srv := fakeServer(t, updateManifest{Version: "1.1.0", SHA256: mine}, nil, "/none")
	useFakeServer(t, srv)
	if r := a.VerifySelf(); r.State != "ok" {
		t.Errorf("指纹一致应该判 ok，拿到 %q（%s）", r.State, r.Message)
	}

	// ② 指纹不一样 → mismatch，且必须把两个值都给出来
	srv2 := fakeServer(t, updateManifest{Version: "1.1.0", SHA256: other}, nil, "/none")
	updateManifestURLOverride = srv2.URL + "/version.json"
	r := a.VerifySelf()
	if r.State != "mismatch" {
		t.Errorf("指纹不同应该判 mismatch，拿到 %q", r.State)
	}
	if r.Local == "" || r.Official == "" {
		t.Error("不一致时必须把本机值和官方值都带回去，用户要能自己看")
	}

	// ③ 服务器没记这个版本 → unknown（不是 mismatch）
	srv3 := fakeServer(t, updateManifest{
		Version: "1.2.0", SHA256: other,
		History: []updateOldFile{{Version: "1.0.0", SHA256: other}},
	}, nil, "/none")
	updateManifestURLOverride = srv3.URL + "/version.json"
	if r := a.VerifySelf(); r.State != "unknown" {
		t.Errorf("服务器没记录应该判 unknown，拿到 %q（%s）", r.State, r.Message)
	}

	// ④ 老版本走 history 也能对得上 → ok
	srv4 := fakeServer(t, updateManifest{
		Version: "1.2.0", SHA256: other,
		History: []updateOldFile{{Version: "1.1.0", SHA256: mine}},
	}, nil, "/none")
	updateManifestURLOverride = srv4.URL + "/version.json"
	if r := a.VerifySelf(); r.State != "ok" {
		t.Errorf("history 里能对上应该判 ok，拿到 %q（%s）", r.State, r.Message)
	}

	/* ④b 同一版本号重建过两次（2026-10-19 新增的核心场景）：
	   新版指纹排在前面，老用户手上是上一次构建的，仍然必须判 ok ——
	   不能因为"清单里最新那条不是他这份"就报"被改过"。 */
	srv4b := fakeServer(t, updateManifest{
		Version: "1.1.0", Build: "20261019.2", SHA256: other,
		History: []updateOldFile{
			{Version: "1.1.0", Build: "20261019.1", SHA256: mine},
			{Version: "1.0.0", Build: "20261009.1", SHA256: other},
		},
	}, nil, "/none")
	updateManifestURLOverride = srv4b.URL + "/version.json"
	if r := a.VerifySelf(); r.State != "ok" {
		t.Errorf("同版本号的上一次构建也应该判 ok，拿到 %q（%s）", r.State, r.Message)
	}

	// ⑤ 连不上 → offline，绝不写成 mismatch
	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
	}))
	defer dead.Close()
	updateManifestURLOverride = dead.URL + "/version.json"
	if r := a.VerifySelf(); r.State != "offline" {
		t.Errorf("服务器出错应该判 offline，拿到 %q", r.State)
	}
}

func TestDownloadUpdate(t *testing.T) {
	pkg := make([]byte, updateMinBytes+4096)
	for i := range pkg {
		pkg[i] = byte(i % 251)
	}
	sum := sha256.Sum256(pkg)
	goodSHA := hex.EncodeToString(sum[:])

	srv := fakeServer(t, updateManifest{}, pkg, "/download/3z9wOpt_v9.9.9.exe")
	useFakeServer(t, srv)
	a := NewApp()

	// ① 正常下载 + 校验通过
	res := a.DownloadUpdate(srv.URL+"/download/3z9wOpt_v9.9.9.exe", goodSHA)
	if res.State != "ok" {
		t.Fatalf("正常下载应该成功，拿到 %q：%s", res.State, res.Message)
	}
	if res.Size != int64(len(pkg)) {
		t.Errorf("大小应该是 %d，拿到 %d", len(pkg), res.Size)
	}
	if _, err := os.Stat(res.Path); err != nil {
		t.Errorf("下载完的文件应该还在：%v", err)
	}
	if !strings.HasSuffix(strings.ToLower(res.Path), ".exe") {
		t.Errorf("落盘文件名该是 .exe，拿到 %q", res.Path)
	}

	// ② 指纹对不上 → 必须失败，而且要把文件删掉（不能留个坏包在机器上）
	badSHA := strings.Repeat("b", 64)
	res2 := a.DownloadUpdate(srv.URL+"/download/3z9wOpt_v9.9.9.exe", badSHA)
	if res2.State == "ok" {
		t.Fatal("指纹不符居然算成功 —— 这是最严重的一种，会把坏文件装上去")
	}
	if res2.Message == "" {
		t.Error("指纹不符要给用户看得懂的原因")
	}
	if res2.Path != "" {
		if _, err := os.Stat(res2.Path); err == nil {
			t.Error("指纹不符的包必须删掉，不能留给替换步骤")
		}
	}

	// ③ 没给指纹 → 直接拒，不许"下完再说"
	res3 := a.DownloadUpdate(srv.URL+"/download/3z9wOpt_v9.9.9.exe", "")
	if res3.State == "ok" {
		t.Error("没有官方指纹就不该下载")
	}

	// ④ 非 https 地址 → 拒绝（沙箱里放开是为了测，线上必须挡）
	updateAllowInsecureForTest = false
	res4 := a.DownloadUpdate(srv.URL+"/download/3z9wOpt_v9.9.9.exe", goodSHA)
	if res4.State == "ok" {
		t.Error("http 地址必须拒绝")
	}
	updateAllowInsecureForTest = true

	// ⑤ 文件太小（服务器返回了错误页）→ 丢弃
	small := []byte("<html>404</html>")
	smallSum := sha256.Sum256(small)
	srvS := fakeServer(t, updateManifest{}, small, "/small.exe")
	updateManifestURLOverride = srvS.URL + "/version.json"
	res5 := a.DownloadUpdate(srvS.URL+"/small.exe", hex.EncodeToString(smallSum[:]))
	if res5.State == "ok" {
		t.Error("几百字节的东西不能当更新包")
	}
}

func TestApplyUpdateRejectsBadInput(t *testing.T) {
	a := NewApp()
	if msg := a.ApplyUpdate(""); msg == "" {
		t.Error("空路径要拒绝")
	}
	dir := t.TempDir()
	if msg := a.ApplyUpdate(dir); msg == "" {
		t.Error("目录要拒绝")
	}
	tiny := filepath.Join(dir, "tiny.exe")
	_ = os.WriteFile(tiny, []byte("xx"), 0o644)
	if msg := a.ApplyUpdate(tiny); msg == "" {
		t.Error("明显不完整的小文件要拒绝")
	}
}

func TestCopyFile(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "a.bin")
	dst := filepath.Join(dir, "b.bin")
	want := make([]byte, 4096)
	for i := range want {
		want[i] = byte(i)
	}
	if err := os.WriteFile(src, want, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := copyFile(src, dst); err != nil {
		t.Fatalf("复制失败：%v", err)
	}
	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(want) {
		t.Fatalf("复制后长度不对：%d vs %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("第 %d 字节不同", i)
		}
	}
	// 目标不可写时要报错，不能沉默
	if err := copyFile(filepath.Join(dir, "不存在.bin"), filepath.Join(dir, "c.bin")); err == nil {
		t.Error("源文件不存在应该报错")
	}
}

// 更新器模式参数不足时直接返回，不许越界（main 里就是靠这个分支先分流的）
func TestUpdateApplierArgsGuard(t *testing.T) {
	runUpdateApplier([]string{"-apply-update"})
	runUpdateApplier([]string{"-apply-update", "1"})
	runUpdateApplier([]string{"-apply-update", "abc", "x", "y"}) // pid 不是数字也不能崩
	_ = fmt.Sprint("")
}
