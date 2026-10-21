package main

// 工具库 · 系统修复类（fix）
// 新增/修改工具：在 init() 里加一条 regTool(...)，详见 toolkit.go 顶部的扩展手册。
// 顺序 = 界面上的显示顺序。

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"syscall"
	"time"
)

func init() {
	// 2026-09-22 新增：优化前先留后路。工具库原本只有「删除还原点」，没有「创建」——
	// 用户改了 154 项注册表却无从回退，是这个工具最大的风险缺口，所以放在系统修复第一位。
	regTool(ToolItem{
		ID: "fix-restore-point", Cat: "fix", Name: "创建系统还原点",
		Desc: "给当前系统拍个快照，改出问题能一键回退",
		Cmd:  `SRSetRestorePointW（srclient.dll）—— 系统原生 API`,
		Risk: "safe", Timeout: 300,
		Note: "建议在做系统优化、改注册表之前先跑一次，留条后路。需要 C 盘的「系统保护」已开启：" +
			"此电脑 → C 盘右键 → 属性 → 系统保护 → 启用；没开启工具会明确告诉你是这个原因。",
	}, toolImpl{Run: func(tc *toolCtx) (int, string) {
		tc.log("head", "▶ 调用系统原生接口 srclient.dll!SRSetRestorePointW")
		if err := createSystemRestorePoint(rpDefaultDesc); err != nil {
			tc.log("err", err.Error())
			return 1, "没能创建成功。\n" + err.Error()
		}
		tc.log("ok", "已创建："+rpDefaultDesc)
		return 0, "系统还原点已创建：" + rpDefaultDesc +
			"\n\n以后哪步改得不顺手，开始菜单搜「系统还原」，选中这一条退回来就行。"
	}})

	regTool(ToolItem{
		ID: "fix-sfc", Cat: "fix", Name: "SFC 系统文件修复",
		Desc: "扫描并用系统缓存自动修复受损的系统文件",
		Cmd:  "sfc /scannow", Risk: "safe", Long: true, Timeout: 3600,
		Recommend: true,
		Note:      "预计 5–15 分钟。若提示「无法修复某些文件」，请接着跑「DISM 组件存储修复」，再回来重跑一次。",
	}, toolImpl{Steps: []toolStep{{exe: "sfc.exe", args: []string{"/scannow"}, silent: "sfc"}}})

	regTool(ToolItem{
		ID: "fix-sfc-verify", Cat: "fix", Name: "SFC 仅校验",
		Desc: "只检查系统文件完整性，不做任何修改",
		Cmd:  "sfc /verifyonly", Risk: "readonly", Long: true, Timeout: 3600,
		Note: "不想动系统文件、只想看看有没有问题时用这个。",
	}, toolImpl{Steps: []toolStep{{exe: "sfc.exe", args: []string{"/verifyonly"}, silent: "sfc"}}})

	regTool(ToolItem{
		ID: "fix-dism-restore", Cat: "fix", Name: "DISM 组件存储修复",
		Desc: "修复系统映像的组件损坏；SFC 修不了时靠它",
		Cmd:  "DISM /Online /Cleanup-Image /RestoreHealth", Risk: "safe", Long: true, Timeout: 3600,
		Note: "需要联网从 Windows 更新拉取源文件。进度长时间停在 20% 或 40% 属正常现象，别关。",
	}, toolImpl{Steps: []toolStep{{exe: "dism.exe", args: []string{"/Online", "/Cleanup-Image", "/RestoreHealth"}}}})

	regTool(ToolItem{
		ID: "fix-dism-scan", Cat: "fix", Name: "DISM 组件存储检查",
		Desc: "只检测系统映像是否损坏，不做修复",
		Cmd:  "DISM /Online /Cleanup-Image /ScanHealth", Risk: "readonly", Long: true, Timeout: 3600,
	}, toolImpl{Steps: []toolStep{{exe: "dism.exe", args: []string{"/Online", "/Cleanup-Image", "/ScanHealth"}}}})

	regTool(ToolItem{
		ID: "fix-boot", Cat: "fix", Name: "系统引导修复",
		Desc: "重建主引导记录、引导扇区与启动配置数据 (BCD)",
		Cmd:  "bootrec /fixmbr + /fixboot + /rebuildbcd", Risk: "caution", Danger: true, Backup: true, Timeout: 600,
		Note:      "仅在开机出现「找不到操作系统」「引导失败」时使用。执行前会自动导出当前引导配置备份。",
		Reboot:    true,
		RebootMsg: "引导配置的改动在下次开机时才会被使用，建议尽快重启并确认能正常进入系统",
	}, toolImpl{Steps: []toolStep{
		{title: "修复主引导记录 (MBR)", exe: "bootrec.exe", args: []string{"/fixmbr"}},
		{title: "修复引导扇区", exe: "bootrec.exe", args: []string{"/fixboot"}},
		// bootrec /rebuildbcd 是交互式的，需喂入选择；A = 全部添加，不喂会一直等输入
		{title: "重建启动配置数据 (BCD)", exe: "bootrec.exe", args: []string{"/rebuildbcd"}, stdin: "A\r\n"},
	}})

	regTool(ToolItem{
		ID: "fix-bcd", Cat: "fix", Name: "引导配置查看",
		Desc: "列出所有启动项与引导参数，排查开机故障",
		Cmd:  "bcdedit /enum all", Risk: "readonly", OutText: true, Timeout: 60,
	}, toolImpl{Steps: []toolStep{{exe: "bcdedit.exe", args: []string{"/enum", "all"}}}})

	regTool(ToolItem{
		ID: "fix-chkdsk", Cat: "fix", Name: "磁盘错误扫描",
		Desc: "只读扫描系统盘的文件系统错误，不修改",
		Cmd:  "chkdsk <系统盘> /scan", Risk: "readonly", Long: true, Timeout: 3600,
		Note: "只扫描并报告。要真正修复需计划下次开机检查，本工具不做。",
	}, toolImpl{Plan: func(string) []toolStep {
		return []toolStep{{exe: "chkdsk.exe", args: []string{driveOrSys(""), "/scan"}}}
	}})

	regTool(ToolItem{
		ID: "fix-update", Cat: "fix", Name: "Windows 更新组件修复",
		Desc: "卡更新、更新报错时：停更新服务、重建缓存目录",
		Cmd:  "停 wuauserv/bits/cryptsvc + 重建 SoftwareDistribution", Risk: "caution", Danger: true, Timeout: 300,
		Note:      "修复期间会暂停 Windows 更新服务；已下载未安装的更新需要重新下载。",
		Reboot:    true,
		RebootMsg: "更新服务的状态与缓存目录要在重启后才彻底恢复干净",
	}, toolImpl{Run: toolFixUpdate})

	regTool(ToolItem{
		ID: "fix-update-list", Cat: "fix", Name: "补丁列表",
		Desc: "列出已安装的更新补丁，供「补丁卸载」复制 Package Identity",
		Cmd:  "dism /online /get-packages", Risk: "readonly", OutText: true, Timeout: 300,
		Note: "复制完整 Package Identity（或 KB 号）填进「补丁卸载」。列表里也有功能包/语言包，不确定的别卸。",
	}, toolImpl{Run: toolUpdateList})

	regTool(ToolItem{
		ID: "fix-update-remove", Cat: "fix", Name: "补丁卸载",
		Desc: "卸载导致蓝屏/异常的历史更新补丁，回滚恢复",
		Cmd:  "dism /online /norestart /remove-package /packagename:<Identity>", Risk: "reboot", Danger: true, Long: true,
		Recommend: true,
		ArgKind:   "updatelist",
		Timeout:   600,
		Reboot:    true,
		RebootMsg: "补丁卸载要重启后才彻底生效；重启前个别组件可能表现异常",
		Note:      "列表按安装时间排序（新的在前），点「卸载」即可。单个卸载 1–5 分钟，别关。",
	}, toolImpl{Run: toolUpdateRemove})

	regTool(ToolItem{
		ID: "fix-store", Cat: "fix", Name: "应用商店缓存重置",
		Desc: "应用商店打不开、下载卡住、装不上应用时用",
		Cmd:  "wsreset", Risk: "safe", Timeout: 180,
		Note: "执行后会弹出应用商店窗口，属正常现象。",
	}, toolImpl{Steps: []toolStep{{exe: "wsreset.exe"}}})
}

// ---- Windows 更新组件修复 ----
func toolFixUpdate(tc *toolCtx) (int, string) {
	svcs := []string{"wuauserv", "bits", "cryptsvc", "msiserver"}
	for _, s := range svcs {
		if tc.ctx.Err() != nil {
			return -1, "已停止"
		}
		tc.log("head", "▶ 停止服务 "+s)
		_, _ = runStepStream(tc.ctx, tc.log, "net.exe", []string{"stop", s}, "")
	}
	win := os.Getenv("SystemRoot")
	stamp := time.Now().Format("20060102_150405")
	for _, p := range []string{
		filepath.Join(win, "SoftwareDistribution"),
		filepath.Join(win, "System32", "catroot2"),
	} {
		if _, err := os.Stat(p); err != nil {
			tc.log("dim", "跳过（不存在）："+p)
			continue
		}
		dst := p + ".old_" + stamp
		if err := os.Rename(p, dst); err != nil {
			tc.log("err", "重建失败（可能被占用）："+p+" —— "+shortErr(err))
			continue
		}
		tc.log("ok", "已重建："+p)
	}
	for _, s := range svcs {
		if tc.ctx.Err() != nil {
			return -1, "已停止"
		}
		tc.log("head", "▶ 启动服务 "+s)
		_, _ = runStepStream(tc.ctx, tc.log, "net.exe", []string{"start", s}, "")
	}
	return 0, "更新组件缓存已重建，现在可以重新打开 Windows 更新"
}

// ---- 补丁管理（列表 + 卸载）----
// dism 输出块是"包标识 → 状态"等多行键值对，中英文系统键名不同（包标识/Package Identity），
// 按行正则解析最稳（比 /format:table 好：表格会被控制台宽度折行截断）。
var (
	rePkgIdentity = regexp.MustCompile(`(?i)^(?:Package Identity|包标识)\s*:\s*(\S+)`)
	rePkgState    = regexp.MustCompile(`(?i)^(?:State|状态)\s*:\s*(.+)$`)
	reKB          = regexp.MustCompile(`(?i)KB\d{7,}`)
)

type updPkg struct {
	identity string
	kb       string
	state    string
}

func parseDismPackages(out string) []updPkg {
	var pkgs []updPkg
	var cur *updPkg
	for _, ln := range strings.Split(out, "\n") {
		if m := rePkgIdentity.FindStringSubmatch(strings.TrimSpace(ln)); m != nil {
			pkgs = append(pkgs, updPkg{identity: m[1]})
			cur = &pkgs[len(pkgs)-1]
			continue
		}
		if cur != nil {
			if m := rePkgState.FindStringSubmatch(strings.TrimSpace(ln)); m != nil {
				cur.state = strings.TrimSpace(m[1])
			}
		}
	}
	for i := range pkgs {
		if m := reKB.FindString(pkgs[i].identity); m != "" {
			pkgs[i].kb = strings.ToUpper(m)
		}
	}
	return pkgs
}

// dismCapture 执行 dism 并捕获解码后的全文（ctx 可取消；隐藏窗口）
func dismCapture(ctx context.Context, timeout time.Duration, args ...string) (string, int, error) {
	c, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(c, "dism.exe", args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: 0x08000000} // CREATE_NO_WINDOW
	out, err := cmd.CombinedOutput()
	code := -1
	if cmd.ProcessState != nil {
		code = cmd.ProcessState.ExitCode()
	}
	return decodeConsole(out), code, err
}

// ---- 补丁列表 ----
func toolUpdateList(tc *toolCtx) (int, string) {
	tc.log("head", "▶ 枚举已安装补丁（dism，可能要等 30–60 秒）")
	out, _, err := dismCapture(tc.ctx, 280*time.Second, "/online", "/get-packages")
	if err != nil && out == "" {
		tc.log("err", "dism 执行失败："+shortErr(err))
		return 1, "枚举失败（需要管理员权限运行）"
	}
	pkgs := parseDismPackages(out)
	if len(pkgs) == 0 {
		tc.log("err", "没解析出任何补丁，原始输出如下（前 20 行）：")
		for i, ln := range strings.Split(strings.ReplaceAll(out, "\r", ""), "\n") {
			if i >= 20 {
				break
			}
			tc.log("dim", ln)
		}
		return 1, "解析失败（dism 输出格式变了？把上面内容发我）"
	}
	installed := 0
	for _, p := range pkgs {
		state := p.state
		if state == "" {
			state = "?"
		}
		if strings.Contains(state, "Installed") || strings.Contains(state, "已安装") {
			installed++
		}
		id := p.identity
		if p.kb != "" {
			id = p.kb + "  " + p.identity
		}
		tc.log("line", "["+state+"]  "+id)
	}
	tc.log("ok", fmt.Sprintf("共 %d 个包，其中已安装 %d 个。卸载请复制完整 Package Identity（或 KB 号）。", len(pkgs), installed))
	return 0, fmt.Sprintf("共 %d 个包（已安装 %d 个）", len(pkgs), installed)
}

// ---- 补丁卸载 ----
func toolUpdateRemove(tc *toolCtx) (int, string) {
	arg := strings.TrimSpace(tc.arg)
	if arg == "" {
		return 1, "请先在「补丁列表」里复制要卸载的 Package Identity 或 KB 号"
	}
	identity := arg
	// 只给了 KB 号：自动到补丁列表里匹配完整 Package Identity
	if !strings.HasPrefix(strings.ToLower(arg), "package") {
		tc.log("dim", "检测到 KB 号，正在匹配完整 Package Identity…")
		out, _, err := dismCapture(tc.ctx, 280*time.Second, "/online", "/get-packages")
		if err != nil && out == "" {
			tc.log("err", "dism 执行失败："+shortErr(err))
			return 1, "匹配失败（需要管理员权限运行）"
		}
		var hits []string
		for _, p := range parseDismPackages(out) {
			if strings.EqualFold(p.kb, arg) || strings.Contains(strings.ToLower(p.identity), strings.ToLower(arg)) {
				hits = append(hits, p.identity)
			}
		}
		switch len(hits) {
		case 0:
			tc.log("err", "没找到匹配「"+arg+"」的补丁。先跑「补丁列表」，从里面复制完整的 Package Identity。")
			return 1, "没找到匹配的补丁"
		case 1:
			identity = hits[0]
			tc.log("dim", "已匹配："+identity)
		default:
			tc.log("warn", "匹配到 "+fmt.Sprint(len(hits))+" 个包，请复制完整的 Package Identity 重试：")
			for _, h := range hits {
				tc.log("line", h)
			}
			return 1, "匹配到多个包，需要完整的 Package Identity"
		}
	}

	tc.log("head", "▶ 卸载 "+identity)
	tc.log("dim", "dism 卸载耗时 1–5 分钟，请勿关闭。")
	// 边流式输出边留底，失败时用全文做错误码归类
	var buf []string
	logf := func(kind, text string) {
		buf = append(buf, text)
		tc.log(kind, text)
	}
	code, err := runStepStream(tc.ctx, logf, "dism.exe", []string{"/online", "/norestart", "/remove-package", "/packagename:" + identity}, "")
	if err != nil && code != 0 {
		tc.log("err", shortErr(err))
		return 1, "卸载中止：" + shortErr(err)
	}
	if code == 0 || code == 3010 { // 3010 = 成功但需重启
		tc.log("ok", "卸载命令执行成功，重启电脑后彻底移除。")
		return 0, "已卸载，需重启彻底生效"
	}
	// 失败：把 dism 的错误码转成人话
	joined := strings.ToLower(strings.Join(buf, "\n"))
	switch {
	case strings.Contains(joined, "0x800f0825"):
		tc.log("err", "这是永久性/内置补丁，系统不允许卸载（正常保护，不是故障）。")
		return 1, "该补丁不可卸载（永久性更新）"
	case strings.Contains(joined, "0x80070005") || strings.Contains(joined, "拒绝访问") || strings.Contains(joined, "access is denied"):
		tc.log("err", "权限不足：请以管理员身份运行本程序再试。")
		return 1, "权限不足，需要管理员运行"
	case strings.Contains(joined, "0x80070002") || strings.Contains(joined, "0x8007007b"):
		tc.log("err", "Package Identity 不对。跑「补丁列表」，完整复制一行 Package Identity 再试。")
		return 1, "Package Identity 不正确"
	case strings.Contains(joined, "0x800f0905") || strings.Contains(joined, "0x800f082f"):
		tc.log("err", "补丁状态与当前系统不符。先跑「DISM 组件存储修复」，再回来重试。")
		return 1, "补丁与系统状态不符"
	default:
		tc.log("err", "卸载失败，具体错误见上方日志（dism 的「错误: 0x…」行）。可把错误码发我分析。")
		return 1, "卸载失败（错误码见日志）"
	}
}

// ---- 补丁列表弹窗数据源 ----

// UpdateItem 补丁卸载弹窗的一行
type UpdateItem struct {
	Kb   string `json:"kb"`
	Desc string `json:"desc"`
	Date string `json:"date"`
}

// GetInstalledUpdates 已安装的系统更新补丁（Win32_QuickFixEngineering，安装时间新的在前）。
// QFE 只含系统更新；功能包等用弹窗底部的手动输入兜底。
func (a *App) GetInstalledUpdates() []UpdateItem {
	rows, err := wmiQuery("root\\cimv2",
		"SELECT HotFixID,Description,InstalledOn FROM Win32_QuickFixEngineering",
		[]string{"HotFixID", "Description", "InstalledOn"})
	if err != nil {
		return nil
	}
	descCn := map[string]string{
		"security update": "安全更新", "update": "更新", "hotfix": "热修复",
		"definition update": "定义更新", "feature pack": "功能包", "service pack": "服务包",
	}
	type row struct {
		it UpdateItem
		t  time.Time
	}
	list := make([]row, 0, len(rows))
	for _, r := range rows {
		kb := strings.TrimSpace(r["HotFixID"])
		if kb == "" {
			continue
		}
		t, _ := parseWmiDate(r["InstalledOn"])
		desc := strings.TrimSpace(r["Description"])
		if cn, ok := descCn[strings.ToLower(desc)]; ok {
			desc = cn
		} else if desc == "" {
			desc = "Windows 系统更新补丁"
		}
		d := ""
		if !t.IsZero() {
			d = t.Format("2006-01-02")
		}
		list = append(list, row{UpdateItem{Kb: kb, Desc: desc, Date: d}, t})
	}
	sort.Slice(list, func(i, j int) bool { return list[i].t.After(list[j].t) })
	out := make([]UpdateItem, 0, len(list))
	for _, r := range list {
		out = append(out, r.it)
	}
	return out
}
