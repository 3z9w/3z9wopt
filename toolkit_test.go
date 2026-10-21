package main

import (
	"bytes"
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"golang.org/x/text/encoding/simplifiedchinese"
	"golang.org/x/text/encoding/unicode"
)

// ---------- 目录完整性（防回退：改动工具清单后这些断言会立刻报警） ----------

func TestToolCatalogIntegrity(t *testing.T) {
	buildCatalog()
	if len(toolCatalog) != 34 { // 2026-09-22 +1：fix 类新增「创建系统还原点」（优化前的后路）
		t.Fatalf("工具总数应为 34，实际 %d", len(toolCatalog))
	}
	// 分类数量分布（与前端展示的口径一致）
	want := map[string]int{"fix": 12, "net": 7, "disk": 4, "info": 5, "util": 6}
	got := map[string]int{}

	seen := map[string]bool{}
	for _, it := range toolCatalog {
		if it.ID == "" || it.Name == "" || it.Desc == "" || it.Cmd == "" {
			t.Fatalf("工具字段不完整：%+v", it)
		}
		if seen[it.ID] {
			t.Fatalf("工具 ID 重复：%s", it.ID)
		}
		seen[it.ID] = true
		got[it.Cat]++

		switch it.Risk {
		case "safe", "readonly", "caution", "reboot":
		default:
			t.Fatalf("工具 %s 的风险等级非法：%q", it.ID, it.Risk)
		}
		switch it.ArgKind {
		case "", "text", "drive", "shutdown", "opentool", "updatelist", "netpanel", "speedtest":
		default:
			t.Fatalf("工具 %s 的参数类型非法：%q", it.ID, it.ArgKind)
		}
		// updatelist / netpanel 自带专属弹窗 UI，不走通用表单，无需 ArgLabel
		if it.ArgKind != "" && it.ArgKind != "updatelist" && it.ArgKind != "netpanel" && it.ArgKind != "speedtest" && it.ArgLabel == "" {
			t.Fatalf("工具 %s 有参数但没写 ArgLabel（界面无法渲染）", it.ID)
		}
		if it.ArgKind == "opentool" && len(it.Opts) == 0 {
			t.Fatalf("工具 %s 是下拉选择但没给选项", it.ID)
		}
		if it.Timeout <= 0 {
			t.Fatalf("工具 %s 没设超时（会挂死）", it.ID)
		}
		if it.Cat != "disk" && it.ArgKind == "drive" {
			t.Fatalf("工具 %s 用了盘符参数但不在磁盘分类", it.ID)
		}
		// 每个工具必须真的能执行：注册表里要能查到实现
		impl, ok := toolImpls[it.ID]
		if !ok {
			t.Fatalf("工具 %s 没有在注册表里登记实现（点了会没反应）", it.ID)
		}
		if impl.Run == nil && impl.Plan == nil && len(impl.Steps) == 0 {
			t.Fatalf("工具 %s 没有任何执行实现", it.ID)
		}
	}

	for k, n := range want {
		if got[k] != n {
			t.Fatalf("分类 %s 应有 %d 个工具，实际 %d", k, n, got[k])
		}
	}
	if len(toolCats) != 5 {
		t.Fatalf("分类应有 5 个，实际 %d", len(toolCats))
	}
	// 注册表里不能有"目录里查不到"的孤儿实现（删工具时容易漏）
	for id := range toolImpls {
		if !seen[id] {
			t.Fatalf("注册表里的 %s 不在工具目录中（删工具时漏删实现？）", id)
		}
	}
	// 目录顺序：必须按 toolCats 的分类顺序成块排列（决定"全部"页签的观感）
	idx := map[string]int{}
	for i, c := range toolCats {
		idx[c.Key] = i
	}
	last := -1
	for _, it := range toolCatalog {
		if idx[it.Cat] < last {
			t.Fatalf("工具 %s 的分类顺序错乱：目录没有按 toolCats 定序", it.ID)
		}
		last = idx[it.Cat]
	}
}

// 注册表守卫：填错必须被拦住（这是给"以后加工具"用的安全网）
func TestToolRegistryGuards(t *testing.T) {
	cats := []ToolCat{{Key: "x", Name: "测试"}}
	valid := ToolItem{ID: "x-1", Cat: "x", Name: "N", Desc: "D", Cmd: "C", Risk: "safe", Timeout: 10}
	steps := []toolStep{{exe: "cmd.exe"}}

	cases := []struct {
		name string
		it   ToolItem
		impl toolImpl
	}{
		{"空 ID", func() ToolItem { v := valid; v.ID = ""; return v }(), toolImpl{Steps: steps}},
		{"缺名称", func() ToolItem { v := valid; v.Name = ""; return v }(), toolImpl{Steps: steps}},
		{"分类不存在", func() ToolItem { v := valid; v.Cat = "nope"; return v }(), toolImpl{Steps: steps}},
		{"风险等级非法", func() ToolItem { v := valid; v.Risk = "high"; return v }(), toolImpl{Steps: steps}},
		{"参数类型非法", func() ToolItem { v := valid; v.ArgKind = "magic"; v.ArgLabel = "x"; return v }(), toolImpl{Steps: steps}},
		{"有参数没标签", func() ToolItem { v := valid; v.ArgKind = "text"; return v }(), toolImpl{Steps: steps}},
		{"下拉没选项", func() ToolItem { v := valid; v.ArgKind = "opentool"; v.ArgLabel = "x"; return v }(), toolImpl{Steps: steps}},
		{"没设超时", func() ToolItem { v := valid; v.Timeout = 0; return v }(), toolImpl{Steps: steps}},
		{"没有任何实现", valid, toolImpl{}},
		{"Run 与 Steps 同时写", valid, toolImpl{Run: func(*toolCtx) (int, string) { return 0, "" }, Steps: steps}},
	}
	for _, c := range cases {
		byCat := map[string][]ToolItem{}
		impls := map[string]toolImpl{}
		if err := registerTool(cats, byCat, impls, c.it, c.impl); err == nil {
			t.Fatalf("「%s」应当被拒绝，却注册成功了", c.name)
		}
	}
	// 重复 ID 必须被拦
	byCat := map[string][]ToolItem{}
	impls := map[string]toolImpl{}
	if err := registerTool(cats, byCat, impls, valid, toolImpl{Steps: steps}); err != nil {
		t.Fatalf("合法工具不该被拒：%v", err)
	}
	if err := registerTool(cats, byCat, impls, valid, toolImpl{Steps: steps}); err == nil {
		t.Fatal("重复 ID 应当被拒绝")
	}
	// 合法注册后：进目录 + 进注册表
	if len(byCat["x"]) != 1 || len(impls) != 1 {
		t.Fatalf("注册结果不对：byCat=%+v impls=%d", byCat, len(impls))
	}
}

// 高风险操作的保护措施（2026-09-21 用户拍板：二次确认 + 自动备份引导配置）
func TestToolDangerGuards(t *testing.T) {
	find := func(id string) *ToolItem {
		for i := range toolCatalog {
			if toolCatalog[i].ID == id {
				return &toolCatalog[i]
			}
		}
		t.Fatalf("找不到工具 %s", id)
		return nil
	}
	boot := find("fix-boot")
	if !boot.Danger || !boot.Backup {
		t.Fatal("「系统引导修复」必须同时具备二次确认(Danger)与自动备份(Backup)")
	}
	// 会改系统设置的都要弹确认
	for _, id := range []string{"fix-update", "fix-update-remove", "net-hosts"} {
		if !find(id).Danger {
			t.Fatalf("工具 %s 会修改系统设置，必须带二次确认", id)
		}
	}
	// 只读类绝不允许带 Danger（避免无谓弹窗吓人）
	for _, it := range toolCatalog {
		if it.Risk == "readonly" && it.Danger {
			t.Fatalf("只读工具 %s 不应要求二次确认", it.ID)
		}
	}
	// 必填参数的工具
	for _, id := range []string{"net-ping", "net-tracert"} {
		if !find(id).ArgReq {
			t.Fatalf("工具 %s 的主机参数必填", id)
		}
	}
}

// stepsOf 取某工具在给定参数下的执行步骤（测试用）
func stepsOf(id, arg string) []toolStep {
	impl, ok := toolImpls[id]
	if !ok {
		return nil
	}
	if impl.Plan != nil {
		return impl.Plan(arg)
	}
	return impl.Steps
}

// 引导修复必须包含三条修复命令；net-reset 已并入 net-smart 集成面板（winsock / tcpip 成为其子动作），
// 这里改锁新现实：昨晚新增的 5 个工具必须在册、net-smart 必须挂推荐、推荐总数 1~5。
func TestToolPlans(t *testing.T) {
	boot := stepsOf("fix-boot", "")
	if len(boot) != 3 {
		t.Fatalf("引导修复应包含 3 条命令，实际 %d", len(boot))
	}
	joined := ""
	for _, s := range boot {
		joined += s.exe + " " + strings.Join(s.args, " ") + "|"
	}
	for _, k := range []string{"/fixmbr", "/fixboot", "/rebuildbcd"} {
		if !strings.Contains(joined, k) {
			t.Fatalf("引导修复缺少 %s", k)
		}
	}
	// rebuildbcd 是交互式的，必须喂入选择，否则会卡住等输入
	if boot[2].stdin == "" {
		t.Fatal("bootrec /rebuildbcd 需要喂入选择，否则会卡住")
	}

	// net-reset 已并入 net-smart 集成面板（winsock / tcpip 成为子动作，见 tools_net.go）
	idExists := func(id string) bool {
		for i := range toolCatalog {
			if toolCatalog[i].ID == id {
				return true
			}
		}
		return false
	}
	for _, id := range []string{"net-smart", "net-lanfix", "net-speed", "fix-update-list", "fix-update-remove"} {
		if !idExists(id) {
			t.Fatalf("缺少工具 %s（网络整合/补丁卸载/测速这一批不能少）", id)
		}
	}
	if idExists("net-reset") {
		t.Fatal("net-reset 已并入 net-smart，不应再单独注册成工具（否则两张卡功能重复）")
	}
	smOK, rec := false, 0
	for i := range toolCatalog {
		if toolCatalog[i].Recommend {
			rec++
		}
		if toolCatalog[i].ID == "net-smart" && toolCatalog[i].Recommend {
			smOK = true
		}
	}
	if !smOK {
		t.Fatal("net-smart 应挂推荐角标")
	}
	if rec < 1 || rec > 5 {
		t.Fatalf("推荐角标应控制在 1~5 个（挂多了等于没挂），实际 %d", rec)
	}

	ping := stepsOf("net-ping", "www.baidu.com")
	if len(ping) != 1 || ping[0].exe != "ping.exe" {
		t.Fatalf("net-ping 计划不对: %+v", ping)
	}
	tr := stepsOf("net-tracert", "www.baidu.com")
	if len(tr) != 1 || tr[0].exe != "tracert.exe" {
		t.Fatalf("net-tracert 计划不对: %+v", tr)
	}
	// 盘符参数：空参数时应回落到系统盘
	df := stepsOf("disk-defrag-analyze", "")
	if len(df) != 1 || !strings.HasSuffix(df[0].args[0], ":") {
		t.Fatalf("碎片分析的空盘符应回落系统盘，实际 %+v", df)
	}
	df2 := stepsOf("disk-defrag-analyze", "D:")
	if df2[0].args[0] != "D:" {
		t.Fatalf("盘符参数未生效：%+v", df2[0].args)
	}
}

// ---------- 控制台编码（中文系统的原生命令输出是 GBK） ----------

func TestDecodeConsoleGBK(t *testing.T) {
	orig := "Windows 资源保护未找到任何完整性冲突。"
	enc, err := simplifiedchinese.GBK.NewEncoder().Bytes([]byte(orig))
	if err != nil {
		t.Fatal(err)
	}
	if got := decodeConsole(enc); got != orig {
		t.Fatalf("GBK 解码不符：\n原文 %q\n实际 %q", orig, got)
	}
	// 纯 ASCII / UTF-8 原样透传
	if got := decodeConsole([]byte("sfc /scannow OK")); got != "sfc /scannow OK" {
		t.Fatalf("ASCII 不应被改写：%q", got)
	}
	utf8CN := []byte("中文直接输出")
	if got := decodeConsole(utf8CN); got != "中文直接输出" {
		t.Fatalf("UTF-8 不应被改写：%q", got)
	}
}

// ---------- sfc 输出编码（sfc 重定向输出是 UTF-16LE，不转就乱码） ----------

func TestConsoleReaderUtf16(t *testing.T) {
	// 用 UTF-16LE 编码 sfc 真实的进度文本（带 BOM）
	src := "验证 97% 完成\r\n发现完整性冲突\r\n"
	enc, err := unicode.UTF16(unicode.LittleEndian, unicode.IgnoreBOM).NewEncoder().Bytes([]byte(src))
	if err != nil {
		t.Fatal(err)
	}
	stream := append([]byte{0xFF, 0xFE}, enc...) // BOM
	var lines []string
	pumpLines(consoleReader(bytes.NewReader(stream)), func(kind, text string) {
		if kind == "line" || kind == "cr" {
			lines = append(lines, text)
		}
	})
	out := strings.Join(lines, "|")
	if !strings.Contains(out, "验证 97% 完成") || !strings.Contains(out, "发现完整性冲突") {
		t.Fatalf("UTF-16 输出应被正确转码：\n%s", out)
	}
	if strings.ContainsAny(out, "\x00") {
		t.Fatalf("转码后不应残留 0x00：%q", out)
	}
}

// 纯 ANSI/GBK 输出不能被误判成 UTF-16（0x00 密度为零）
func TestConsoleReaderAnsiPassthrough(t *testing.T) {
	orig := "Windows 资源保护未找到任何完整性冲突。\r\n"
	enc, _ := simplifiedchinese.GBK.NewEncoder().Bytes([]byte(orig))
	var lines []string
	pumpLines(consoleReader(bytes.NewReader(enc)), func(kind, text string) {
		if kind == "line" || kind == "cr" {
			lines = append(lines, text)
		}
	})
	if len(lines) == 0 || lines[0] != "Windows 资源保护未找到任何完整性冲突。" {
		t.Fatalf("GBK 输出应原样透传给 decodeConsole：\n%q", lines)
	}
}

func TestIsUtf16Density(t *testing.T) {
	// 无 BOM 的 UTF-16：ASCII 段 0x00 密度约 50%
	u16 := []byte{'h', 0, 'i', 0, ' ', 0, '9', 0, '7', 0, '%', 0}
	if !isUtf16(u16) {
		t.Fatal("无 BOM 的 UTF-16 ASCII 段应按密度识别")
	}
	// 真实 sfc 首块（2026-09-21 抓包复刻）：无 BOM，中文+控制符混合，密度约 10%
	// —— 阈值定 25% 时它会被漏判成 GBK 全程乱码，必须能识别
	sfcFirst, err := unicode.UTF16(unicode.LittleEndian, unicode.IgnoreBOM).NewEncoder().Bytes(
		[]byte("\r\r\n开始系统扫描。此过程将需要一些时间。\r\r\n开始系统扫描的验证阶段。"))
	if err != nil {
		t.Fatal(err)
	}
	if !isUtf16(sfcFirst) {
		d := 100 * bytes.Count(sfcFirst, []byte{0}) / len(sfcFirst)
		t.Fatalf("真实 sfc 首块（0x00 密度 %d%%）应识别为 UTF-16", d)
	}
	// GBK/UTF-8 中文不含 0x00，绝不能误判
	if isUtf16([]byte("开始系统扫描，这将需要一些时间")) {
		t.Fatal("普通 ANSI/UTF-8 文本不应误判为 UTF-16")
	}
	if isUtf16([]byte{'a', 'b'}) {
		t.Fatal("过短的块不应判定")
	}
}

// 用真机抓包的 sfc 原始输出（UTF-16LE 无 BOM）做端到端验证（抓包文件不在则跳过）
func TestConsoleReaderSfcCapture(t *testing.T) {
	raw, err := os.ReadFile(`D:\3z9wopt\_proto\sfc_raw.bin`)
	if err != nil {
		t.Skip("无 sfc 抓包文件，跳过")
	}
	var lines []string
	pumpLines(consoleReader(bytes.NewReader(raw)), func(kind, text string) {
		// sfc 全程用 \r 结尾（含结论行），cr 在界面上是原地覆盖，也要收集
		if kind == "line" || kind == "cr" {
			lines = append(lines, text)
		}
	})
	out := strings.Join(lines, "\n")
	if !strings.Contains(out, "Windows 资源保护未找到任何完整性冲突") {
		t.Fatalf("真实 sfc 抓包应解出结论行：\n%s", out)
	}
	if !strings.Contains(out, "验证 100% 已完成") {
		t.Fatalf("应解出进度终值：\n%s", out)
	}
	if strings.ContainsAny(out, "\x00") {
		t.Fatal("不应残留 0x00")
	}
}

// ---------- sfc 结果摘要（sfc 不回显输出，CBS.log 是唯一结果源；2026-09-21 用户实测踩坑） ----------

func TestParseSfcSummary(t *testing.T) {
	content := strings.Join([]string{
		"2026-09-21 14:20:00, Info                  CSI    00000001 [SR] Beginning Verify and Repair Transaction",
		"2026-09-21 14:30:10, Info                  CSI    00000010 [SR] Repairing corrupted file [l:24]" + `\??\C:\Windows\System32\bad1.dll` + " from store",
		"2026-09-21 14:30:11, Info                  CSI    00000011 [SR] Repairing file " + `\??\C:\Windows\System32\bad2.dll` + " from store",
		"2026-09-21 14:30:12, Info                  CSI    00000012 [SR] Cannot repair member file [l:24]" + `\??\C:\Windows\System32\hopeless.dll` + " of microsoft-windows-foo",
		"2026-09-21 14:40:45, Info                  CSI    00000020 [SR] Repair complete",
		"2026-09-21 14:40:49, Info                  CSI    00000021 [SR] Verify and Repair Transaction completed. All files and registry keys listed in this transaction have been successfully repaired",
		"2026-09-20 09:00:00, Info                  CSI    00000030 [SR] Repairing file \\??\\C:\\old.dll from store", // 上一次的，必须忽略
	}, "\n")
	since := time.Date(2026, 9, 21, 14, 15, 0, 0, time.Local)
	res := parseSfcSummary(content, since)
	if res.Detail != 6 {
		t.Fatalf("应识别 6 条本次 [SR] 行，实际 %d（旧日期的必须被过滤）", res.Detail)
	}
	if res.Repaired != 2 {
		t.Fatalf("应统计 2 处修复，实际 %d", res.Repaired)
	}
	if len(res.Cannot) != 1 || !strings.Contains(res.Cannot[0], "hopeless.dll") {
		t.Fatalf("无法修复文件识别错误：%v", res.Cannot)
	}
	// 修复过 + 有修不了的 → 结论应为"建议 DISM 后重跑"
	// 全无冲突的场景
	clean := "2026-09-21 14:40:49, Info  CSI  00000021 [SR] Verify and Repair Transaction completed."
	res2 := parseSfcSummary(clean, since)
	if res2.Repaired != 0 || len(res2.Cannot) != 0 {
		t.Fatalf("无冲突场景不应有修复/失败记录：%+v", res2)
	}
	// since 之前的活动全部忽略
	res3 := parseSfcSummary(content, time.Date(2026, 9, 21, 15, 0, 0, 0, time.Local))
	if res3.Detail != 0 {
		t.Fatalf("晚于扫描时段的记录应为 0，实际 %d", res3.Detail)
	}
}

func TestParseWmiDate(t *testing.T) {
	// CIM_DATETIME
	if ts, ok := parseWmiDate("20240115083012.500000+480"); !ok || ts.Year() != 2024 || ts.Month() != 1 || ts.Day() != 15 {
		t.Fatalf("CIM_DATETIME 解析失败：%v %v", ts, ok)
	}
	// 纯日期
	if ts, ok := parseWmiDate("20240115"); !ok || ts.Day() != 15 {
		t.Fatalf("纯日期解析失败：%v %v", ts, ok)
	}
	// ISO
	if ts, ok := parseWmiDate("2024-01-15"); !ok || ts.Year() != 2024 {
		t.Fatalf("ISO 日期解析失败：%v %v", ts, ok)
	}
	// 空值必须返回 false，不能崩
	if _, ok := parseWmiDate(""); ok {
		t.Fatal("空串不应解析成功")
	}
	if _, ok := parseWmiDate("不是日期"); ok {
		t.Fatal("垃圾输入不应解析成功")
	}
}

// ---------- 实机只读验证 ----------

// wmiQuery 必须在本机可用（硬件信息/磁盘健康/补丁清单全靠它）
func TestWmiQueryReal(t *testing.T) {
	rows, err := wmiQuery("root\\cimv2",
		"SELECT Caption,Version FROM Win32_OperatingSystem",
		[]string{"Caption", "Version"})
	if err != nil {
		t.Fatalf("WMI 查询失败：%v", err)
	}
	if len(rows) == 0 {
		t.Fatal("WMI 没返回操作系统信息")
	}
	if strings.TrimSpace(rows[0]["Caption"]) == "" {
		t.Fatalf("系统名称是空的：%+v", rows[0])
	}
	t.Logf("实机系统：%s (%s)", rows[0]["Caption"], rows[0]["Version"])
}

// 只读工具必须能在真机跑出内容（不需要 UI）
func collectLog() (*toolCtx, *[]string) {
	var lines []string
	tc := &toolCtx{
		a:   &App{},
		ctx: context.Background(),
		log: func(kind, text string) { lines = append(lines, text) },
	}
	return tc, &lines
}

func TestToolUptimeReal(t *testing.T) {
	tc, lines := collectLog()
	code, _ := toolInfoUptime(tc)
	if code != 0 {
		t.Fatalf("系统运行时长工具返回 %d", code)
	}
	out := strings.Join(*lines, "\n")
	if !strings.Contains(out, "本次开机时长") {
		t.Fatalf("输出缺少开机时长：\n%s", out)
	}
	t.Log(out)
}

func TestToolDiskSmartReal(t *testing.T) {
	tc, lines := collectLog()
	code, _ := toolDiskSmart(tc)
	if code != 0 {
		t.Fatalf("磁盘健康检测返回 %d", code)
	}
	out := strings.Join(*lines, "\n")
	if !strings.Contains(out, "磁盘 0") {
		t.Fatalf("没读到物理磁盘：\n%s", out)
	}
	t.Log(out)
}

func TestToolPortQueryReal(t *testing.T) {
	// 直接验证绝对路径解析：本测试进程的 PATH 里没有 System32，
	// 若走裸命令名会报 "executable file not found in %PATH%"（实测踩过）
	raw, rerr := runHiddenOut(30*time.Second, resolveExe("netstat.exe"), "-ano")
	if rerr != nil {
		t.Fatalf("netstat 执行失败：%v", rerr)
	}
	t.Logf("netstat 原始输出 %d 字节", len(raw))

	tc, lines := collectLog()
	tc.arg = "" // 列出所有监听端口
	if code, _ := toolNetPort(tc); code != 0 {
		t.Fatalf("端口查询返回 %d", code)
	}
	if len(*lines) <= 2 {
		t.Fatalf("端口查询只输出 %d 行，说明只打印了标题没解析到数据：%v", len(*lines), *lines)
	}
	t.Logf("端口查询输出 %d 行", len(*lines))
	t.Log((*lines)[1])
	t.Log((*lines)[2])
}
