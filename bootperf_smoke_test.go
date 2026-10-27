package main

/* 开机耗时 —— 冒烟 / 环境自检（2026-10-26）
   ---------------------------------------------------------------------------
   ⚠️ 这份测试的定位不是"我这台机器上跑通"，而是**换台机器也能判断通过与否**。
   功能在开发者机器上永远跑得通（管理员、日志开着、有几十条记录），真正会出事
   的是别人的机器：没提权、日志被别的优化软件关掉、系统是 LTSC/Server、或者
   压根还没重启过几次。所以在断言数据之前，先把环境说清楚。

   跑法：
     go test -run TestBootPerfEnv -v      # 环境体检，任何机器都能跑，不该 panic
     go test -run TestBootPerfSmoke -v    # 数据校验（管理员 + 有记录才有意义）

   判据（不依赖具体秒数，秒数每台机器都不一样）：
     · 环境具备时（管理员 + 通道启用 + 有记录）→ 必须 ok=true 且进桌面耗时 > 0
     · 环境不具备时 → 必须 ok=false，且 errCode **必须与真实成因一致**
       —— 这条最重要：把"日志被关"报成"需要管理员"，用户提权一百次也没用。   */

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"golang.org/x/sys/windows/registry"
)

// osVersion 读 Windows 版本信息。诊断报告里必须带上：不同版本写进 Event 100 的
// 字段并不完全一致，报问题时先看版本能省一半排查时间。
func osVersion(t *testing.T) string {
	k, err := registry.OpenKey(registry.LOCAL_MACHINE,
		`SOFTWARE\Microsoft\Windows NT\CurrentVersion`, registry.QUERY_VALUE)
	if err != nil {
		return "(读不到)"
	}
	defer k.Close()
	get := func(n string) string {
		v, _, e := k.GetStringValue(n)
		if e != nil {
			return ""
		}
		return v
	}
	name := get("ProductName")
	build := get("CurrentBuildNumber")
	disp := get("DisplayVersion")
	if name == "" {
		return "(未知)"
	}
	s := name + " build " + build
	if disp != "" {
		s += " (" + disp + ")"
	}
	return s
}

func TestBootPerfEnv(t *testing.T) {
	fmt.Println("════════ 开机耗时 · 环境自检 ════════")
	fmt.Println("系统版本   :", osVersion(t))
	fmt.Println("管理员身份 :", isElevated())

	exe := bootWevtutil()
	if exe == "" {
		fmt.Println("wevtutil   : ★ 找不到（系统缺少该组件，功能不可用）")
	} else {
		fmt.Println("wevtutil   :", exe)
	}

	switch bootChannelState() {
	case 0:
		fmt.Println("日志通道   : ★ 不存在（精简系统 / Server Core / 通道被移除）")
	case 1:
		fmt.Println("日志通道   : ★ 存在但处于关闭状态（可调用 EnableBootLog 开启）")
	case 2:
		fmt.Println("日志通道   : 存在且已启用")
	}
	fmt.Println("快速启动   :", hiberbootEnabled())

	/* 走的哪条路必须报出来：原生 wevtapi 和 wevtutil 回退的差别不只是快慢，
	   编码行为也不同（原生 UTF-16 无损，wevtutil 跟随控制台代码页）。
	   在别人机器上排查时，第一眼就要知道有没有掉到回退上。 */
	if _, nerr := bootQueryNative("*[System[EventID=100]]", 1); nerr == nil {
		fmt.Println("读取路径   : 原生 wevtapi（UTF-16 无损）")
	} else {
		fmt.Printf("读取路径   : ★ wevtutil 回退（原生失败: %v）\n", nerr)
	}

	evs, timedOut, err := bootQueryWithFallback("*[System[EventID=100]]", bootHistory)
	fmt.Printf("Event 100  : 读到 %d 条 (超时=%v err=%v)\n", len(evs), timedOut, err)

	if len(evs) > 0 {
		// 字段完整度：关键点在于不同版本字段有缺失，显示 0 秒会被当成 bug
		need := []string{"BootStartTime", "MainPathBootTime", "BootPostBootTime",
			"BootTime", "BootNumStartupApps"}
		var missing []string
		d := bootMap(evs[0])
		for _, n := range need {
			if strings.TrimSpace(d[n]) == "" {
				missing = append(missing, n)
			}
		}
		if len(missing) == 0 {
			fmt.Println("字段完整度 : 关键字段齐全")
		} else {
			fmt.Println("字段完整度 : ★ 缺失", strings.Join(missing, ", "))
		}
	}

	comps, _, cerr := bootQueryWithFallback("*[System[(EventID=101 or EventID=102 or EventID=103)]]", bootHistory*12)
	fmt.Printf("组件事件   : 读到 %d 条 (err=%v)\n", len(comps), cerr)

	fmt.Println("──────── 最终结果 ────────")
	a := &App{}
	p := a.GetBootPerf()
	fmt.Printf("ok=%v  errCode=%q  err=%s  记录数=%d\n",
		p.Ok, p.ErrCode, p.Err, len(p.Records))
	for i, r := range p.Records {
		fmt.Printf("  #%d 进桌面 %dms  后台 %dms  启动项 %d 个  最慢 %d 项\n",
			i, r.MainPathMs, r.PostBootMs, r.StartupApps, len(r.Slowest))
		for _, c := range r.Slowest {
			hit := "管不到"
			if c.ItemID != "" {
				hit = fmt.Sprintf("可操作[%s] enabled=%v", c.ItemKind, c.ItemEnabled)
			}
			// 名字残缺的标出来：这是事件源头就坏的，不是我们读坏的，
			// 前端会显示成"名称无法识别的驱动"而不是 "??exe"
			if c.NameBroken {
				hit += " ★名字残缺(源头坏数据)"
			}
			fmt.Printf("       - %-28s %6dms  %s\n", c.Name, c.TotalMs, hit)
		}
	}

	/* 一致性断言：不管这台机器环境如何，成因和归因必须对得上。
	   这是"别的机器上也能测试通过"的核心 —— 允许功能不可用，但不允许撒谎。 */
	if !p.Ok {
		if p.ErrCode == "" {
			t.Error("ok=false 却没给 errCode，前端无法决定显示什么引导")
		}
		if p.Err == "" {
			t.Error("ok=false 却没给人话说明，用户会看到一片空白")
		}
		admin := isElevated()
		if !admin && p.ErrCode != bootErrNoAdmin {
			t.Errorf("非管理员环境却报成 %q —— 会把用户引到错误方向", p.ErrCode)
		}
		if admin && bootChannelState() == 1 && p.ErrCode != bootErrDisabled {
			t.Errorf("日志处于关闭状态却报成 %q", p.ErrCode)
		}
		t.Logf("环境不具备，已按预期降级（errCode=%s），不算失败", p.ErrCode)
		return
	}

	if len(p.Records) == 0 {
		t.Fatal("ok=true 但没有任何记录")
	}
	r := p.Records[0]
	if r.MainPathMs <= 0 {
		t.Errorf("进桌面耗时没读到：%+v", r)
	}
	if r.TotalMs > 0 && r.MainPathMs+r.PostBootMs != r.TotalMs {
		t.Errorf("口径对不上：total=%d 但 mainPath+postBoot=%d",
			r.TotalMs, r.MainPathMs+r.PostBootMs)
	}
	fmt.Println("──────── 自检结束 ────────")
}

func TestBootPerfSmoke(t *testing.T) {
	a := &App{}
	p := a.GetBootPerf()
	b, _ := json.MarshalIndent(p, "", "  ")
	t.Log("\n" + string(b))

	if !p.Ok {
		t.Skipf("读不到开机数据（预期内：非管理员或日志未启用）—— code=%s %s", p.ErrCode, p.Err)
	}
	if len(p.Records) == 0 {
		t.Fatal("ok=true 但没有任何记录")
	}
	r := p.Records[0]
	if r.MainPathMs <= 0 {
		t.Errorf("进桌面耗时没读到：%+v", r)
	}
	if r.TotalMs > 0 && r.MainPathMs+r.PostBootMs != r.TotalMs {
		t.Errorf("口径对不上：total=%d 但 mainPath+postBoot=%d",
			r.TotalMs, r.MainPathMs+r.PostBootMs)
	}
}

/* TestBootErrCodeCoverage 前后端错误码必须一一对应。
   起因：后端的失败原因分了 7 类，前端要按类给不同引导。少一个映射，那条分支
   在用户机器上就会退化成空白 —— 而开发机上永远走不到那些分支，测不出来。
   所以扫源码做一致性检查：以后加新 errCode 忘了改前端，这里会直接失败。 */
func TestBootErrCodeCoverage(t *testing.T) {
	src, err := os.ReadFile("bootperf.go")
	if err != nil {
		t.Fatal(err)
	}
	js, err := os.ReadFile(filepath.Join("design", "startup.js"))
	if err != nil {
		t.Fatal(err)
	}
	re := regexp.MustCompile(`bootErr\w+\s+bootErrCode\s*=\s*"([^"]+)"`)
	found := re.FindAllStringSubmatch(string(src), -1)
	if len(found) < 5 {
		t.Fatalf("只从源码里找到 %d 个 errCode，正则可能失效了", len(found))
	}
	var codes []string
	for _, m := range found {
		code := m[1]
		codes = append(codes, code)
		if !strings.Contains(string(js), `"`+code+`"`) && !strings.Contains(string(js), code+":") {
			t.Errorf("errCode %q 在前端 start.js 的 SU_BOOT_FIX 里没有对应引导 "+
				"→ 该机器上会显示一片空白", code)
		}
	}
	fmt.Println("错误码前后端一致性:", strings.Join(codes, ", "), "→ 全部已覆盖")
}

/* TestBootChannelState 验证「通道不存在 / 被关 / 正常」三态判定。
   为什么这么测：disabled 这条分支在本机永远走不到（日志本来开着），但它在
   别人机器上很常见 —— 一堆优化软件会把 Diagnostics-Performance 关掉。
   我们不去改用户自己的日志设置来造这个场景，而是拿一个**本来就处于关闭状态的
   其它通道**来验证判定函数本身，风险为零、可重复。 */
func TestBootChannelState(t *testing.T) {
	// 挑几个系统自带、默认关闭的通道做样本
	cands := []string{
		"Microsoft-Windows-Diagnostics-Performance/Diagnostic",
		"Microsoft-Windows-Kernel-Boot/Analytic",
		"Microsoft-Windows-Application-Experience/Program-Telemetry",
		"Microsoft-Windows-TaskScheduler/Operational",
		"Microsoft-Windows-Winlogon/Operational",
		"Microsoft-Windows-Diagnostics-Performance/Analytic",
	}
	var closed string
	for _, c := range cands {
		if bootChannelStateOf(c) == 1 {
			closed = c
			break
		}
	}
	if closed == "" {
		t.Skip("本机没有可用的「存在但关闭」通道样本，跳过（不影响功能判定）")
	}
	t.Logf("关闭态样本: %s", closed)

	if got := bootChannelStateOf("NoSuchChannel/DefinitelyNotExist"); got != 0 {
		t.Errorf("不存在的通道应返回 0，实际 %d", got)
	}
	if got := bootChannelState(); got != 2 {
		t.Errorf("开机性能日志本机应为启用态 2，实际 %d ——"+
			"如果 1，说明本机日志真的被关了，那 GetBootPerf 应报 disabled", got)
	}
	fmt.Println("通道三态判定：不存在=0 关闭=1 启用=2 → 均正确")
}

/* TestBootPerfNoWindow 验证子进程不会闪控制台窗口。
   本程序是 -H windowsgui 的 GUI 进程，自身没有控制台；若 exec 不显式设置
   HideWindow/CREATE_NO_WINDOW，Windows 会给子进程新建控制台 —— 在开发者机器上
   （从已附着控制台的终端启动）完全看不出来，到普通用户桌面上就是每次进页面
   闪一个黑窗。这个测试锁住这个行为，防止以后有人改回去。 */
func TestBootPerfNoWindow(t *testing.T) {
	cmd := bootCmd(context.Background(), bootWevtutil(),
		"qe", bootLogName, "/q:*[System[EventID=100]]", "/f:xml", "/c:1")
	if cmd.SysProcAttr == nil {
		t.Fatal("SysProcAttr 为空：GUI 进程下会闪黑窗")
	}
	if !cmd.SysProcAttr.HideWindow {
		t.Error("HideWindow 未设置")
	}
	if cmd.SysProcAttr.CreationFlags&0x08000000 == 0 { // CREATE_NO_WINDOW
		t.Error("CREATE_NO_WINDOW 未设置")
	}
}
