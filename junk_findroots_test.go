package main

// junk_findroots_test.go —— 数据根发现引擎的单测（2026-11-12 新增）。
// 全部用临时目录注入，不碰真实磁盘；生产入口（finderBases 走真实盘）只测
// 「返回不报错」，具体命中靠 -cli 真机验证。

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func mkTree(t *testing.T, base string, rel ...string) string {
	t.Helper()
	for _, r := range rel {
		p := filepath.Join(base, filepath.FromSlash(r))
		if err := os.MkdirAll(p, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return base
}

func TestFindNamedRootsFindsCustomDepth(t *testing.T) {
	base := t.TempDir()
	mkTree(t, base,
		`App Data/QQ/Tencent Files/10000/nt_qq/nt_data/Emoji`,
		`普通软件/别的东西`,
		`xwechat_files/wxid_abc/cache`,
	)
	got := findNamedRoots([]string{base}, []string{"Tencent Files"}, 3)
	if len(got) != 1 || !strings.HasSuffix(filepath.ToSlash(got[0]), "App Data/QQ/Tencent Files") {
		t.Fatalf("深度3的自定义数据根没找到: %v", got)
	}
	// 剪枝：Windows 等系统目录之下不找
	got = findNamedRoots([]string{base}, []string{"xwechat_files"}, 3)
	if len(got) != 1 {
		t.Fatalf("根下一层的特征目录没找到: %v", got)
	}
}

func TestFindNamedRootsSkipsReparseAndMissing(t *testing.T) {
	base := t.TempDir()
	mkTree(t, base, `Tencent Files`, `Windows/Tencent Files`)
	// 不存在的候选根：跳过不报错
	got := findNamedRoots([]string{base, filepath.Join(base, "nope")}, []string{"Tencent Files"}, 3)
	if len(got) != 1 {
		t.Fatalf("应命中1个（Windows 下剪掉）: %v", got)
	}
}

func TestBrowserOldGlobsKeepsNewest(t *testing.T) {
	base := t.TempDir()
	mkTree(t, base, `Google/Chrome/Application/131.0.6778.205`, `Google/Chrome/Application/153.0.8010.50`)
	app := filepath.Join(base, `Google`, `Chrome`, `Application`)
	got := browserOldGlobsFrom([]string{app})
	if len(got) != 1 || !strings.Contains(got[0], `131.0.6778.205`) {
		t.Fatalf("应只留旧版 131 的展开式: %v", got)
	}
	if !strings.HasSuffix(got[0], `*`) {
		t.Fatalf("目标应是版本目录下的一级内容（\\* 结尾）: %v", got)
	}
}

func TestBrowserOldGlobsSingleVersionUntouched(t *testing.T) {
	base := t.TempDir()
	mkTree(t, base, `Edge/Application/120.0.2210.61`)
	app := filepath.Join(base, `Edge`, `Application`)
	if got := browserOldGlobsFrom([]string{app}); len(got) != 0 {
		t.Fatalf("单版本目录不许动（可能是正在用的主程序）: %v", got)
	}
}

func TestBrowserOldGlobsPortableProtected(t *testing.T) {
	base := t.TempDir()
	mkTree(t, base, `Google/Chrome/Application/131.0.6778.205`, `Google/Chrome/Application/153.0.8010.50`)
	app := filepath.Join(base, `Google`, `Chrome`, `Application`)
	if err := os.WriteFile(filepath.Join(app, "chrome++.ini"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := browserOldGlobsFrom([]string{app}); len(got) != 0 {
		t.Fatalf("chrome++ 便携特征在，整个位置必须跳过: %v", got)
	}
}

func TestCmpVerSegs(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"131.0.6778.205", "153.0.8010.50", -1},
		{"153.0.8010.50", "153.0.8010.50", 0},
		{"153.0.8010.9", "153.0.8010.10", -1}, // 数字比较不是字符串
		{"9.0.0.0", "10.0.0.0", -1},
	}
	for _, c := range cases {
		if got := cmpVerSegs(c.a, c.b); got != c.want {
			t.Fatalf("cmpVerSegs(%s,%s)=%d want %d", c.a, c.b, got, c.want)
		}
	}
}

func TestParseJunkRulesGoFindKey(t *testing.T) {
	junkRuleSkipLog = nil // 全局累积字段：本用例独立断言，先清空
	rules := parseJunkRules([]byte("[browserold]\nname = 测试\ngroup = 浏览器缓存\ngofind1 = browserold\n"))
	if len(rules) != 1 || len(rules[0].gofind) != 1 || rules[0].gofind[0] != "browserold" {
		t.Fatalf("gofind 键没解析出来: %+v", rules)
	}
	// 只有 gofind、没有任何 dir/filekey/electron：合法（不因「无目标」被跳过）
	if len(junkRuleSkipLog) != 0 {
		t.Fatalf("不应有跳过记录: %v", junkRuleSkipLog)
	}
}

func TestScanOneRequireEmptySemantic(t *testing.T) {
	// 2026-11-12 语义修正：目标解析出来了（哪怕 require 全不存在）不许判「未安装」。
	base := t.TempDir()
	if err := os.WriteFile(filepath.Join(base, "x.bin"), make([]byte, 4096), 0o644); err != nil {
		t.Fatal(err)
	}
	it := &JunkItem{ID: "t", Group: "临时文件", Name: "t", dirs: []string{base},
		require: []string{filepath.Join(base, "not-exist")}}
	scanOne(context.Background(), it)
	if it.Empty {
		t.Fatalf("有目标却被判未安装: %+v", it)
	}
	if it.Bytes != 4096 {
		t.Fatalf("字节数不对: %d", it.Bytes)
	}
	// 目标一个都没有 + require 全不存在 → 才判未安装
	it2 := &JunkItem{ID: "t2", Group: "临时文件", Name: "t2",
		require: []string{filepath.Join(base, "not-exist")}}
	scanOne(context.Background(), it2)
	if !it2.Empty {
		t.Fatalf("无目标且 require 落空应判未安装")
	}
}

func TestResolveTargetsGoFindWired(t *testing.T) {
	// gofind → finderGlobs → resolveGlob 全链路（注册一个临时 finder）
	base := t.TempDir()
	mkTree(t, base, `Emoji/a.gif`)
	old, ok := ruleFinders["_testfind"]
	defer func() {
		if ok {
			ruleFinders["_testfind"] = old
		} else {
			delete(ruleFinders, "_testfind")
		}
		resetFinderCache()
	}()
	// 临时注册 + 预置缓存结果（不跑全盘）
	finderMu.Lock()
	finderCache["_testfind"] = []string{filepath.Join(base, `Emoji`)}
	finderMu.Unlock()
	ruleFinders["_testfind"] = func() []string { return finderCache["_testfind"] }

	it := &JunkItem{ID: "t3", Group: "应用缓存", Name: "t3", gofind: []string{"_testfind"}}
	dirs, files := resolveTargets(context.Background(), it)
	if len(files) != 0 || len(dirs) != 1 || !strings.HasSuffix(dirs[0], "Emoji") {
		t.Fatalf("gofind 目标没接入（Emoji 应作为 dir 目标清空内容）: dirs=%v files=%v", dirs, files)
	}
}

// ---------- find 声明（2026-11-12 数据化：规则一个文件） ----------

func TestParseFindSpec(t *testing.T) {
	s, ok := parseFindSpec(`Tencent Files | *\nt_qq\nt_data\Emoji;*\nt_qq\nt_data\log`)
	if !ok || len(s.names) != 1 || s.names[0] != "Tencent Files" || len(s.subs) != 2 {
		t.Fatalf("基本语法没解析出来: %+v ok=%v", s, ok)
	}
	// 多特征名 + 多子目录 + 多余空格
	s, ok = parseFindSpec(` xwechat_files ; We Chat | *\cache ; *\temp `)
	if !ok || len(s.names) != 2 || len(s.subs) != 2 || s.subs[0] != `*\cache` {
		t.Fatalf("多段没解析出来: %+v ok=%v", s, ok)
	}
	// 非法：没有竖线 / 竖线一边为空
	for _, bad := range []string{"没有竖线", "a |", "| b", "   "} {
		if _, ok := parseFindSpec(bad); ok {
			t.Fatalf("非法值不该通过: %q", bad)
		}
	}
}

func TestSpecGlobsFrom(t *testing.T) {
	base := t.TempDir()
	mkTree(t, base, `App Data/QQ/Tencent Files/10000/nt_qq/nt_data/Emoji`)
	spec := findSpec{names: []string{"Tencent Files"}, subs: []string{`*\nt_qq\nt_data\Emoji`, `*\nt_qq\nt_data\log`}}
	got := specGlobsFrom([]string{base}, spec)
	if len(got) != 2 || !strings.HasSuffix(filepath.ToSlash(got[0]), "App Data/QQ/Tencent Files/*/nt_qq/nt_data/Emoji") {
		t.Fatalf("数据根没拼上子目录: %v", got)
	}
	// 特征目录不存在 → 空（不是错误）
	if got := specGlobsFrom([]string{base}, findSpec{names: []string{"不存在"}, subs: []string{`*\x`}}); len(got) != 0 {
		t.Fatalf("未命中应返回空: %v", got)
	}
}

func TestParseJunkRulesFindKey(t *testing.T) {
	junkRuleSkipLog = nil // 全局累积字段：本用例独立断言，先清空
	rules := parseJunkRules([]byte("[t9]\nname = 测试\ngroup = 应用缓存\nfind1 = Tencent Files | *\\Emoji\nfind2 = 语法不对的\n"))
	if len(rules) != 1 || len(rules[0].finds) != 1 {
		t.Fatalf("find 键/非法值没按预期处理（合法1条、非法丢弃）: %+v", rules)
	}
	// 只有 find、没有任何 dir/filekey/electron：合法（不因「无目标」被跳过）
	if len(junkRuleSkipLog) != 0 {
		t.Fatalf("不应有跳过记录: %v", junkRuleSkipLog)
	}
}

func TestResolveTargetsFindWired(t *testing.T) {
	// find → findSpecGlobs → resolveGlob 全链路。findSpecGlobs 的候选根走真实盘，
	// 单测预置缓存结果（key 与 findSpecGlobs 的归一化格式一致），不跑全盘。
	base := t.TempDir()
	mkTree(t, base, `Emoji/a.gif`)
	spec := findSpec{names: []string{"t find"}, subs: []string{`Emoji`}}
	key := "spec:" + strings.Join(spec.names, ";") + "|" + strings.Join(spec.subs, ";")
	defer resetFinderCache()
	finderMu.Lock()
	finderCache[key] = []string{filepath.Join(base, `Emoji`)}
	finderMu.Unlock()

	it := &JunkItem{ID: "t5", Group: "应用缓存", Name: "t5", finds: []findSpec{spec}}
	dirs, files := resolveTargets(context.Background(), it)
	if len(files) != 0 || len(dirs) != 1 || !strings.HasSuffix(dirs[0], "Emoji") {
		t.Fatalf("find 目标没接入（Emoji 应作为 dir 目标清空内容）: dirs=%v files=%v", dirs, files)
	}
}
