package main

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// 本机配置页（pcinfo.go / pcRender）的显示口径回归测试。
// 这一页是纯只读展示页，数值口径错了没人会崩，但小白一看就觉得软件在瞎报，
// 所以把几条「说人话」的规矩钉在这里。

func TestFmtRAMHumanReadable(t *testing.T) {
	cases := []struct {
		in   int64
		want string
	}{
		{8589934592, "8 GB"},   // 8GiB 的条子：按 1000 口径会变成 8.6 GB，小白以为内存条是怪规格
		{4294967296, "4 GB"},   // 单条 4GB，不能写成「4.0 GB」
		{1633939456, "1.5 GB"}, // 小数保留一位
		{0, "—"},
		{-1, "—"},
	}
	for _, c := range cases {
		if got := fmtRAM(c.in); got != c.want {
			t.Errorf("fmtRAM(%d) = %q, 期望 %q", c.in, got, c.want)
		}
	}
}

func TestHumanUptime(t *testing.T) {
	cases := []struct {
		minutes int
		want    string
	}{
		{12, "12 分钟"},
		{59, "59 分钟"},
		{60, "1 小时 0 分"},
		{90, "1 小时 30 分"},
		{60 * 24, "1 天 0 小时"},
		{60*24*2 + 60*5, "2 天 5 小时"},
	}
	for _, c := range cases {
		if got := humanUptime(int64(c.minutes) * 60 * 1000); got != c.want {
			t.Errorf("humanUptime(%d 分钟) = %q, 期望 %q", c.minutes, got, c.want)
		}
	}
	// 不足 1 分钟说「不到 1 分钟」，不是「0 分钟」
	if got := humanUptime(30 * 1000); got != "不到 1 分钟" {
		t.Errorf("humanUptime(30 秒) = %q, 期望「不到 1 分钟」", got)
	}
	// 取不到就留空（前端显示「读不到」），不许编一个时长出来
	if got := humanUptime(0); got != "" {
		t.Errorf("humanUptime(0) = %q, 期望空串", got)
	}
	if got := humanUptime(-5); got != "" {
		t.Errorf("humanUptime(-5) = %q, 期望空串", got)
	}
}

func TestStickLabel(t *testing.T) {
	if got, want := stickLabel([]int64{4294967296, 4294967296}, "Kingston"), "2 条 4 GB · Kingston"; got != want {
		t.Errorf("同容量双条 = %q, 期望 %q", got, want)
	}
	if got, want := stickLabel([]int64{4294967296, 8589934592}, ""), "2 条（4 GB + 8 GB）"; got != want {
		t.Errorf("混插 = %q, 期望 %q", got, want)
	}
	if got := stickLabel(nil, "Kingston"); got != "" {
		t.Errorf("读不到条子时应留空，得到 %q", got)
	}
}

// 内存占用必须和顶栏芯片同源：两条路（WMI / Win32）取的不是同一时点，
// 同屏会出现两个百分比 —— 2026-09-23 走查点名过，这里钉死防复发。
func TestMemUsageSameSourceAsTopbar(t *testing.T) {
	src := readFileT(t, "pcinfo.go")
	if strings.Contains(src, "FreePhysicalMemory") {
		t.Error("pcinfo.go 又去查 WMI FreePhysicalMemory 了：内存占用必须走 memPhys()，和顶栏芯片同源")
	}
	if !strings.Contains(src, "memPhys()") {
		t.Error("pcinfo.go 里找不到 memPhys()：内存占用没有走与顶栏同源的取法")
	}
	// 旧写法是内联拼「X 天 Y 小时 Z 分」，要的是 humanUptime()
	if strings.Contains(src, "d.Minutes())%60") {
		t.Error("pcinfo.go 里还留着内联拼时长的机器话（X 天 Y 小时 Z 分），应该走 humanUptime()")
	}
}

/* 本机配置页排版（2026-09-24 重排后的契约）
   用户点名要的顺序：① 台式机/笔记本（图形背景）→ ② 操作系统 → ③ 主要部件概况 → ④ 部件详情。
   布局是左右分栏：左「主要部件概况」（hover 弹详情），右「运行情况」。
   FALSIFY：谁把顺序调回去（先堆参数表、机型藏到最后）或者把左右栏改成上下堆卡片，这里会红。 */
func TestPcPageLayoutWired(t *testing.T) {
	css := readFileT(t, "frontend/dist/styles.css")
	js := readFileT(t, "frontend/dist/app.js")

	// ① 左右分栏：左部件 / 右运行，右栏不能窄到把环形仪表挤变形
	if !regexp.MustCompile(`(?s)\.pc-main\s*\{[^}]*grid-template-columns:\s*minmax\(0,\s*1\.15fr\)\s+minmax\(330px,\s*\.85fr\)`).MatchString(css) {
		t.Error("主要部件/运行情况不是左右两栏（minmax(0,1.15fr) minmax(330px,.85fr)）：右栏会被挤扁")
	}
	// ② 系统和 BIOS 两列网格（竖排 6 行太长）
	if !regexp.MustCompile(`(?s)\.pc-sys\s*\{[^}]*grid-template-columns:\s*1fr 1fr`).MatchString(css) {
		t.Error("「系统与 BIOS」不是两列网格：6 项竖排太长")
	}
	if !strings.Contains(js, `class="page pc-page"`) {
		t.Error("本机配置页丢了 pc-page 类：.pc-page 下的紧凑间距会失效")
	}
	if strings.Contains(js, "pc-row") || strings.Contains(css, "pc-row") {
		t.Error("还留着旧的单列 pc-row 样式/结构")
	}

	// ③ 版式顺序：机型横幅 → 左右分栏 → 次要信息折叠，且「机型」在「操作系统」之上
	tpl := js[strings.Index(js, "PAGES.pcinfo = () =>"):]
	tpl = tpl[:strings.Index(tpl, "`;")]
	iHero := strings.Index(tpl, `id="pcHero"`)
	iOS := strings.Index(tpl, `id="pcOS"`)
	iKind := strings.Index(tpl, `id="pcKind"`)
	iMain := strings.Index(tpl, `class="pc-main`)
	iMore := strings.Index(tpl, `id="pcMore"`)
	if iKind < 0 || iOS < 0 || iHero < 0 || iMain < 0 || iMore < 0 {
		t.Fatalf("页面模板缺关键节点：kind=%d os=%d hero=%d main=%d more=%d", iKind, iOS, iHero, iMain, iMore)
	}
	if iKind > iOS {
		t.Error("「机型」排在「操作系统」后面了：用户要的是先看清是台式机还是笔记本")
	}
	if !(iHero < iMain && iMain < iMore) {
		t.Error("版式顺序不对，应该是：机型横幅 → 主要部件/运行情况 → 次要信息折叠")
	}
	mainBlock := pcMainBlock(css)
	// ④ 部件详情走浮层，不是堆在列表里（列表会变成一坨）
	if !strings.Contains(js, `id="pcPop"`) || !strings.Contains(js, "pcBindPop") {
		t.Error("部件详情浮层没了：详情堆回列表会让「概况」变成一坨")
	}
	/* ⑤ 两栏必须等高（2026-09-24 用户反馈「左右高度不一致，看着不协调」：
	   实测左 494 / 右 712，右栏底下多一截）。
	   光写 stretch 还不够 —— 卡片得是 flex 竖排、列表得 flex:1 把多余高度吃下去，
	   否则卡片齐了、里面内容还是缩在顶上，底下照样空一截。 */
	if strings.Contains(mainBlock, "align-items: start") {
		t.Error(".pc-main 又写了 align-items: start：两栏会各自按内容收高，底边一长一短")
	}
	if !regexp.MustCompile(`(?s)\.pc-parts\s*\{[^}]*display:\s*flex`).MatchString(css) ||
		!regexp.MustCompile(`(?s)\.pc-parts\s*\{[^}]*flex-direction:\s*column`).MatchString(css) {
		t.Error(".pc-parts 不是 flex 竖排：栏被拉高后多出来的高度传不下去给部件行")
	}
	if !regexp.MustCompile(`(?s)\.pc-part-list\s*\{[^}]*flex:\s*1`).MatchString(css) {
		t.Error(".pc-part-list 没有 flex:1：部件行吃不到多出来的高度，卡片底部会空一截")
	}
	if !regexp.MustCompile(`(?s)\.pc-run\s*\{[^}]*display:\s*flex`).MatchString(css) {
		t.Error(".pc-run 不是 flex 竖排：左栏更高时右栏吃不下多余高度，又会错开")
	}
}

// 取 CSS 里 .pc-main 的样式块（含后面的注释不影响，取到第一个 } 为止）
func pcMainBlock(css string) string {
	i := strings.Index(css, ".pc-main {")
	if i < 0 {
		return ""
	}
	if j := strings.Index(css[i:], "}"); j >= 0 {
		return css[i : i+j+1]
	}
	return ""
}

/* FALSIFY：机型线稿写死高度会被横幅 overflow:hidden 切掉机箱底座。
   横幅高度取决于文字行数（本机 122px），线稿写死 148px 就被切了 ——
   必须靠 top/bottom 撑满 + SVG 自己缩放，不能写死 px 高度。 */
func TestHeroFigureNotClipped(t *testing.T) {
	css := readFileT(t, "frontend/dist/styles.css")
	block := regexp.MustCompile(`(?s)\.pc-hero-fig\s*\{[^}]*\}`).FindString(css)
	if block == "" {
		t.Fatal("找不到 .pc-hero-fig 样式块")
	}
	if regexp.MustCompile(`height:\s*\d+px`).MatchString(block) {
		t.Error("线稿写死了 px 高度：横幅高度随文字行数变，写死就会被 overflow:hidden 切掉图形")
	}
	if !strings.Contains(block, "top:") || !strings.Contains(block, "bottom:") {
		t.Error("线稿没有 top/bottom 撑满：撑不满就没法跟着横幅高度自适应")
	}
	// 线稿必须是「描边」不是「实心块」——实心黑块盖在横幅上就是一坨
	if !regexp.MustCompile(`(?s)\.pc-fig\s+:where\([^)]*\)\s*\{[^}]*fill:\s*none`).MatchString(css) {
		t.Error("线稿图形没有 fill:none：会变成一排实心黑块盖在横幅上")
	}
}

// 同一个事实只在一处出现：BIOS 归「系统和主板」区，主板卡就别再写一遍。
func TestNoDuplicateBiosOnPcPage(t *testing.T) {
	js := readFileT(t, "frontend/dist/app.js")
	start := strings.Index(js, "function pcRender(h)")
	end := strings.Index(js, "/* ============ 健康分")
	if start < 0 || end < start {
		t.Fatal("找不到 pcRender()，测试需要跟着改")
	}
	body := js[start:end]
	if n := strings.Count(body, "h.bios"); n != 1 {
		t.Errorf("pcRender 里 h.bios 出现 %d 次，应该只出现 1 次（主板卡和系统区重复显示 BIOS）", n)
	}
}

// 虚拟显示器（远程/投屏用）不能冒充显卡：本机实测 GameViewer、向日葵 OrayIddDriver
// 都会被 Win32_VideoController 报出来，混进真卡里会让小白以为自己有两块显卡。
func TestVirtualGpuNotListedAsReal(t *testing.T) {
	src := readFileT(t, "pcinfo.go")
	if !strings.Contains(src, "h.Virts = append") {
		t.Error("pcinfo.go 没有把虚拟显示器分到 Virts：会混进真显卡列表")
	}
	if !strings.Contains(src, "h.GPUs = append") {
		t.Error("pcinfo.go 不再往 GPUs 里放真卡了")
	}
	js := readFileT(t, "frontend/dist/app.js")
	if !strings.Contains(js, "h.virts") {
		t.Error("前端没有消费 h.virts：虚拟显示器要么没显示，要么又混进了真卡")
	}
}

/* 浮层必须和它压着的卡片分清层次（2026-09-24 用户反馈「浮层背景和后面背景太相近」）。
   根因：.pc-pop 直接用 var(--pc-card)（白），所在卡片也是白 → 两块糊在一起。
   FALSIFY：谁把背景改回 var(--pc-card) 或 var(--card)，这里立刻红。 */
func TestPcPopContrastsWithCard(t *testing.T) {
	css := readFileT(t, "frontend/dist/styles.css")
	block := regexp.MustCompile(`(?s)\.pc-pop\s*\{[^}]*\}`).FindString(css)
	if block == "" {
		t.Fatal("找不到 .pc-pop 样式块")
	}
	if regexp.MustCompile(`background:\s*var\(--(pc-)?card\)`).MatchString(block) {
		t.Error("浮层背景又写回 var(--pc-card)/var(--card) 了：和所在卡片同色，两块糊在一起分不清")
	}
	/* 只写 background 简写（渐变）时 backgroundColor 是透明的，
	   探针量不出「浮层 vs 卡片」的亮度差 —— 所以必须有一层实色。 */
	if !strings.Contains(block, "background-color:") {
		t.Error("浮层没写实色底：backgroundColor 会是透明的，量不出和卡片的色差（渐变要叠在实色上）")
	}
	if !strings.Contains(block, "rgba(79, 70, 229") && !strings.Contains(block, "rgba(79,70,229") {
		t.Error("浮层描边没有主色：光靠浅灰边在白卡片上还是分不出层次")
	}
}

/* 两栏高度压缩（2026-09-24 用户反馈「主要部件和运行情况太高了，压缩一下」）。
   CSS 里量不到运行时高度，所以钉住「不许退回旧的大尺寸」：
   环形 84→64、秒表格 62→52、部件行上下内距 9→6。
   FALSIFY：谁把环形改回 84px，这里红。 */
func TestPcColumnsStayCompact(t *testing.T) {
	css := readFileT(t, "frontend/dist/styles.css")
	ring := regexp.MustCompile(`(?s)\.pc-g-ring\s*\{[^}]*\}`).FindString(css)
	if regexp.MustCompile(`(width|height):\s*(8[0-9]|[1-9][0-9]{2,})px`).MatchString(ring) {
		t.Errorf("环形仪表又变回大尺寸了（%s）：两栏高度会被顶回 700+，用户点名嫌高", ring)
	}
	if lv := regexp.MustCompile(`(?s)\.pc-lv\s*\{[^}]*min-height:\s*(\d+)px`).FindStringSubmatch(css); len(lv) == 2 {
		if n := atoiT(lv[1]); n > 56 {
			t.Errorf("实时格 min-height=%d 又变高了（压到 52 是用户要求的紧凑度）", n)
		}
	}
	part := regexp.MustCompile(`(?s)\.pc-part\s*\{[^}]*padding:\s*(\d+)px`).FindStringSubmatch(css)
	if len(part) == 2 {
		if n := atoiT(part[1]); n > 7 {
			t.Errorf("部件行上下内距 %dpx 又变胖了（压到 6 是用户要求的紧凑度）", n)
		}
	}
	/* 「温度与风扇」那格天生比上面一行高（实测 139 vs 64）。
	   用 grid-auto-rows: 1fr 会把矮的那行也拉到 139，凭空多出 75px —— 用户嫌两栏太高。 */
	liveBlk := regexp.MustCompile(`(?s)\.pc-run\s+\.pc-live\s*\{[^}]*\}`).FindString(css)
	if strings.Contains(liveBlk, "grid-auto-rows: 1fr") {
		t.Error(".pc-run .pc-live 又写成 grid-auto-rows: 1fr 了：矮的那行会被温度格拉到一样高，两栏凭空多 75px")
	}
}

func atoiT(s string) int {
	n := 0
	for _, c := range s {
		if c < '0' || c > '9' {
			return n
		}
		n = n*10 + int(c-'0')
	}
	return n
}

/* 机型线稿得像机器，不能像个计算器（2026-09-24 用户原话「顶部台式机的图不像台式机，像个计算器」）。
   旧版台式机是「一个方盒 + 上面一块小屏 + 两个圆点 + 三根横杠」—— 单盒无线条特征，
   小白读成计算器。台式机的识别特征是**屏幕 + 立着的主机两个形体并列**。
   这里钉两条可判别的几何事实：
     ① 有一个「瘦高」的主体（高 ≥ 2× 宽，即立式主机）
     ② 有一个「横宽」的屏（宽 > 高）
   FALSIFY：把图形改回单盒，① 立刻红。 */
func TestFormFigureLooksLikeTheMachine(t *testing.T) {
	js := readFileT(t, "frontend/dist/app.js")
	start := strings.Index(js, "function pcFormFigure(factor)")
	if start < 0 {
		t.Fatal("找不到 pcFormFigure()，测试要跟着改")
	}
	body := js[start : start+3000]

	rects := regexp.MustCompile(`<rect x="([\d.]+)" y="([\d.]+)" width="([\d.]+)" height="([\d.]+)"`).FindAllStringSubmatch(body, -1)
	if len(rects) < 4 {
		t.Fatalf("机型图形只有 %d 个矩形，看不出是台机器", len(rects))
	}
	var tall, wide bool
	for _, r := range rects {
		w, h := parseFloatT(r[3]), parseFloatT(r[4])
		if h >= w*2 {
			tall = true // 立式主机
		}
		if w > h && w >= 50 {
			wide = true // 显示器那块屏
		}
	}
	if !tall {
		t.Error("台式机线稿里没有「瘦高」的主机（高 ≥ 2× 宽）：一个方盒会被看成计算器")
	}
	if !wide {
		t.Error("台式机线稿里没有横着的屏幕：单画主机认不出是电脑")
	}

	// 四种机型都得有图 —— 笔记本/平板也要变，不能所有机型都画台式机
	for _, f := range []string{"desktop", "laptop", "tablet"} {
		if !strings.Contains(body, f+":") {
			t.Errorf("pcFormFigure 少了 %s 的图形：换机型时页面图形不变", f)
		}
	}
	if !strings.Contains(body, "other") {
		t.Error("pcFormFigure 没有判不出机型时的兜底图形")
	}
}

func parseFloatT(s string) float64 {
	var intPart, frac float64
	dot := false
	div := 10.0
	for _, c := range s {
		if c == '.' {
			dot = true
			continue
		}
		if dot {
			frac += float64(c-'0') / div
			div *= 10
		} else {
			intPart = intPart*10 + float64(c-'0')
		}
	}
	return intPart + frac
}

/* 读的必须是**当前这台机器**（2026-09-24 用户点名：「不能换了机器还是显示原机器的配置」）。
   两条硬规矩：
     ① GetPCFull 每次都现读，不许读/写本地缓存文件；
     ② 源码里不许出现任何本机专属的型号串（写死就等于换机器还显示旧的）。
   FALSIFY：谁为了提速在这里加一层缓存，或者照着开发机硬编码一份默认值，这里红。 */
func TestPcInfoIsReadLiveNotCached(t *testing.T) {
	src := readFileT(t, "pcinfo.go")
	start := strings.Index(src, "func (a *App) GetPCFull()")
	if start < 0 {
		t.Fatal("找不到 GetPCFull()，测试要跟着改")
	}
	fn := src[start:strings.Index(src[start:], "\n}\n")+start+3]
	for _, bad := range []string{"os.ReadFile", "os.WriteFile", "json.Unmarshal", "ioutil.ReadFile", "os.Open"} {
		if strings.Contains(fn, bad) {
			t.Errorf("GetPCFull 里出现了 %s：本机配置不许走本地缓存，必须每次现读当前机器", bad)
		}
	}
	if !strings.Contains(fn, "f.ReadAt = time.Now()") {
		t.Error("GetPCFull 没打读取时间戳：页面上就没有「这是刚读的」这条证据")
	}
	js := readFileT(t, "frontend/dist/app.js")
	if !strings.Contains(js, "h.readAt") {
		t.Error("前端没消费 readAt：页面上不显示读取时间，用户没法确认是现读的")
	}
}

/* 本机专属型号串不许进代码（注释里提一次当例子可以，代码里写死不行 ——
   写死了在别人机器上就是报错数据，等于「换了机器还显示我机器的配置」）。 */
func TestNoHardcodedMachineValues(t *testing.T) {
	files := []string{"pcinfo.go", "hwspec.go", "hwspec_cpu.go", "hwbrand.go", "hwform.go", "hwlive.go", "frontend/dist/app.js"}
	banned := []string{"G5400", "H310M", "WD5000AZLX", "99P5732", "DS2"}
	for _, f := range files {
		code := stripComments(readFileT(t, f), f)
		for _, b := range banned {
			if strings.Contains(code, b) {
				t.Errorf("%s 里写死了本机专属串 %q：换台机器就会显示开发机的配置", f, b)
			}
		}
	}
}

// stripComments 去掉注释再查硬编码 —— 注释里举自家机器的例子是允许的。
// Go 和 JS 的两种注释（双斜杠行注释、斜杠星号块注释）都得剥，
// 不然 hwspec.go 里那段「实测本机 G5400：4 个子叶」的块注释会被当成代码。
func stripComments(src, file string) string {
	var out []string
	inBlock := false
	for _, line := range strings.Split(src, "\n") {
		var keep []rune
		for i := 0; i < len(line); {
			if inBlock {
				if i+1 < len(line) && line[i] == '*' && line[i+1] == '/' {
					inBlock = false
					i += 2
				}
				i++
				continue
			}
			if i+1 < len(line) && line[i] == '/' && line[i+1] == '*' {
				inBlock = true
				i += 2
				continue
			}
			if i+1 < len(line) && line[i] == '/' && line[i+1] == '/' {
				break
			}
			keep = append(keep, rune(line[i]))
			i++
		}
		out = append(out, string(keep))
	}
	return strings.Join(out, "\n")
}

/* 保存截图（2026-09-24 用户反馈「截图功能不可用，取不到像素」）。
   实测复现：1220×780 主窗口上 PrintWindow 成功但 GetDIBits 返回 0（GetLastError 也是 0），
   缓冲全 0 → 页面报「取不到像素」。小窗口（160×28）上反而不复现，所以别拿小窗当回归证据。
   三条规矩钉死：
     ① 自建 DIB 画布（CreateDIBSection），不许回到 GetDIBits；
     ② 窗口最小化时 GetWindowRect 给的是任务栏按钮尺寸，必须先恢复；
     ③ 同名窗口不止一个，按面积取最大的，不许「抓到第一个就停」。
   FALSIFY：把 CreateDIBSection 换回 CreateCompatibleBitmap+GetDIBits，① 红。 */
func TestWindowCaptureWired(t *testing.T) {
	src := stripComments(readFileT(t, "pcshare.go"), "pcshare.go")
	if strings.Contains(src, "GetDIBits") {
		t.Error("pcshare.go 又用 GetDIBits 取像素了：实测在主窗口上会返回 0（缓冲全 0），页面报「取不到像素」")
	}
	if !strings.Contains(src, "CreateDIBSection") {
		t.Error("pcshare.go 没有 CreateDIBSection：必须自建 DIB 画布，让 PrintWindow 直接画进我们的内存")
	}
	if !strings.Contains(src, `"PrintWindow"`) {
		t.Error("pcshare.go 丢了 PrintWindow：WebView2 只有这条路抓得到内容")
	}
	if !strings.Contains(src, "IsIconic") || !strings.Contains(src, "ShowWindow") {
		t.Error("没有处理最小化窗口：最小化时 GetWindowRect 只有任务栏按钮那点尺寸（实测 160×28），会截出一条边")
	}
	if !strings.Contains(src, "area") {
		t.Error("窗口选取没有按面积挑最大的：同进程可能有多个可见窗口，抓错就截出小图")
	}
}

/* 温度格不再挂「去装 LibreHardwareMonitor」引导（2026-09-24 用户点名：这个提示不要了）。
   规矩：读不到就照实写「读不到 + 为什么读不到」，页面不许替用户决定要不要装第三方软件。
   FALSIFY：把 .pc-lv-cta 那个按钮加回去，①②红。 */
func TestNoLHMInstallPrompt(t *testing.T) {
	js := readFileT(t, "frontend/dist/app.js")
	css := readFileT(t, "frontend/dist/styles.css")
	// ① 前端不许再出现 LHM 引导的任何痕迹（按钮 / 探测调用 / 外链文案）
	for _, s := range []string{"LibreHardwareMonitor", "pcLhmBtn", "GetLHMStatus", "pc-lv-cta", "pc.lhm"} {
		if strings.Contains(js, s) {
			t.Errorf("app.js 还留着 LHM 引导的痕迹 %q：用户点名不要这个提示了", s)
		}
	}
	// ② 样式里那个引导按钮也得清掉，不然哪天谁照着样式把按钮加回来
	if strings.Contains(css, "pc-lv-cta") {
		t.Error("styles.css 还留着 .pc-lv-cta：留着就有人照着加回引导按钮")
	}
	// ③ 去掉引导不等于什么都不说：温度格仍要写清为什么读不到
	if !strings.Contains(js, "这台机器没开放") {
		t.Error("温度格的「为什么读不到」说明没了：去掉引导按钮可以，读不到就装哑巴不行")
	}
	if !strings.Contains(js, "温度与风扇") {
		t.Error("温度与风扇那格整块没了")
	}
}

/* 底部「更多」默认展开（2026-09-24 用户要求：别藏起来）。
   此处钉的是模板初始态 —— 只改 pcBindMore 的默认值不算数，
   首屏 HTML 里 body 带 hidden 的话，展开前页面就还是折叠的。
   FALSIFY：给 pcMoreBody 加回 hidden，①红；aria-expanded 改回 false，②红。 */
func TestPcMoreOpenByDefault(t *testing.T) {
	js := readFileT(t, "frontend/dist/app.js")
	i := strings.Index(js, `id="pcMoreBody"`)
	if i < 0 {
		t.Fatal("页面模板里找不到 pcMoreBody")
	}
	// ① body 初始不能 hidden（同一行里出现 hidden 就算折叠）
	if strings.Contains(js[i:i+120], "hidden") {
		t.Error("pcMoreBody 初始还是 hidden：底部「更多」默认折叠，用户点开之前看不到硬盘分区/网卡")
	}
	// ② 按钮初始态：aria-expanded=true + on 类（箭头朝下、文案一致）
	btn := regexp.MustCompile(`<button[^>]*id="pcMoreBtn"[^>]*>`).FindString(js)
	if btn == "" {
		t.Fatal("页面模板里找不到 pcMoreBtn")
	}
	if !strings.Contains(btn, `aria-expanded="true"`) {
		t.Error("pcMoreBtn 的 aria-expanded 不是 true：无障碍状态和实际展开状态对不上")
	}
	if !strings.Contains(btn, "pc-more-t on") {
		t.Error("pcMoreBtn 初始不带 on 类：箭头不会转 90°，看着像折叠的")
	}
}

/* 多块硬盘要显示编号（2026-09-24 用户要求）。
   不编号的话「型号 / 容量」在详情里重复出现两遍，谁也分不清哪行是哪块盘。
   编号还得跟系统对得上：后端按 PHYSICALDRIVEn 的序号排序，
   并把「磁盘 N」（磁盘管理同口径）一并摆出来。
   FALSIFY：把 diskNo 前缀去掉（单盘多盘都不编号），①红；删掉后端排序，③红。 */
func TestMultiDiskNumbered(t *testing.T) {
	/* 这里读原文、不剥注释：app.js 里有含 /* 的字串，朴素的剥注释会把大段代码一并吞掉，
	   那样「代码还在」的断言会假绿。改成匹配代码形态（带引号/带箭头函数），注释里写不算数。 */
	js := readFileT(t, "frontend/dist/app.js")
	// ① 多盘判定 + 编号函数都还在
	if !strings.Contains(js, "const multiDisk = phys.length > 1") {
		t.Error("没了 multiDisk 判定：单盘也会被标上「硬盘 1」，一块盘还编号是废话")
	}
	if !strings.Contains(js, "硬盘${i + 1}") {
		t.Error("编号不是「硬盘N」：多块盘的型号/容量行会重名，分不清是哪块")
	}
	// ② 三处都得带编号：部件卡型号行、详情参数表、更多区的列表徽标
	if !strings.Contains(js, `class="pc-tag idx"`) {
		t.Error("更多区硬盘列表没有编号徽标：两块盘并排显示时看不出谁是谁")
	}
	if !strings.Contains(js, `pfx + "系统盘号"`) {
		t.Error("详情里没标系统盘号：用户要去磁盘管理里对号时无从对照")
	}
	/* 只断言「编号函数还在」不够 —— 把参数表里的 pfx 前缀去掉后，
	   上面几条全都能通过（实测）。所以要正面钉住这条赋值，
	   它才是让详情里每行都挂上「硬盘N·」的那一处。 */
	if !strings.Contains(js, "const pfx = multiDisk ? `${diskNo(i)}·` : \"\"") {
		t.Error("硬盘详情的参数表不再带编号前缀：两块盘的「型号/容量」会各出现两遍，分不清哪行是哪块盘")
	}
	// ③ 后端：盘序号真的读出来了，而且按序号排（WMI 返回顺序不保证）
	src := stripComments(readFileT(t, "hwdev.go"), "hwdev.go")
	if !strings.Contains(src, `json:"index"`) {
		t.Error("hwDiskDrive 没有 index 字段：前端拿不到系统盘号，编号只能靠数组顺序猜")
	}
	if !strings.Contains(src, "sort.SliceStable(out") {
		t.Error("物理盘没按系统序号排序：换台机器 WMI 顺序一乱，编号就跟磁盘管理对不上")
	}
}

/* 切页必须给上一页收尾（2026-09-24）。
   renderPage 以前只调 mount、从不调 unmount：本机配置页明明定义了 PAGES.pcinfo.unmount
   （停 pc.timer + 摘 visibilitychange/focus 监听），却一个调用点都没有 ——
   离开那页之后 1 秒一次的 GetLivePerf 还在后台跑，监听器每进一次页面叠一个。
   现在是路由统一收尾：谁以后定义了 unmount 就自动生效。
   FALSIFY：把 renderPage 里那句 PAGES[mountedPage].unmount() 去掉，②红。 */
func TestPageUnmountIsWired(t *testing.T) {
	js := readFileT(t, "frontend/dist/app.js") // 读原文、不剥注释（app.js 注释里有 /*，剥了会假绿）
	// ① pcinfo 确实把收尾挂上了，收的正是那两样东西
	if !strings.Contains(js, "PAGES.pcinfo.unmount = () => { pcStopLive(); }") {
		t.Error("pcinfo 没把 pcStopLive 挂到 unmount 上：切走之后定时器不会停")
	}
	if !strings.Contains(js, `document.removeEventListener("visibilitychange", pc.onWake)`) {
		t.Error("pcStopLive 没摘 visibilitychange 监听：每进一次本机配置页就多挂一个")
	}
	// ② 路由真的会调上一页的 unmount —— 光声明不算，之前就是只声明不调用。
	//    只匹配裸字串会假绿：把那行注释掉，字串还在，测试照样过（2026-09-24 反证实测）。
	//    所以只看 renderPage 函数体，并且先剥掉行注释再断言。
	rp := strings.Index(js, "function renderPage() {")
	if rp < 0 {
		t.Fatal("找不到 renderPage，路由写法是不是变了")
	}
	end := strings.Index(js[rp:], "\n}")
	if end < 0 {
		t.Fatal("renderPage 函数体没有正常结束")
	}
	kept := make([]string, 0, 12)
	for _, ln := range strings.Split(js[rp:rp+end], "\n") {
		if i := strings.Index(ln, "//"); i >= 0 {
			ln = ln[:i]
		}
		kept = append(kept, ln)
	}
	if !strings.Contains(strings.Join(kept, "\n"), "PAGES[mountedPage].unmount()") {
		t.Error("renderPage 没有真正调用上一页的 unmount：unmount 形同虚设，后台轮询照跑")
	}
	// ③ 挂上之后要记下来，下次切页才知道该收谁的尾
	if !strings.Contains(js, `let mountedPage = ""`) {
		t.Error("没有 mountedPage 记录当前挂载页：下一次切页不知道该给谁收尾")
	}
	// ④ 必须是 mount 之后才登记（顺序反了会把刚挂上的页立刻收掉）
	mi := strings.Index(js, "if (PAGES[curPage]?.mount) {")
	if mi < 0 {
		t.Fatal("找不到 mount 调用点，路由写法是不是变了")
	}
	if !strings.Contains(js[mi:mi+220], "mountedPage =") {
		t.Error("mount 之后没有登记 mountedPage：下一次切页收不到尾")
	}
}

func readFileT(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		/* v1.1 的前端是单文件 design/ui-prototype.html，不再有 frontend/dist/app.js。
		   这些断言是 v1.0 时代针对前端源码的"接线检查"，在 1.1 里没有对象可查 ——
		   如实跳过这一条，而不是 t.Fatal 把整个包判失败（那会把同文件里的后端断言一起埋掉）。
		   1.1 对应的前端检查改为 GUI 截图实证（见 _devtools/gui_shots.py）。 */
		if os.IsNotExist(err) {
			t.Skipf("跳过前端源码断言：%s 不存在（v1.1 前端已改为单文件 design/ui-prototype.html）", p)
		}
		t.Fatalf("读不到 %s：%v", p, err)
	}
	return string(b)
}
