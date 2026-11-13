package main

/*
更新检查 / 官方原版校验（2026-11-12 误报治理第 7 项：改折中方案，用户拍板）。

更新方式：**软件内只做「检查」** —— 比对服务器清单版本号，有新版时引导用户
去官方发布页（https://opt.3z9w.com/）浏览器下载、自行覆盖。软件本身不再
下载 exe、不再自替换。原因：「下载可执行文件 + 替换自身」是无签名工具被
ML 引擎判为 Malgent（被武器化的开源工具）的核心特征，砍掉这条链是误报
治理里性价比最高的一刀；功能上无损——发布形态本来就是官网 zip 全量覆盖。

2026-11-12 前版本的历史：曾实现「软件内下载 + SHA256 校验 + 复制自身为
updater + -apply-update 自替换」全自动更新，该链路已按用户拍板整体移除
（「不留下蛋的机制」），相关代码见 git 历史。

保留的：
  · CheckUpdate：GET 官方 version.json 只读比对（网络失败绝不显示成「已是最新」）
  · VerifySelf：拿官方清单核对当前 exe 的 SHA256（官方原版校验）
  · CleanUpdateTemp：清掉旧版本全自动更新留在 %TEMP% 的包/updater

清单文件放在用户的发布站上：https://opt.3z9w.com/version.json
（站点根目录 www/wwwroot/opt.3z9w.com/，文件结构见交接说明）

设计红线：
  · 网络失败**绝不**被显示成「已是最新」—— 那是把"没读到"演成"读到了且没问题"，
    和项目里「不许拿 0 顶未获取到」是同一条规矩。失败一律带原因中文回前端。
  · 不写 .cmd 批处理 —— 目标路径里可能带括号/中文（如 D:\...\3z9wopt(v1.1)\），
    批处理的 if/for 块遇到 `)` 会被提前闭合，GBK/UTF-8 也容易翻车（项目里踩过）。
*/

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// 清单地址。改这里要同步改 update_test.go 里的期望。
const updateManifestURL = "https://opt.3z9w.com/version.json"

/*
单测专用开关（项目里同样的做法见 blockedStoreOverride）：
单测要能起本地 httptest 服务把检查 / 失败路径全跑一遍。
*/
var (
	updateManifestURLOverride string // 非空则用它代替正式清单地址
)

func manifestURL() string {
	if updateManifestURLOverride != "" {
		return updateManifestURLOverride
	}
	return updateManifestURL
}

const (
	updateHTTPTimeout = 10 * time.Second // 拉清单：超时短一点，别让界面干等
	updateDirName     = "3z9wOpt_update" // 旧版全自动更新留下的临时目录名（CleanUpdateTemp 用）
)

// 服务器上的 version.json 结构（用户按这个格式发版；实际由 tools/relgen 自动生成）
type updateManifest struct {
	Version   string          `json:"version"`   // 版本号，如 "1.1.0"
	Build     string          `json:"build"`     // 构建号 YYYYMMDD.N（同版本号重建靠它区分先后；可空）
	BuildDate string          `json:"buildDate"` // 构建日期
	Notes     []string        `json:"notes"`     // 更新说明，一行一条
	URL       string          `json:"url"`       // 更新包下载地址（必须 https）
	SHA256    string          `json:"sha256"`    // 更新包的 SHA256（小写十六进制）
	Size      int64           `json:"size"`      // 字节数，可空（0 表示不知道）
	History   []updateOldFile `json:"history"`   // 历史构建，用来校验"当前跑的这份是不是官方原版"
}

type updateOldFile struct {
	Version   string `json:"version"`
	Build     string `json:"build"`
	BuildDate string `json:"buildDate"`
	SHA256    string `json:"sha256"`
	Size      int64  `json:"size"`
}

// UpdateInfo 检查更新结果（前端直接照着渲染，不用二次判断）
type UpdateInfo struct {
	State        string   `json:"state"` // new / latest / offline / badmanifest / dev
	Current      string   `json:"current"`
	CurrentBuild string   `json:"currentBuild"`
	Latest       string   `json:"latest"`
	LatestBuild  string   `json:"latestBuild"`
	BuildDate    string   `json:"buildDate"`
	Notes        []string `json:"notes"`
	URL          string   `json:"url"`
	Size         int64    `json:"size"`
	SHA256       string   `json:"sha256"`
	Message      string   `json:"message"` // 出错时的中文原因（state=offline/badmanifest 时非空）
	CheckedAt    string   `json:"checkedAt"`
}

// VerifyResult 官方原版校验结果
type VerifyResult struct {
	State    string `json:"state"` // ok / mismatch / unknown / offline / dev
	Version  string `json:"version"`
	Local    string `json:"local"`    // 本机这份的 SHA256
	Official string `json:"official"` // 服务器记的官方值
	Message  string `json:"message"`
}

// ---------- 基础工具 ----------

// sha256File 算一个文件的 SHA256（小写十六进制）。GetSelfHash 也走这里，避免两处实现分叉。
func sha256File(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func selfExePath() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	// 解析符号链接 / 8.3 短名，保证跟"用户双击的那个路径"一致
	if p, err2 := filepath.EvalSymlinks(exe); err2 == nil {
		exe = p
	}
	return exe, nil
}

/*
verLess 比较两个版本号：a 比 b 旧返回 true。

	必须按数字段比，不能按字符串比 —— "1.10.0" < "1.9.0" 在字符串比较下是 true（错）。
	段数不齐补 0；无法解析的段当 0。带 -dev 后缀的视为比同号正式版旧。
*/
func verLess(a, b string) bool {
	an, apre := splitVer(a)
	bn, bpre := splitVer(b)
	n := len(an)
	if len(bn) > n {
		n = len(bn)
	}
	for i := 0; i < n; i++ {
		var x, y int
		if i < len(an) {
			x = an[i]
		}
		if i < len(bn) {
			y = bn[i]
		}
		if x != y {
			return x < y
		}
	}
	// 数字段完全一样：正式版 > 预发布版
	return apre && !bpre
}

func splitVer(v string) ([]int, bool) {
	v = strings.TrimSpace(v)
	v = strings.TrimPrefix(v, "v")
	v = strings.TrimPrefix(v, "V")
	pre := false
	if i := strings.IndexAny(v, "-+"); i >= 0 {
		rest := v[i+1:]
		pre = strings.Contains(strings.ToLower(rest), "dev") || strings.Contains(strings.ToLower(rest), "beta") || strings.Contains(strings.ToLower(rest), "rc")
		v = v[:i]
	}
	parts := strings.Split(v, ".")
	out := make([]int, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		n := 0
		for _, c := range p {
			if c < '0' || c > '9' {
				break
			}
			n = n*10 + int(c-'0')
		}
		out = append(out, n)
	}
	return out, pre
}

func updateTempDir() string {
	return filepath.Join(os.TempDir(), updateDirName)
}

// ---------- 拉清单 ----------

func fetchManifest() (updateManifest, error) {
	var m updateManifest
	base := manifestURL()
	sep := "?"
	if strings.Contains(base, "?") {
		sep = "&"
	}
	// 加时间戳：宝塔/CDN 上 json 很容易被缓存住，不加的话用户永远看到旧清单
	reqURL := base + sep + "t=" + strconv.FormatInt(time.Now().Unix(), 10)

	req, err := http.NewRequest(http.MethodGet, reqURL, nil)
	if err != nil {
		return m, errors.New("地址不对：" + err.Error())
	}
	req.Header.Set("User-Agent", "3z9wOpt/"+appVersion)
	req.Header.Set("Cache-Control", "no-cache")

	cli := &http.Client{Timeout: updateHTTPTimeout}
	resp, err := cli.Do(req)
	if err != nil {
		return m, errors.New("连不上更新服务器，检查一下网络（" + trimErr(err) + "）")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return m, fmt.Errorf("更新服务器返回 %d（清单文件可能没上传对位置）", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return m, errors.New("读更新清单失败：" + trimErr(err))
	}
	if err := json.Unmarshal(body, &m); err != nil {
		return m, errors.New("更新清单格式不对（不是合法 JSON），请检查 version.json")
	}
	return m, nil
}

func trimErr(err error) string {
	s := err.Error()
	if len(s) > 160 {
		s = s[:160] + "…"
	}
	return s
}

// 清单自检：缺哪一项都别往下走，否则会拿半份配置去下载/校验
func (m updateManifest) validate() error {
	if strings.TrimSpace(m.Version) == "" {
		return errors.New("更新清单里没有 version 字段")
	}
	if strings.TrimSpace(m.URL) == "" {
		return errors.New("更新清单里没有 url 字段")
	}
	if len(strings.TrimSpace(m.SHA256)) != 64 {
		return errors.New("更新清单里的 sha256 不是 64 位（应该填更新包的完整 SHA256）")
	}
	// build 可空（老清单），但填了就必须是 YYYYMMDD.N，否则同版本重建的先后判断会失效
	if strings.TrimSpace(m.Build) != "" {
		if _, _, ok := parseBuildNum(m.Build); !ok {
			return errors.New("更新清单里的 build 格式不对（应该像 20261019.2）")
		}
	}
	return nil
}

// ---------- 对外接口 ----------

/*
CheckUpdate 去服务器比对版本。前端拿到的 state：

	new        有新版，可以下载
	latest     已经是最新
	dev        当前是开发版（版本号带 -dev），不做更新提示
	offline    没拉到清单（网络/服务器/文件位置问题），Message 里有原因
	badmanifest 拉到了但清单不合法
*/
func (a *App) CheckUpdate() UpdateInfo {
	out := UpdateInfo{Current: appVersion, CurrentBuild: buildNum, CheckedAt: time.Now().Format("2006-01-02 15:04")}
	if strings.Contains(appVersion, "-dev") {
		out.State = "dev"
		out.Message = "当前是开发版，不检查更新"
		return out
	}
	m, err := fetchManifest()
	if err != nil {
		out.State = "offline"
		out.Message = err.Error()
		return out
	}
	if err := m.validate(); err != nil {
		out.State = "badmanifest"
		out.Message = err.Error()
		return out
	}
	out.Latest = m.Version
	out.LatestBuild = m.Build
	out.BuildDate = m.BuildDate
	out.Notes = m.Notes
	out.URL = m.URL
	out.Size = m.Size
	out.SHA256 = m.SHA256
	/* 判定顺序：
	   ① 版本号更高 → 有新版
	   ② 版本号相同、但服务器构建号更新 → 也有新版（改动重建，不用动版本号）
	      清单没有 build 字段（老格式）时不比构建号，否则本机会一直提示更新同一个版本
	   ③ 其余 → 已是最新 */
	if verLess(appVersion, m.Version) {
		out.State = "new"
	} else if !verLess(m.Version, appVersion) && strings.TrimSpace(m.Build) != "" && buildLess(buildNum, m.Build) {
		out.State = "new"
	} else {
		out.State = "latest"
	}
	return out
}

/*
parseBuildNum 拆构建号 "20261019.2" → (20261019, 2, ok)。

	只认「8 位日期 + 可选 .序号」，其它写法一律当"没有构建号"（ok=false）。
	宽松点：允许只有日期（"20261019" → 序号 0），方便手工填。
*/
func parseBuildNum(s string) (int, int, bool) {
	s = strings.TrimSpace(strings.TrimPrefix(strings.TrimPrefix(s, "v"), "V"))
	if s == "" {
		return 0, 0, false
	}
	datePart := s
	seqPart := ""
	if i := strings.IndexByte(s, '.'); i >= 0 {
		datePart, seqPart = s[:i], s[i+1:]
	}
	if len(datePart) != 8 {
		return 0, 0, false
	}
	d := 0
	for _, c := range datePart {
		if c < '0' || c > '9' {
			return 0, 0, false
		}
		d = d*10 + int(c-'0')
	}
	seq := 0
	if seqPart != "" {
		for _, c := range seqPart {
			if c < '0' || c > '9' {
				return 0, 0, false
			}
			seq = seq*10 + int(c-'0')
		}
	}
	return d, seq, true
}

/*
buildLess 构建号 a 比 b 旧返回 true。

	⚠️ 必须按「日期 + 序号」两个数字比，不能当字符串比 —— "20261019.10" < "20261019.9"
	在字符串比较下是 true（错），当天第 10 次发布会被判成老版本，从此收不到更新。

	没有构建号的两边处理得不一样，别写反：
	  · 对方没有（老格式清单）→ false，不比。否则本机会一直提示更新同一个版本；
	  · 自己没有（发布构建之前的老客户端）→ true，视为最旧。这批人正是最该被推
	    一次的人：他们跑的是同版本号的上一次构建，如果按 false 处理，版本号不变
	    就永远收不到提示。升上来一次之后就有构建号了。
*/
func buildLess(a, b string) bool {
	ad, as, aok := parseBuildNum(a)
	bd, bs, bok := parseBuildNum(b)
	if !bok {
		return false
	}
	if !aok {
		return true
	}
	if ad != bd {
		return ad < bd
	}
	return as < bs
}

/*
officialEntries 从清单里挑出「本地这个版本」发布过的所有官方指纹。

	为什么按版本号挑、而不是"任意一条能对上就算"：要能区分
	  · 服务器**根本没记过**这个版本 → 拿不到任何指纹 → unknown（不能冤枉用户"你被改过"）
	  · 服务器记过、但指纹不是这个 → mismatch（这才是真的不一样）
	同一个版本号下允许有多条（每次重建一条），任意一条对上就算官方原版 ——
	这正是把版本号和构建号拆开的意义：重建不覆盖旧指纹。
*/
func officialEntries(m updateManifest, ver string) []updateOldFile {
	var out []updateOldFile
	add := func(v, b, h string, size int64) {
		h = strings.ToLower(strings.TrimSpace(h))
		if len(h) != 64 {
			return // 坏指纹不能当官方值，也不能参与"有没有记录"的判断
		}
		out = append(out, updateOldFile{Version: v, Build: b, SHA256: h, Size: size})
	}
	if m.Version == ver {
		add(m.Version, m.Build, m.SHA256, m.Size)
	}
	for _, h := range m.History {
		if h.Version == ver {
			add(h.Version, h.Build, h.SHA256, h.Size)
		}
	}
	return out
}

// buildLabel 人看的「哪一次构建」：有构建号用构建号，没有就退回日期。
func buildLabel(build, date string) string {
	if b := strings.TrimSpace(build); b != "" {
		return "构建 " + b
	}
	if d := strings.TrimSpace(date); d != "" {
		return "构建 " + d
	}
	return ""
}

/*
VerifySelf 校验「我现在跑的这份是不是官方原版」。

	拿本机 exe 的 SHA256，跟服务器清单里「本机这个版本号」发布过的所有指纹比：
	  只要和其中任意一条对上 → ok（同一个版本号重建过几次，就有几条，都是官方的）
	  服务器一条都没记过 → unknown（说明不了问题，绝不写成"有问题"）
	  记过但对不上 → mismatch

	⚠️ 这里必须按版本号取集合、而不是"清单里任意一条能对上就算"：
	后者会让从没发布过的版本也判成 mismatch，等于冤枉用户"你这份被改过"。
*/
func (a *App) VerifySelf() VerifyResult {
	out := VerifyResult{Version: appVersion}
	exe, err := selfExePath()
	if err != nil {
		out.State = "unknown"
		out.Message = "取不到程序路径，没法校验"
		return out
	}
	local, err := sha256File(exe)
	if err != nil {
		out.State = "unknown"
		out.Message = "读不到本程序文件（可能被杀毒软件占用）：" + trimErr(err)
		return out
	}
	out.Local = local

	if strings.Contains(appVersion, "-dev") {
		out.State = "dev"
		out.Message = "开发版没有官方指纹，无法校验"
		return out
	}

	m, err := fetchManifest()
	if err != nil {
		out.State = "offline"
		out.Message = err.Error()
		return out
	}

	ents := officialEntries(m, appVersion)
	if len(ents) == 0 {
		out.State = "unknown"
		out.Message = "服务器上没有 v" + appVersion + " 的指纹记录，无法核对（换过版本、或清单里 history 还没补这一条）"
		return out
	}
	for _, e := range ents {
		if strings.EqualFold(e.SHA256, local) {
			out.State = "ok"
			out.Official = e.SHA256
			lbl := buildLabel(e.Build, e.BuildDate)
			if lbl != "" {
				out.Message = "跟官方发布的 v" + appVersion + "（" + lbl + "）完全一致"
			} else {
				out.Message = "跟官方发布的 v" + appVersion + " 完全一致"
			}
			return out
		}
	}
	out.Official = ents[0].SHA256
	out.State = "mismatch"
	out.Message = "本机这份跟官方 v" + appVersion + " 的指纹不一样 —— 可能被改过、或者是自己编译的版本"
	return out
}

/*
CleanUpdateTemp 清掉**旧版本全自动更新**留在 %TEMP% 的包 / 更新器（应用启动时调一次）。
本版已无自替换链路，这个函数只为给老版本升级上来的用户擦尾，清完它就是纯遗留兼容。
*/
func CleanUpdateTemp() {
	dir := updateTempDir()
	ents, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range ents {
		p := filepath.Join(dir, e.Name())
		if e.IsDir() {
			continue
		}
		// 只删我们自己的东西：更新包 / 更新器 / 备份
		low := strings.ToLower(e.Name())
		if strings.Contains(low, "3z9wopt") {
			_ = os.Remove(p)
		}
	}
}
