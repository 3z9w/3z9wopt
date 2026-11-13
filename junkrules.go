package main

// ============================================================================
// 垃圾清理规则引擎（2026-11-12，浏览器/应用/AI 三类缓存统一规则化）
//
// 【为什么】这三类共 51 项原来是 Go 代码里的字面量清单，加一个新软件要改代码、
// 重新编译、重跑全量验收。业内标准做法（CCleaner winapp2 / BleachBit CleanerML /
// Windows cleanmgr 的 VolumeCaches + DataDrivenCleaner）都是「引擎与规则分离」：
// 规则是数据文件，加新软件 = 加一节文本。现在把 51 项迁入 rules/junkrules.ini
//（go:embed 内置），并支持 exe 旁边的 custom.ini 追加/覆盖。
//
// 【误报治理红线（刻意阉割的能力）】
//   本引擎的规则永远只能描述「删哪些文件/目录」：
//     ✗ 无命令执行（规则里写不了任何命令）
//     ✗ 无注册表删除（不学 winapp2 的 RegKey）
//     ✗ 无网络下载（规则只随 exe 版本走，绝不在运行时联网拉规则）
//   —— 数据不是代码：杀软对路径字符串本就不敏感，这三个"不做"保证规则化
//   不会引入任何新的误报面。规则文件随版本更新走现有 exe 发布通道。
//
// 【安全边界】
//   - 节名（ID）只认 ^[a-z0-9]+$，防止和现有清理项撞车；
//   - group 只认三个既分组（前端 CATS 只渲染已知分组，乱填会导致条目不可见，
//     干脆在引擎侧拒掉并记日志）；
//   - 一节至少要有一个 dir/filekey/electron，否则跳过（写错只影响那一条）；
//   - custom.ini 解析失败/条目非法 = 跳过该条，绝不 panic、绝不影响其他项。
// ============================================================================

import (
	_ "embed"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

//go:embed rules/junkrules.ini
var junkRulesEmbedded []byte

// junkRule 一节规则的中间表示（filekey/electron 展开前）。
type junkRule struct {
	id     string
	name   string
	group  string
	desc   string
	risk   string
	renew  bool
	heavy  bool
	na     bool
	defoff bool

	dirs    []string
	globs   []string
	require []string
	excl    []string
	gofind  []string   // 数据根发现（junk_findroots.go 注册的 Go 特型 finder 名）：全盘找自定义路径
	finds   []findSpec // 数据化搜索声明（findN = 特征目录名 | 子目录glob）：不用写 Go 就能全盘兜底
}

// ruleGroups 规则文件允许的分组（与前端 CATS 的 g 数组一一对应）。
var ruleGroups = map[string]bool{
	"浏览器缓存":  true,
	"应用缓存":   true,
	"AI 应用缓存": true,
}

// junkRuleSkipLog 被跳过的规则及原因（诊断用；正常应为空）。
var junkRuleSkipLog []string

// ruleEnvVars 规则路径变量表（小写键）。%ProgramFiles86% 故意不带括号。
func ruleEnvVars() map[string]string {
	U := os.Getenv("USERPROFILE")
	return map[string]string{
		"localappdata":  os.Getenv("LOCALAPPDATA"),
		"appdata":       os.Getenv("APPDATA"),
		"userprofile":   U,
		"documents":     filepath.Join(U, "Documents"),
		"locallow":      filepath.Join(U, `AppData\LocalLow`),
		"programdata":   os.Getenv("ProgramData"),
		"windir":        os.Getenv("windir"),
		"programfiles":  os.Getenv("ProgramFiles"),
		"programfiles86": os.Getenv("ProgramFiles(x86)"),
	}
}

// expandRulePath 把 %Var% 展开成真实路径（未知变量原样保留，不展开也不吞掉）。
func expandRulePath(p string, env map[string]string) string {
	var b strings.Builder
	for {
		i := strings.IndexByte(p, '%')
		if i < 0 {
			b.WriteString(p)
			return b.String()
		}
		j := strings.IndexByte(p[i+1:], '%')
		if j < 0 { // 只有半个百分号：原样收尾
			b.WriteString(p)
			return b.String()
		}
		name := p[i+1 : i+1+j]
		if v, ok := env[strings.ToLower(name)]; ok {
			b.WriteString(p[:i])
			b.WriteString(v)
		} else {
			b.WriteString(p[:i+1+j+1]) // 未知变量：连同两个 % 原样保留
		}
		p = p[i+1+j+1:]
	}
}

// parseJunkRules 解析 ini 文本。语法：[id] 节 + key = value；
// detectN / dirN / filekeyN / electronN / excludeN 为编号键（N 任意，按出现顺序收集）。
// 写错的节直接跳过并记入 junkRuleSkipLog —— 用户手改 custom.ini 不许把程序搞挂。
func parseJunkRules(data []byte) []junkRule {
	var rules []junkRule
	var cur *junkRule
	env := ruleEnvVars()
	flush := func() {
		if cur == nil {
			return
		}
		why := ""
		switch {
		case cur.id == "":
			why = "节名为空"
		case !isRuleID(cur.id):
			why = "节名只许小写字母数字"
		case cur.name == "":
			why = "缺 name"
		case !ruleGroups[cur.group]:
			why = "group 只许：浏览器缓存 / 应用缓存 / AI 应用缓存"
		case len(cur.dirs) == 0 && len(cur.globs) == 0 && len(cur.gofind) == 0 && len(cur.finds) == 0:
			why = "一个 dir/filekey/electron/gofind/find 都没有"
		}
		if why != "" {
			junkRuleSkipLog = append(junkRuleSkipLog, fmt.Sprintf("[%s] %s", cur.id, why))
		} else {
			rules = append(rules, *cur)
		}
		cur = nil
	}
	s := strings.TrimPrefix(string(data), "\uFEFF")
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimRight(line, "\r")
		t := strings.TrimSpace(line)
		if t == "" || strings.HasPrefix(t, ";") || strings.HasPrefix(t, "#") {
			continue
		}
		if strings.HasPrefix(t, "[") && strings.HasSuffix(t, "]") {
			flush()
			cur = &junkRule{id: strings.TrimSpace(t[1 : len(t)-1])}
			continue
		}
		if cur == nil {
			continue // 节外的 key=value：忽略（常见于手写时的遗漏）
		}
		eq := strings.IndexByte(t, '=')
		if eq < 0 {
			continue
		}
		key := strings.ToLower(strings.TrimSpace(t[:eq]))
		val := strings.TrimSpace(t[eq+1:])
		switch key {
		case "name":
			cur.name = val
		case "group":
			cur.group = val
		case "desc":
			cur.desc = val
		case "risk":
			if strings.EqualFold(val, "warn") {
				cur.risk = "warn"
			}
		case "renew", "heavy", "na", "defoff":
			v := parseRuleBool(val)
			switch key {
			case "renew":
				cur.renew = v
			case "heavy":
				cur.heavy = v
			case "na":
				cur.na = v
			case "defoff":
				cur.defoff = v
			}
		default:
			// 编号键：去掉结尾数字拿基础名
			base := key
			for len(base) > 0 && base[len(base)-1] >= '0' && base[len(base)-1] <= '9' {
				base = base[:len(base)-1]
			}
			p := expandRulePath(val, env)
			switch base {
			case "detect":
				cur.require = append(cur.require, p)
			case "dir":
				cur.dirs = append(cur.dirs, p)
			case "filekey":
				cur.globs = append(cur.globs, p)
			case "exclude":
				cur.excl = append(cur.excl, p)
			case "electron":
				// Electron/Chromium 系快捷写法：与 electronCacheGlobs 完全同一套展开
				cur.globs = append(cur.globs, electronCacheGlobs(p)...)
			case "gofind":
				// 数据根发现：值是 junk_findroots.go 里注册的 finder 名（不是路径，不做 %Var% 展开）
				cur.gofind = append(cur.gofind, strings.ToLower(val))
			case "find":
				// 数据化搜索声明：findN = 特征目录名[; …] | 子目录glob[; …]
				// （语法不对丢弃该条，写错只影响那一条 —— 容错口径与节级校验一致）
				if s, ok := parseFindSpec(val); ok {
					cur.finds = append(cur.finds, s)
				}
			}
		}
	}
	flush()
	return rules
}

// isRuleID 节名（ID）合法性：小写字母数字。与现有 51 个迁移 ID 命名一致。
func isRuleID(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c < 'a' || c > 'z' {
			if c < '0' || c > '9' {
				return false
			}
		}
	}
	return true
}

func parseRuleBool(v string) bool {
	switch strings.ToLower(v) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

// mergeJunkRules custom 规则并入内置：同 ID = 原位覆盖（用户改官方规则），
// 新 ID = 追加到同分组的最后一项之后（保持组内顺序可预期）。
func mergeJunkRules(builtin, custom []junkRule) []junkRule {
	out := make([]junkRule, len(builtin))
	copy(out, builtin)
	pos := map[string]int{} // id -> 在 out 里的下标
	for i, r := range out {
		pos[r.id] = i
	}
	for _, r := range custom {
		if i, ok := pos[r.id]; ok {
			out[i] = r // 覆盖：位置不变
			continue
		}
		// 追加：插到同分组最后一项之后；该组还没有任何项就放最后
		at := len(out)
		for j := len(out) - 1; j >= 0; j-- {
			if out[j].group == r.group {
				at = j + 1
				break
			}
		}
		out = append(out, junkRule{})
		copy(out[at+1:], out[at:])
		out[at] = r
		for id, p := range pos { // 平移后的下标全部 +1（在插入点之后）
			if p >= at {
				pos[id] = p + 1
			}
		}
		pos[r.id] = at
	}
	return out
}

// —— 进程内缓存：catalog / 扫描 / 清理都会调 junkItems()，规则只解析一次 ——

type junkRulesCache struct {
	items  []JunkItem
	skips  []string      // custom 规则新增项的根目录排除（兜底层防双算）
	built  map[string]bool // 内置规则 ID 集
}

var (
	junkRulesOnce sync.Once
	junkRulesData junkRulesCache
)

func loadJunkRules() junkRulesCache {
	builtin := parseJunkRules(junkRulesEmbedded)
	builtinIDs := map[string]bool{}
	for _, r := range builtin {
		builtinIDs[r.id] = true
	}
	// custom.ini：exe 旁边，与官方规则同格式。读不到 = 没有自定义规则，不是错误。
	var custom []junkRule
	if exe, err := os.Executable(); err == nil {
		if data, err := os.ReadFile(filepath.Join(filepath.Dir(exe), "custom.ini")); err == nil {
			custom = parseJunkRules(data)
		}
	}
	merged := mergeJunkRules(builtin, custom)

	items := make([]JunkItem, 0, len(merged))
	for _, r := range merged {
		items = append(items, JunkItem{
			ID: r.id, Group: r.group, Name: r.name, Desc: r.desc, Risk: r.risk,
			Renew: r.renew, Heavy: r.heavy, Na: r.na, DefOff: r.defoff,
			dirs: r.dirs, globs: r.globs, require: r.require, excl: r.excl,
			gofind: r.gofind, finds: r.finds,
		})
	}
	return junkRulesCache{items: items, skips: ruleCustomSkips(merged, builtinIDs), built: builtinIDs}
}

// ruleCustomSkips 从「custom 新增的规则」推导应用根目录排除模式。
// 内置 51 项的排除沿用 scanGenericAppCaches 里的既有手工名单（行为零变化）；
// 用户 custom.ini 新加的软件没有手工名单可依，这里自动补上：
// 取该规则各路径在 %LOCALAPPDATA% / %APPDATA% 下的第一段目录名（不含通配），
// 生成 `\xxx\` 子串模式 —— 与手工名单同一语义。
func ruleCustomSkips(rules []junkRule, builtinIDs map[string]bool) []string {
	L := os.Getenv("LOCALAPPDATA")
	R := os.Getenv("APPDATA")
	var out []string
	seen := map[string]bool{}
	add := func(seg string) {
		pat := `\` + seg + `\`
		if !seen[pat] {
			seen[pat] = true
			out = append(out, pat)
		}
	}
	for _, r := range rules {
		if builtinIDs[r.id] {
			continue // 内置项：既有手工名单已覆盖，不重复加
		}
		paths := make([]string, 0, len(r.dirs)+len(r.globs)+len(r.require))
		paths = append(paths, r.dirs...)
		paths = append(paths, r.globs...)
		paths = append(paths, r.require...)
		for _, p := range paths {
			for _, base := range []string{L, R} {
				if base == "" {
					continue
				}
				lb := strings.ToLower(base) + `\`
				lp := strings.ToLower(p)
				if !strings.HasPrefix(lp, lb) {
					continue
				}
				seg := lp[len(lb):]
				if i := strings.IndexByte(seg, '\\'); i >= 0 {
					seg = seg[:i]
				}
				if seg == "" || strings.ContainsAny(seg, "*?") {
					continue
				}
				add(seg)
			}
		}
	}
	return out
}

// junkRuleItems 全部规则项（内置 + custom 合并），顺序稳定。
func junkRuleItems() []JunkItem {
	junkRulesOnce.Do(func() { junkRulesData = loadJunkRules() })
	return junkRulesData.items
}

// junkRuleCustomSkips custom 新增项的兜底排除模式（传给 scanGenericAppCaches）。
func junkRuleCustomSkips() []string {
	junkRulesOnce.Do(func() { junkRulesData = loadJunkRules() })
	return junkRulesData.skips
}

// resetJunkRuleCache 测试用：强制下次重新解析（测试改环境后调用）。
func resetJunkRuleCache() {
	junkRulesOnce = sync.Once{}
	junkRulesData = junkRulesCache{}
}
