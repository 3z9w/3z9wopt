package main

// ============================================================================
// 3z9wOpt v1.1 · 磁盘删除「可删除性」判断链
//
// 2026-11-02 用户反馈：磁盘空间页只做了「找出内容相同的文件」，没做
// 「哪些能删、哪些不能删」。真实样本：
//
//	D:\DingDing\main\current\plugins\dtwebview\libcef.dll       236502528
//	D:\DingDing\main\current_new\plugins\dtwebview\libcef.dll   236502528
//
// 这是钉钉热更新的双版本目录，MD5 相同，一定会被判成「重复」。而原来的
// 保留规则只是 sort.Strings(paths) 后把 pi==0 当正本 —— current\ 排在
// current_new\ 前面，于是**旧版被保留、正在被 7 个进程加载的新版被当成副本删掉**。
// 删除又是永久删除、不进回收站，钉钉当场报废。
//
// 核心认知：**「内容重复」不等于「有一份是多余的」**。程序自身的多版本目录、
// 组件库、游戏资源、硬链接、卷影副本，都是设计上的冗余，不是用户手抖复制的垃圾。
//
// 本文件补的是一条完整判断链，四级：
//
//	block   永不删除：系统/引导/驱动、被系统保护或正被占用、密钥凭据、
//	         云同步目录（删本地会反向删云端）、版本控制仓库元数据
//	high    默认锁死，可显式解锁后删：程序目录与组件、游戏资源与存档、
//	         浏览器/通讯软件用户数据、备份唯一副本、网络共享位置
//	caution 可勾选，但醒目提示「不可恢复」：个人文档/媒体/压缩包、副本命名
//	safe    可勾选：临时文件、缓存、日志、崩溃转储
//
// 特别处理用户强调的「影响下一次正常使用」：程序/游戏目录里的文件即使当前
// **没有在运行**也照锁不误，不能因为「现在删得掉」就放行。
//
// 复用了项目既有的两处能力，避免两套口径打架：
//   - probeCanDelete()        junk_priv.go：DELETE 句柄实测，判据与真删一致
//   - isProtectedRuntimePath() junk_clean.go：GAC / .NET / DriverStore 硬保护
//
// ⚠️ 后端必须自己复核，不能只信前端：Wails 绑定是可以被直接调用的。
// ============================================================================

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"golang.org/x/sys/windows/registry"
)

const (
	riskBlock   = "block"   // 永不删除
	riskHigh    = "high"    // 默认锁死，需显式解锁
	riskCaution = "caution" // 可勾选，醒目提示
	riskSafe    = "safe"    // 可勾选，自动生成/可重建
)

// FileRisk 一条「这个文件能不能删」的结论，前端直接拿来渲染。
type FileRisk struct {
	Path   string   `json:"path"`
	Level  string   `json:"level"`
	Reason string   `json:"reason"`
	Tags   []string `json:"tags"`
	InUse  bool     `json:"inUse"` // 正被程序占用（动态信号）
	Keep   bool     `json:"keep"`  // 同组里建议保留的正本
}

func newRisk(path, level, tag, reason string, inUse bool) FileRisk {
	return FileRisk{
		Path: path, Level: level, Reason: reason,
		Tags: []string{tag}, InUse: inUse,
	}
}

// ---------- 规则表 ----------
// 命中即定级，顺序 = 优先级。parts 统一是「带前后反斜杠的路径片段」，
// 全部小写，不写死盘符（系统盘不在 C: 照样生效，沿用项目的铁律）。
type riskRule struct {
	tag    string   // 分类名，前端显示成徽标
	level  string   // 风险级别
	reason string   // 给用户看的人话
	parts  []string // 路径片段
	exts   []string // 扩展名（含点）
	files  []string // 精确文件名
}

func (r riskRule) hit(seg, base, ext string) bool {
	for _, f := range r.files {
		if base == f {
			return true
		}
	}
	for _, e := range r.exts {
		if ext == e {
			return true
		}
	}
	for _, p := range r.parts {
		if strings.Contains(seg, p) {
			return true
		}
	}
	return false
}

func firstRule(rules []riskRule, seg, base, ext string) *riskRule {
	for i := range rules {
		if rules[i].hit(seg, base, ext) {
			return &rules[i]
		}
	}
	return nil
}

func fromRule(path string, r *riskRule, inUse bool) FileRisk {
	return newRisk(path, r.level, r.tag, r.reason, inUse)
}

// L1 —— 系统 / 引导 / 驱动 / NTFS 元文件：删了可能蓝屏、无法开机。
var blockSystemRules = []riskRule{
	{tag: "系统文件", level: riskBlock,
		reason: "Windows 系统 / 引导 / 驱动文件：删除后可能蓝屏、无法开机或系统功能失效，一律禁止",
		files: []string{"pagefile.sys", "hiberfil.sys", "swapfile.sys", "bootmgr", "ntldr",
			"bootnxt", "bootsect.bak", "bcd", "io.sys", "msdos.sys", "config.sys", "autoexec.bat", "boot.sdi"},
		parts: []string{`\windows\`, `\system32\`, `\syswow64\`, `\winsxs\`, `\driverstore\`,
			`\drivers\`, `\boot\`, `\efi\`, `\recovery\`, `\system volume information\`,
			`\$recycle.bin\`, `\windowsapps\`, `\programdata\microsoft\windows\`,
			`\system32\config\`, `\fonts\`, `\ntfs\`, `\$mft`, `\$boot`, `\$logfile`,
			`\$volume`, `\$attrdef`, `\$badclus`, `\$bitmap`, `\$secure`, `\$upcase`,
			`\$extend\`, `\system restore\`},
		exts: []string{".sys", ".drv", ".efi", ".vxd", ".mof", ".cat", ".inf"}},
	{tag: "BitLocker", level: riskBlock,
		reason: "BitLocker 恢复密钥 / 加密元数据：删除后磁盘可能永久无法解锁",
		exts:  []string{".bek"}, parts: []string{`\bitlocker\`, `\recovery key\`}},
}

// L2 —— 后果不可逆、且用户几乎不可能自己意识到的几类数据。
var blockDataRules = []riskRule{
	{tag: "云同步", level: riskBlock,
		reason: "云同步目录：删除本地文件会被同步成「删除云端副本」，可能连云端一起丢，一律禁止",
		parts: []string{`\onedrive\`, `\onedrive -`, `\dropbox\`, `\google drive\`, `\iclouddrive\`,
			`\坚果云\`, `\nutstore\`, `\百度网盘\`, `\baidunetdisk\`, `\阿里云盘\`,
			`\aliyundrive\`, `\腾讯微云\`, `\weiyun\`, `\nextcloud\`, `\seafile\`,
			`\syncthing\`, `\pcloud\`, `\同步空间\`, `\同步盘\`}},
	{tag: "密钥凭据", level: riskBlock,
		reason: "密钥 / 凭据 / 钱包文件：删除后可能无法登录、解密历史数据或找回资产，一律禁止",
		files: []string{"id_rsa", "id_ed25519", "id_ecdsa", "id_dsa", ".git-credentials",
			"credentials", "wallet.dat", "keystore.json", "known_hosts"},
		parts: []string{`\.ssh\`, `\.gnupg\`, `\.aws\`, `\.kube\`, `\.docker\`, `\.git-credentials`},
		exts:  []string{".kdbx", ".kdb", ".p12", ".pfx", ".pem", ".key", ".keystore", ".jks", ".asc", ".gpg"}},
	{tag: "版本库", level: riskBlock,
		reason: "版本控制仓库元数据：删掉后整个提交历史、分支与未推送记录不可恢复，一律禁止",
		parts: []string{`\.git\`, `\.svn\`, `\.hg\`, `\.bzr\`}},
	{tag: "虚拟机/邮箱", level: riskBlock,
		reason: "虚拟机磁盘 / 邮箱 / 数据库主文件：这是数据的唯一载体，删除不可恢复，一律禁止",
		exts: []string{".vmdk", ".vhdx", ".vhd", ".vdi", ".qcow2", ".vmx", ".nvram",
			".mdf", ".ldf", ".edb", ".pst", ".ost"}},
}

// L3 —— 程序本体 / 运行时 / 组件。即使当前没在运行也锁。
var highAppRules = []riskRule{
	{tag: "软件多版本", level: riskHigh,
		reason: "软件热更新 / 多版本目录（current、current_new 等）：同名文件常常两份都在被程序使用，不是手工复制的重复文件，删除可能让软件下次启动失败",
		parts: []string{`\current_new\`, `\current_old\`, `\current\`, `\new_version\`, `\old_version\`}},
	{tag: "程序目录", level: riskHigh,
		reason: "软件安装目录：这里的文件由软件自身管理，即使现在没在运行，删除后软件下次启动也可能报错或需要重装",
		parts: append([]string{
			`\program files\`, `\program files (x86)\`, `\programdata\`,
			`\appdata\local\programs\`, `\appdata\roaming\`,
		}, knownAppDirs...)},
	{tag: "程序组件", level: riskHigh,
		reason: "程序运行所需的组件 / 库文件：删除后对应软件下次启动可能报错、缺功能甚至打不开",
		parts: []string{`\bin\`, `\bin64\`, `\bin32\`, `\plugins\`, `\plugin\`, `\lib\`, `\libs\`,
			`\runtime\`, `\engine\`, `\binaries\`, `\win64\`, `\win32\`, `\x64\`, `\x86\`,
			`\resources\`, `\modules\`, `\components\`, `\flutter\`, `\dtwebview\`, `\cef\`,
			`\node_modules\`, `\jre\`, `\jdk\`, `\site-packages\`, `\microsoft.net\`,
			`\vcredist\`, `\directx\`, `\gac\`, `\gac_32\`, `\gac_64\`, `\gac_msil\`,
			`\assembly\`, `\addins\`, `\extensions\`, `\flutter\`},
		exts: []string{".exe", ".dll", ".ocx", ".so", ".node", ".pyd", ".jar", ".tlb", ".ax",
			".cpl", ".scr", ".msc", ".mui", ".nls", ".winmd", ".manifest"}},
	{tag: "配置授权", level: riskHigh,
		reason: "软件配置 / 授权 / 卸载信息：删除后可能丢失激活状态、设置，或再也无法正常卸载",
		parts: []string{`\config\`, `\conf\`, `\settings\`, `\license\`, `\licenses\`, `\prefs\`},
		files: []string{"unins000.exe", "unins000.dat", "uninstall.dat", "license.dat", "desktop.ini"},
		exts:  []string{".lic", ".properties"}},
	{tag: "正在下载", level: riskHigh,
		reason: "未下载完的临时文件：删除会中断下载，需要重新下",
		exts:  []string{".part", ".crdownload", ".!ut", ".downloading", ".dltemp"}},
}

// knownAppDirs 已知软件 / 游戏厂商目录（含中文目录名）。命中即按程序目录处理。
// 不写死盘符，也不要求恰好是某一级目录。
var knownAppDirs = []string{
	`\dingding\`, `\钉钉\`, `\awesun\`, `\oray\`, `\sunlogin\`, `\向日葵\`, `\todesk\`,
	`\wechat\`, `\微信\`, `\weixin\`, `\企业微信\`, `\tencent\`, `\腾讯\`, `\qq\`,
	`\feishu\`, `\飞书\`, `\lark\`, `\netease\`, `\网易\`, `\cloudmusic\`, `\网易云音乐\`,
	`\bilibili\`, `\哔哩哔哩\`, `\douyin\`, `\抖音\`, `\capcut\`, `\剪映\`,
	`\baidu\`, `\百度\`, `\xunlei\`, `\迅雷\`, `\360\`, `\360se\`, `\qihoo\`,
	`\kingsoft\`, `\wps\`, `\adobe\`, `\acrobat\`, `\photoshop\`,
	`\microsoft office\`, `\office\`, `\oracle\`, `\java\`, `\python\`, `\nodejs\`,
	`\git\`, `\visual studio\`, `\jetbrains\`, `\pycharm\`, `\idea\`,
	`\steam\`, `\steamapps\`, `\epic games\`, `\epicgames\`, `\riot games\`,
	`\league of legends\`, `\英雄联盟\`, `\blizzard\`, `\battle.net\`, `\battlenet\`,
	`\overwatch\`, `\守望先锋\`, `\origin\`, `\ea games\`, `\ubisoft\`, `\uplay\`,
	`\gog galaxy\`, `\xboxgames\`, `\genshin impact\`, `\原神\`, `\mihoyo\`,
	`\hoyoverse\`, `\米哈游\`, `\honkai\`, `\崩坏\`, `\unrealengine\`, `\unity\`,
	`\unityhub\`, `\godot\`, `\obs-studio\`, `\discord\`, `\telegram\`, `\slack\`,
	`\zoom\`, `\google\chrome\`, `\microsoft\edge\`, `\mozilla\`, `\firefox\`,
	`\vmware\`, `\virtualbox\`, `\docker\`, `\nvidia\`, `\amd\`, `\intel\`,
	`\realtek\`, `\logitech\`, `\razer\`,
}

// 注意：不要把「本软件自己的目录名」写进这张表 —— 项目源码 / 构建临时目录
// 都可能叫这个名字，会把正常文件全锁死。真正在运行的 exe 由 probeCanDelete
// 的「正被占用」动态层拦住，更准也更不会误伤。

// L4 —— 游戏资源与存档：动辄几十 GB，删了要整包重下，进度也没了。
var highGameRules = []riskRule{
	{tag: "游戏资源", level: riskHigh,
		reason: "游戏资源 / 数据包：删除后游戏下次启动会校验失败、缺贴图或无法进入，通常要重新下载整包",
		parts: []string{`\steamapps\`, `\content\paks\`, `\paks\`, `\binaries\win64\`,
			`\assets\`, `\shaders\`, `\maps\`, `\levels\`, `\mods\`, `\汉化\`, `\localization\`},
		exts: []string{".pak", ".uasset", ".umap", ".unity3d", ".bundle", ".wad", ".asset",
			".assets", ".pck", ".vpk", ".arc", ".bnk", ".wem", ".bank", ".toc", ".cas",
			".xpk", ".obb", ".rpf", ".big", ".fsb", ".upk", ".acf"}},
	{tag: "游戏存档", level: riskHigh,
		reason: "游戏存档 / 进度：删除后进度不可恢复",
		parts: []string{`\savedata\`, `\savegames\`, `\saves\`, `\save\`, `\userdata\`, `\profiles\`},
		exts:  []string{".sav", ".save", ".slot"}},
}

// L5 —— 用户数据资产：删了等于把软件恢复出厂设置 / 丢资料。
var highUserDataRules = []riskRule{
	{tag: "软件用户数据", level: riskHigh,
		reason: "浏览器 / 通讯软件的用户数据（书签、密码库、聊天记录、会话）：删除后等于把软件恢复出厂设置",
		parts: []string{`\user data\`, `\profiles\`, `\fileStorage\`, `\filestorage\`,
			`\msg\`, `\wechat files\`, `\微信文件\`, `\appdata\roaming\tencent\`,
			`\appdata\local\google\chrome\`, `\appdata\local\microsoft\edge\`},
		exts: []string{".sqlite", ".sqlite3", ".db", ".mdb", ".accdb", ".ldb"}},
	{tag: "备份文件", level: riskHigh,
		reason: "备份 / 系统镜像：这可能就是唯一一份备份，删除后无法回滚",
		parts: []string{`\backup\`, `\备份\`, `\系统镜像\`, `\还原点\`},
		exts:  []string{".wim", ".gho", ".vib", ".bkf", ".spf"}},
	{tag: "项目源码", level: riskHigh,
		reason: "项目 / 源码目录：删除后工作成果不可恢复",
		parts: []string{`\workspace\`, `\projects\`, `\我的项目\`, `\source\`},
		exts:  []string{".sln", ".vcxproj", ".csproj", ".gradle", ".ipynb"}},
}

// L6 —— 真缓存 / 临时文件：程序生成、可自动重建。
// 注意：组件型扩展名（.dll/.pak/...）即使在缓存目录里也不降级，避免游戏着色器
// 缓存里混着真资源时被误放行。这条在 L3/L4/L5 之前作为「例外」先判。
var safeRules = []riskRule{
	{tag: "临时缓存", level: riskSafe,
		reason: "临时文件 / 缓存 / 日志：由程序生成，删除后通常会自动重建",
		parts: []string{`\temp\`, `\tmp\`, `\cache\`, `\caches\`, `\cachestorage\`,
			`\gpucache\`, `\shadercache\`, `\code cache\`, `\thumbnails\`, `\thumbs\`,
			`\crashdumps\`, `\logs\`, `\log\`, `\minidump\`},
		files: []string{"thumbs.db"},
		exts:  []string{".tmp", ".temp", ".log", ".dmp", ".etl"}},
}

// componentExts 组件型扩展名：命中时不适用「缓存例外」，仍要走程序/游戏规则。
var componentExts = func() map[string]bool {
	m := map[string]bool{}
	for _, r := range append(append([]riskRule{}, highAppRules...), highGameRules...) {
		for _, e := range r.exts {
			m[e] = true
		}
	}
	return m
}()

// L7 —— 普通个人文件：可勾选，但必须明说不可恢复。
var cautionRules = []riskRule{
	{tag: "个人文件", level: riskCaution,
		reason: "个人文件：删除后不可恢复，请确认不再需要",
		parts: []string{`\desktop\`, `\桌面\`, `\documents\`, `\文档\`, `\downloads\`,
			`\下载\`, `\pictures\`, `\图片\`, `\videos\`, `\视频\`, `\music\`, `\音乐\`},
		exts: []string{".mp4", ".mkv", ".avi", ".mov", ".wmv", ".flv", ".webm", ".ts",
			".jpg", ".jpeg", ".png", ".gif", ".bmp", ".heic", ".webp", ".tif", ".tiff",
			".mp3", ".flac", ".wav", ".aac", ".m4a", ".ogg", ".pdf", ".doc", ".docx",
			".xls", ".xlsx", ".ppt", ".pptx", ".zip", ".7z", ".rar", ".iso", ".txt",
			".md", ".epub", ".torrent", ".apk", ".dmg", ".msi", ".bak"}},
}

// copyNameParts 手工复制痕迹。只影响「同组里保留哪一份」，不单独降级。
var copyNameParts = []string{"副本", "备份", "- copy", "copy of", "(1)", "(2)", "(3)",
	"(4)", " (1)", " (2)", "_old", "-old", "_bak", "- 副本", "复件"}

func hasCopyName(base string) bool {
	for _, s := range copyNameParts {
		if strings.Contains(base, s) {
			return true
		}
	}
	return false
}

func tempLikePath(norm string) bool {
	for _, s := range []string{`\temp\`, `\tmp\`, `\cache\`, `\logs\`, `\log\`} {
		if strings.Contains(norm+`\`, s) {
			return true
		}
	}
	return false
}

// ---------- 上下文：开机自启 / 系统服务引用的路径 ----------

type riskCtx struct {
	refs map[string]bool // 小写、规范化后的完整路径
}

var (
	riskMu    sync.Mutex
	riskCache *riskCtx
	riskAt    time.Time
)

// currentRiskCtx 注册表引用只查一次，60 秒内复用（服务状态会变，不做永久缓存）。
func currentRiskCtx() *riskCtx {
	riskMu.Lock()
	defer riskMu.Unlock()
	if riskCache != nil && time.Since(riskAt) < 60*time.Second {
		return riskCache
	}
	riskCache = &riskCtx{refs: loadRefPaths()}
	riskAt = time.Now()
	return riskCache
}

// loadRefPaths 收集「删了下次开机就少个东西」的引用路径：
// HKLM/HKCU Run + RunOnce + 所有服务的 ImagePath。读不到就静默跳过，
// 这一层只是补强，主判断链不依赖它。
func loadRefPaths() map[string]bool {
	out := map[string]bool{}
	add := func(s string) {
		for _, p := range exePathsFromCommand(s) {
			out[p] = true
		}
	}
	for _, root := range []registry.Key{registry.LOCAL_MACHINE, registry.CURRENT_USER} {
		for _, sub := range []string{
			`Software\Microsoft\Windows\CurrentVersion\Run`,
			`Software\Microsoft\Windows\CurrentVersion\RunOnce`,
			`Software\Wow6432Node\Microsoft\Windows\CurrentVersion\Run`,
		} {
			k, err := registry.OpenKey(root, sub, registry.QUERY_VALUE)
			if err != nil {
				continue
			}
			if names, err := k.ReadValueNames(512); err == nil {
				for _, n := range names {
					if v, _, err := k.GetStringValue(n); err == nil {
						add(v)
					}
				}
			}
			k.Close()
		}
	}
	if k, err := registry.OpenKey(registry.LOCAL_MACHINE,
		`SYSTEM\CurrentControlSet\Services`, registry.ENUMERATE_SUB_KEYS); err == nil {
		if names, err := k.ReadSubKeyNames(4096); err == nil {
			for _, n := range names {
				sk, err := registry.OpenKey(k, n, registry.QUERY_VALUE)
				if err != nil {
					continue
				}
				if v, _, err := sk.GetStringValue("ImagePath"); err == nil {
					add(v)
				}
				sk.Close()
			}
		}
		k.Close()
	}
	return out
}

// exePathsFromCommand 从命令行里抠出 exe 路径：处理引号、\??\ 前缀、
// %SystemRoot% 这类环境变量，以及 svchost -k 这种没有 exe 的情况。
func exePathsFromCommand(cmd string) []string {
	s := strings.TrimSpace(cmd)
	if s == "" {
		return nil
	}
	s = strings.ReplaceAll(s, `\??\`, "")
	var cand []string
	if i := strings.IndexByte(s, '"'); i >= 0 {
		if j := strings.IndexByte(s[i+1:], '"'); j >= 0 {
			cand = append(cand, s[i+1:i+1+j])
		}
	}
	for _, tok := range strings.Fields(s) {
		tok = strings.Trim(tok, `"`)
		if strings.HasSuffix(strings.ToLower(tok), ".exe") {
			cand = append(cand, tok)
		}
	}
	var out []string
	for _, p := range cand {
		for _, v := range []string{"SystemRoot", "windir", "ProgramFiles",
			"ProgramFiles(x86)", "SystemDrive", "LOCALAPPDATA", "APPDATA"} {
			p = strings.ReplaceAll(p, "%"+v+"%", os.Getenv(v))
		}
		p = strings.Trim(strings.TrimSpace(p), `"`)
		if strings.HasPrefix(strings.ToLower(p), `systemroot\`) {
			p = filepath.Join(os.Getenv("SystemRoot"), p[len(`systemroot\`):])
		}
		if !filepath.IsAbs(p) {
			continue
		}
		out = append(out, strings.ToLower(filepath.Clean(p)))
	}
	return out
}

// ---------- 单文件评估 ----------

func normalizeRiskPath(path string) string {
	p := strings.ToLower(strings.TrimSpace(path))
	p = strings.ReplaceAll(p, `/`, `\`)
	for len(p) > 3 && strings.HasSuffix(p, `\`) {
		p = p[:len(p)-1]
	}
	return p
}

func isDriveRoot(norm string) bool {
	return len(norm) == 2 && norm[0] >= 'a' && norm[0] <= 'z' && norm[1] == ':'
}

// assessPath 完整判断链。ctx 可为 nil（测试时可注入空上下文）。
func assessPath(path string, ctx *riskCtx) FileRisk {
	raw := strings.TrimSpace(path)
	norm := normalizeRiskPath(raw)
	base := strings.ToLower(filepath.Base(strings.ReplaceAll(raw, `/`, `\`)))
	ext := strings.ToLower(filepath.Ext(base))
	seg := norm + `\` // 保证最后一段也带闭合反斜杠，\windows\ 之类才命中

	if raw == "" || len(norm) < 3 || isDriveRoot(norm) {
		return newRisk(raw, riskBlock, "根目录",
			"路径为空、是磁盘根目录或一级目录，禁止整体删除", false)
	}
	if strings.HasPrefix(raw, `\\`) {
		return newRisk(raw, riskHigh, "网络位置",
			"网络共享 / UNC 位置：删除会影响其他人和本机脱机缓存，默认锁定", false)
	}

	// L1 系统 / 引导 / 驱动
	if r := firstRule(blockSystemRules, seg, base, ext); r != nil {
		return fromRule(raw, r, false)
	}
	// L2 密钥凭据 / 云同步 / 版本库 / 虚拟机
	if r := firstRule(blockDataRules, seg, base, ext); r != nil {
		return fromRule(raw, r, false)
	}

	// L6 例外：真缓存 / 临时文件（组件型扩展名不降级）
	if !componentExts[ext] {
		if r := firstRule(safeRules, seg, base, ext); r != nil {
			return fromRule(raw, r, false)
		}
	}

	// 动态层：项目既有的运行时硬保护 + 服务/启动项引用 + 实测占用
	if isProtectedRuntimePath(raw) {
		return newRisk(raw, riskBlock, "运行时组件",
			".NET / 驱动仓库等运行时组件区：不是缓存，删了不会自动重建，依赖它的程序会静默崩溃", false)
	}
	if ctx != nil && ctx.refs[norm] {
		return newRisk(raw, riskBlock, "开机自启",
			"系统服务或开机启动项正在引用这个程序：删除后下次开机该服务/软件将无法启动", true)
	}
	switch probeCanDelete(raw) {
	case "locked":
		return newRisk(raw, riskBlock, "正在使用",
			"文件正被程序占用（运行的程序、已加载的 DLL 或服务）：删除会失败，且强删可能让软件崩溃。关闭相关程序后刷新可重试", true)
	case "denied":
		return newRisk(raw, riskBlock, "系统保护",
			"受系统保护（TrustedInstaller 等）：当前权限删不掉，也不应绕过保护强删", false)
	}

	// L3 程序 / 运行时 / 组件
	if r := firstRule(highAppRules, seg, base, ext); r != nil {
		return fromRule(raw, r, false)
	}
	// L4 游戏资源与存档
	if r := firstRule(highGameRules, seg, base, ext); r != nil {
		return fromRule(raw, r, false)
	}
	// L5 用户数据资产
	if r := firstRule(highUserDataRules, seg, base, ext); r != nil {
		return fromRule(raw, r, false)
	}
	// L7 普通个人文件
	if r := firstRule(cautionRules, seg, base, ext); r != nil {
		return fromRule(raw, r, false)
	}
	if hasCopyName(base) {
		return newRisk(raw, riskCaution, "疑似副本",
			"文件名像是手工复制的副本，但仍请确认：删除后不可恢复", false)
	}
	return newRisk(raw, riskCaution, "普通文件",
		"普通文件：删除后不可恢复，请确认不再需要", false)
}

// ---------- 重复文件组 ----------

func riskRank(level string) int {
	switch level {
	case riskBlock:
		return 0
	case riskHigh:
		return 1
	case riskCaution:
		return 2
	case riskSafe:
		return 3
	}
	return 2
}

// keepBetter 判断 a 是否比 b 更该被保留为「正本」。
// 关键：**越不能删的那份越该留**（原来的实现是路径字母序第一份，完全没考虑这个）。
func keepBetter(a, b FileRisk) bool {
	ra, rb := riskRank(a.Level), riskRank(b.Level)
	if ra != rb {
		return ra < rb
	}
	if a.InUse != b.InUse {
		return a.InUse // 正在被使用的那份优先保留
	}
	ca, cb := hasCopyName(strings.ToLower(filepath.Base(a.Path))),
		hasCopyName(strings.ToLower(filepath.Base(b.Path)))
	if ca != cb {
		return !ca // 不像副本的那份优先
	}
	ta, tb := tempLikePath(normalizeRiskPath(a.Path)), tempLikePath(normalizeRiskPath(b.Path))
	if ta != tb {
		return !ta // 不在临时目录的那份优先
	}
	ma, mb := fileMTime(a.Path), fileMTime(b.Path)
	if ma != mb {
		return ma > mb // 新版本优先（软件热更新目录 current/current_new 靠这条）
	}
	return a.Path < b.Path
}

func fileMTime(path string) int64 {
	if st, err := os.Stat(path); err == nil {
		return st.ModTime().UnixNano()
	}
	return 0
}

func chooseKeep(rs []FileRisk) int {
	if len(rs) == 0 {
		return -1
	}
	best := 0
	for i := 1; i < len(rs); i++ {
		if keepBetter(rs[i], rs[best]) {
			best = i
		}
	}
	return best
}

// assessDupGroups 给每组补上逐路径风险 + 正本索引。findDuplicates 产出结果后调用。
func assessDupGroups(groups []DupGroup) {
	ctx := currentRiskCtx()
	for i := range groups {
		g := &groups[i]
		g.Risks = make([]FileRisk, len(g.Paths))
		for j, p := range g.Paths {
			g.Risks[j] = assessPath(p, ctx)
		}
		g.Keep = chooseKeep(g.Risks)
		if g.Keep >= 0 && g.Keep < len(g.Risks) {
			g.Risks[g.Keep].Keep = true
		}
	}
}

// SpaceAssess 供目录分析 / 大文件页在删除前批量评估（一次绑定调用）。
func (a *App) SpaceAssess(paths []string) []FileRisk {
	ctx := currentRiskCtx()
	out := make([]FileRisk, 0, len(paths))
	for _, p := range paths {
		out = append(out, assessPath(p, ctx))
	}
	return out
}
