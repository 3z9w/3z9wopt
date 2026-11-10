package main

import (
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
	svcQrDir      = "frontend/dist/"                       // embed 路径前缀（main.go 里 go:embed all:frontend/dist）
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
	svcGithubURL  = "https://github.com/3z9w/3z9wopt"          // GitHub 开源仓库（关于页入口；只走 GetAboutInfo，别放 SvcLinks）
	// 反馈面板（2026-11-10 只留三项）：GitHub Issues / 邮件 / QQ 群，都不放进 SvcLinks
	svcIssuesURL    = "https://github.com/3z9w/3z9wopt/issues/new" // 直达「提交问题」页（不是 issues 列表）
	svcFeedbackMail = "64756341@qq.com"                         // 邮件反馈地址（复制用）
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
	GithubURL  string `json:"github"`    // GitHub 开源仓库（外部地址，只走 GetAboutInfo，别放 SvcLinks）
	IssuesURL  string `json:"issues"`    // GitHub Issues（反馈面板提交问题）
	Email      string `json:"email"`     // 邮件反馈地址（反馈面板复制用）
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
		GithubURL:  svcGithubURL,
		IssuesURL:  svcIssuesURL,
		Email:      svcFeedbackMail,
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

// ---------- 剪贴板（写文本） ----------

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
