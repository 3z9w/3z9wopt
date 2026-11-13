package main

// ============================================================================
// 数据根发现引擎（2026-11-12，修「自定义安装路径的软件整项隐身」）
//
// 【为什么】规则引擎的路径全部按默认安装位置写（%Documents%\Tencent Files、
// %LocalAppData%\…）。用户把 QQ / 微信 / 浏览器的数据目录搬到其他盘（新版都支持），
// detect 判据全部落空 → 整项标「未安装」，一个字节都扫不到。本机实测：
//   D:\App Data\QQ\Tencent Files\…\nt_data\Emoji   224.7 MB / 1206 文件（QQ 表情包）
//   D:\App Data\We Chat\xwechat_files\<id>\cache   181.2 MB（微信缓存）
//   %LocalAppData%\Google\Chrome\Application\131.0.6778.205\Installer\chrome.7z 372.4 MB
// 火绒全部能扫到、我们全是 0 —— 差距就差在「只会按默认路径找」。
//
// 【做法】规则里声明 gofind = <finder 名>（Go 侧特型逻辑），或者 —— 2026-11-12
// 19:50 用户拍板「规则尽量一个文件」—— 用**数据化的搜索声明** find1 = … 直接写在
// ini 节里，不再需要动 Go 代码。两类并存：
//   find1 = Tencent Files | *\nt_qq\nt_data\Emoji;*\nt_qq\nt_data\log   ← 数据化（新，覆盖 99% 场景）
//   gofind = browserold                                                  ← Go 特型（比版本号留最新这类逻辑）
// qqdata / wxdata 两个硬编码 finder 已迁移成 find 声明（junkrules.ini 的 [qqnt]/[wechatcache]）。
//
// 【跨机器原则（用户 2026-11-12 拍板）】Win10/Win11 通用：
//   - 不写死盘符：候选根 = %Documents% + fixedDrives() 动态枚举的固定盘
//   - 不写死用户名：全部走环境变量与真实目录名匹配
//
// 【杀软红线（同日拍板）】发现 = 只 ReadDir 比对目录名，不打开任何文件内容；
//   清理仍走既有引擎（纯 os.Remove），无命令 / 无注册表写入 / 无联网，
//   与 CCleaner、火绒的「找目录然后删」行为同类，不新增任何可被执行的特征串。
// ============================================================================

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
)

// ruleFinders gofind 名 → glob 模式列表（返回的每条都交给 resolveGlob 展开，
// 支持通配段，也支持纯固定路径 —— resolveGlob 对固定段会 Stat 验证）。
// 只剩逻辑无法数据化的特型 finder；纯「按目录名全盘找 + 拼子目录」的一律走 find 声明。
var ruleFinders = map[string]func() []string{
	"browserold": browserOldGlobs,
}

// finderCache 进程内缓存：一次进程里数据根不会变（fixedDrives 只认固定盘），
// catalog / 扫描 / 清理 / 复核都会问，只跑一遍全盘搜索。
var (
	finderMu    sync.Mutex
	finderCache = map[string][]string{}
)

// finderGlobs 按名字取 finder 结果（缓存）。未知名字返回 nil，规则里写错名字
// 只影响那一条（与 parseJunkRules 的容错口径一致）。
func finderGlobs(name string) []string {
	finderMu.Lock()
	defer finderMu.Unlock()
	if got, ok := finderCache[name]; ok {
		return got
	}
	f, ok := ruleFinders[name]
	if !ok {
		finderCache[name] = nil
		return nil
	}
	got := f()
	finderCache[name] = got
	return got
}

// hasFinderResults 任意一个 finder 名有结果（catalog 阶段判「装没装」用）。
func hasFinderResults(names []string) bool {
	for _, n := range names {
		if len(finderGlobs(n)) > 0 {
			return true
		}
	}
	return false
}

// resetFinderCache 测试用：换环境后强制重新发现。
func resetFinderCache() {
	finderMu.Lock()
	finderCache = map[string][]string{}
	finderMu.Unlock()
}

// ---------------------------------------------------------------------------
// 全盘特征目录搜索
// ---------------------------------------------------------------------------

// findSkipDirs 这些目录之下不会有用户自装软件的数据目录，剪枝（小写比对）。
// 剪枝只影响速度，不影响正确性：漏剪只是多扫几个目录。
var findSkipDirs = map[string]bool{
	"windows": true, "program files": true, "program files (x86)": true,
	"programdata": true, "$recycle.bin": true, "system volume information": true,
	"perflogs": true, "appdata": true, "appdata\\local": true,
}

// findNamedRoots 在候选根下按目录名找数据根（BFS，深度受限）。
//
// 候选根固定为：%Documents%（默认位置）+ 每个固定盘的根（自定义位置）。
// 深度 3 的依据：真实世界自定义路径的分布是
//   D:\Tencent Files（1 层）/ D:\App Data\QQ\Tencent Files（3 层）/
//   C:\Users\<u>\Documents\Tencent Files（Documents 下 1 层）
// 更深的（D:\软件\聊天\…\Tencent Files）属于极端个例，不值得为它扫全盘 4 层。
// 参数化 bases 是为了单测能注入临时目录（生产入口 qqDataGlobs/wechatDataGlobs）。
func findNamedRoots(bases []string, names []string, maxDepth int) []string {
	want := map[string]bool{}
	for _, n := range names {
		want[strings.ToLower(n)] = true
	}
	var out []string
	seen := map[string]bool{}
	type job struct {
		dir   string
		depth int
	}
	queue := make([]job, 0, 64)
	for _, b := range bases {
		if b == "" {
			continue
		}
		if _, err := os.Stat(b); err != nil {
			continue // 候选根本身不存在：跳过（不是错误）
		}
		queue = append(queue, job{b, 0})
	}
	for len(queue) > 0 {
		j := queue[0]
		queue = queue[1:]
		entries, err := os.ReadDir(j.dir)
		if err != nil {
			continue // 读不动就跳过（权限/占用）：与扫描引擎同一容错口径
		}
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			nm := strings.ToLower(e.Name())
			if findSkipDirs[nm] {
				continue
			}
			p := filepath.Join(j.dir, e.Name())
			if isReparsePoint(p) {
				continue // junction/符号链接：绝不跟着走，防环与跨盘重复
			}
			if want[nm] && !seen[strings.ToLower(p)] {
				seen[strings.ToLower(p)] = true
				out = append(out, p)
				// 命中即不再下钻（数据根内部结构交给 glob 规则）
				continue
			}
			if j.depth+1 < maxDepth {
				queue = append(queue, job{p, j.depth + 1})
			}
		}
	}
	sort.Strings(out)
	return out
}

// finderBases 候选根：Documents + 所有固定盘根。测试不直接用它（参数化注入）。
func finderBases() []string {
	bases := append([]string{}, fixedDrives()...)
	if U := os.Getenv("USERPROFILE"); U != "" {
		bases = append(bases, filepath.Join(U, "Documents"))
	}
	return bases
}

// hasFindSpecResults 任意一条 find 声明搜得到数据根（catalog 阶段判「装没装」用）。
func hasFindSpecResults(specs []findSpec) bool {
	for _, s := range specs {
		if len(findSpecGlobs(s)) > 0 {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// find 声明：数据化的数据根搜索（「规则一个文件」的核心）
// ---------------------------------------------------------------------------

// findSpec ini 里的搜索声明，语法（分号分隔多项，竖线分隔两段）：
//
//	find1 = Tencent Files | *\nt_qq\nt_data\Emoji;*\nt_qq\nt_data\log
//	        └─ 特征目录名（多个用 ; ）─┘└──── 命中后拼接的 glob（多个用 ; ）────┘
//
// 语义与原 qqdata/wxdata 硬编码 finder 完全一致：候选根 = %Documents% + 各固定盘根，
// BFS 深度 3 按目录名找特征目录（剪系统目录 / reparse，只 ReadDir 不开文件内容），
// 命中后把每条 sub 拼在数据根下交给 resolveGlob。安全边界原样：无命令 / 无注册表 /
// 无联网，子目录写什么就清什么 —— 宁少清不误删由写规则的人把握（聊天记录别写进来）。
type findSpec struct {
	names []string // 特征目录名（大小写不敏感）
	subs  []string // 命中后拼接的 glob 段
}

// parseFindSpec 解析一条 find 值。格式不对返回 ok=false（调用方丢弃该条，
// 与「写错只影响那一条」的容错口径一致）。
func parseFindSpec(val string) (findSpec, bool) {
	i := strings.IndexByte(val, '|')
	if i < 0 {
		return findSpec{}, false
	}
	var s findSpec
	for _, n := range strings.Split(val[:i], ";") {
		if n = strings.TrimSpace(n); n != "" {
			s.names = append(s.names, n)
		}
	}
	for _, sub := range strings.Split(val[i+1:], ";") {
		if sub = strings.TrimSpace(sub); sub != "" {
			s.subs = append(s.subs, sub)
		}
	}
	if len(s.names) == 0 || len(s.subs) == 0 {
		return findSpec{}, false
	}
	return s, true
}

// findSpecGlobs 展开（进程内缓存，键 = 声明原文归一化）。
func findSpecGlobs(spec findSpec) []string {
	key := "spec:" + strings.Join(spec.names, ";") + "|" + strings.Join(spec.subs, ";")
	finderMu.Lock()
	if got, ok := finderCache[key]; ok {
		finderMu.Unlock()
		return got
	}
	finderMu.Unlock()
	got := specGlobsFrom(finderBases(), spec)
	finderMu.Lock()
	finderCache[key] = got
	finderMu.Unlock()
	return got
}

// specGlobsFrom 参数化版本（单测注入临时候选根）。
func specGlobsFrom(bases []string, spec findSpec) []string {
	roots := findNamedRoots(bases, spec.names, 3)
	var globs []string
	for _, r := range roots {
		for _, sub := range spec.subs {
			globs = append(globs, filepath.Join(r, sub))
		}
	}
	return globs
}

// ---------------------------------------------------------------------------
// Chrome / Edge：旧版本目录残留
// ---------------------------------------------------------------------------

// verDirRe 版本号目录名（Chrome/Edge Application 下的四段式版本）。
var verDirRe = regexp.MustCompile(`^\d+(\.\d+){3}$`)

// browserAppDirs Chrome/Edge 的 Application 候选位置（环境变量展开，不写死盘符）。
func browserAppDirs() []string {
	var out []string
	add := func(p string) {
		if p != "" {
			out = append(out, p)
		}
	}
	for _, v := range []string{
		os.Getenv("LOCALAPPDATA"),
		os.Getenv("ProgramFiles"),
		os.Getenv("ProgramFiles(x86)"),
	} {
		if v == "" {
			continue
		}
		add(filepath.Join(v, `Google`, `Chrome`, `Application`))
		add(filepath.Join(v, `Microsoft`, `Edge`, `Application`))
	}
	return out
}

// browserOldGlobs 旧版本目录残留：同一 Application 下并存 ≥2 个版本目录时，
// 保留版本号最大的（Chrome/Edge 更新机制只会往高走），其余按旧版残留返回。
//
// 目标是 `<旧版本目录>\*`（目录下一级内容）而非目录本身 —— 清理引擎对
// dir 目标的语义是「清空内容、保留壳」，用 \* 展开成一级子项后整棵可删，
// 只留一个空壳目录，与浏览器自身清理后的状态一致。
//
// 便携增强保护：Application 下出现 chrome++.ini（Chrome++ 等绿色增强套件的特征，
// 本机就有）→ 该位置的版本目录一概不动 —— 用户手动配置的便携浏览器，
// 单版本也可能是「正在用的主程序」而非残留，宁可漏清不误删。
func browserOldGlobs() []string {
	return browserOldGlobsFrom(browserAppDirs())
}

// browserOldGlobsFrom 参数化版本（单测注入临时目录）。
func browserOldGlobsFrom(appDirs []string) []string {
	var out []string
	for _, app := range appDirs {
		entries, err := os.ReadDir(app)
		if err != nil {
			continue
		}
		if _, err := os.Stat(filepath.Join(app, "chrome++.ini")); err == nil {
			continue // 便携增强套件特征：整个位置跳过
		}
		var vers []string
		for _, e := range entries {
			if !e.IsDir() || !verDirRe.MatchString(e.Name()) {
				continue
			}
			vers = append(vers, e.Name())
		}
		if len(vers) < 2 {
			continue // 只有一个版本：可能是正在用的主程序，不动
		}
		sort.Slice(vers, func(i, j int) bool {
			return cmpVerSegs(vers[i], vers[j]) < 0
		})
		for _, v := range vers[:len(vers)-1] { // 保留最高版本
			out = append(out, filepath.Join(app, v, `*`))
		}
	}
	return out
}

// cmpVerSegs 四段版本号逐段数字比较（"10.0.6778.205" → [10 0 6778 205]）。
// 非法段按 0 处理；返回 -1/0/1。
func cmpVerSegs(a, b string) int {
	as := strings.Split(a, ".")
	bs := strings.Split(b, ".")
	n := len(as)
	if len(bs) > n {
		n = len(bs)
	}
	for i := 0; i < n; i++ {
		av, bv := 0, 0
		if i < len(as) {
			av = atoiSafe(as[i])
		}
		if i < len(bs) {
			bv = atoiSafe(bs[i])
		}
		if av != bv {
			if av < bv {
				return -1
			}
			return 1
		}
	}
	return 0
}

func atoiSafe(s string) int {
	n := 0
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c < '0' || c > '9' {
			return 0
		}
		n = n*10 + int(c-'0')
	}
	return n
}
