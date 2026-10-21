package main

// ============================================================================
// 远程服务板块（2026-09-22）
//
// 这个文件里的东西全部是「一处声明、前端自动渲染」，改内容不用动前端：
//
// 【怎么改一项服务】改 serviceItems() 里对应的那一条即可（增删也行）。
//   - Tag 只能填 ""、"热门"、"紧急" 之一（写错测试会报红）
//   - ToolID 只能填工具库真实存在的工具 ID（写错测试会报红，见 service_test.go）
//   - PageID 只能填软件里真实存在的页面 ID（home/clean/optimize/space/
//     sec/driver/bsod/tools/appx/set/doc）
//     注：quick / setup / undo（一键优化 / 新机开荒 / 优化还原）已于 2026-09-22 整组删除，别再引用
//   - ToolID 与 PageID 是先自检按钮的目标；注意：卡片上的「先自检」按钮已于 2026-09-22 按用户要求隐藏，
//     这三个字段（含 ActLabel）仍保留并受 service_test.go 校验 —— 要恢复按钮，在 app.js 的 svcCard 里加回一行即可
//   - 对外链接统一写在下面 const 里，前端不硬编码 URL
//
// 【怎么加一条自检清单】往 serviceGuides() 里加一条：症状 + 几步自查 + 对应工具/页面 + 网站文章。
// 【怎么加一类网站文章】往 serviceArticleCats() 里加一条。
// 【诊断报告改字段】改 buildDiagReport()，报告是纯文本，给用户复制后发给服务方。
// ============================================================================

import (
	"fmt"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unsafe"
)

// ---------- 对外链接（唯一事实来源，前端通过 SvcLinks() 取） ----------
const (
	svcSiteURL    = "https://www.3z9w.com"
	svcOrderURL   = "https://www.3z9w.com/dashu"           // 报修落地页
	svcToolsURL   = "https://www.3z9w.com/tools"           // 网站工具库
	svcBsodURL    = "https://www.3z9w.com/bsod"            // 蓝屏在线查询
	svcDocsURL    = "https://www.3z9w.com/category/record" // 技术文库总入口
	svcWxID       = "RESBJD"                               // 微信号（扫码弹窗展示 + 一键复制）
	svcQrName     = "weixin-qr.png"                        // 微信二维码：放进 frontend/dist/ 后自动出现在界面上
	svcRewardName = "zanshang-qr.png"                      // 赞赏码：同样放进 frontend/dist/ 自动出现（没有则隐藏）
	svcAvatarName = "dashu-avatar.png"                     // 服务方头像：放进 frontend/dist/ 自动替换字母头像（没有则用「叔」字）
	svcQrDir      = "frontend/dist/"                       // embed 路径前缀（main.go 里 go:embed all:frontend/dist）
	// UU远程官网：服务流程里「下载安装 UU远程」那一步的链接（全软件唯一的非本站外链）
	// 用户 2026-09-22 指定：把流程里的「UU远程」四个字做成超链接
	svcUUURL = "https://uuyc.163.com/"
	// ---------- 关于页（2026-09-22 新增） ----------
	//                            侧栏与关于页都从 GetAboutInfo() 取这里。必须与 frontend/dist/app.js 的 APP_VER 一致，
	//                            且主版本（前两段）必须与 app.manifest 一致 —— 有 version_sync_test.go 钉住。
	//                            注意：app.manifest 的版本号四段每段上限 65535，261001 放不进去，故 manifest 用 1.0.1001.0（1001=10月1日）。
	// 群码图片：把 wxgroup-qr.png（微信群）/ qqgroup-qr.png（QQ 群）放进 frontend/dist 即自动显示，
	// 没放就只显示群号（群号可一键复制）
	svcQQGroup    = "214524661"                               // QQ 交流群号
	svcQQQrName   = "qqgroup-qr.png"                          // QQ 群二维码（可选素材）
	svcWxQrGroup  = "wxgroup-qr.png"                          // 微信群二维码（可选素材，没有则回退到个人微信码 svcQrName）
	svcBugURL     = "https://www.3z9w.com/1892.html#comments" // bug 反馈入口（官网那篇的评论区）
	svcPublishURL = "https://opt.3z9w.com/"                   // 软件发布页（最新版 / 下载 / 更新日志）
)

/*
── 版本号（2026-10-09 起改为「语义化版本 + 构建日期」）─────────────────────

	对外版本号：MAJOR.MINOR.PATCH，由构建注入 main.appVersion（见 main.go）；
	构建日期：单独由 main.buildDate 注入，界面显示为 "v1.1.0 · 构建 2026-10-09"。
	为什么不再把日期塞进版本号：GitHub 的 tag / Release 排序 / 自动更新都认语义化版本。
	契约：app.manifest 的 assemblyIdentity version 前两段，必须与这里的前两段一致
	     （svcAppVer = v1.1.x  ↔  app.manifest = 1.1.0.0）。
*/
var svcAppVer = "v" + appVersion

// diagVer 诊断报告里那行「工具版本」：版本号 + 构建号/构建日期，两者单列（README 的版本命名规范）。
// appVersion 是构建注入的（-X main.appVersion=），可能已经被写成 "v1.1.0" 也可能没有，
// 统一先剥 v 再补 —— 前端就踩过 "vv1.1.0" 的坑，这里别再踩一次。
// 有构建号就优先显示构建号（它本身含日期、且能区分同版本号的两次构建）；
// 只有日期就显示日期；都没有就只给版本号：宁可少写，也不把 unknown 这种东西发给用户。
func diagVer() string {
	v := "v" + strings.TrimPrefix(strings.TrimSpace(appVersion), "v")
	if b := strings.TrimSpace(buildNum); b != "" {
		return v + "（构建 " + b + "）"
	}
	if buildDate != "" && buildDate != "unknown" {
		return v + "（构建 " + buildDate + "）"
	}
	return v
}

// ---------- 服务目录 ----------

// SvcItem 一项远程服务
type SvcItem struct {
	ID       string   `json:"id"`
	Name     string   `json:"name"`
	Tag      string   `json:"tag"`      // ""/"热门"/"紧急"
	Icon     string   `json:"icon"`     // 卡片图标键（必须是 app.js ICONS 里真实存在的键）
	Desc     string   `json:"desc"`     // 一句话症状
	Points   []string `json:"points"`   // 我们怎么做（3 条）
	Time     string   `json:"time"`     // 预计时长
	ToolID   string   `json:"toolId"`   // 先自检目标 → 工具库里的工具（卡片按钮当前隐藏，字段保留）
	PageID   string   `json:"pageId"`   // 先自检目标 → 软件内页面（ToolID 优先；按钮当前隐藏，字段保留）
	ActLabel string   `json:"actLabel"` // 自检按钮文案
}

func serviceItems() []SvcItem {
	return []SvcItem{
		{
			ID: "svc-reinstall", Name: "原版系统重装", Tag: "热门",
			Icon:   "refresh",
			Desc:   "系统被折腾坏了、越用越卡？原版纯净安装，装完驱动齐全直接用。",
			Points: []string{"微软官方原版镜像 · 无捆绑无精简", "Win10 / Win11 驱动完整匹配", "重要资料先备份，私人文件全程不碰"},
			Time:   "1~2 小时", ToolID: "info-hw", ActLabel: "先看配置",
		},
		{
			ID: "svc-setup", Name: "系统开荒优化", Tag: "",
			Icon:   "bolt",
			Desc:   "新机或刚重装，预装软件一堆、后台乱、开机慢？一次开荒把系统调到顺手状态。",
			Points: []string{"预装软件清理 + 无用服务关闭", "隐私与广告开关一次性调好", "启动项精简，开机速度看得见"},
			Time:   "30~60 分钟", PageID: "home", ActLabel: "先看本机状态",
		},
		{
			ID: "svc-clean", Name: "系统垃圾清理", Tag: "",
			Icon:   "broom",
			Desc:   "C 盘爆红、弹窗关不掉、主页被劫持？垃圾清理、广告拦截、主页修复一站搞定。",
			Points: []string{"C 盘 / 全盘垃圾深度清理，无损扩容", "弹窗广告 / 捆绑软件 / 流氓软件清除", "浏览器主页劫持修复 + 长效拦截规则"},
			Time:   "30~60 分钟", PageID: "clean", ActLabel: "先清一遍垃圾",
		},
		{
			ID: "svc-bsod", Name: "蓝屏黑屏检测修复", Tag: "紧急",
			Icon:   "warn",
			Desc:   "蓝屏进不去系统、黑屏只剩鼠标、反复重启？远程定位根源，给结论也给方案。",
			Points: []string{"读本机转储，锁定错误码与嫌疑驱动", "黑屏 / 无限重启 / 自动修复循环处理", "⚠ 远程只做检测与方案，换件维修费自担"},
			Time:   "30~90 分钟", PageID: "bsod", ActLabel: "先分析蓝屏",
		},
		{
			ID: "svc-hw", Name: "电脑硬件故障检测", Tag: "",
			Icon:   "cpu",
			Desc:   "怀疑内存、硬盘、主板有问题？远程全面检测，出一份能看懂的结论与方案。",
			Points: []string{"磁盘 SMART / 读写速度全面检测", "内存与温度压力测试，软件层问题免费处理", "⚠ 远程只做检测与方案，换件维修费自担"},
			Time:   "20~40 分钟", ToolID: "disk-smart", ActLabel: "先做磁盘体检",
		},
		{
			ID: "svc-other", Name: "其他电脑问题 · 一对一排查", Tag: "",
			Icon:   "message",
			Desc:   "上面都没对上？把症状说清楚，远程一对一排查，先判断能不能修再决定。",
			Points: []string{"微信发截图，先免费判断能不能修", "软件类远程直接修，硬件类给方案", "修不好 100% 退款"},
			Time:   "视问题而定",
		},
	}
}

// GetServices 返回服务目录（前端服务大厅页数据源）
func (a *App) GetServices() []SvcItem { return serviceItems() }

// ---------- 服务大厅：首屏 / 信任数字 / 服务流程 / 真实口碑 ----------
//
// 转化设计三条硬规矩（改内容时请守住）：
//   1) 第一眼看到的是「人」和「承诺」，不是一堆功能名
//   2) 数字只能来自官网公开口径，禁止编造；官网两处口径打架时只取不冲突项
//   3) 流程必须写全「怎么下单、怎么退款」—— 用户看不到下一步，就不会下单

// SvcHero 首屏名片（人 + 承诺 + 行动）
type SvcHero struct {
	Name     string `json:"name"`     // 服务方署名
	Avatar   string `json:"avatar"`   // 头像文件名（素材缺失则为空，前端用字母头像兜底）
	Identity string `json:"identity"` // 身份一句话（建立专业信任）
	Title    string `json:"title"`    // 大标题（结果承诺，降低心理门槛）
	Sub      string `json:"sub"`      // 副标
	Primary  string `json:"primary"`  // 主按钮（零门槛动作）
	Second   string `json:"second"`   // 次按钮（自助动作）
}

func svcHero() SvcHero {
	return SvcHero{
		Name:     "无名大叔",
		Avatar:   avatarFile(),
		Identity: "18 年一线维修工程师，系统重装 / 蓝屏修复 / 清理优化都拿手",
		Title:    "电脑出问题？先免费诊断，再决定修不修",
		Sub:      "系统重装 · 蓝屏黑屏 · 垃圾清理 · 硬件检测 —— 能自己解决的免费搞定，搞不定的 1V1 远程帮你处理，修不好全额退款",
		Primary:  "微信联系大叔",
		Second:   "生成诊断报告",
	}
}

// SvcPromise 首屏承诺条一项（图标键见 app.js 的 ICONS）
// ⚠ 承诺条与「信任数字」「Hero 主张」三层各说一件事，同一卖点只许出现一次 —— 用户明确要求「整合的目的就是为了不重复」
type SvcPromise struct {
	Icon string `json:"icon"`
	Text string `json:"text"`
}

func svcPromise() []SvcPromise {
	return []SvcPromise{
		{"wrench", "专业技术支撑"},
		{"bolt", "远程极速接入"},
		{"sliders", "个性化方案订制"},
		{"shield", "全程加密 · 不碰隐私"},
		{"search", "先诊断再报价"},
		{"check", "售后退款无忧"},
	}
}

// SvcStat 信任数字（口径来自官网公开页面，只取不冲突项）
type SvcStat struct {
	Value string `json:"value"`
	Label string `json:"label"`
	Icon  string `json:"icon"` // 前端图标键（见 app.js 的 ICONS）
}

func svcStats() []SvcStat {
	return []SvcStat{
		{"18 年", "维修经验", "history"},
		{"10000+", "台电脑修复", "wrench"},
		{"3 分钟", "平均响应", "bolt"},
		{"7×24", "小时在线", "clock"},
		{"100%", "修不好全额退款", "shield"},
	}
}

// ---------- 服务流程 ----------
// 2026-09-22 19:30 再简化（用户反馈：步骤太繁琐，客户看着不敢下单）：
//   客户只需要知道【下单三步】：扫码加微信 → 装 UU远程发验证码 → 大叔远程修好你验收。
//   诊断 / 报价 / 维修 / 退款这些内部环节不再摊开画成分叉图，压成一行「三种结局」兜底承诺 ——
//   信息一条不少，但视觉上只有「三步就能开始」，降低下单心理门槛。
// ⚠ 「UU远程」四个字必须是超链接 —— 用 Link + LinkText 声明，
//   前端按 LinkText 在标题里定位并替换成 <a>（别在标题里写死 HTML，数据与渲染要分开）

// SvcFlowStep 下单路径上的一步（有先后顺序，最多 3~4 步，多了客户就嫌烦）
type SvcFlowStep struct {
	Icon     string `json:"icon"`     // 图标键（见 app.js 的 ICONS）
	Title    string `json:"title"`    // 步骤名
	Desc     string `json:"desc"`     // 一句大白话说明（悬浮可见，不占版面）
	Link     string `json:"link"`     // 非空 → 标题里的 LinkText 渲染成超链接（外链，交给系统浏览器打开）
	LinkText string `json:"linkText"` // 要变成链接的那几个字
}

// SvcFlowEnd 一种结局（一行小字把最坏情况说清 —— 客户最怕的不是步骤多，是不知道坑在哪）
type SvcFlowEnd struct {
	Key  string `json:"key"`  // A / B / C（内部标识，前端用 Icon + Text 呈现）
	Icon string `json:"icon"` // 图标键（见 app.js 的 ICONS）
	Text string `json:"text"` // 十来个字讲完：什么情况 → 怎么处理
	Tone string `json:"tone"` // 配色：ok 修好 / warn 退款 / mute 未成交
}

// SvcFlow 服务流程整块：三步就能下单 + 一行兜底（三种结局）
type SvcFlow struct {
	Title string        `json:"title"`
	Desc  string        `json:"desc"`
	Steps []SvcFlowStep `json:"steps"` // 下单三步
	Ends  []SvcFlowEnd  `json:"ends"`  // 三种结局，一行排完
}

func svcFlow() SvcFlow {
	return SvcFlow{
		Title: "三步就能修",
		Desc:  "扫码 → 装个远控 → 大叔上手。先诊断后报价，修好才收钱，修不好全额退款。",
		Steps: []SvcFlowStep{
			{Icon: "message", Title: "联系大叔", Desc: "微信扫码，说清故障、发张截图"},
			{Icon: "download", Title: "装 UU远程 · 发验证码", Desc: "官网装好，把 ID 和验证码发给大叔",
				Link: svcUUURL, LinkText: "UU远程"},
			{Icon: "monitor", Title: "大叔远程接入", Desc: "你坐屏幕前看着，修完当场验收"},
		},
		Ends: []SvcFlowEnd{
			{Key: "A", Icon: "check", Text: "修好 · 你验收再收尾", Tone: "ok"},
			{Key: "B", Icon: "refresh", Text: "修不好 · 全额退款", Tone: "warn"},
			{Key: "C", Icon: "shield", Text: "报价不认可 · 不收费", Tone: "mute"},
		},
	}
}

// SvcReview 真实口碑：客户评价 / 服务案例（内容来自官网公开页面，禁止编造）
type SvcReview struct {
	Kind  string   `json:"kind"`  // "review" 评价 / "case" 案例
	Title string   `json:"title"` // 案例标题（评价为空）
	Name  string   `json:"name"`  // 客户姓氏
	Meta  string   `json:"meta"`  // 身份 · 城市
	Stars int      `json:"stars"` // 星级（案例为 0）
	Text  string   `json:"text"`
	Tags  []string `json:"tags"`
}

func svcReviews() []SvcReview {
	return []SvcReview{
		{
			Kind: "case", Title: "游戏帧率翻倍",
			Text: "《原神》从 60 帧提升到 120 帧，《赛博朋克》恢复流畅画质，告别掉帧卡顿和高温降频。",
			Tags: []string{"帧率平均提升 60%+", "游戏优化"},
		},
		{
			Kind: "review", Name: "周先生", Meta: "设计师 · 上海", Stars: 5,
			Text: "做图时电脑突然崩溃，文件差点全丢了。工程师远程紧急抢救，不仅恢复了系统，还帮我把重要数据备份好。专业又细心，太感动了！以后电脑问题就认准无名大叔。",
			Tags: []string{"系统修复", "数据备份", "紧急救援"},
		},
		{
			Kind: "review", Name: "赵女士", Meta: "自由职业者 · 北京", Stars: 5,
			Text: "电脑突然蓝屏真的崩溃。工程师先查看错误报告给出方案，重装系统遇到的问题都一遍遍指导解决，还帮忙做了配置优化，十分负责！价格也很公道，强烈推荐。",
			Tags: []string{"蓝屏修复", "系统重装", "配置优化"},
		},
	}
}

// SvcPage 服务大厅数据（一次取全，前端零硬编码）
type SvcPage struct {
	Hero    SvcHero      `json:"hero"`
	Stats   []SvcStat    `json:"stats"`
	Promise []SvcPromise `json:"promise"`
	Items   []SvcItem    `json:"items"`
	Flow    SvcFlow      `json:"flow"`
	Reviews []SvcReview  `json:"reviews"`
}

// GetSvcPage 服务大厅一次性数据源（前端 mount 一次调用拿全）
func (a *App) GetSvcPage() SvcPage {
	return SvcPage{Hero: svcHero(), Stats: svcStats(), Promise: svcPromise(), Items: serviceItems(), Flow: svcFlow(), Reviews: svcReviews()}
}

// ---------- 技术文库：本机自检清单 ----------

// SvcGuide 一条按症状的自检清单
type SvcGuide struct {
	ID           string   `json:"id"`
	Title        string   `json:"title"`    // 症状
	Steps        []string `json:"steps"`    // 自查步骤（从简到繁，最后一步通常指向人工服务）
	ToolID       string   `json:"toolId"`   // 一键执行工具库里这个工具
	PageID       string   `json:"pageId"`   // 一键打开软件内页面（ToolID 优先）
	ActLabel     string   `json:"actLabel"` // 按钮文案
	Article      string   `json:"article"`  // 网站文章链接（空则不显示）
	ArticleTitle string   `json:"articleTitle"`
}

func serviceGuides() []SvcGuide {
	return []SvcGuide{
		{
			ID: "g-slow", Title: "开机慢、越用越卡",
			Steps: []string{
				"看启动项：把不认识的、用不上的开机自启关掉（这一条最见效）",
				"清一遍系统垃圾与浏览器缓存",
				"看 C 盘剩余空间：低于 10% 会明显拖慢系统",
				"做完还是卡，多半是硬盘老化或后台服务异常，找大叔远程看",
			},
			PageID: "sec", ActLabel: "打开启动项管理",
			Article: "https://www.3z9w.com/1856.html", ArticleTitle: "Win10/11 优化 15 项：卡顿、掉帧一次解决",
		},
		{
			ID: "g-cdisk", Title: "C 盘爆满、总是红色警告",
			Steps: []string{
				"先用「磁盘空间」逐层钻取，看清到底哪个目录最占地方",
				"清理系统垃圾、休眠文件、旧系统还原点",
				"微信 / QQ 聊天记录、桌面大文件搬到其他盘",
				"要无损扩容 C 盘（动分区）有一定风险，建议远程操作",
			},
			PageID: "space", ActLabel: "打开磁盘空间分析",
			Article: "https://www.3z9w.com/1870.html", ArticleTitle: "C 盘空间不足？Win11 清理 5 步 + 千万不能删的文件",
		},
		{
			ID: "g-bsod", Title: "开机蓝屏、进不去系统",
			Steps: []string{
				"先在本机读一遍蓝屏转储：能进系统就直接看错误码和嫌疑驱动",
				"能进系统：优先更新或回滚显卡、网卡驱动，卸载最近装的补丁",
				"进不去系统：用 WinRE 或安全模式，按文章顺序排查，别急着重装",
				"蓝屏反复出现通常不是单一原因，交给远程处理更省事",
			},
			PageID: "bsod", ActLabel: "打开蓝屏分析",
			Article: "https://www.3z9w.com/1875.html", ArticleTitle: "Windows 自动修复进不去系统？WinRE / 安全模式按序排查",
		},
		{
			ID: "g-bsodcode", Title: "蓝屏代码看不懂是什么原因",
			Steps: []string{
				"有转储文件：用蓝屏分析读本机记录，直接给中文结论和处理建议",
				"没有转储：用在线查询输入错误码，看常见原因清单",
				"查完仍然不确定是哪个驱动，可以把错误码截图发给大叔判断",
			},
			PageID: "bsod", ActLabel: "打开蓝屏分析",
			Article: "https://www.3z9w.com/1881.html", ArticleTitle: "10 种常见蓝屏代码对照表 + 按顺序排查",
		},
		{
			ID: "g-nosound", Title: "电脑没声音了",
			Steps: []string{
				"先排除硬件：音箱 / 耳机插对孔了吗，是不是静音或选错输出设备",
				"看设备管理器：声卡有没有黄色感叹号（驱动问题）",
				"重启音频服务，或重装声卡驱动",
			},
			PageID: "driver", ActLabel: "打开驱动管理",
			Article: "https://www.3z9w.com/1879.html", ArticleTitle: "电脑没声音了怎么解决？7 个排查方法",
		},
		{
			ID: "g-black", Title: "黑屏只剩鼠标箭头",
			Steps: []string{
				"按 Ctrl+Alt+Del：能调出任务管理器就说明系统还活着",
				"任务管理器 → 运行新任务 → 输入 explorer.exe，桌面通常能回来",
				"反复出现说明系统文件或用户配置文件损坏，可用 SFC 修复",
			},
			ToolID: "fix-sfc", ActLabel: "执行 SFC 系统文件修复",
			Article: "https://www.3z9w.com/1877.html", ArticleTitle: "电脑黑屏只有鼠标箭头？6 种方法不用重装",
		},
		{
			ID: "g-nonet", Title: "上不了网、网卡掉线",
			Steps: []string{
				"先用「网络修复」一键重置 Winsock 与 TCP/IP（需重启生效）",
				"看网卡驱动状态，无线掉线经常是驱动或省电设置的问题",
				"家里有别的设备能上网吗？能上网说明是本机问题，不是宽带",
			},
			ToolID: "net-smart", ActLabel: "执行 网络修复",
			Article: "https://www.3z9w.com/1198.html", ArticleTitle: "电脑上不了网怎么办？",
		},
		{
			ID: "g-ads", Title: "弹窗广告关不掉、主页被劫持",
			Steps: []string{
				"先找出是谁弹的：任务管理器看可疑进程，或看最近装了什么软件",
				"用「应用清理」把来源不明的全家桶卸干净，再清残留",
				"清理浏览器快捷方式与主页设置，随后加拦截规则",
				"劫持往往藏得深，清不干净建议远程处理一次做彻底",
			},
			PageID: "appx", ActLabel: "打开应用清理",
			Article: "https://www.3z9w.com/1854.html", ArticleTitle: "流氓软件怎么彻底删除：清理 + 拦截 + 防护三合一",
		},
		{
			ID: "g-printer", Title: "打印机 / 局域网共享连不上",
			Steps: []string{
				"先重启打印服务（Print Spooler）",
				"再跑一次「局域网共享修复」，它会处理共享组件与 RPC 加固引起的连不上",
				"确认两台机器在同一网段，共享账号有权限",
			},
			ToolID: "net-lanfix", ActLabel: "执行 局域网共享修复",
		},
		{
			ID: "g-game", Title: "游戏掉帧、卡顿、延迟高",
			Steps: []string{
				"先跑一遍「系统优化」：关掉后台冗余、游戏相关的几个开关",
				"更新显卡驱动，按文章调显卡控制台与 G-SYNC",
				"看内存和磁盘余量：磁盘几乎满了会明显卡顿",
			},
			PageID: "optimize", ActLabel: "打开系统优化",
			Article: "https://www.3z9w.com/1860.html", ArticleTitle: "NVIDIA 控制台怎么设置打游戏最流畅",
		},
	}
}

// GetGuides 返回自检清单
func (a *App) GetGuides() []SvcGuide { return serviceGuides() }

// ---------- 技术文库：网站文章分类与精选 ----------

// SvcCat 网站文章分类（点击进浏览器）
type SvcCat struct {
	Name  string `json:"name"`
	Count string `json:"count"`
	URL   string `json:"url"`
}

func serviceArticleCats() []SvcCat {
	return []SvcCat{
		{"全部技术文章", "120", svcDocsURL},
		{"系统问题", "43", svcDocsURL + "/xitongwenti"},
		{"设备应用问题", "70", svcDocsURL + "/shebeiyingyong"},
		{"游戏影音", "37", svcDocsURL + "/youxiuyingyin"},
		{"第三方软件问题", "21", svcDocsURL + "/disanfang"},
		{"会员专享", "15", svcDocsURL + "/vipnews"},
		{"BIOS 设置相关", "10", svcDocsURL + "/bios"},
		{"上网问题", "10", svcDocsURL + "/shangwangwenti"},
		{"驱动程序", "4", svcDocsURL + "/qudong"},
	}
}

// SvcArticle 精选文章
type SvcArticle struct {
	Title string `json:"title"`
	URL   string `json:"url"`
	Tag   string `json:"tag"`
}

func serviceArticles() []SvcArticle {
	return []SvcArticle{
		{"电脑蓝屏是什么原因？10 种常见蓝屏代码对照表＋按顺序排查", "https://www.3z9w.com/1881.html", "蓝屏"},
		{"C 盘空间不足？Windows 11 清理 5 步＋千万不能删的文件", "https://www.3z9w.com/1870.html", "清理"},
		{"Windows 自动修复进不去系统？WinRE / 安全模式 / DISM 按序排查", "https://www.3z9w.com/1875.html", "系统"},
		{"电脑黑屏只有鼠标箭头怎么回事？6 种方法不用重装系统", "https://www.3z9w.com/1877.html", "显示"},
		{"电脑没声音了怎么解决？7 个排查方法＋原因对照表", "https://www.3z9w.com/1879.html", "声卡"},
		{"电脑玩游戏卡顿、掉帧、延迟高？15 项 Win10/11 优化设置", "https://www.3z9w.com/1856.html", "游戏"},
		{"Win10/11 流氓软件怎么彻底删除？清理 + 拦截 + 防护三合一", "https://www.3z9w.com/1854.html", "清理"},
		{"BIOS/UEFI 快捷键进入完整指南｜全品牌 BIOS 热键对照表", "https://www.3z9w.com/1864.html", "BIOS"},
	}
}

// GetArticleCats 网站文章分类
func (a *App) GetArticleCats() []SvcCat { return serviceArticleCats() }

// GetArticles 精选文章
func (a *App) GetArticles() []SvcArticle { return serviceArticles() }

// ---------- 外链与素材 ----------

// qrFile 二维码是否已放进来（放 frontend/dist/weixin-qr.png 即自动出现，无需改代码）
func qrFile() string {
	if f, err := assets.Open(svcQrDir + svcQrName); err == nil {
		f.Close()
		svcQrReady = true // 供测试断言用
		return svcQrName
	}
	return ""
}

// rewardFile 赞赏码是否已放进来（放 frontend/dist/zanshang-qr.png 即自动出现）
func rewardFile() string {
	if f, err := assets.Open(svcQrDir + svcRewardName); err == nil {
		f.Close()
		return svcRewardName
	}
	return ""
}

// wxGroupFile 微信群码是否已放进来（放 frontend/dist/wxgroup-qr.png 即自动显示）
func wxGroupFile() string {
	if f, err := assets.Open(svcQrDir + svcWxQrGroup); err == nil {
		f.Close()
		return svcWxQrGroup
	}
	return ""
}

// qqQrFile QQ 群码是否已放进来（放 frontend/dist/qqgroup-qr.png 即自动显示，没有就只显示群号）
func qqQrFile() string {
	if f, err := assets.Open(svcQrDir + svcQQQrName); err == nil {
		f.Close()
		return svcQQQrName
	}
	return ""
}

// avatarFile 头像是否已放进来（放 frontend/dist/dashu-avatar.png 即自动替换字母头像）
func avatarFile() string {
	if f, err := assets.Open(svcQrDir + svcAvatarName); err == nil {
		f.Close()
		return svcAvatarName
	}
	return ""
}

// svcQrReady 记录二维码素材是否可用（仅测试与调试用）
var svcQrReady bool

// ---------- 关于页数据（2026-09-22 新增） ----------
// 前端不硬编码：版本号、群号、外链、致谢项目全在这里取。
// ⚠ 别把 QQ 群号 / GitHub 链接塞进 SvcLinks() —— 那张表要求外链必须是本站 https，
//
//	关于页这两项天然是外部地址，所以单开这一个接口。
type AboutInfo struct {
	Version    string `json:"version"`   // 版本号（侧栏与关于页共用这一处）
	BuildDate  string `json:"buildDate"` // 构建日期（构建时注入；未注入为 unknown）
	BuildNum   string `json:"buildNum"`  // 构建号 YYYYMMDD.N（发布构建才有，日常构建为空）
	Site       string `json:"site"`      // 官网
	PublishURL string `json:"publish"`   // 发布页（最新版 / 下载）
	BugURL     string `json:"bug"`       // Bug 反馈入口
	WxID       string `json:"wxid"`      // 作者微信号
	WxGroupQr  string `json:"wxGroupQr"` // 微信群码（没素材返回空，前端回退到个人微信码）
	WxQr       string `json:"wxQr"`      // 个人微信码（回退用）
	QQGroup    string `json:"qq"`        // QQ 群号
	QQGroupQr  string `json:"qqQr"`      // QQ 群码（没素材返回空，只显示群号）
}

// GetAboutInfo 关于页一次性数据源
func (a *App) GetAboutInfo() AboutInfo {
	return AboutInfo{
		Version:    svcAppVer,
		BuildDate:  buildDate,
		BuildNum:   buildNum,
		Site:       svcSiteURL,
		PublishURL: svcPublishURL,
		BugURL:     svcBugURL,
		WxID:       svcWxID,
		WxGroupQr:  wxGroupFile(),
		WxQr:       qrFile(),
		QQGroup:    svcQQGroup,
		QQGroupQr:  qqQrFile(),
	}
}

// SvcLinks 服务板块所有对外链接（前端不硬编码 URL）
func (a *App) SvcLinks() map[string]string {
	return map[string]string{
		"site":   svcSiteURL,
		"order":  svcOrderURL,
		"tools":  svcToolsURL,
		"bsod":   svcBsodURL,
		"docs":   svcDocsURL,
		"qr":     qrFile(),
		"reward": rewardFile(),
		"wxid":   svcWxID,
		// 注：关于页那套（QQ 群号 / GitHub 致谢外链）走 GetAboutInfo()，别塞进这里 ——
		//     这张表有「外链必须是本站 https」的规矩（见 service_test.go），外部地址放进来会报红
	}
}

// ---------- 机器诊断报告 ----------

// BuildDiagReport 生成一份给服务方看的机器诊断报告（纯文本，用户复制后微信发给大叔）
// note：用户自述的症状（可空）
func (a *App) BuildDiagReport(note string) string {
	var b strings.Builder
	line := func(f string, args ...interface{}) { fmt.Fprintf(&b, f+"\n", args...) }
	kv := func(k, v string) {
		v = strings.TrimSpace(v)
		if v == "" {
			v = "—"
		}
		line("%-10s%s", k, v)
	}

	line("【无名大叔 · 远程服务诊断报告】")
	/* 版本号必须有：这份报告是要发给服务方的，没版本号人家不知道是哪版工具生成的，
	   排查时很容易对着旧行为白费劲。构建日期不进版本号，单列（README 版本命名规范）。 */
	kv("工具版本", diagVer())
	line("生成时间    %s", time.Now().Format("2006-01-02 15:04:05"))
	line("")

	line("── 系统 ──")
	os0 := wmiFirst(`root\cimv2`,
		"SELECT Caption,Version,BuildNumber,OSArchitecture,InstallDate,CSName FROM Win32_OperatingSystem",
		[]string{"Caption", "Version", "BuildNumber", "OSArchitecture", "InstallDate", "CSName"})
	kv("计算机名", os0["CSName"])
	kv("操作系统", os0["Caption"])
	ver, bn, arch := strings.TrimSpace(os0["Version"]), strings.TrimSpace(os0["BuildNumber"]), strings.TrimSpace(os0["OSArchitecture"])
	if bn != "" && !strings.Contains(ver, bn) {
		ver += " build " + bn
	}
	if arch != "" {
		ver += "（" + arch + "）"
	}
	kv("版本", ver)
	if t, ok := parseWmiDate(os0["InstallDate"]); ok {
		kv("安装日期", t.Format("2006-01-02"))
	}
	if up := systemUptimeMs(); up > 0 {
		d := time.Duration(up) * time.Millisecond
		kv("已开机", fmt.Sprintf("%d 天 %d 小时 %d 分", int(d.Hours())/24, int(d.Hours())%24, int(d.Minutes())%60))
	}
	line("")

	line("── 硬件 ──")
	if rows, err := wmiQuery(`root\cimv2`,
		"SELECT Name,NumberOfCores,NumberOfLogicalProcessors FROM Win32_Processor",
		[]string{"Name", "NumberOfCores", "NumberOfLogicalProcessors"}); err == nil && len(rows) > 0 {
		r := rows[0]
		kv("处理器", strings.TrimSpace(r["Name"]))
		// 有些精简系统/虚拟机的 WMI 不上报核心数，别输出「核 / 线程」这种空壳
		cores, threads := strings.TrimSpace(r["NumberOfCores"]), strings.TrimSpace(r["NumberOfLogicalProcessors"])
		switch {
		case cores != "" && threads != "":
			kv("核心线程", cores+" 核 / "+threads+" 线程")
		case threads != "":
			kv("核心线程", threads+" 线程")
		case cores != "":
			kv("核心线程", cores+" 核")
		}
	} else {
		kv("处理器", "查询失败（"+shortErr(err)+"）")
	}

	mem := wmiFirst(`root\cimv2`, "SELECT TotalPhysicalMemory FROM Win32_ComputerSystem", []string{"TotalPhysicalMemory"})
	if tot, err := strconv.ParseInt(strings.TrimSpace(mem["TotalPhysicalMemory"]), 10, 64); err == nil && tot > 0 {
		if st, err2 := strconv.ParseInt(strings.TrimSpace(wmiFirst(`root\cimv2`,
			"SELECT FreePhysicalMemory FROM Win32_OperatingSystem", []string{"FreePhysicalMemory"})["FreePhysicalMemory"]), 10, 64); err2 == nil {
			kv("内存", fmt.Sprintf("%s（可用 %s）", fmtBytes(tot), fmtBytes(st*1024)))
		} else {
			kv("内存", fmtBytes(tot))
		}
	}

	if rows, err := wmiQuery(`root\cimv2`,
		"SELECT Name,DriverVersion FROM Win32_VideoController", []string{"Name", "DriverVersion"}); err == nil {
		for _, r := range rows {
			kv("显卡", strings.TrimSpace(r["Name"]+"（驱动 "+emptyDash(r["DriverVersion"])+"）"))
		}
	}

	mb := wmiFirst(`root\cimv2`, "SELECT Manufacturer,Product FROM Win32_BaseBoard", []string{"Manufacturer", "Product"})
	kv("主板", strings.TrimSpace(emptyDash(mb["Manufacturer"])+" "+emptyDash(mb["Product"])))
	bios := wmiFirst(`root\cimv2`, "SELECT SMBIOSBIOSVersion,ReleaseDate FROM Win32_BIOS", []string{"SMBIOSBIOSVersion", "ReleaseDate"})
	if t, ok := parseWmiDate(bios["ReleaseDate"]); ok {
		kv("BIOS", fmt.Sprintf("%s（%s）", emptyDash(bios["SMBIOSBIOSVersion"]), t.Format("2006-01-02")))
	}
	line("")

	line("── 磁盘 ──")
	if ds := (&App{}).GetDiskList(); len(ds) > 0 {
		for _, d := range ds {
			pct := 0.0
			if d.Total > 0 {
				pct = float64(d.Total-d.Free) / float64(d.Total) * 100
			}
			line("%-10s%s 总计 %s，可用 %s（已用 %.0f%%）", "磁盘", d.Drive, fmtBytes(d.Total), fmtBytes(d.Free), pct)
		}
	} else {
		kv("磁盘", "查询失败")
	}
	line("")

	line("── 故障线索 ──")
	dumps := (&App{}).BsodList()
	if len(dumps) == 0 {
		kv("蓝屏记录", "无转储文件")
	} else {
		kv("蓝屏记录", fmt.Sprintf("共 %d 次（列出最近 5 次）", len(dumps)))
		for i, d := range dumps {
			if i >= 5 {
				break
			}
			if !d.Valid {
				line("           %s  %s（解析失败：%s）", d.Time, d.Name, shortErrStr(d.Err))
				continue
			}
			drv := "未定位"
			if len(d.Drivers) > 0 {
				names := make([]string, 0, len(d.Drivers))
				for _, x := range d.Drivers {
					names = append(names, x.Name)
				}
				drv = strings.Join(names, " / ")
			}
			line("           %s  %s %s  嫌疑驱动：%s", d.Time, d.CodeHex, emptyDash(d.Zh), drv)
		}
	}
	if rb := (&App{}).GetPendingReboot(); rb.N > 0 {
		kv("待重启项", fmt.Sprintf("%d 项：%s", rb.N, strings.Join(rb.Items, "、")))
	} else {
		kv("待重启项", "无")
	}
	if s := strings.TrimSpace(note); s != "" {
		line("")
		line("── 用户自述 ──")
		line("%s", s)
	}
	line("")
	line("──────────────────────────────")
	line("由「无名大叔系统优化工具箱」自动生成，供远程服务参考")
	line("1V1远程支持（付费）：%s", svcOrderURL)
	return b.String()
}

// shortErrStr 给字符串型错误做一次清洗（空的显示「未知」）
func shortErrStr(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return "未知"
	}
	return s
}

// ---------- 剪贴板（写文本，供「复制诊断报告」用） ----------

var (
	svcU32               = syscall.NewLazyDLL("user32.dll")
	svcK32               = syscall.NewLazyDLL("kernel32.dll")
	procOpenClipboard    = svcU32.NewProc("OpenClipboard")
	procEmptyClipboard   = svcU32.NewProc("EmptyClipboard")
	procSetClipboardData = svcU32.NewProc("SetClipboardData")
	procCloseClipboard   = svcU32.NewProc("CloseClipboard")
	procGlobalAlloc      = svcK32.NewProc("GlobalAlloc")
	procGlobalLock       = svcK32.NewProc("GlobalLock")
	procGlobalUnlock     = svcK32.NewProc("GlobalUnlock")
)

const (
	cfUnicodeText = 13
	gmemMoveable  = 0x0002
)

// CopyText 把文本写进系统剪贴板；成功返回空串，失败返回原因。
// 剪贴板可能被别的程序短暂占用，所以重试几次。
func (a *App) CopyText(s string) string {
	if s == "" {
		return "内容为空"
	}
	utf, err := syscall.UTF16FromString(s)
	if err != nil {
		return "编码失败：" + err.Error()
	}
	n := uintptr(len(utf) * 2)

	var opened bool
	for i := 0; i < 8; i++ {
		if r, _, _ := procOpenClipboard.Call(0); r != 0 {
			opened = true
			break
		}
		time.Sleep(60 * time.Millisecond)
	}
	if !opened {
		return "剪贴板被其他程序占用，请稍后重试"
	}
	defer procCloseClipboard.Call()

	procEmptyClipboard.Call()
	h, _, _ := procGlobalAlloc.Call(gmemMoveable, n)
	if h == 0 {
		return "内存分配失败"
	}
	ptr, _, _ := procGlobalLock.Call(h)
	if ptr == 0 {
		return "内存锁定失败"
	}
	/* GlobalLock 把内存指针当返回值交出来（uintptr）。直接 uintptr→unsafe.Pointer 会被 go vet
	   判为 unsafe.Pointer 误用；这里用一次类型重解释把指针取回来 —— 二进制语义与转换完全等价。 */
	p := *(*unsafe.Pointer)(unsafe.Pointer(&ptr))
	copy(unsafe.Slice((*byte)(p), n), unsafe.Slice((*byte)(unsafe.Pointer(&utf[0])), n))
	procGlobalUnlock.Call(h)
	if r, _, e := procSetClipboardData.Call(cfUnicodeText, h); r == 0 {
		return "写入剪贴板失败：" + shortErr(e)
	}
	return ""
}
