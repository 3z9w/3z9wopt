package main

// ============================================================================
// 3z9wOpt v1.1 · 垃圾清理项清单（junk catalog）
// 来源：v1.0（D:\3z9wopt(v2)\3z9wOptV2\app.go junkItems）真机实测版，复制迁移。
//
// v1.1 相对 v1.0 的改动（2026-10-08 用户拍板，1.0 源文件未做任何修改）：
//   A. 重复项合并：确认清的是同一目标的项已合为一项（wucache+wucachedeep+delivery、
//      thumb+thumbdeep、rootjunk+deepjunk）。合计体积一律走 TotalUnique() 按路径去重，
//      父目录覆盖子路径，同一个文件永不计两次。
//   B. driverres 转激进：不再只写死 C 盘，遍历所有固定盘根目录，命中驱动解压目录
//      整个清空（只留系统正在用的驱动，DriverStore / INF / System32\drivers 不碰）。
//   C. dotnetgac 风险等级维持 v1.0 原样（warn），不升不降。
//   D. 盘符不再写死：iis / msocache / driverres 全部遍历所有固定盘。
//   E. 扫描提速：ResolveAll / ScanAll 全并发（item 级 + 目录级工作池）。
// ============================================================================

import (
	"context"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"unsafe"
)

type JunkItem struct {
	ID    string `json:"id"`
	Seq   int    `json:"seq"`
	Group string `json:"group"`
	Name  string `json:"name"`
	Desc  string `json:"desc"`
	Risk  string `json:"risk"` // ""=安全  "warn"=谨慎
	Cmd   string `json:"cmd"`  // 命令型: recycle/dismreset/reg:xxx，目录型为空

	// Impact：这一项清理时属于哪一类动作。**标记由后端给，前端不许按 ID 写死。**
	// ⚠️ 只有 "explorer" 会在动手前弹窗确认（屏幕会短暂黑屏，必须提前说一声）；
	//    service / slow 只是"描述这一项会做什么"，**不弹窗、不打扰用户**
	//    —— 2026-10-10 用户定：除资源管理器重启之外，清理一律不需要提示。
	//   "explorer" = 会重启资源管理器（桌面/任务栏闪一下，已打开的文件夹窗口会关掉）
	//   "service"  = 会暂停系统服务（Windows 更新相关）
	//   "slow"     = 这一项要跑好几分钟（不是卡住，别中途断电/关机）
	// 空缺 = 正常删文件，用户无感。
	Impact string `json:"impact,omitempty"`

	// Async：这一项会被放到**后台**跑（主流程不等它，见 junk_clean.go 的 asyncCmds）。
	// 分钟级的项才这样 —— 用户不该干等，其它项清完就出结果、让他先走。
	// 前端据此在动手前提示一句"它要跑几分钟、会在后台自己跑完"，并允许就地取消。
	// ⚠️ 值由 GetJunkCatalog 按 asyncCmds 派生，不要在清单里手写（免得两处对不上）。
	Async bool `json:"async,omitempty"`

	Bytes int64  `json:"bytes"`
	Files int    `json:"files"`
	Empty bool   `json:"empty"` // 2026-09-20 自适应：require 全不存在 → 前端隐藏该行（未安装的软件不占位）

	ScanMs int64 `json:"scanMs"` // v1.1：该项本次扫描耗时（毫秒），用于找出拖慢整体的项

	// —— v1.1.0 新增：给前端算账用的两个字段 ——
	// Paths：本次扫描解析到的「目标路径 -> 体积」清单。tsize 是小写字段不进 JSON，
	//        前端拿不到路径就只能把每项 Bytes 简单相加，deepjunk 与各日志项重叠的部分会被算两遍。
	//        下发之后前端可按「父目录覆盖子路径」自己算，界面数字与后端 TotalUnique 完全同口径。
	// Heavy：这项动的是「你自己的文件 / 整包」（微信QQ接收的文件、本地大模型权重、Office 安装源、
	//        驱动解压包、全盘零散垃圾…）。默认同样勾选、同样参与清理，只是界面把它单独算一行。
	//        口径分层是 2026-10-09 用户拍板：名单不删，但「可重建缓存」与「你的文件/整包」分开显示。
	Paths []PathBytes `json:"paths,omitempty"`
	Heavy bool        `json:"heavy,omitempty"`

	// Renew：这一项「边用边长」—— 只要对应的程序/系统还在跑，删掉之后立刻或很快就会被写回来
	// （浏览器缓存、应用缓存、临时文件、日志、缩略图、字体缓存、全盘零散文件…）。
	// 2026-10-13 用户拍板口径：这类项**照旧勾选、照旧清理**，只是不计入「这次真正能清掉」的主数字，
	// 单独列一行并给出「关掉程序再清一次」的引导 —— 否则用户清完再扫看到一堆，会以为软件没清干净。
	// ⚠️ 这是显示口径，不是删除范围：Renew 为 true 不代表少删一个字节。
	Renew bool `json:"renew,omitempty"`

	// Cleanable / Locked* / Denied*：扫描阶段**实测**出来的结论 —— 这些目标现在到底删不删得动。
	// 判据不是猜的：每个目标都按独占方式试开一次（Windows 自己回答，见 junk_deletable.go）。
	//   Cleanable   = 现在没被占着、能删掉的部分
	//   Locked*     = 正被程序占着（关掉那个程序 / 重启就能删，是可解的）
	//   Denied*     = 系统保护（TrustedInstaller 那种，任何清理软件都删不掉）
	// 2026-10-15 用户拍板：用这套「当下实测」顶替原来靠历史记忆（blocked.json / 压制名单）
	//   决定"这一项出不出来"的写法 —— 扫描结果一律照常列出、照常可勾，删不删得动由用户看数字决定。
	// ⚠️ 与 Renew/Heavy 一样只是**结算口径**，不影响删除范围：勾了就照删，不看这些字段。
	Cleanable   int64 `json:"cleanable,omitempty"`
	LockedBytes int64 `json:"lockedBytes,omitempty"`
	LockedFiles int   `json:"lockedFiles,omitempty"`
	DeniedBytes int64 `json:"deniedBytes,omitempty"`
	DeniedFiles int   `json:"deniedFiles,omitempty"`

	// Unread：本项里有 N 个目录**本次没读到**（被占用 / 权限不足，见 walkSizeFast）。
	// 这部分体积一个字节都没进 Bytes —— 数字是**偏小**的，不是准确值。
	// 2026-11-12 修「数字跳变」：同一目录清完 12 MB → 重扫 663 MB，就是读不动的
	// 子树被静默按 0 计账、锁一放开又冒出来造成的。以前不留痕，用户只会觉得数字瞎跳；
	// 现在把「没量到」如实上报，宁可见小数+提示，不假装量准了。
	// ⚠️ 同样只是显示口径：不影响扫描/删除范围。
	Unread int `json:"unread,omitempty"`

	// Na：默认**不勾选**，要用户自己手动勾上才清（2026-10-14 加）。
	// 判据：**删了不可逆，或者那些本来就是用户自己的东西** —— 一键清理不该替用户做这种决定。
	//   Cookies / 浏览历史（清空后所有网站要重新登录、历史找不回来）、
	//   QQ 与微信接收的文件（别人发来的真实文件）、本地大模型权重（数 GB，删了要重下）、
	//   Office 安装源 / Installer 补丁缓存（删了无法修复或卸载更新）、
	//   全盘零散垃圾（.bak/.old 很可能就是你自己的备份）、驱动解压残留、损坏应用包、
	//   WinSxS /ResetBase（清完连已装的更新都无法卸载）。
	// 这些项照旧扫描、照旧显示体积，用户想清随时能勾 —— 只是不替他默认勾上。
	// 🔴 与 Heavy 是两回事：Heavy 只决定"显示时算哪一行"，Na 才决定默认勾不勾。
	Na bool `json:"na,omitempty"`

	// DefOff：**扫描完成后默认不勾**（2026-11-12 加，只给 recycleother 用）。
	// 与 Na 的区别要说清，别混：
	//   Na      = 「不替用户做这个决定」，理想语义是默认不勾（但前端目前只读 empty，实际没生效，
	//             见 junk_na_real_test.go 的注释 —— 那是历史遗留，本次不动它）；
	//   DefOff  = 「扫描时照扫（选分类页两项都勾着），扫完把这一项**取消勾选**」，
	//             但仍然留在结果页上、数字照给，用户想清随时勾上。
	// 为什么要它：其他账户的回收站默认清 = 替用户删别人的东西；默认不清又必须让用户看得见
	// 有多大体量。所以「扫到但不选」才是准确表达。
	// ⚠️ 只影响「扫描后的默认勾选」，不影响扫描范围（选分类页仍然是勾着的）。
	DefOff bool `json:"defOff,omitempty"`

	dirs    []string // 固定目录（清空内容，保留目录本身）
	globs   []string // 动态枚举模式，支持 * 单段通配、** 任意深度、文件名通配
	excl    []string // 解析结果排除（路径包含任一子串则丢弃）
	files   []string // 固定单文件
	require []string // 自适应门槛：任一路径存在才扫描/显示（「按已装软件出列表」）
	gofind  []string   // 数据根发现（junk_findroots.go）：软件装在自定义盘/路径时的兜底，产出也是 glob
	finds   []findSpec // 数据化搜索声明（ini 的 findN 键）：同一套全盘搜索，但不用写 Go 代码

	// —— v1.1 新增：全选去重 ——
	targets []string         // 本次扫描实际解析到的目标路径（去重合计用）
	tsize   map[string]int64 // 路径 -> 体积（目录为其递归总体积）
	tfiles  map[string]int   // 路径 -> 文件数（与 tsize 一一对应；剔除「清不掉」的路径时靠它精确重算 Files，不许按比例估）
	dup     string           // 重叠组 key：同一 key 的项合计时只取最大者（v1.1 目前仅 winsxs 组使用）
}

// PathBytes 一条目标路径及其体积（目录为递归总体积）。
type PathBytes struct {
	Path  string `json:"p"`
	Bytes int64  `json:"b"`
}

// syncPaths 把 tsize 摊平成可序列化的 Paths（按路径排序，输出稳定）。
// 每次 scanOne 结束前调用；注册表/快捷方式这类没有路径的项自然得到 nil。
func (it *JunkItem) syncPaths() {
	if len(it.tsize) == 0 {
		it.Paths = nil
		return
	}
	ps := make([]PathBytes, 0, len(it.tsize))
	for p, s := range it.tsize {
		ps = append(ps, PathBytes{Path: p, Bytes: s})
	}
	sort.Slice(ps, func(i, j int) bool { return ps[i].Path < ps[j].Path })
	it.Paths = ps
}

// dupGroup 返回该项所属的重叠组（空串 = 不与他人重叠）
func (it *JunkItem) dupGroup() string { return it.dup }

// electronCacheCacheNames Chromium / Electron 系的「可重建缓存目录」白名单（小写比对）。
//
// 2026-10-13 大幅补齐。原来的 3 个名字（Cache / Code Cache / GPUCache）只覆盖到
// Chromium 117 及更早：118 之后渲染管线换成 Dawn/WebGPU，着色器缓存搬到了
// GrShaderCache / ShaderCache / GraphiteDawnCache / DawnGraphiteCache / DawnWebGPUCache，
// 老的 GPUCache 基本写死在 1.57 MB 不再增长（实测本机每台机器都卡在同一个数）。
// 不补 = 所有 Electron 应用（豆包 / 钉钉 / Kimi / Cursor / TRAE / 向日葵…）的着色器
// 缓存一个都扫不到，本机漏 7.40 MB（仅豆包一家）。
var electronCacheCacheNames = []string{
	// 网页 / 脚本 / GPU 缓存（老版 Chromium 与仍在用的）
	"cache", "code cache", "gpucache",
	// 着色器缓存（Chromium 118+ 的 Dawn 管线）
	"shadercache", "grshadercache",
	"graphitedawncache", "dawngraphitecache", "dawnwebgpucache",
	// Service Worker 离线资源（键值存储，自动重建）
	"cachestorage", "scriptcache",
	// 组件更新包 / 浏览器指标（删后用到时自动重新下载）
	"component_crx_cache", "extensions_crx_cache", "browsermetrics",
	// Firefox 内核（Gecko）应用的资源包缓存
	"gecko_cache",
}

// electronCacheNever 名字里带"cache"但**是数据不是缓存**的目录，黑名单优先于白名单。
// 实测本机这几个装了几个 GB，清掉就是删用户数据：WebStorage / Local Storage /
// IndexedDB 是站点的离线数据，Partitions 是 QQ / 微信的多账号资料，logs 是运行日志。
var electronCacheNever = map[string]bool{
	"webstorage": true, "local storage": true, "indexeddb": true,
	"session storage": true, "storage": true, "partitions": true,
	"local extension settings": true, "databases": true,
}

// electronCacheGlobs 给一批 base 生成缓存 glob（** 与 * 都给，因为 Electron 的
// User Data\Default\Cache 是第三层，老的 *\Cache 只覆盖得到第二层）。
func electronCacheGlobs(bases ...string) []string {
	var out []string
	for _, b := range bases {
		out = append(out,
			filepath.Join(b, `**\Cache`),
			filepath.Join(b, `**\Code Cache`),
			filepath.Join(b, `**\Service Worker\CacheStorage`),
			filepath.Join(b, `Cache`),
			filepath.Join(b, `Code Cache`),
		)
	}
	return out
}

func junkItems() []JunkItem {
	L := os.Getenv("LOCALAPPDATA")
	R := os.Getenv("APPDATA")
	PD := os.Getenv("ProgramData")
	W := os.Getenv("windir")
	/* 2026-11-12 规则化后 LL / PF / PFX / docs 不再被静态项引用
	  （用到它们的 51 项全部迁入 rules/junkrules.ini，变量名换成 %LocalLow% /
	   %ProgramFiles% / %ProgramFiles86% / %Documents%）。 */

	items := []JunkItem{
		// ===== 临时文件 =====
		{ID: "usertemp", Group: "临时文件", Name: "用户临时文件", Desc: "应用程序运行时产生的临时数据",
			// 第二个路径独立于 %TEMP%：用户改过 TEMP 变量时，脚本原版也会清这里
			dirs: []string{os.TempDir(), filepath.Join(L, "Temp")}},
		{ID: "systemp", Group: "临时文件", Name: "系统临时文件", Desc: "Windows 系统目录下的临时文件",
			dirs: []string{filepath.Join(W, "Temp")}},
		{ID: "recent", Group: "临时文件", Name: "最近使用记录", Desc: "最近打开文件列表与任务栏跳转列表",
			dirs: []string{filepath.Join(R, `Microsoft\Windows\Recent`)}},
		/* Group 独立成"回收站"（2026-10-14 用户指出）：原来挂在"临时文件"下，
		   界面上就成了"临时文件 1.0 GB"，而其中 1012 MB 根本不是临时文件、是回收站 ——
		   用户看着会以为系统临时目录里堆了 1 GB，实际只有几十 MB。这是误导，不是显示技巧。 */
		{ID: "recycle", Group: "回收站", Name: "回收站", Desc: "已删除文件的存放区，清空后不可恢复", Risk: "warn", Cmd: "recycle"},
		/* 2026-11-12 用户拍板：其他账户的回收站**既不能装作不存在，也不能替用户默认删**。
		   做法是单独成一项、照常扫描、数字照给，但扫描完**默认不勾** —— 要清得用户自己勾。
		   扫到多少与默认选多少都摆在界面上（「扫到 X · 已选 Y」），决定权在用户手里。
		   范围护栏见 junk_clean.go 的 orphanRecycleDirs / clearRecycleSIDDir：
		   只认「标准用户 SID + 本机账户列表里查无此人 + 非当前账户 + 非链接目录」。
		   Desc 会在扫描后被 recycleOtherMeasure 换成带真实体积的版本。 */
		{ID: "recycleother", Group: "回收站", Name: "回收站（其他账户残留）",
			Desc: "其他账户或已删除账户留下的残留，本机已没有对应账户；勾选后永久删除、无法恢复",
			Risk: "warn", Cmd: "recycleother", DefOff: true},
		/* cleanmgr（系统磁盘清理）**整项已删除**（2026-10-14 用户拍板）：
		   它清的东西我们已覆盖八成（临时文件/回收站/更新缓存/缩略图/着色器/错误报告/传递优化/Windows.old），
		   剩下两成是旧驱动包与未使用语言包 —— 那两块在 DriverStore / WinSxS，
		   属于保护清单禁区（当年删 GAC 把 .NET 程序全搞崩就是这类事故），不该自己动手删。
		   而它本身要跑几分钟、还会弹出系统自己的窗口，体积无法预估、本就不计入合计。
		   Async 那套后台执行机制保留（asyncCmds 现在是空表），以后有分钟级项可以再加。 */

		// ===== 系统缓存 =====
		// v1.1 合并：wucache + wucachedeep + delivery 三项清的是同一批目录
		// （SoftwareDistribution\Download 与传递优化缓存），v1.0 全选会双算，此处合一，只算一次体积
		{ID: "wucache", Group: "系统缓存", Name: "Windows 更新与传递优化缓存（深度清理）", Desc: "先停 wuauserv / cryptSvc / bits / msiserver 再清，能清掉普通方式被占用跳过的文件（清完不重启服务，与脚本一致）", Risk: "warn", Cmd: "wucachedeep", Impact: "service",
		dirs: []string{
				filepath.Join(W, `SoftwareDistribution\Download`),
				filepath.Join(W, `ServiceProfiles\NetworkService\AppData\Local\Microsoft\Windows\DeliveryOptimization\Cache`),
			}},
		{ID: "prefetch", Group: "系统缓存", Name: "Prefetch 预读取", Desc: "程序启动预读取数据，清理后首次启动稍慢", Risk: "warn",
			dirs: []string{filepath.Join(W, "Prefetch")}},
		{ID: "shader", Group: "系统缓存", Name: "DirectX 着色器缓存", Desc: "游戏编译的着色器缓存，可安全重建",
			dirs: []string{filepath.Join(L, "D3DSCache")}},
		/* v1.1 合并：thumb 与 thumbdeep 清的是同一批 iconcache 与 thumbcache 的 db 文件。
		   🔴 2026-10-14 改：**不再结束资源管理器**（Cmd 清空 → 走普通清理链）。
		   实测本机 20 个文件里 9 个被 explorer 独占（11.7 / 13.7 MB），为这十几 MB
		   去黑一次屏、还把用户已打开的文件夹窗口全关掉，不值当 —— 用户实测后要求。
		   现在：删得动的当场删，删不动的由 MoveFileEx 排队到下次开机删，
		   全程不弹窗、不黑屏、不结束任何进程。 */
		{ID: "thumb", Group: "系统缓存", Name: "缩略图与图标缓存", Desc: "资源管理器存放的图标与缩略图缓存。正在被占用的文件不会强行删除（屏幕不会黑），它们会排到下次开机时自动清除", Risk: "warn",
		globs: []string{
				filepath.Join(L, `Microsoft\Windows\Explorer\iconcache_*.db`),
				filepath.Join(L, `Microsoft\Windows\Explorer\thumbcache_*.db`),
			},
			files: []string{filepath.Join(L, "IconCache.db")}},
		{ID: "iconrefresh", Group: "系统缓存", Name: "刷新系统图标缓存", Desc: "调用 ie4uinit 刷新图标缓存索引，不释放空间，让上面的清理立即生效", Cmd: "iconrefresh"},
		{ID: "fontcache", Group: "系统缓存", Name: "字体缓存", Desc: "系统字体缓存，可安全重建", Risk: "warn",
			dirs:  []string{filepath.Join(W, `ServiceProfiles\LocalService\AppData\Local\FontCache`)},
			files: []string{filepath.Join(W, `System32\FNTCACHE.DAT`)}},
		{ID: "searchidx", Group: "系统缓存", Name: "Windows 搜索索引", Desc: "搜索索引数据库，清理后自动重建（重建期间搜索变慢）", Risk: "warn",
			dirs: []string{filepath.Join(PD, `Microsoft\Search\Data`)}},
		// v1.1：winsxstmp 与 dismreset 指向同一处（组件清理会覆盖 WinSxS\Temp），
		// 标同一 dup 组，合计时只算一次（取较大者），但两项仍分开显示——清法不同、力度不同
		{ID: "winsxstmp", Group: "系统缓存", Name: "WinSxS 临时文件", Desc: "组件存储的临时工作文件（直删，不含组件清理）", dup: "winsxs",
			dirs: []string{filepath.Join(W, `WinSxS\Temp`)}},
		{ID: "dismreset", Group: "深度清理", Name: "WinSxS 组件清理", Desc: "脚本同款参数（含 /ResetBase）：释放更多，但清完之后连已装的旧更新也无法卸载；耗时数分钟起。与上方同一处，合计只算一次", Risk: "warn", Cmd: "dismreset", dup: "winsxs", Impact: "slow"},
		// ⛔ 原「.NET 程序集缓存 (GAC)」项已于 2026-10-13 整项移除，不要再加回来。
		// 事故经过：该项清的是 C:\Windows\Microsoft.NET\assembly\GAC_32 / GAC_64 / GAC_MSIL，
		// 但 GAC **不是缓存**，是 .NET Framework 强命名程序集的安装区，删掉不会自动重建。
		// 实测后果：所有依赖 GAC 的 .NET Framework 程序启动即抛 0xE0434352 静默崩溃（双击无反应），
		// 用户报的「DirectX Repair 增强版打不开」就是这么来的 —— 补回 GAC 后才恢复正常。
		// 用户 2026-10-13 原话：「如果清理之后影响机器的正常使用，那是不可饶恕的！」
		// 防御：junk_clean.go 的 protectedRuntimePaths 已把 GAC 三目录硬拦，
		//      就算以后有人再加回来也删不掉（扫描仍会显示，但清理阶段跳过）。

		// ===== 日志转储 =====
		{ID: "wer", Group: "日志与注册表", Name: "错误报告 (WER)", Desc: "Windows 错误报告队列与存档",
			dirs: []string{filepath.Join(PD, `Microsoft\Windows\WER`), filepath.Join(L, `Microsoft\Windows\WER`)}},
		{ID: "dumps", Group: "日志与注册表", Name: "崩溃转储文件", Desc: "程序/系统崩溃时生成的 .dmp 文件",
			dirs:  []string{filepath.Join(W, "Minidump"), filepath.Join(L, "CrashDumps")},
			globs: []string{filepath.Join(W, "*.dmp")}},
		{ID: "syslogs", Group: "日志与注册表", Name: "系统日志文件", Desc: "CBS / DISM 等 Windows 日志，被占用自动跳过",
			dirs: []string{filepath.Join(W, "Logs")}},
		{ID: "svclogs", Group: "日志与注册表", Name: "系统服务日志", Desc: "System32\\LogFiles 下各服务运行日志，被占用自动跳过",
			dirs: []string{filepath.Join(W, `System32\LogFiles`)}},
		// v1.1：不再写死 C 盘，所有固定盘都扫（IIS 装在 D/E 盘也能扫到）
		{ID: "iis", Group: "日志与注册表", Name: "IIS 日志", Desc: "IIS 网站访问日志（所有磁盘，未装 IIS 则为空）",
			dirs: allDrivesSub(`inetpub\logs`)},

		/* ===== 浏览器缓存 / 应用缓存 / AI 应用缓存（51 项）已迁入规则文件 =====
		   2026-11-12 规则引擎化（用户拍板「一次到位」）：
		   - 内置规则：rules/junkrules.ini（go:embed），ID/分组/文案/路径与迁移前逐项一致；
		   - 引擎：junkrules.go —— 只认「路径 + 通配符」，无命令执行 / 无注册表删除 / 无网络；
		   - 自定义：exe 旁 custom.ini 同格式追加（新软件）或覆盖（同 ID），免编译立即生效；
		   - 兜底：appcacheauto 通用自动发现照旧；custom 新增项的根目录自动排除、不双算。
		   组内顺序与迁移前一致（三类在 items 尾部整体追加，各分组内部相对顺序不变）。 */
		// ===== 注册表垃圾（2026-09-20 只报确定失效的项，宁漏勿错） =====
		{ID: "regmui", Group: "日志与注册表", Name: "MuiCache 失效条目", Desc: "资源管理器的应用名称缓存，指向的程序已被删除；系统会自动重建，可放心清",
			Cmd: "reg:mui"},
		{ID: "regapppath", Group: "日志与注册表", Name: "App Paths 死链", Desc: "注册表登记的程序快速启动路径，目标 exe 已不存在",
			Cmd: "reg:apppath"},
		{ID: "regshared", Group: "日志与注册表", Name: "SharedDLLs 死链", Desc: "共享 DLL 引用计数里指向已不存在文件的条目",
			Cmd: "reg:shared"},
		{ID: "reguninst", Group: "日志与注册表", Name: "已卸载软件残留键", Desc: "卸载器本体已丢失的软件遗留的卸载信息条目（MSI 托管与系统组件不碰）", Risk: "warn",
			Cmd: "reg:uninst"},
		{ID: "regclsid", Group: "日志与注册表", Name: "无效 COM 组件注册", Desc: "指向的 DLL/EXE 已不存在的组件注册；可移动/网络盘上的目标一律不判死", Risk: "warn",
			Cmd: "reg:clsid"},

		// ===== 快捷方式 =====
		/* Group 原为「快捷方式」（2026-10-14 并入「临时文件」）：全组只有这一项，
		   而前端大类早把它并进「临时文件」显示了（CATS g:['临时文件']），后端单独一组名不副实。 */
		{ID: "badlnk", Group: "临时文件", Name: "失效快捷方式", Desc: "桌面 / 开始菜单 / 快速启动里目标已不存在的 .lnk；指向可移动与网络设备的不算",
			Cmd: "badlnk"},

		// ===== 使用痕迹（系统/浏览器/Office 三路，谨慎项默认不勾） =====
		{ID: "regrunmru", Group: "使用痕迹", Name: "运行框历史", Desc: "Win+R 运行框里输入过的命令记录（痕迹类默认不勾，需要再手动勾选）", Risk: "warn",
			Cmd: "reg:runmru"},
		{ID: "regtyped", Group: "使用痕迹", Name: "地址栏历史", Desc: "资源管理器地址栏输入过的路径记录（痕迹类默认不勾）", Risk: "warn",
			Cmd: "reg:typedpaths"},
		{ID: "regofficemru", Group: "使用痕迹", Name: "Office 最近文档", Desc: "Word / Excel / PPT 等的最近打开文件列表（痕迹类默认不勾）", Risk: "warn",
			Cmd: "reg:officemru"},
		{ID: "regopensave", Group: "使用痕迹", Name: "打开/保存对话框记录", Desc: "各软件打开/保存文件对话框留下的最近位置记录（PIDL）", Risk: "warn",
			Cmd: "reg:opensave"},
		{ID: "browserhist", Group: "使用痕迹", Name: "浏览器历史记录", Desc: "Chrome/Edge 的 History 与 Visited Links、Firefox 的 places.sqlite，清空后无法恢复浏览历史；浏览器开着时会被占用自动跳过", Risk: "warn",
			globs: []string{
				filepath.Join(L, `Microsoft\Edge\User Data\*\History`),
				filepath.Join(L, `Microsoft\Edge\User Data\*\Visited Links`),
				filepath.Join(L, `Google\Chrome\User Data\*\History`),
				filepath.Join(L, `Google\Chrome\User Data\*\Visited Links`),
				filepath.Join(R, `Mozilla\Firefox\Profiles\*\places.sqlite`),
				filepath.Join(L, `Mozilla\Firefox\Profiles\*\places.sqlite`),
			}},

		// ===== 深度清理（高风险，需手动勾选） =====
		// v1.2.0 误报治理：以下两项改为「只给系统设置路径」的卡片，不再由本程序执行系统级强删。
		{ID: "patchcache", Group: "深度清理", Name: "Installer 补丁缓存", Desc: "MSI 补丁缓存，清理后部分软件无法修复/卸载更新", Risk: "warn",
			dirs: []string{filepath.Join(W, `Installer\$PatchCache$`)}},
		{ID: "msocache", Group: "深度清理", Name: "Office 安装源缓存", Desc: "Office 本地安装源（所有磁盘），清理后修复/更改安装需插入介质", Risk: "warn",
			dirs: allDrivesSub(`MSOCache`)},
		// v1.1 合并：rootjunk（各盘根目录 5 类文件）已被 deepjunk（递归全盘 5 类文件）完全覆盖，
		// 两项合一，体积由全盘扫描一次性算出，不再双算
		{ID: "deepjunk", Group: "深度清理", Name: "全盘零散垃圾文件", Desc: "递归所有固定磁盘清 .tmp/.log/.bak/.old/.dmp（已含各盘根目录残留，只算一次）。会触及子目录里软件依赖的日志与备份", Risk: "warn", Cmd: "deepjunk"},
		// v1.1 转激进：不再只写死 C:\AMD\Intel\NVIDIA，遍历所有固定盘根目录，
		// 命中驱动解压目录就整个清空（只留系统真正在用的驱动，DriverStore 一律不碰）
		{ID: "driverres", Group: "深度清理", Name: "驱动解压残留（激进）", Desc: "各磁盘根目录的 AMD / Intel / NVIDIA / ATI / Drivers 等驱动解压目录，整个清空。只清厂商解压目录，不碰 DriverStore、INF、System32\\drivers，不影响已装驱动正常工作", Risk: "warn",
			dirs: driverResDirs()},
		{ID: "rdpcache", Group: "系统缓存", Name: "远程桌面 (RDP) 缓存", Desc: "远程桌面连接的位图缓存，可安全清理",
			dirs: []string{filepath.Join(L, `Microsoft\Terminal Server Client\Cache`)}},
		{ID: "netnative", Group: "系统缓存", Name: ".NET 程序集缓存", Desc: "NGen 预编译的原生映像，清理后系统自动重建（首次启动稍慢）", Risk: "warn",
			dirs: []string{filepath.Join(W, `assembly\NativeImages_v4.0.30319_32`), filepath.Join(W, `assembly\NativeImages_v4.0.30319_64`)}},
		{ID: "diagnosis", Group: "日志与注册表", Name: "系统诊断数据", Desc: "Windows 诊断与遥测日志（Diagnosis），被占用自动跳过",
			dirs: []string{filepath.Join(PD, `Microsoft\Diagnosis`)}},
		{ID: "brokenappx", Group: "深度清理", Name: "损坏的应用包", Desc: "移除状态为 Error 的 UWP 应用包，清理后需重新安装对应应用", Risk: "warn", Cmd: "brokenappx"},
	}
	// 三类缓存（浏览器/应用/AI，51 项）从规则文件加载 —— ID、分组、文案与迁移前一致，
	// 上方 51 条字面量的删除位置即此挂载点（见 rules/junkrules.ini 与 junkrules.go）。
	items = append(items, junkRuleItems()...)
	for i := range items {
		items[i].Seq = i
	}
	// 通用自动发现（2026-09-20 用户要求：按本机实际安装动态出列表，不限于预置规则）
	// custom 新增规则的应用根目录自动排除（防同一份缓存双算）；内置项沿用函数内既有手工名单。
	if found := scanGenericAppCaches(L, R, junkRuleCustomSkips()); len(found) > 0 {
		items = append(items, JunkItem{ID: "appcacheauto", Group: "应用缓存", Name: "其他应用缓存（自动发现）",
			Desc: "自动查找本机已装应用里名为 Cache / Code Cache / ShaderCache / GPUCache 的重建型缓存目录（含新版 Chromium 着色器缓存），清后由各应用自动重建；不碰 WebStorage / 账号数据 / 日志，已单独列出的应用不重复计入",
			dirs: found})
	}
	for i := range items {
		items[i].Seq = i
		/* 2026-11-12 规则化：三类缓存的 Heavy/Renew/Na 已随规则文件带出（junkrules.go），
		   这里改用「或」合成 —— 规则文件带了的以规则为准，名单里剩的是未迁移的静态项。
		   三类迁移项已从下面三个名单里删掉，两边不会重复定义。 */
		items[i].Heavy = items[i].Heavy || heavyIDs[items[i].ID] // 口径分层：界面把"你的文件 / 整包"单独算一行
		items[i].Renew = items[i].Renew || renewIDs[items[i].ID] // 显示口径：边用边长，不计入「这次真正能清掉」
		items[i].Na    = items[i].Na || naIDs[items[i].ID]       // 默认不勾选：不可逆 / 用户自己的东西，等用户手动勾
	}
	return items
}

// heavyIDs 口径分层的名单（2026-10-09 用户拍板：名单不删、全部默认勾选，界面分两行显示）。
// 判据：清掉之后「不可逆 / 是你自己的东西 / 是一整个包」，而不是"能自动重建的缓存"。
//   - 微信QQ接收的文件、本地大模型权重：是用户真实数据
//   - MSOCache / Installer 补丁缓存：清了就没法修复、卸载更新
//   - 驱动解压残留 / 全盘零散文件：整目录、整盘，且 .bak/.old 可能就是你自己的备份
//   - 回收站 / Cookies / 浏览历史：清了找不回来
// 注意：这是「显示分层」，不是风险等级 —— Risk 字段照旧不动。
var heavyIDs = map[string]bool{
	// 2026-11-12 规则化：cookies / qqrecv / wechatrecv / aimodels 四个迁移项改由
	// rules/junkrules.ini 的 heavy 字段带出，名单里只留未迁移的静态项。
	"recycle": true, "recycleother": true, "browserhist": true,
	"msocache": true, "patchcache": true,
	"deepjunk": true, "driverres": true, "brokenappx": true,
}

// renewIDs「边用边长」名单。
// 判据（2026-10-14 实测修正）：**清完之后，程序/系统在正常使用几分钟内就会重新写回可观体积**。
//   只有"对应程序活着就在持续写"的才算：浏览器/应用/AI 客户端缓存、最近使用记录、
//   Prefetch（每启动一个程序生成一条）、缩略图/着色器/字体缓存（一用就重建）、
//   搜索索引（重建）、使用痕迹（一用就又有了）。
//
// 🔴 2026-10-14 实测纠错（对照实验）：清掉 843.5 MB 后立刻复扫只剩 2.1 MB，
//    说明下面这些**并不会**在清完几分钟内长回来，以前按"文件类型"想当然标成 Renew 是错的：
//      - 临时文件（usertemp/systemp）：目录里堆的是几个月攒下的陈旧文件，没人再写
//      - Windows 更新与传递优化缓存（wucache）：清完 Windows 不会立刻重新下载
//      - .NET 原生映像（netnative）/ 系统诊断数据（diagnosis）：缓慢产生
//      - 系统与服务日志（syslogs/svclogs/evtx/iis）：缓慢追加，不可能几分钟长回几百 MB
//      - 各类应用运行日志（onedrivelog/steam/wegame/baidunet/zoom）：同上
//      - 全盘零散垃圾（deepjunk）：各盘 .tmp/.log/.bak/.old/.dmp 基本都是过期文件
//    它们是**真的一次性空间**，必须从 Renew 里摘掉，否则主数字会被凭空砍掉一大截。
// ⚠️ 这个集合**只影响显示**：Renew 项照旧勾选、照旧清理。
//    想让它「真的清不掉」是另一回事，不要在这里动删除行为。
var renewIDs = map[string]bool{
	// 2026-11-12 规则化：浏览器/应用/AI 三类的迁移项（共 42 个 renew 项）改由
	// rules/junkrules.ini 的 renew 字段带出，这里只留静态项与 appcacheauto。
	// 临时文件里唯一"一打开文件就回来"的
	"recent": true,
	// 系统缓存里"边用边重建"的几项
	"prefetch": true, "thumb": true, "shader": true, "fontcache": true, "searchidx": true,
	// 通用自动发现兜底（程序活着就写回）
	"appcacheauto": true,
	// 使用痕迹（一用就又有了）
	"regrunmru": true, "regtyped": true,
}

// naIDs「默认不勾选」名单（2026-10-14 加，JunkItem.Na 字段的判据见该字段注释）。
// 判据只有一条：**删了不可逆，或者那本来就是用户自己的东西**。
// 一键清理不该替用户做这种决定 —— 项照旧扫出来、照旧显示体积，他想清随时能勾。
// ⚠️ 回收站**不在此列**：文件是用户自己主动删进去的，清空是预期行为，
//    也是清理软件最基本的一项，业界（Dism++ / CCleaner 等）同样默认勾选。
var naIDs = map[string]bool{
	// 2026-11-12 规则化：cookies / qqrecv / wechatrecv / aimodels 四个迁移项改由
	// rules/junkrules.ini 的 na 字段带出，这里只留未迁移的静态项。
	// 清了要重新登录 / 找不回来
	"browserhist": true,
	// 删了无法回退或恢复系统
	"dismreset": true,
	// 删了无法修复 / 卸载更新
	"msocache": true, "patchcache": true,
	// .bak/.old 很可能就是用户自己的备份；整目录激进清空同样要用户点头
	"deepjunk": true, "driverres": true,
	// 清完需要重新安装对应应用
	"brokenappx": true,
	// cleanmgr 已整项删除（见 junk_items.go 清单里该项位置的注释），这条跟着去掉
}

// scanGenericAppCaches 通用应用缓存自动发现：遍历 %LOCALAPPDATA% / %APPDATA% 的
// 一级应用目录，**递归 4 层**收集名字在 electronCacheCacheNames 白名单里的目录。
//
// 2026-10-13 重写。原来只看应用目录的直接子层（两层），而 Electron 应用的实际结构是
//   %LOCALAPPDATA%\豆包\User Data\Default\Cache      ← 第三层
//   %APPDATA%\flutter_webview_windows\AweSun\EBWebView\Default\Cache  ← 第四层
// 两层规则一台 Electron 应用都扫不到，实测本机：
//   旧规则 10 个目录 / 0.00 MB   →   新规则 342 个目录 / 794.02 MB
// 其中向日葵（27.77 MB）、钉钉（256 MB）都是这样捞出来的，不需要为它们单开清理项。
//
// 三条纪律（2026-10-13）：
//  ① 只按**目录名白名单**匹配，这些名字按惯例都是可重建缓存；不猜路径、不看内容。
//  ② electronCacheNever 黑名单优先 —— 名字像 cache 但其实是用户数据的（WebStorage/
//     Partitions/logs）一律挡掉，宁漏勿错。
//  ③ 已有具名规则的应用根目录整段排除，避免同一份数据被算两遍（前端会按路径去重，
//     但重复路径会让"合计"虚高，用户看到的数字对不上实际释放量）。
func scanGenericAppCaches(L, R string, extraSkip []string) []string {
	var out []string
	seen := map[string]bool{}
	skipSub := []string{ // 已有具名规则/系统组件的根目录，防止重复计数
		`\microsoft\`, `\google\`, `\mozilla\`, `\tencent\`, `\netease\`, `\dingtalk\`,
		`\douyin`, `\baidu\`, `\thunder\`, `\kingsoft\`, `\adobe\`, `\steam\`, `\jetbrains\`,
		`\discord\`, `\code\`, `\zoom\`, `\packages\`, `\inetcache\`, `\squirreltemp\`, `\crashdumps\`,
		`\comms\`, `\connecteddevicesplatform\`, `\windows\`, `\temp\`,
		// AI 应用缓存组（2026-09-21）：已有具名规则，防同一份缓存被算两遍
		`\doubao\`, `\jimeng\`, `\kimi\`, `\yuanbao\`, `\tongyi\`, `\wenxin\`, `\xinghuo\`,
		`\sparkdesk\`, `\erniebot\`, `\chatgpt\`, `\anthropicclaude\`, `\cursor\`, `\jianyingpro\`,
		`\genieworkbuddy`, `\workbuddy\`, `\trae\`, `\windsurf\`, `\deepseek\`, `\zhipu\`,
		`\chatglm\`, `\cherrystudio\`, `\chatbox\`,
	}
	// custom.ini 新增规则的应用根目录（自动推导，见 junkrules.go 的 ruleCustomSkips）
	skipSub = append(skipSub, extraSkip...)
	want := map[string]bool{}
	for _, n := range electronCacheCacheNames {
		want[n] = true
	}
	consider := func(p string) {
		if seen[strings.ToLower(p)] {
			return
		}
		st, err := os.Stat(p)
		if err != nil || !st.IsDir() || isReparsePoint(p) {
			return
		}
		seen[strings.ToLower(p)] = true
		out = append(out, p)
	}
	for _, base := range []string{L, R} {
		apps, err := os.ReadDir(base)
		if err != nil {
			continue
		}
		for _, app := range apps {
			if !app.IsDir() {
				continue
			}
			appPath := filepath.Join(base, app.Name())
			lp := strings.ToLower(appPath) + `\`
			skip := false
			for _, s := range skipSub {
				if strings.Contains(lp, s) {
					skip = true
					break
				}
			}
			if skip {
				continue
			}
			// 递归下钻。命中白名单就收手不再往里走（Cache 里没有更值钱的缓存层）；
			// 命中黑名单（WebStorage / Partitions…）整棵子树跳过。
			var walk func(p string, depth int)
			walk = func(p string, depth int) {
				if depth > 4 { // 4 层足够覆盖 <app>\<配置>\Default\Cache 与 <app>\a\b\EBWebView\Default\Cache
					return
				}
				subs, err := os.ReadDir(p)
				if err != nil {
					return
				}
				for _, sub := range subs {
					if !sub.IsDir() {
						continue
					}
					ln := strings.ToLower(sub.Name())
					cp := filepath.Join(p, sub.Name())
					if electronCacheNever[ln] {
						continue
					}
					if want[ln] {
						consider(cp)
						continue
					}
					walk(cp, depth+1)
				}
			}
			walk(appPath, 1)
		}
	}
	sort.Strings(out) // 路径有序 → 多 worker 并发扫描时结果稳定，不因遍历顺序抖动
	return out
}

// isReparsePoint 判断路径是否为重解析点（junction / 符号链接 / 挂载点）。
// 必要性：Windows 自带一堆兼容性 junction（如 %LOCALAPPDATA%\Application Data 指回自身、
// C:\Documents and Settings 指向 C:\Users）。遍历时若不跳过，会无限嵌套或把同一批文件
// 重复计数（实测虚高 3.7 倍），全盘 ** 递归更是直接卡死。
func isReparsePoint(p string) bool {
	p16, err := syscall.UTF16PtrFromString(p)
	if err != nil {
		return false
	}
	attrs, err := syscall.GetFileAttributes(p16)
	if err != nil {
		return false
	}
	return attrs&syscall.FILE_ATTRIBUTE_REPARSE_POINT != 0
}

// cache：同一 base 的 "**" 枚举结果复用（key 为小写 base 路径），可为 nil
func resolveGlob(ctx context.Context, pattern string, cache map[string][]string) (dirs, files []string) {
	vol := filepath.VolumeName(pattern)
	rest := strings.TrimPrefix(pattern[len(vol):], `\`)
	segs := strings.Split(rest, `\`)
	cur := []string{vol + `\`}
	for i, seg := range segs {
		last := i == len(segs)-1
		var next []string
		for _, base := range cur {
			if seg == "**" {
				// v1.1 提速两处：
				//  ① 原版串行 WalkDir 是最大时间黑洞（实测单项 8.7s）→ 改并发枚举 listDirsFast
				//  ② 同一个 base 的 "**" 结果做缓存：同一项的多个 glob 往往共用前缀
				//     （如 chromium 的 **\User Data\Default\Cache 与 GPUCache），
				//     否则同一棵目录树会被整棵枚举 N 遍
				key := strings.ToLower(base)
				got, ok := cache[key]
				if !ok {
					got = listDirsFast(ctx, base, 8)
					if cache != nil {
						cache[key] = got
					}
				}
				next = append(next, got...)
				continue
			}
			if !strings.ContainsAny(seg, "*?") {
				p := filepath.Join(base, seg)
				fi, err := os.Stat(p)
				if err != nil {
					continue
				}
				if last {
					if fi.IsDir() {
						dirs = append(dirs, p)
					} else {
						files = append(files, p)
					}
				} else if fi.IsDir() {
					next = append(next, p)
				}
				continue
			}
			entries, err := readDirGated(base)
			if err != nil {
				continue
			}
			lp := strings.ToLower(seg)
			for _, e := range entries {
				ok, _ := path.Match(lp, strings.ToLower(e.Name()))
				if !ok {
					continue
				}
				p := filepath.Join(base, e.Name())
				if last {
					if e.IsDir() {
						dirs = append(dirs, p)
					} else {
						files = append(files, p)
					}
				} else if e.IsDir() {
					next = append(next, p)
				}
			}
		}
		cur = next
		if len(cur) == 0 {
			break
		}
	}
	return dedupe(dirs), dedupe(files)
}

func dedupe(in []string) []string {
	seen := map[string]bool{}
	out := in[:0]
	for _, s := range in {
		k := strings.ToLower(s)
		if !seen[k] {
			seen[k] = true
			out = append(out, s)
		}
	}
	return out
}

func excluded(p string, excl []string) bool {
	lp := strings.ToLower(p)
	for _, e := range excl {
		if strings.Contains(lp, e) {
			return true
		}
	}
	return false
}

func resolveTargets(ctx context.Context, it *JunkItem) (dirs, files []string) {
	dirs = append(dirs, it.dirs...)
	files = append(files, it.files...)
	gcache := map[string][]string{}
	// gofind：动态发现的数据根也按 glob 展开（2026-11-12：自定义安装路径兜底，
	// 进程内缓存所以这里每次调用只查表，全盘搜索只在第一次跑）。
	for _, name := range it.gofind {
		for _, g := range finderGlobs(name) {
			d, f := resolveGlob(ctx, g, gcache)
			dirs = append(dirs, d...)
			files = append(files, f...)
		}
	}
	// find 声明（同日数据化）：与 gofind 同一条展开路，只是声明来自 ini 而非 Go。
	for _, spec := range it.finds {
		for _, g := range findSpecGlobs(spec) {
			d, f := resolveGlob(ctx, g, gcache)
			dirs = append(dirs, d...)
			files = append(files, f...)
		}
	}
	for _, g := range it.globs {
		d, f := resolveGlob(ctx, g, gcache)
		dirs = append(dirs, d...)
		files = append(files, f...)
	}
	var fd, ff []string
	for _, d := range dedupe(dirs) {
		if !excluded(d, it.excl) {
			fd = append(fd, d)
		}
	}
	for _, f := range dedupe(files) {
		if !excluded(f, it.excl) {
			ff = append(ff, f)
		}
	}
	// v1.1.0：字符串不同的两个路径也可能指向同一份数据（junction / 软链接，例如
	// INetCache 与 Temporary Internet Files）。按真实文件标识去重，扫描与清理走同一条路。
	return dedupeByTarget(fd), dedupeByTarget(ff)
}

// dedupeByTarget 按「真实文件标识」去重：os.SameFile 比的是卷序列号 + 文件索引，
// junction / 软链接指向同一处时会拿到同一个标识。Stat 失败的照旧保留，交给后续流程处理。
func dedupeByTarget(paths []string) []string {
	out := make([]string, 0, len(paths))
	var seen []os.FileInfo
	for _, p := range paths {
		fi, err := os.Stat(p)
		if err != nil {
			out = append(out, p)
			continue
		}
		dup := false
		for _, s := range seen {
			if os.SameFile(s, fi) {
				dup = true
				break
			}
		}
		if dup {
			continue
		}
		seen = append(seen, fi)
		out = append(out, p)
	}
	return out
}

func fixedDrives() []string {
	p := syscall.NewLazyDLL("kernel32.dll").NewProc("GetDriveTypeW")
	var out []string
	for c := 'C'; c <= 'Z'; c++ {
		root := string(c) + `:\`
		p16, err := syscall.UTF16PtrFromString(root)
		if err != nil {
			continue
		}
		if r, _, _ := p.Call(uintptr(unsafe.Pointer(p16))); r == 3 {
			out = append(out, root)
		}
	}
	return out
}

// ============================================================================
// v1.1 审查处置记录（2026-10-08 用户拍板；v1.0 源文件未做任何修改）
//  1. 重复/交叠 —— 合计体积只算一次（TotalUnique 按实际路径去重，父目录覆盖子路径）
//     - wucache + wucachedeep + delivery → 已合并为一项 wucache（清的是同一批目录）
//     - thumb + thumbdeep                → 已合并为一项 thumb（同一批 iconcache/thumbcache 文件）
//     - rootjunk + deepjunk              → 已合并为一项 deepjunk（全盘递归已完全覆盖各盘根目录）
//     - winsxstmp + dismreset            → 不合并（不是同一东西：一个直删 Temp，一个是组件清理），
//                                           标同一 dup 组，合计取较大者
//     - cleanmgr                         → 保留独立项；无法预估体积（恒 0），不参与合计，不会双算
//  2. 命名混淆：netnative 与 dotnetgac 原名撞车 —— 原型 UI 已改名「.NET 原生映像缓存 (NGen)」
//  3. driverres（用户要求激进）：遍历所有固定盘根目录的 AMD / Intel / NVIDIA / ATI / Drivers
//     等驱动解压目录，整个清空。红线：DriverStore / INF / System32\drivers 一律不收录
//  4. dotnetgac：维持 v1.0 风险等级 warn，不升不降（用户决定）
//  5. 硬编码盘符：iis / msocache / driverres 已改为遍历所有固定盘（allDrivesSub / driverResDirs）
//  6. iconrefresh 恒 0B：不释放空间，UI 必须标注「不释放空间，仅刷新索引」，否则看起来像 bug
// ============================================================================
