package main

// 工具库 · 网络工具类（net）
// 新增/修改工具：在 init() 里加一条 regTool(...)，详见 toolkit.go 顶部的扩展手册。
//
// 2026-09-21 整合：原先分散的十余个网络修复项（清代理 / 刷新 DNS / 清 ARP /
// 重置 Winsock、TCP/IP / 重置 Hosts / 同步时间 / 重启网卡 / 网络详情 / 续租 IP）
// 已全部并入「网络修复」这一张卡片的集成面板，作为它的子动作由 toolNetSmart
// 按参数分派，不再各自单独注册成工具 —— 界面上不再出现一堆功能重复的卡片。

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

func init() {
	regTool(ToolItem{
		ID: "net-smart", Cat: "net", Name: "网络修复",
		Desc: "常用网络修复集于一体：一键智能全修复 + 单项修复 + 网络诊断，结果实时输出",
		Cmd:  "集成面板 · 一键项：清除系统代理 + 刷新 DNS + 清理 ARP + 同步时间 + 续租 IP", Risk: "safe", Timeout: 300,
		Recommend: true,
		ArgKind:   "netpanel",
		Note:      "一键智能全修复全程不需要重启，期间会短暂断网几秒（重新申请 IP）。如还无法上网，再点「重置 Winsock/TCP/IP」并重启。",
	}, toolImpl{Run: toolNetSmart})

	regTool(ToolItem{
		ID: "net-lanfix", Cat: "net", Name: "局域网共享修复",
		Desc: "一键修复局域网共享与打印机共享：服务、防火墙、网络发现、RPC 全部打通",
		Cmd:  "共享服务启动 + 防火墙放通 + 网络发现开启 + RPC 加固（中英文规则组容错）", Risk: "caution", Danger: true, Timeout: 240,
		Recommend: true,
		Note:      "共享用不了的根因通常就这几处：公用网络禁发现、发现类服务没起、防火墙拦了共享端口、RPC/SMB 异常。本工具逐项修复，SMB 与 Guest 只检查不改动（避免引入兼容/安全问题）。",
		Reboot:    true,
		RebootMsg: "共享相关的服务与网络发现配置要重启后才完全生效，重启前建议先试一次访问共享",
	}, toolImpl{Run: toolLanFix})

	regTool(ToolItem{
		ID: "net-speed", Cat: "net", Name: "宽带测速",
		Desc: "下载 / 上传带宽 + 延迟抖动丢包，自动挑最快节点，实时速度曲线",
		Cmd:  "多线程并发测速（内置国内镜像 + Cloudflare 节点）", Risk: "safe", Long: true, Timeout: 120,
		Recommend: true,
		ArgKind:   "speedtest",
		Note:      "测速会消耗几十 MB 流量；用 WiFi 建议靠近路由器，测速时别同时下载东西，结果才准。",
	}, toolImpl{Run: toolSpeedTest})

	regTool(ToolItem{
		ID: "net-ping", Cat: "net", Name: "连通性测试",
		Desc: "向指定主机发包，看丢包与延迟",
		Cmd:  "ping -n 4 <主机>", Risk: "safe", Timeout: 120, ArgReq: true,
		ArgKind: "text", ArgLabel: "主机", ArgHint: "域名或 IP，例如 www.baidu.com", ArgDef: "www.baidu.com",
	}, toolImpl{Plan: func(arg string) []toolStep {
		return []toolStep{{exe: "ping.exe", args: []string{"-n", "4", arg}}}
	}})

	regTool(ToolItem{
		ID: "net-tracert", Cat: "net", Name: "路由跟踪",
		Desc: "逐跳追踪到目标，定位卡在哪一环",
		Cmd:  "tracert -d <主机>", Risk: "safe", Long: true, Timeout: 300, ArgReq: true,
		ArgKind: "text", ArgLabel: "主机", ArgHint: "域名或 IP，例如 www.baidu.com", ArgDef: "www.baidu.com",
	}, toolImpl{Plan: func(arg string) []toolStep {
		return []toolStep{{exe: "tracert.exe", args: []string{"-d", arg}}}
	}})

	regTool(ToolItem{
		ID: "net-port", Cat: "net", Name: "端口占用查询",
		Desc: "谁占着某个端口（含占用进程名）",
		Cmd:  "netstat -ano + 进程反查", Risk: "readonly", OutText: true, Timeout: 60,
		ArgKind: "text", ArgLabel: "端口", ArgHint: "留空 = 列出所有监听端口，例如 8080",
	}, toolImpl{Run: toolNetPort})

	regTool(ToolItem{
		ID: "net-hosts", Cat: "net", Name: "Hosts 文件编辑",
		Desc: "打开 hosts 文件（屏蔽域名 / 本地调试），改动前自动备份",
		Cmd:  `备份 + 打开 %windir%\System32\drivers\etc\hosts`, Risk: "caution", Danger: true, Timeout: 60,
		Note: "保存后可能需执行「DNS 缓存刷新」才生效。",
	}, toolImpl{Run: toolNetHosts})
}

// ---- 网络修复面板：按子动作分派（前端传进来的 arg） ----
// 子动作：all(一键) / proxy / dns / arp / winsock / tcpip / hosts / time / nic / show
func toolNetSmart(tc *toolCtx) (int, string) {
	act := strings.TrimSpace(tc.arg)
	if act == "" {
		act = "all"
	}
	switch act {
	case "all":
		return runSteps(tc, []toolStep{
			{title: "清除系统代理", exe: "netsh.exe", args: []string{"winhttp", "reset", "proxy"}},
			{title: "关闭用户级代理开关", fn: proxyDisable},
			{title: "清除 PAC 自动配置", fn: proxyClearPAC},
			{title: "刷新 DNS 缓存", exe: "ipconfig.exe", args: []string{"/flushdns"}},
			{title: "清理 ARP 缓存", exe: "arp.exe", args: []string{"-d", "*"}},
			{title: "同步系统时间", exe: "w32tm.exe", args: []string{"/resync", "/rediscover", "/nowait"}},
			{title: "释放当前 IP", exe: "ipconfig.exe", args: []string{"/release"}},
			{title: "重新申请 IP", exe: "ipconfig.exe", args: []string{"/renew"}},
		})
	case "proxy":
		return runSteps(tc, []toolStep{
			{title: "复位 WinHTTP 系统代理", exe: "netsh.exe", args: []string{"winhttp", "reset", "proxy"}},
			{title: "关闭用户级代理开关", fn: proxyDisable},
			{title: "清除 PAC 自动配置", fn: proxyClearPAC},
		})
	case "dns":
		return runSteps(tc, []toolStep{{exe: "ipconfig.exe", args: []string{"/flushdns"}}})
	case "arp":
		return runSteps(tc, []toolStep{{exe: "arp.exe", args: []string{"-d", "*"}}})
	case "winsock":
		code, msg := runSteps(tc, []toolStep{{exe: "netsh.exe", args: []string{"winsock", "reset"}}})
		netSmartReboot(tc, code, "重置 Winsock", "Winsock 已重置，建议重启后让应用程序重新装载网络组件")
		return code, msg
	case "tcpip":
		code, msg := runSteps(tc, []toolStep{{exe: "netsh.exe", args: []string{"int", "ip", "reset"}}})
		netSmartReboot(tc, code, "重置 TCP/IP", "TCP/IP 协议栈已重置，建议重启后生效")
		return code, msg
	case "hosts":
		return netHostsReset(tc)
	case "time":
		return netTimeSync(tc)
	case "nic":
		return netNicRestart(tc)
	case "show":
		return runSteps(tc, []toolStep{{exe: "ipconfig.exe", args: []string{"/all"}}})
	}
	tc.log("err", "未知的修复动作："+act)
	return 1, "未知动作"
}

// ---- 代理开关 / PAC：原生注册表读写，不再依赖 reg.exe ----
// 精简版系统（LTSC 精简、第三方裁剪镜像）常把 reg.exe 一并移除，原先这两步用
// reg.exe add / delete 在这种机器上会直接失败。改用原生 registry API，写进去的
// 键、值、类型与原命令完全一致。

const inetSettingsKey = `Software\Microsoft\Windows\CurrentVersion\Internet Settings`

// proxyDisable 关闭用户级代理开关
// 等价：reg add HKCU\...\Internet Settings /v ProxyEnable /t REG_DWORD /d 0 /f
func proxyDisable() error {
	k, err := registry.OpenKey(registry.CURRENT_USER, inetSettingsKey, registry.SET_VALUE)
	if err != nil {
		return err
	}
	defer k.Close()
	return k.SetDWordValue("ProxyEnable", 0)
}

// proxyClearPAC 清除 PAC 自动配置地址
// 等价：reg delete HKCU\...\Internet Settings /v AutoConfigURL /f
// 值本来就不存在时算已达成目的，不算失败（reg.exe 版本这里会报错但不影响结果）。
func proxyClearPAC() error {
	k, err := registry.OpenKey(registry.CURRENT_USER, inetSettingsKey, registry.SET_VALUE)
	if err != nil {
		return err
	}
	defer k.Close()
	if err := k.DeleteValue("AutoConfigURL"); err != nil && !errors.Is(err, windows.ERROR_FILE_NOT_FOUND) {
		return err
	}
	return nil
}

// netSmartReboot 子动作需要重启时：打提示 + 落进待重启清单（与工具级 Reboot 同款机制）
func netSmartReboot(tc *toolCtx, code int, name, msg string) {
	if code != 0 {
		return
	}
	tc.log("warn", "[需重启生效] "+msg+"。重启前界面会一直保留这条提醒，重启后自动消失。")
	addPendingReboot([]string{name})
}

// ---- 局域网共享 / 打印机共享修复（集成面板，按子动作分派） ----
// 子动作：all(一键全修复) / printer-restart / printer-panel / share-list /
//         sessions / clear-cred / smb1 / check
// 覆盖共享打不通的四个根因：网络发现被公用网络禁用、发现类服务没起、
// 防火墙拦了共享规则、RPC/SMB 异常。SMB 与 Guest 只检查不改动。
func toolLanFix(tc *toolCtx) (int, string) {
	act := strings.TrimSpace(tc.arg)
	if act == "" {
		act = "all"
	}
	switch act {
	case "all":
		return lanFixAll(tc)
	case "printer-restart":
		tc.log("head", "▶ 重启打印服务（Print Spooler）")
		if _, err := runStepStream(tc.ctx, tc.log, resolveExe("net.exe"), []string{"stop", "Spooler"}, ""); err != nil {
			tc.log("dim", "停止未成功（可能本来就没在运行）")
		}
		if _, err := runStepStream(tc.ctx, tc.log, resolveExe("net.exe"), []string{"start", "Spooler"}, ""); err != nil {
			tc.log("err", "启动打印服务失败：" + shortErr(err))
			return 1, "打印服务启动失败"
		}
		tc.log("ok", "打印服务已重启")
		return 0, "打印服务已重启，打印队列已清空重置"
	case "printer-panel":
		tc.log("line", "正在打开打印机面板…")
		if err := exec.Command(resolveExe("control.exe"), "printers").Start(); err != nil {
			tc.log("err", "打开失败：" + shortErr(err))
			return 1, "打开失败"
		}
		return 0, "已打开打印机面板"
	case "share-list":
		tc.log("head", "▶ 本机对外共享列表")
		out, err := runHiddenOut(30*time.Second, resolveExe("net.exe"), "share")
		if err != nil {
			tc.log("err", "查询失败：" + shortErr(err))
			return 1, "查询失败"
		}
		tc.log("line", decodeConsole([]byte(out)))
		return 0, ""
	case "sessions":
		tc.log("head", "▶ 当前连接本机共享的设备（会话）")
		out, err := runHiddenOut(30*time.Second, resolveExe("net.exe"), "session")
		if err != nil {
			tc.log("dim", "没有设备连接，或需要管理员权限：" + shortErr(err))
			return 0, "当前没有已连接的共享会话"
		}
		tc.log("line", decodeConsole([]byte(out)))
		return 0, ""
	case "clear-cred":
		tc.log("head", "▶ 断开所有共享映射并清除已保存凭据")
		if _, err := runStepStream(tc.ctx, tc.log, resolveExe("net.exe"), []string{"use", "*", "/delete", "/y"}, ""); err != nil {
			tc.log("warn", "部分映射清除失败（可能本来就没有映射）：" + shortErr(err))
		}
		tc.log("ok", "已执行完毕，下次访问共享会重新要求输入凭据")
		return 0, "网络凭据已清除，下次访问共享需重新输入账号密码"
	case "smb1":
		tc.log("head", "▶ 安装 SMB1.0 组件（老设备兼容，有安全风险）")
		code, msg := runSteps(tc, []toolStep{
			{title: "安装 SMB1Protocol 功能", exe: "dism.exe", args: []string{"/online", "/enable-feature", "/featurename:SMB1Protocol", "/all", "/norestart"}},
		})
		netSmartReboot(tc, code, "开启 SMB1.0", "SMB1 组件已安装，需要重启后生效")
		return code, msg
	case "check":
		return lanFixCheck(tc)
	}
	tc.log("err", "未知的共享修复动作：" + act)
	return 1, "未知动作"
}

// lanFixCheck SMB 与 Guest 只读检查：只报告，不改动（改 SMB1 有安全隐患，启用 Guest 有暴露风险）
func lanFixCheck(tc *toolCtx) (int, string) {
	tc.log("head", "① SMB 共享协议状态（只读检查，不修改）")
	out, err := runHiddenOut(30*time.Second, resolveExe("powershell.exe"),
		"-NoProfile", "-ExecutionPolicy", "Bypass", "-Command",
		"(Get-SmbServerConfiguration | Select-Object EnableSMB1Protocol,EnableSMB2Protocol | Format-List | Out-String).Trim()")
	if err != nil || strings.TrimSpace(out) == "" {
		tc.log("dim", "未取到 SMB 配置（SMB 组件可能未安装）")
	} else {
		tc.log("line", strings.TrimSpace(out))
		tc.log("dim", "SMB2 保持开启即可；仅连接 WinXP/老 NAS 才需要 SMB1（不建议常开）")
	}
	tc.log("head", "② Guest 账户状态（只报告，不自动启用）")
	gout, gerr := runHiddenOut(30*time.Second, resolveExe("powershell.exe"),
		"-NoProfile", "-ExecutionPolicy", "Bypass", "-Command",
		"$u=Get-LocalUser -Name 'Guest' -ErrorAction SilentlyContinue; if($u){'Guest 账户启用状态: '+$u.Enabled} else {'未找到 Guest 账户'}")
	if gerr != nil || strings.TrimSpace(gout) == "" {
		tc.log("dim", "未取到 Guest 状态")
	} else {
		tc.log("line", strings.TrimSpace(gout))
		tc.log("dim", "对方要求匿名访问时才需要启用 Guest；启用后任何人可访问共享，请自行权衡")
	}
	return 0, "状态检查完成（未做任何修改）"
}

// lanFixAll 一键全修复：共享服务 + 防火墙（中英文规则组容错）+ 专用网络 + 重启打印服务
func lanFixAll(tc *toolCtx) (int, string) {
	ok, warn := 0, 0

	// ① 共享相关服务：设为自动启动并拉起
	svcs := []struct{ name, desc string }{
		{"FDResPub", "网络发现（资源发布）"},
		{"SSDPSRV", "SSDP 设备发现"},
		{"upnphost", "UPnP 设备宿主"},
		{"LanmanServer", "文件与打印机共享（服务端）"},
		{"LanmanWorkstation", "网络客户端（访问他人共享）"},
		{"RpcSs", "RPC 远程过程调用"},
		{"RpcEptMapper", "RPC 端点映射（打印机共享依赖）"},
	}
	tc.log("head", "① 启动共享相关服务")
	for _, s := range svcs {
		tc.log("line", "· " + s.desc + "（" + s.name + "）")
		// 先设为自动启动（失败只提示，服务可能被系统策略禁用，不阻断）
		if _, err := runStepStream(tc.ctx, tc.log, resolveExe("sc.exe"), []string{"config", s.name, "start=", "auto"}, ""); err != nil {
			tc.log("dim", "  （设为自动启动未成功：可能不存在或受策略限制）")
		}
		if _, err := runStepStream(tc.ctx, tc.log, resolveExe("net.exe"), []string{"start", s.name}, ""); err != nil {
			tc.log("warn", "  启动未成功：" + shortErr(err))
			warn++
		} else {
			tc.log("ok", "  已运行")
			ok++
		}
	}

	// ② 防火墙放通：中文/英文规则组都试，命中哪个算哪个
	tc.log("head", "② 防火墙放通共享规则（中英文规则组都试）")
	groups := []string{
		"网络发现", "Network Discovery",
		"文件和打印机共享", "File and Printer Sharing",
		"远程服务管理", "Remote Service Management",
	}
	fwOK := 0
	for _, g := range groups {
		code, err := runStepStream(tc.ctx, tc.log, resolveExe("netsh.exe"),
			[]string{"advfirewall", "firewall", "set", "rule", "group=" + g, "new", "enable=yes"}, "")
		if err == nil && code == 0 {
			tc.log("ok", "  ✓ " + g)
			fwOK++
		} else {
			tc.log("dim", "  — " + g + "（本机无此规则组，跳过）")
		}
	}
	if fwOK == 0 {
		tc.log("warn", "没有匹配到任何共享规则组（可能防火墙服务被禁用或组名已变），请手动检查「高级安全防火墙」")
		warn++
	} else {
		ok++
	}

	// ③ 网络配置文件设为专用：公用网络会强制关闭网络发现，这是共享用不了的头号原因
	tc.log("head", "③ 把当前网络设为「专用」（公用网络下网络发现被强制关闭）")
	if _, err := runStepStream(tc.ctx, tc.log, resolveExe("powershell.exe"), []string{
		"-NoProfile", "-ExecutionPolicy", "Bypass", "-Command",
		"Get-NetConnectionProfile | Set-NetConnectionProfile -NetworkCategory Private",
	}, ""); err != nil {
		tc.log("warn", "设置失败：" + shortErr(err))
		warn++
	} else {
		tc.log("ok", "已设为专用网络")
		ok++
	}

	// ④ 重启打印服务：打印机共享打不开多半是 Spooler 卡死
	tc.log("head", "④ 重启打印服务（Print Spooler）")
	runStepStream(tc.ctx, tc.log, resolveExe("net.exe"), []string{"stop", "Spooler"}, "")
	if _, err := runStepStream(tc.ctx, tc.log, resolveExe("net.exe"), []string{"start", "Spooler"}, ""); err != nil {
		tc.log("warn", "打印服务启动失败：" + shortErr(err))
		warn++
	} else {
		tc.log("ok", "打印服务已重启")
		ok++
	}

	// ⑤ SMB / Guest 只读检查（不改动，见 lanFixCheck）
	lanFixCheck(tc)

	tc.log("head", "▶ 完成")
	tc.log("line", fmt.Sprintf("成功 %d 项，需留意 %d 项。重启后共享/打印机发现通常就能正常使用。", ok, warn))
	if warn > 0 {
		return 0, fmt.Sprintf("已修复（%d 项需留意，见上方日志），建议重启后验证", warn)
	}
	return 0, "共享修复完成，建议重启后验证访问"
}

// ---- 端口占用查询 ----
func toolNetPort(tc *toolCtx) (int, string) {
	tc.log("head", "▶ 枚举当前网络连接")
	out, _ := runHiddenOut(60*time.Second, resolveExe("netstat.exe"), "-ano")
	text := decodeConsole([]byte(out))
	port := strings.TrimSpace(tc.arg)

	pids := map[string]bool{}
	type row struct{ proto, local, remote, state, pid string }
	var rows []row
	for _, ln := range strings.Split(text, "\n") {
		ln = strings.TrimRight(ln, "\r")
		f := strings.Fields(ln)
		if len(f) < 4 {
			continue
		}
		if !strings.EqualFold(f[0], "TCP") && !strings.EqualFold(f[0], "UDP") {
			continue
		}
		r := row{proto: f[0], local: f[1]}
		if len(f) == 5 {
			r.remote, r.state, r.pid = f[2], f[3], f[4]
		} else {
			r.remote, r.pid = f[2], f[3]
		}
		if port != "" {
			if !strings.HasSuffix(r.local, ":"+port) && !strings.HasSuffix(r.remote, ":"+port) {
				continue
			}
		} else if r.state != "LISTENING" && r.state != "" && !strings.EqualFold(r.proto, "UDP") {
			continue // 无端口参数时只列监听中的，避免几千行噪音
		}
		if _, err := strconv.Atoi(r.pid); err != nil {
			continue
		}
		pids[r.pid] = true
		rows = append(rows, r)
	}

	// PID → 进程名
	nameOf := map[string]string{}
	if len(pids) > 0 {
		if tl, err := runHiddenOut(60*time.Second, resolveExe("tasklist.exe"), "/FO", "CSV", "/NH"); err == nil {
			for _, ln := range strings.Split(decodeConsole([]byte(tl)), "\n") {
				parts := strings.Split(strings.TrimSpace(ln), `","`)
				if len(parts) < 2 {
					continue
				}
				nm := strings.Trim(parts[0], `"`)
				pid := strings.Trim(parts[1], `"`)
				if pids[pid] {
					nameOf[pid] = nm
				}
			}
		}
	}

	sort.Slice(rows, func(i, j int) bool {
		pi, _ := strconv.Atoi(rows[i].pid)
		pj, _ := strconv.Atoi(rows[j].pid)
		return pi < pj
	})

	if len(rows) == 0 {
		// 区分「确实没有」与「命令根本没跑起来」——后者要报警，否则用户以为一切正常
		if strings.TrimSpace(text) == "" {
			tc.log("err", "netstat 没有返回任何内容（命令不可用或被安全软件拦截）")
			return 1, "命令无输出"
		}
		if port != "" {
			tc.log("line", "没有进程占用端口 "+port)
		} else {
			tc.log("line", "未发现监听中的端口")
		}
		return 0, ""
	}
	tc.log("line", fmt.Sprintf("%-5s %-24s %-24s %-11s %-7s %s", "协议", "本地地址", "外部地址", "状态", "PID", "进程"))
	tc.log("line", strings.Repeat("-", 92))
	for _, r := range rows {
		nm := nameOf[r.pid]
		if nm == "" {
			nm = "—"
		}
		tc.log("line", fmt.Sprintf("%-5s %-24s %-24s %-11s %-7s %s", r.proto, r.local, r.remote, r.state, r.pid, nm))
	}
	return 0, fmt.Sprintf("共 %d 条记录", len(rows))
}

// ---- Hosts 编辑 ----
func toolNetHosts(tc *toolCtx) (int, string) {
	p := filepath.Join(os.Getenv("SystemRoot"), "System32", "drivers", "etc", "hosts")
	dir := filepath.Join(os.Getenv("LOCALAPPDATA"), "3z9wOpt", "reports")
	if err := os.MkdirAll(dir, 0755); err != nil {
		tc.log("err", "无法创建备份目录："+err.Error())
		return 1, "备份失败"
	}
	bak := filepath.Join(dir, "hosts_backup_"+time.Now().Format("20060102_150405")+".txt")
	if b, err := os.ReadFile(p); err == nil {
		if err := os.WriteFile(bak, b, 0644); err != nil {
			tc.log("err", "备份失败："+shortErr(err))
			return 1, "备份失败"
		}
		tc.log("ok", "原文件已备份："+bak)
	} else {
		tc.log("dim", "未找到 hosts 文件，将新建")
	}
	tc.log("line", "正在打开 hosts（记事本）："+p)
	if err := exec.Command(resolveExe("notepad.exe"), p).Start(); err != nil {
		tc.log("err", "打开失败："+shortErr(err))
		return 1, "打开失败"
	}
	return 0, "已用记事本打开，保存后如需立即生效请再点一次「刷新 DNS 缓存」"
}

// ---- 面板子动作：重置 Hosts ----
func netHostsReset(tc *toolCtx) (int, string) {
	p := filepath.Join(os.Getenv("SystemRoot"), "System32", "drivers", "etc", "hosts")
	dir := filepath.Join(os.Getenv("LOCALAPPDATA"), "3z9wOpt", "reports")
	if err := os.MkdirAll(dir, 0755); err != nil {
		tc.log("err", "无法创建备份目录："+err.Error())
		return 1, "备份失败"
	}
	bak := filepath.Join(dir, "hosts_backup_reset_"+time.Now().Format("20060102_150405")+".txt")
	if b, err := os.ReadFile(p); err == nil {
		if err := os.WriteFile(bak, b, 0644); err != nil {
			tc.log("err", "备份失败："+shortErr(err))
			return 1, "备份失败"
		}
		tc.log("ok", "原 Hosts 已备份："+bak)
	}
	defaultHosts := "# 默认空 Hosts 文件\r\n# 由 3z9wOpt 重置生成\r\n# 如需自定义请使用「Hosts 文件编辑」\r\n"
	if err := os.WriteFile(p, []byte(defaultHosts), 0644); err != nil {
		tc.log("err", "重置失败："+shortErr(err))
		return 1, "重置失败"
	}
	tc.log("ok", "Hosts 已重置为默认空状态")
	return 0, "Hosts 已重置，建议再点一次「刷新 DNS 缓存」"
}

// ---- 面板子动作：同步时间（修 SSL 证书报错） ----
func netTimeSync(tc *toolCtx) (int, string) {
	servers := "time.windows.com,0x09 time1.aliyun.com,0x09 time1.cloud.tencent.com,0x09"
	tc.log("head", "▶ 配置时间服务器："+servers)
	if _, err := runHiddenOut(30*time.Second, resolveExe("w32tm.exe"), "/config", "/manualpeerlist:"+servers, "/syncfromflags:manual", "/reliable:yes", "/update"); err != nil {
		tc.log("warn", "配置时间服务器时出错（可能服务未运行）："+shortErr(err))
	}
	tc.log("line", "正在停止 Windows Time 服务…")
	runHiddenOut(20*time.Second, resolveExe("net.exe"), "stop", "w32time")
	tc.log("line", "正在启动 Windows Time 服务…")
	if _, err := runHiddenOut(20*time.Second, resolveExe("net.exe"), "start", "w32time"); err != nil {
		tc.log("warn", "启动 w32time 失败："+shortErr(err))
	}
	tc.log("head", "▶ 立即同步时间")
	out, err := runHiddenOut(60*time.Second, resolveExe("w32tm.exe"), "/resync", "/rediscover", "/nowait")
	if err != nil {
		tc.log("err", "同步失败："+shortErr(err))
		return 1, "时间同步失败（请确认网络已连通）"
	}
	if strings.TrimSpace(out) != "" {
		tc.log("line", strings.TrimSpace(out))
	}
	tc.log("ok", "时间同步指令已下发")
	return 0, "时间同步完成，HTTPS 证书报错可再试"
}

// ---- 面板子动作：强力重启网卡 ----
func netNicRestart(tc *toolCtx) (int, string) {
	tc.log("head", "▶ 枚举可用网卡")
	out, err := runHiddenOut(30*time.Second, resolveExe("netsh.exe"), "interface", "show", "interface")
	if err != nil {
		tc.log("err", "无法枚举网卡："+shortErr(err))
		return 1, "网卡枚举失败"
	}
	text := decodeConsole([]byte(out))
	var nics []string
	for _, ln := range strings.Split(text, "\n") {
		trimmed := strings.TrimSpace(strings.TrimRight(ln, "\r"))
		if trimmed == "" || strings.HasPrefix(trimmed, "管理员") || strings.HasPrefix(trimmed, "---") {
			continue
		}
		f := strings.Fields(trimmed)
		if len(f) < 4 || f[0] != "已启用" {
			continue // 只重启处于启用状态的网卡
		}
		name := strings.TrimSpace(trimmed[strings.Index(trimmed, f[2]):])
		if name != "" {
			nics = append(nics, name)
		}
	}
	if len(nics) == 0 {
		tc.log("warn", "未找到处于启用状态的网卡")
		return 1, "未找到网卡"
	}
	for _, n := range nics {
		tc.log("line", "· " + n)
	}
	for _, n := range nics {
		tc.log("head", "▶ 禁用：" + n)
		if _, err := runHiddenOut(30*time.Second, resolveExe("netsh.exe"), "interface", "set", "interface", `name="`+n+`"`, "admin=disabled"); err != nil {
			tc.log("warn", "禁用失败："+shortErr(err))
		}
	}
	for _, n := range nics {
		tc.log("head", "▶ 启用：" + n)
		if _, err := runHiddenOut(30*time.Second, resolveExe("netsh.exe"), "interface", "set", "interface", `name="`+n+`"`, "admin=enabled"); err != nil {
			tc.log("warn", "启用失败："+shortErr(err))
		}
	}
	tc.log("ok", "网卡重启完成")
	return 0, "网卡已重新初始化，请等待 3–10 秒重连"
}
