package main

import (
	"os"
	"regexp"
	"strings"
	"syscall"
	"testing"
	"unsafe"
)

// ---------- 工具/页面引用完整性 ----------

// toolExists 该 ID 是否真实存在于工具库注册表
func toolExists(id string) bool {
	if id == "" {
		return false
	}
	if _, ok := toolImpls[id]; ok {
		return true
	}
	buildCatalog()
	for _, it := range toolCatalog {
		if it.ID == id {
			return true
		}
	}
	return false
}

// navPageIDs 从 app.js 的 NAV 里解析出所有页面 ID（服务卡引用的 PageID 必须在此集合内）
// 这样前端改了页面 ID，测试会立刻发现服务卡指向了不存在的页
func navPageIDs(t *testing.T) map[string]bool {
	t.Helper()
	raw, err := os.ReadFile(`frontend/dist/app.js`)
	if err != nil {
		if os.IsNotExist(err) {
			/* v1.1 的前端是单文件 design/ui-prototype.html，不再有 frontend/dist/app.js。
			   这些断言查的是"前端源码里的 NAV / 图标表 / 文案"，1.1 里没有对象可查 → 如实跳过，
			   而不是 t.Fatal 把整个包判失败（那会把同文件里的后端断言一起埋掉）。 */
			t.Skipf("跳过前端源码断言：frontend/dist/app.js 不存在（v1.1 前端为单文件 design/ui-prototype.html）")
		}
		t.Fatalf("读 app.js 失败：%v", err)
	}
	s := string(raw)
	i := strings.Index(s, "const NAV = [")
	j := strings.Index(s[i:], "\n];")
	if i < 0 || j < 0 {
		t.Fatal("app.js 里找不到 NAV 定义")
	}
	nav := s[i : i+j]
	out := map[string]bool{}
	for _, m := range regexp.MustCompile(`id:\s*"([^"]+)"`).FindAllStringSubmatch(nav, -1) {
		out[m[1]] = true
	}
	// 2026-09-23 调整：菜单原先 4 组（末组「其他工具」含 关于/设置/技术文库/服务大厅），
	// 那一组已整组迁走（侧栏底部 + 顶栏下拉），NAV 只剩 3 组共 9 项，旧的 <15 阈值随即失效。
	// 这条断言要抓的是「NAV 解析逻辑坏了」（那时会返回 0~2 项，甚至直接 Fatal），
	// 而不是把菜单项数量钉死 —— 钉死会让每次正常增删菜单项都无谓报错。
	// 取 6：给菜单调整留余量，仍能抓到解析失效。
	if len(out) < 6 {
		t.Fatalf("NAV 解析结果太少（%d），解析逻辑可能已失效", len(out))
	}
	return out
}

var svcTagsAllowed = map[string]bool{"": true, "热门": true, "紧急": true, "免费检测": true}

// 服务目录：结构完整性 + 自检引用必须真实存在
func TestServiceCatalogIntegrity(t *testing.T) {
	items := serviceItems()
	if len(items) != 6 {
		t.Fatalf("服务项目应为 6 项，实际 %d", len(items))
	}
	pages := navPageIDs(t)
	icons := iconKeys(t)
	seen := map[string]bool{}
	for _, it := range items {
		if it.ID == "" || seen[it.ID] {
			t.Errorf("服务 ID 为空或重复：%q", it.ID)
		}
		seen[it.ID] = true
		if it.Name == "" || it.Desc == "" || it.Time == "" {
			t.Errorf("%s 缺少必填字段（Name/Desc/Time 不能空）", it.ID)
		}
		if len(it.Points) != 3 {
			t.Errorf("%s 的卖点应为 3 条，实际 %d", it.ID, len(it.Points))
		}
		if !svcTagsAllowed[it.Tag] {
			t.Errorf("%s 标签 %q 不在允许集合（空/热门/紧急）", it.ID, it.Tag)
		}
		if it.Icon == "" || !icons[it.Icon] {
			t.Errorf("%s 的卡片图标 %q 不在 app.js ICONS 里（前端会渲染成空白方块）", it.ID, it.Icon)
		}
		if it.ToolID != "" && !toolExists(it.ToolID) {
			t.Errorf("%s 引用的工具 %q 在工具库里不存在（改了工具 ID 必须同步这里）", it.ID, it.ToolID)
		}
		if it.PageID != "" && !pages[it.PageID] {
			t.Errorf("%s 引用的页面 %q 不在 app.js 的 NAV 里（页面 ID 对不上）", it.ID, it.PageID)
		}
		if (it.ToolID != "" || it.PageID != "") && it.ActLabel == "" {
			t.Errorf("%s 有自检动作但没写按钮文案 ActLabel", it.ID)
		}
	}
	if !seen["svc-bsod"] || !seen["svc-clean"] || !seen["svc-other"] {
		t.Error("关键服务项（蓝屏修复/清理/其他问题）不能删")
	}
}

// 自检清单：步骤与引用检查
func TestServiceGuidesIntegrity(t *testing.T) {
	gs := serviceGuides()
	if len(gs) < 8 {
		t.Fatalf("自检清单至少 8 条，实际 %d", len(gs))
	}
	pages := navPageIDs(t)
	seen := map[string]bool{}
	for _, g := range gs {
		if g.ID == "" || seen[g.ID] {
			t.Errorf("清单 ID 为空或重复：%q", g.ID)
		}
		seen[g.ID] = true
		if g.Title == "" || len(g.Steps) < 2 {
			t.Errorf("%s 标题为空或步骤少于 2 步", g.ID)
		}
		if g.ToolID != "" && !toolExists(g.ToolID) {
			t.Errorf("%s 引用的工具 %q 不存在", g.ID, g.ToolID)
		}
		if g.PageID != "" && !pages[g.PageID] {
			t.Errorf("%s 引用的页面 %q 不在 NAV 里", g.ID, g.PageID)
		}
		if (g.ToolID != "" || g.PageID != "") && g.ActLabel == "" {
			t.Errorf("%s 有动作但没写按钮文案", g.ID)
		}
		if g.Article != "" && !strings.HasPrefix(g.Article, "https://www.3z9w.com/") {
			t.Errorf("%s 文章链接必须是本站 https 地址，实际 %q", g.ID, g.Article)
		}
		if g.Article != "" && g.ArticleTitle == "" {
			t.Errorf("%s 有文章链接但没写标题", g.ID)
		}
	}
}

// 链接与分类/文章：域名与 https 必须正确（防止写错协议或外域名）
func TestServiceLinksAndArticles(t *testing.T) {
	for k, v := range (&App{}).SvcLinks() {
		if v == "" {
			continue // qr 未放素材时为空，合法
		}
		if k == "qr" || k == "reward" {
			continue // 本地素材文件名，不是外链；素材没放时为空也合法
		}
		if k == "wxid" {
			continue // 微信号，不是外链；下面单独校验非空
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
		t.Errorf("缺少微信号 wxid（扫码弹窗展示 + 复制用）")
	}
	cats := serviceArticleCats()
	if len(cats) != 9 {
		t.Errorf("网站文章分类应为 9 类，实际 %d", len(cats))
	}
	for _, c := range cats {
		if c.Name == "" || c.Count == "" || !strings.HasPrefix(c.URL, svcDocsURL) {
			t.Errorf("分类 %q 字段不完整或 URL 不对：%s", c.Name, c.URL)
		}
	}
	arts := serviceArticles()
	if len(arts) < 6 {
		t.Errorf("精选文章至少 6 篇，实际 %d", len(arts))
	}
	for _, a := range arts {
		if a.Title == "" || !strings.HasPrefix(a.URL, "https://www.3z9w.com/") || a.Tag == "" {
			t.Errorf("文章 %q 字段不完整或链接不对：%s", a.Title, a.URL)
		}
	}
}

// ---------- 诊断报告 ----------

// 报告必须能生成、含全部小节、带上用户自述、且不含乱码/控制字符
func TestBuildDiagReport(t *testing.T) {
	note := "一开机就蓝屏，代码 0x000000D1"
	rep := (&App{}).BuildDiagReport(note)

	if len(rep) < 300 {
		t.Fatalf("报告太短（%d 字节），可能采集全失败：\n%s", len(rep), rep)
	}
	for _, sec := range []string{"【无名大叔 · 远程服务诊断报告】", "── 系统 ──", "── 硬件 ──", "── 磁盘 ──", "── 故障线索 ──", "── 用户自述 ──", svcOrderURL} {
		if !strings.Contains(rep, sec) {
			t.Errorf("报告缺少小节：%q", sec)
		}
	}
	if !strings.Contains(rep, note) {
		t.Error("报告没有带上用户自述内容")
	}
	if strings.ContainsAny(rep, "\x00") || strings.Contains(rep, "\uFFFD") {
		t.Error("报告里出现了空字节或替换字符（编码问题）")
	}
	if strings.Contains(rep, "%!") {
		t.Error("报告里有格式化失败残留（%!…），说明 fmt 参数对不上")
	}
	// 不带自述时不能崩，也不能出现空的「用户自述」小节
	rep2 := (&App{}).BuildDiagReport("   ")
	if strings.Contains(rep2, "── 用户自述 ──") {
		t.Error("没有自述时不应输出「用户自述」小节")
	}
	t.Log("REPORT_BEGIN|" + rep + "|REPORT_END")
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

// ---------- 服务大厅转化版：首屏 / 信任数字 / 流程 / 口碑 ----------

// iconKeys 从 app.js 的 ICONS 里解析图标键集合（服务大厅数据引用的图标必须真实存在）
func iconKeys(t *testing.T) map[string]bool {
	t.Helper()
	raw, err := os.ReadFile(`frontend/dist/app.js`)
	if err != nil {
		if os.IsNotExist(err) {
			/* v1.1 的前端是单文件 design/ui-prototype.html，不再有 frontend/dist/app.js。
			   这些断言查的是"前端源码里的 NAV / 图标表 / 文案"，1.1 里没有对象可查 → 如实跳过，
			   而不是 t.Fatal 把整个包判失败（那会把同文件里的后端断言一起埋掉）。 */
			t.Skipf("跳过前端源码断言：frontend/dist/app.js 不存在（v1.1 前端为单文件 design/ui-prototype.html）")
		}
		t.Fatalf("读 app.js 失败：%v", err)
	}
	s := string(raw)
	i := strings.Index(s, "const ICONS")
	if i < 0 {
		t.Fatal("app.js 里找不到 ICONS 定义")
	}
	seg := s[i:]
	if len(seg) > 6000 {
		seg = seg[:6000]
	}
	out := map[string]bool{}
	for _, m := range regexp.MustCompile(`(?m)^\s*([a-z0-9_]+):`).FindAllStringSubmatch(seg, -1) {
		out[m[1]] = true
	}
	if len(out) < 20 {
		t.Fatalf("ICONS 解析结果太少（%d），解析逻辑可能已失效", len(out))
	}
	return out
}

// TestSvcPageIntegrity 服务大厅是转化页：首屏、数字、流程、口碑缺一块转化就断
func TestSvcPageIntegrity(t *testing.T) {
	pg := (&App{}).GetSvcPage()

	// ① 首屏：人 + 承诺
	h := pg.Hero
	for _, c := range []struct{ k, v string }{
		{"Name", h.Name}, {"Identity", h.Identity}, {"Title", h.Title},
		{"Sub", h.Sub}, {"Primary", h.Primary}, {"Second", h.Second},
	} {
		if strings.TrimSpace(c.v) == "" {
			t.Errorf("首屏 %s 不能为空（第一眼要看到人和承诺）", c.k)
		}
	}
	// 首屏三层（主张 / 证据 / 承诺）各自完整，且互不重复
	if len(pg.Promise) != 6 {
		t.Fatalf("承诺条应为 6 项，实际 %d", len(pg.Promise))
	}
	pms := map[string]bool{}
	for _, p := range pg.Promise {
		if strings.TrimSpace(p.Text) == "" || strings.TrimSpace(p.Icon) == "" {
			t.Errorf("承诺项缺文案或图标：%+v", p)
		}
		if pms[p.Text] {
			t.Errorf("承诺项重复：%q", p.Text)
		}
		pms[p.Text] = true
	}

	// ② 服务卡数量（与目录测试保持一致）
	if len(pg.Items) != 6 {
		t.Fatalf("服务卡应为 6 项，实际 %d", len(pg.Items))
	}

	// ③ 信任数字：5 项，图标必须真实存在（改图标名忘了同步会报红）
	if len(pg.Stats) != 5 {
		t.Fatalf("信任数字应为 5 项，实际 %d", len(pg.Stats))
	}
	icons := iconKeys(t)
	for _, st := range pg.Stats {
		if strings.TrimSpace(st.Value) == "" || strings.TrimSpace(st.Label) == "" {
			t.Errorf("信任数字缺值或标签：%+v", st)
		}
		if !icons[st.Icon] {
			t.Errorf("信任数字 %q 的图标 %q 不在 app.js 的 ICONS 里", st.Label, st.Icon)
		}
	}

	// ④ 服务流程：下单三步 + 三种结局兜底（2026-09-22 19:30 用户拍板 —— 别把内部环节摊给客户看）
	fl := pg.Flow
	for _, c := range []struct{ k, v string }{
		{"Title", fl.Title}, {"Desc", fl.Desc},
	} {
		if strings.TrimSpace(c.v) == "" {
			t.Errorf("服务流程 %s 不能为空（流程是转化闭环，缺一段用户就走不下去）", c.k)
		}
	}
	// 下单路径只能有 3 步：步骤一多，客户看着就嫌烦、不下单（用户 2026-09-22 明确）
	if len(fl.Steps) != 3 {
		t.Fatalf("下单路径应为 3 步，实际 %d —— 超过 3 步客户会觉得繁琐", len(fl.Steps))
	}
	stepText := ""
	uuLinked := false
	for i, st := range fl.Steps {
		if strings.TrimSpace(st.Title) == "" || strings.TrimSpace(st.Desc) == "" {
			t.Errorf("第 %d 步接入缺标题或说明：%+v", i+1, st)
		}
		if !icons[st.Icon] {
			t.Errorf("第 %d 步接入的图标 %q 不在 app.js 的 ICONS 里", i+1, st.Icon)
		}
		stepText += st.Title + st.Desc
		// 有 Link 就必须有 LinkText，反之亦然（只写一半前端会渲染成普通文字，链接无声失效）
		if (st.Link == "") != (st.LinkText == "") {
			t.Errorf("第 %d 步的 Link 与 LinkText 必须成对出现：%+v", i+1, st)
		}
		if st.Link != "" {
			if !strings.HasPrefix(st.Link, "https://") || !strings.Contains(st.Link, "uuyc.163.com") {
				t.Errorf("第 %d 步的外链必须是 UU远程 官网 https 地址，实际 %q", i+1, st.Link)
			}
			if st.LinkText != "UU远程" {
				t.Errorf("超链接文字必须是「UU远程」四个字（用户明确要求），实际 %q", st.LinkText)
			}
			if !strings.Contains(st.Title, "UU远程") {
				t.Errorf("带链接的那一步标题里必须出现「UU远程」，否则前端定位不到、渲染不出链接：%q", st.Title)
			}
			uuLinked = true
		}
	}
	if !uuLinked {
		t.Error("接入阶段没有任何一步带 UU远程 官网超链接 —— 用户明确要求把「UU远程」做成超链接")
	}
	for _, kw := range []string{"联系大叔", "故障", "UU远程", "验证码", "远程接入"} {
		if !strings.Contains(stepText, kw) {
			t.Errorf("接入阶段缺少关键词 %q —— 这 5 步是用户定好的，不能少", kw)
		}
	}

	/* 三种结局：一行兜底，把最坏情况讲在明处 */
	if len(fl.Ends) != 3 {
		t.Fatalf("结局应为 3 条（修好 / 退款 / 不收费），实际 %d", len(fl.Ends))
	}
	endTone, endKey := map[string]bool{}, map[string]bool{}
	endText := ""
	for i, e := range fl.Ends {
		if strings.TrimSpace(e.Key) == "" || strings.TrimSpace(e.Text) == "" {
			t.Errorf("第 %d 条结局缺编号或文案：%+v", i+1, e)
		}
		if endKey[e.Key] {
			t.Errorf("结局编号重复：%q", e.Key)
		}
		endKey[e.Key] = true
		if e.Tone != "ok" && e.Tone != "warn" && e.Tone != "mute" {
			t.Errorf("第 %d 条结局的配色 %q 不合法（只能 ok / warn / mute）", i+1, e.Tone)
		}
		if endTone[e.Tone] {
			t.Errorf("结局配色重复（%q）—— 三种结局要一眼分得开", e.Tone)
		}
		endTone[e.Tone] = true
		if !icons[e.Icon] {
			t.Errorf("第 %d 条结局的图标 %q 不在 app.js 的 ICONS 里", i+1, e.Icon)
		}
		if len([]rune(e.Text)) > 14 {
			t.Errorf("第 %d 条结局文案 %d 字太长 —— 一行要排得下三条", i+1, len([]rune(e.Text)))
		}
		endText += e.Text
	}
	// 客户最关心的三件事必须写在明处：修好怎么办 / 修不好怎么办 / 不认可报价怎么办
	for _, kw := range []string{"修好", "退款", "不收费"} {
		if !strings.Contains(endText, kw) {
			t.Errorf("三种结局里缺少 %q —— 不把最坏情况说清，客户不敢下单", kw)
		}
	}

	// ⑤ 真实口碑：至少 2 条客户评价 + 1 条案例，字段不能空
	reviews, cases := 0, 0
	for _, r := range pg.Reviews {
		switch r.Kind {
		case "review":
			reviews++
			if r.Name == "" || r.Meta == "" || r.Text == "" || r.Stars < 1 || r.Stars > 5 {
				t.Errorf("评价字段不完整：%+v", r)
			}
		case "case":
			cases++
			if r.Title == "" || r.Text == "" {
				t.Errorf("案例字段不完整：%+v", r)
			}
		default:
			t.Errorf("口碑 Kind 只能填 review / case，实际 %q", r.Kind)
		}
		if len(r.Tags) == 0 {
			t.Errorf("口碑 %q 缺少标签", r.Name+r.Title)
		}
	}
	if reviews < 2 || cases < 1 {
		t.Errorf("真实口碑至少 2 条评价 + 1 条案例，实际 评价 %d / 案例 %d", reviews, cases)
	}
}

// 价格守卫：用户 2026-09-22 明确要求「软件内完全不展示价格」，报价留在人工环节。
// 这条测试防止以后手滑把网站价目表贴回软件（贴回来 = 把报价权提前交出去）
var pricePattern = regexp.MustCompile(`\d+\s*元|元\s*/\s*[项次台]|元起|￥|\$`)

func TestSvcNoPriceLeak(t *testing.T) {
	pg := (&App{}).GetSvcPage()
	texts := []string{pg.Hero.Title, pg.Hero.Sub, pg.Hero.Identity, pg.Hero.Primary, pg.Hero.Second}
	for _, p := range pg.Promise {
		texts = append(texts, p.Text)
	}
	for _, it := range pg.Items {
		texts = append(texts, it.Name, it.Desc, it.Time, it.ActLabel)
		texts = append(texts, it.Points...)
	}
	for _, sp := range pg.Flow.Steps {
		texts = append(texts, sp.Title, sp.Desc, sp.LinkText)
	}
	texts = append(texts, pg.Flow.Title, pg.Flow.Desc)
	for _, e := range pg.Flow.Ends {
		texts = append(texts, e.Text)
	}
	for _, r := range pg.Reviews {
		texts = append(texts, r.Title, r.Text)
		texts = append(texts, r.Tags...)
	}
	for _, s := range texts {
		if pricePattern.MatchString(s) {
			t.Errorf("出现价格字样 %q —— 软件内不展示价格（用户明确要求，报价留在人工环节）", s)
		}
	}
}

// ---------- 表述守卫 ----------

// 全软件不得出现「免费检测」字样（用户 2026-09-22 明确：这个词不能这么表述）
// 扫的是两个承载用户可见文案的文件：后端 service.go + 前端 app.js
// （该表述与「自助免费 / 人工收费」的定位冲突：加微信是找付费人工，不是免费检测）
func TestNoFreeCheckWording(t *testing.T) {
	const bad = "免费检测"
	for _, f := range []string{"service.go", "frontend/dist/app.js"} {
		b, err := os.ReadFile(f)
		if err != nil {
			if os.IsNotExist(err) {
				/* 同上：v1.1 没有 frontend/dist/app.js（前端是单文件 design/ui-prototype.html）→ 跳过这一份，
				   service.go 那一份照查（后端文案同样不允许出现该表述）。 */
				t.Logf("跳过前端源码：%s 不存在（v1.1 前端为单文件）", f)
				continue
			}
			t.Fatalf("读取 %s 失败：%v", f, err)
		}
		txt := string(b)
		if i := strings.Index(txt, bad); i >= 0 {
			t.Errorf("%s 第 %d 行仍出现「%s」——该表述已作废，请改用「先诊断再报价」/「微信联系大叔」等口径",
				f, strings.Count(txt[:i], "\n")+1, bad)
		}
	}
}

// 结构守卫：服务大厅不再单独设「机器诊断报告」区块。
// 用户 2026-09-22 拍板 —— 首屏与联系卡已各有一个生成入口，再摆一个独立区是画蛇添足；
// 报告改由两个入口就地弹窗展示。这条测试钉住裁决，防止以后又被"补"回一个区块。
func TestSvcNoStandaloneReportSection(t *testing.T) {
	js, err := os.ReadFile(`frontend/dist/app.js`)
	if err != nil {
		if os.IsNotExist(err) {
			t.Skipf("跳过前端源码断言：frontend/dist/app.js 不存在（v1.1 前端为单文件 design/ui-prototype.html）")
		}
		t.Fatalf("读 app.js 失败：%v", err)
	}
	s := string(js)
	if strings.Contains(s, `class="card svc-report"`) {
		t.Error("服务大厅又出现了独立的「机器诊断报告」区块 —— 该区块已被用户取消，报告只在弹窗里")
	}
	// 两个入口 + 弹窗里负责展示报告的元素，一个都不能少（删区块不能把功能一起删掉）
	for _, need := range []string{`id="svcRep"`, `id="svcRep3"`, `id="repOut"`, "BuildDiagReport"} {
		if !strings.Contains(s, need) {
			t.Errorf("app.js 里找不到 %s —— 诊断报告入口或弹窗被误删", need)
		}
	}
	css, err := os.ReadFile(`frontend/dist/styles.css`)
	if err != nil {
		t.Fatalf("读 styles.css 失败：%v", err)
	}
	if strings.Contains(string(css), ".svc-report {") {
		t.Error("styles.css 里还留着已删区块的样式 .svc-report")
	}
	if !strings.Contains(string(css), ".svc-out") {
		t.Error("styles.css 缺少 .svc-out —— 弹窗里的报告正文会没有样式")
	}
}

// 转化入口守卫：用户 2026-09-22 —— 联系卡（页面最底部那张「1V1 远程支持」）主按钮点了必须直接弹二维码。
// 原先是「打开报修页」跳浏览器，多一步跳转就多一次流失；软件内所有付费转化按钮统一走 svcHire。
// 这条测试钉住裁决，并确保不会有按钮偷偷绑回外链。
func TestSvcContactPrimaryOpensQR(t *testing.T) {
	raw, err := os.ReadFile(`frontend/dist/app.js`)
	if err != nil {
		if os.IsNotExist(err) {
			/* v1.1 的前端是单文件 design/ui-prototype.html，不再有 frontend/dist/app.js。
			   这些断言查的是"前端源码里的 NAV / 图标表 / 文案"，1.1 里没有对象可查 → 如实跳过，
			   而不是 t.Fatal 把整个包判失败（那会把同文件里的后端断言一起埋掉）。 */
			t.Skipf("跳过前端源码断言：frontend/dist/app.js 不存在（v1.1 前端为单文件 design/ui-prototype.html）")
		}
		t.Fatalf("读 app.js 失败：%v", err)
	}
	s := string(raw)

	if !strings.Contains(s, `id="svcOrder2"`) {
		t.Error("联系卡主按钮 #svcOrder2 不见了 —— 页面最底部的转化入口不能丢")
	}
	if !strings.Contains(s, "if (b1) b1.onclick = svcHire;") {
		t.Error("联系卡主按钮没有绑到 svcHire —— 点了就不会弹二维码")
	}
	if strings.Contains(s, "b1.onclick = svcOrder") {
		t.Error("联系卡主按钮又被绑回「打开报修页」外链 —— 用户要求点了直接出二维码")
	}
	if strings.Contains(s, "打开报修页") {
		t.Error("app.js 里还留着「打开报修页」文案 —— 该表述已作废，付费入口一律「微信联系大叔」")
	}
	// 全软件四处转化按钮（首屏主按钮 / 服务卡 1V1 按钮 / 联系卡主按钮 / 报告弹窗扫码按钮）都得到 svcHire
	if n := strings.Count(s, "svcHire"); n < 4 {
		t.Errorf("svcHire 在 app.js 里只出现 %d 次 —— 转化入口应有 4 处以上（定义 1 + 绑定 3~4）", n)
	}
	if !strings.Contains(s, `closest("[data-order]")`) {
		t.Error("服务卡上的「1V1远程支持（付费）」按钮没接上 svcHire")
	}
}


// 渲染守卫：服务流程是一整张 SVG 真流程图，前端必须有渲染器 / 判断菱形 / 走线 / 自动布局常量，
// 旧版"把用户的话排成三行文字"的痕迹（svcFlowLineHTML / .flow-line / .flow-chip…）不许残留 —— 否则页面一半图一半字。
func TestSvcFlowRenderedInApp(t *testing.T) {
	raw, err := os.ReadFile(`frontend/dist/app.js`)
	if err != nil {
		if os.IsNotExist(err) {
			/* v1.1 的前端是单文件 design/ui-prototype.html，不再有 frontend/dist/app.js。
			   这些断言查的是"前端源码里的 NAV / 图标表 / 文案"，1.1 里没有对象可查 → 如实跳过，
			   而不是 t.Fatal 把整个包判失败（那会把同文件里的后端断言一起埋掉）。 */
			t.Skipf("跳过前端源码断言：frontend/dist/app.js 不存在（v1.1 前端为单文件 design/ui-prototype.html）")
		}
		t.Fatalf("读 app.js 失败：%v", err)
	}
	s := string(raw)
	for _, need := range []string{`id="svcFlow"`, "svcFlowHTML", "fpStepHTML", "fp-steps", "fp-ends", `class="flow-lk"`} {
		if !strings.Contains(s, need) {
			t.Errorf("app.js 里找不到 %s —— 服务流程没渲染出来", need)
		}
	}
	// 旧版那些又高又繁琐的大图 / 三路分支网格已经删掉，别让残骸再长回来
	for _, bad := range []string{"svcFlowSVG", "fgDiamondSVG", "fgEdgeSVG", "fgRoundPath", "fgLabelSVG",
		"fgNodeSVG", "FG.ROW_Y", "fgBoxSVG", "fgStepSVG", "fp-grid", "fp-chip", "fp-fork", "fp-tag", "fp-shared"} {
		if strings.Contains(s, bad) {
			t.Errorf("app.js 里还留着旧版繁琐流程的痕迹 %s —— 会把客户吓退", bad)
		}
	}
	css, err := os.ReadFile(`frontend/dist/styles.css`)
	if err != nil {
		t.Fatalf("读 styles.css 失败：%v", err)
	}
	for _, need := range []string{".fp-steps", ".fp-step", ".fp-ends", ".fp-end", ".fp-ic"} {
		if !strings.Contains(string(css), need) {
			t.Errorf("styles.css 缺少 %s —— 流程样式不全", need)
		}
	}
	for _, bad := range []string{".fg-box", ".fg-edge", ".fg-dia", ".fg-n ", ".fg-lg", ".fg-legend", ".fg-ic-bg",
		".fp-grid", ".fp-chip", ".fp-fork", ".fp-tag", ".fp-sub"} {
		if strings.Contains(string(css), bad) {
			t.Errorf("styles.css 里还留着旧版样式 %s", bad)
		}
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
