package main

// 规则引擎测试（2026-11-12 规则化）：
//   ① 内置规则文件本身必须干净（51 节、零跳过、ID 合法、分组正确）；
//   ② 关键项展开正确（electron 展开成 5 个 glob、路径变量、多 Profile 修复）；
//   ③ 合并语义（custom 覆盖原位、新 ID 插到同组尾）；
//   ④ junkItems() 整体：迁移项 ID 不丢、不与静态项撞车、总数不回退。

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestJunkRulesEmbedded(t *testing.T) {
	resetJunkRuleCache()
	defer resetJunkRuleCache()
	rules := parseJunkRules(junkRulesEmbedded)
	if len(rules) != 54 {
		t.Fatalf("内置规则应为 54 节（53 + officeupdate），实得 %d", len(rules))
	}
	if n := len(junkRuleSkipLog); n != 0 {
		t.Fatalf("内置规则不允许有跳过项（跳过 %d 条：%v）", n, junkRuleSkipLog)
	}
	groups := map[string]int{}
	ids := map[string]bool{}
	for _, r := range rules {
		if !ruleGroups[r.group] {
			t.Errorf("[%s] 分组非法：%q", r.id, r.group)
		}
		if ids[r.id] {
			t.Errorf("ID 重复：%s", r.id)
		}
		ids[r.id] = true
		if r.name == "" || r.desc == "" {
			t.Errorf("[%s] 缺 name/desc", r.id)
		}
		if len(r.dirs) == 0 && len(r.globs) == 0 && len(r.gofind) == 0 && len(r.finds) == 0 {
			t.Errorf("[%s] 没有任何 dir/filekey/electron/gofind/find", r.id)
		}
		groups[r.group]++
	}
	// 与迁移前的分组分布逐一对账（junk_items.go 删除前的字面量数量；
	// 2026-11-12 浏览器缓存 +2：browupdate / browserold）
	if groups["浏览器缓存"] != 9 || groups["应用缓存"] != 26 || groups["AI 应用缓存"] != 19 {
		t.Errorf("分组分布与预期不符：%v", groups)
	}
}

func TestJunkRulesKeyItems(t *testing.T) {
	rules := parseJunkRules(junkRulesEmbedded)
	byID := map[string]junkRule{}
	for _, r := range rules {
		byID[r.id] = r
	}

	// edgecache：8 个 glob，全部在 Edge 下
	if got := len(byID["edgecache"].globs); got != 8 {
		t.Errorf("edgecache glob 数应 8，实得 %d", got)
	}
	// chromecache：detect + 8 glob，展开后含真实盘符
	cc := byID["chromecache"]
	if len(cc.globs) != 8 || len(cc.require) != 1 {
		t.Errorf("chromecache 结构不符：require=%d globs=%d", len(cc.require), len(cc.globs))
	}
	if !strings.Contains(strings.Join(cc.globs, "|"), `Google\Chrome\User Data\*\Cache`) {
		t.Errorf("chromecache glob 缺少 Google\\Chrome 路径：%v", cc.globs)
	}
	// chromium：多 Profile 修复（* 而非 Default）+ 排除
	ch := byID["chromium"]
	join := strings.Join(ch.globs, "|")
	if !strings.Contains(join, `User Data\*\Cache`) || strings.Contains(join, "Default") {
		t.Errorf("chromium 应为全 Profile glob 且不含 Default：%v", ch.globs)
	}
	if len(ch.excl) != 2 {
		t.Errorf("chromium excl 应 2 条，实得 %v", ch.excl)
	}
	// doubao：electron 5 glob + 4 条 filekey = 9
	if got := len(byID["doubao"].globs); got != 9 {
		t.Errorf("doubao glob 应 9（electron 5 + filekey 4），实得 %d", got)
	}
	// electron 展开形态抽查：base\**\Cache 必须在
	db := byID["doubao"]
	want := filepath.Join(os.Getenv("LOCALAPPDATA"), `Doubao\User Data`) + `\**\` + `Cache`
	if !containsStr(db.globs, want) {
		t.Errorf("doubao 缺 electron 展开 glob %q：%v", want, db.globs)
	}
	// aimodels：USERPROFILE 变量展开
	am := byID["aimodels"]
	if !containsStr(am.globs, filepath.Join(os.Getenv("USERPROFILE"), `.ollama\models`)) {
		t.Errorf("aimodels 未展开 UserProfile 变量：%v", am.globs)
	}
	if !am.heavy || !am.na || am.renew {
		t.Errorf("aimodels 应 heavy+na 且非 renew：%v", am)
	}
	// steam 用 %ProgramFiles86%
	st := byID["steam"]
	if len(st.dirs) != 2 || !strings.Contains(strings.Join(st.dirs, "|"), os.Getenv(`ProgramFiles(x86)`)) {
		t.Errorf("steam 未展开 ProgramFiles86：%v", st.dirs)
	}
	if st.renew {
		t.Errorf("steam 日志类不应标 renew")
	}
	// java 用 %LocalLow%
	if !containsStr(byID["java"].dirs, filepath.Join(os.Getenv("USERPROFILE"), `AppData\LocalLow`, `Sun\Java\Deployment\cache`)) {
		t.Errorf("java 未展开 LocalLow：%v", byID["java"].dirs)
	}
	// qqnt：12 条 filekey + 1 条 find 声明（数据化全盘兜底）；wechatcache：4 glob + 2 dir + 2 find
	if got := len(byID["qqnt"].globs); got != 12 {
		t.Errorf("qqnt filekey 应 12，实得 %d", got)
	}
	if qn := byID["qqnt"]; len(qn.finds) != 1 || len(qn.finds[0].subs) != 2 {
		t.Errorf("qqnt find 应 1 条 2 个子目录（Emoji+log）：%+v", qn.finds)
	}
	wc := byID["wechatcache"]
	if len(wc.globs) != 4 || len(wc.dirs) != 2 || len(wc.require) != 4 || len(wc.finds) != 2 {
		t.Errorf("wechatcache 结构不符：globs=%d dirs=%d require=%d finds=%d",
			len(wc.globs), len(wc.dirs), len(wc.require), len(wc.finds))
	}
	// cookies：renew + heavy + na
	ck := byID["cookies"]
	if !ck.renew || !ck.heavy || !ck.na {
		t.Errorf("cookies 应 renew+heavy+na：%v", ck)
	}
}

func containsStr(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

func TestJunkRulesMerge(t *testing.T) {
	builtin := []junkRule{
		{id: "edgecache", name: "旧", group: "浏览器缓存"},
		{id: "chromecache", name: "旧", group: "浏览器缓存"},
		{id: "wpscache", name: "旧", group: "应用缓存"},
	}
	custom := []junkRule{
		{id: "chromecache", name: "改过的", group: "浏览器缓存"},        // 覆盖：原位
		{id: "wecom", name: "企业微信", group: "应用缓存"},             // 追加：同组尾
		{id: "newai", name: "明天的AI", group: "AI 应用缓存"},          // 追加：新组尾（该组原本无项）
	}
	merged := mergeJunkRules(builtin, custom)
	if len(merged) != 5 {
		t.Fatalf("合并后应 5 项，实得 %d", len(merged))
	}
	if merged[1].name != "改过的" {
		t.Errorf("覆盖应原位替换：merged[1]=%v", merged[1])
	}
	// 追加位置：wecom 跟在 wpscache（应用缓存最后一项）后面
	if merged[3].id != "wecom" {
		t.Errorf("wecom 应插在应用缓存组尾（merged[3]），实得 %s", merged[3].id)
	}
	if merged[4].id != "newai" {
		t.Errorf("newai 应在最后，实得 %s", merged[4].id)
	}
}

func TestJunkRuleCustomSkips(t *testing.T) {
	L := os.Getenv("LOCALAPPDATA")
	R := os.Getenv("APPDATA")
	builtinIDs := map[string]bool{"chromecache": true}
	rules := []junkRule{
		{id: "chromecache", group: "浏览器缓存", globs: []string{filepath.Join(L, `Google\Chrome\User Data\*\Cache`)}}, // 内置：不参与
		{id: "wecom", group: "应用缓存",
			require: []string{filepath.Join(R, "Tencent", "WXWork")},
			globs:   []string{filepath.Join(R, `Tencent\WXWork\**\Cache`)},
		},
		{id: "newai", group: "AI 应用缓存", globs: []string{filepath.Join(L, `NewAI\**\Cache`)}},
		{id: "wild", group: "AI 应用缓存", globs: []string{filepath.Join(L, `**\Cache`)}}, // 通配首段：不产出
	}
	skips := ruleCustomSkips(rules, builtinIDs)
	got := map[string]bool{}
	for _, s := range skips {
		got[s] = true
	}
	if !got[`\tencent\`] || !got[`\newai\`] {
		t.Errorf("应产出 \\tencent\\ 与 \\newai\\，实得 %v", skips)
	}
	if got[`\google\`] {
		t.Errorf("内置项不应参与自动排除：%v", skips)
	}
	for _, s := range skips {
		if strings.ContainsAny(strings.Trim(s, `\`), "*?") {
			t.Errorf("通配段不应进排除名单：%v", s)
		}
	}
}

func TestJunkItemsAfterRuleMigration(t *testing.T) {
	resetJunkRuleCache()
	defer resetJunkRuleCache()
	items := junkItems()
	if len(items) < 89 {
		t.Fatalf("清单项数不应少于 89（迁移前基线），实得 %d", len(items))
	}
	ids := map[string]bool{}
	for _, it := range items {
		if ids[it.ID] {
			t.Errorf("ID 重复：%s", it.ID)
		}
		ids[it.ID] = true
	}
	// 迁移项全部在场（抽核心 12 个：三组各 4）
	for _, id := range []string{"edgecache", "chromecache", "chromium", "cookies",
		"wpscache", "wechatcache", "qqnt", "vscode",
		"doubao", "kimi", "aimodels", "chatbox"} {
		if !ids[id] {
			t.Errorf("迁移项丢失：%s", id)
		}
	}
	// 迁移项的属性经 junkItems() 合成后仍然正确（Heavy/Renew/Na 走「或」合成）
	for _, it := range items {
		switch it.ID {
		case "cookies", "qqrecv", "wechatrecv", "aimodels":
			if !it.Heavy || !it.Na {
				t.Errorf("%s 应 Heavy+Na", it.ID)
			}
		case "doubao", "edgecache", "chromecache", "chromium", "firefox":
			if !it.Renew {
				t.Errorf("%s 应 Renew", it.ID)
			}
		case "steam", "baidunet", "wegame", "zoom", "onedrivelog", "java":
			if it.Renew {
				t.Errorf("%s 不应 Renew（日志类一次性空间）", it.ID)
			}
		}
	}
}
