package main

// 工具库 · 实用工具类（util）
// 新增/修改工具：在 init() 里加一条 regTool(...)，详见 toolkit.go 顶部的扩展手册。

import (
	"fmt"
	"os/exec"
	"strconv"
	"strings"
)

func init() {
	regTool(ToolItem{
		ID: "util-shutdown", Cat: "util", Name: "定时关机 / 重启",
		Desc: "设定指定时间后关机、重启或睡眠",
		Cmd:  "shutdown /s | /r | /h /t <分钟>", Risk: "safe", Timeout: 60,
		ArgKind: "shutdown", ArgLabel: "时间",
	}, toolImpl{Run: toolShutdown})

	regTool(ToolItem{
		ID: "util-shutdown-cancel", Cat: "util", Name: "取消关机计划",
		Desc: "撤销已设定的定时关机或重启",
		Cmd:  "shutdown /a", Risk: "safe", Timeout: 30,
	}, toolImpl{Steps: []toolStep{{exe: "shutdown.exe", args: []string{"/a"}}}})

	regTool(ToolItem{
		ID: "util-timesync", Cat: "util", Name: "系统时间同步",
		Desc: "与互联网时间服务器重新对时",
		Cmd:  "w32tm /resync", Risk: "safe", Timeout: 60,
	}, toolImpl{Steps: []toolStep{{exe: "w32tm.exe", args: []string{"/resync"}}}})

	regTool(ToolItem{
		ID: "util-spooler", Cat: "util", Name: "打印机服务重启",
		Desc: "打印队列卡死、点了没反应时用",
		Cmd:  "net stop spooler + net start spooler", Risk: "safe", Timeout: 120,
		Note: "会清空当前打印队列中卡住的任务。",
	}, toolImpl{Steps: []toolStep{
		{title: "停止打印后台服务", exe: "net.exe", args: []string{"stop", "spooler"}},
		{title: "启动打印后台服务", exe: "net.exe", args: []string{"start", "spooler"}},
	}})

	regTool(ToolItem{
		ID: "util-opentool", Cat: "util", Name: "快速打开系统工具",
		Desc: "任务管理器、服务、设备管理器、注册表等一键打开",
		Cmd: "按选择启动系统组件", Risk: "safe", Timeout: 30,
		ArgKind: "opentool", ArgLabel: "工具", ArgReq: true, ArgDef: "taskmgr",
		Opts: []ToolOpt{
			{V: "taskmgr", T: "任务管理器"},
			{V: "services", T: "服务"},
			{V: "devmgmt", T: "设备管理器"},
			{V: "diskmgmt", T: "磁盘管理"},
			{V: "regedit", T: "注册表编辑器"},
			{V: "gpedit", T: "本地组策略"},
			{V: "eventvwr", T: "事件查看器"},
			{V: "msconfig", T: "系统配置"},
			{V: "msinfo", T: "系统信息"},
			{V: "netconn", T: "网络连接"},
			{V: "firewall", T: "防火墙设置"},
			{V: "control", T: "控制面板"},
		},
	}, toolImpl{Run: toolOpenTool})

	regTool(ToolItem{
		ID: "util-activation", Cat: "util", Name: "系统激活状态",
		Desc: "查看 Windows 版本、授权类型与激活状态",
		Cmd:  "cscript slmgr.vbs /dli", Risk: "readonly", OutText: true, Timeout: 60,
	}, toolImpl{Steps: []toolStep{{exe: "cscript.exe", args: []string{"//nologo", resolveExe("slmgr.vbs"), "/dli"}}}})
}

// ---- 定时关机 / 重启 ----
func toolShutdown(tc *toolCtx) (int, string) {
	// arg 形如 "s:60"（动作:分钟）
	action, mins := "s", 60
	if parts := strings.SplitN(tc.arg, ":", 2); len(parts) == 2 {
		action = parts[0]
		if n, err := strconv.Atoi(parts[1]); err == nil && n >= 0 {
			mins = n
		}
	}
	label := map[string]string{"s": "关机", "r": "重启", "h": "睡眠"}[action]
	if label == "" {
		label, action = "关机", "s"
	}
	args := []string{"/" + action}
	if action != "h" {
		args = append(args, "/t", strconv.Itoa(mins*60))
	}
	if action == "s" || action == "r" {
		args = append(args, "/c", "3z9wOpt 工具库：定时"+label)
	}
	code, err := runStepStream(tc.ctx, tc.log, "shutdown.exe", args, "")
	if err != nil {
		tc.log("err", shortErr(err))
		return 1, "设置失败"
	}
	_ = code
	tc.log("ok", fmt.Sprintf("已设定 %d 分钟后%s", mins, label))
	if action != "h" {
		tc.log("dim", "反悔了？用「取消关机计划」随时撤销。")
	}
	return 0, fmt.Sprintf("已设定 %d 分钟后%s", mins, label)
}

// ---- 快速打开系统工具 ----
var openToolMap = map[string][]string{
	"taskmgr":  {"taskmgr.exe"},
	"services": {"mmc.exe", "services.msc"},
	"devmgmt":  {"mmc.exe", "devmgmt.msc"},
	"diskmgmt": {"mmc.exe", "diskmgmt.msc"},
	"regedit":  {"regedit.exe"},
	"gpedit":   {"mmc.exe", "gpedit.msc"},
	"eventvwr": {"mmc.exe", "eventvwr.msc"},
	"msconfig": {"msconfig.exe"},
	"msinfo":   {"msinfo32.exe"},
	"netconn":  {"control.exe", "ncpa.cpl"},
	"firewall": {"control.exe", "firewall.cpl"},
	"control":  {"control.exe"},
}

func toolOpenTool(tc *toolCtx) (int, string) {
	cmd := openToolMap[tc.arg]
	if len(cmd) == 0 {
		return 1, "未知的系统工具：" + tc.arg
	}
	exe := resolveExe(cmd[0])
	args := make([]string, 0, len(cmd)-1)
	for _, a := range cmd[1:] {
		args = append(args, resolveExe(a))
	}
	tc.log("line", "正在打开："+exe)
	if err := exec.Command(exe, args...).Start(); err != nil {
		tc.log("err", "打开失败："+shortErr(err))
		return 1, "打开失败"
	}
	tc.log("ok", "已启动。部分工具（如组策略）在家庭版系统上不存在，属正常。")
	return 0, ""
}
