package main

// 3z9wOpt v1.1.0 入口（Wails）。
// 带 -cli 参数时不启窗口，直接跑命令行自检（真机扫描，打印分组 / 去重合计 / 耗时），排障用。

import (
	"bytes"
	"context"
	"embed"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/wailsapp/wails/v2"
	"github.com/wailsapp/wails/v2/pkg/options"
	"github.com/wailsapp/wails/v2/pkg/options/assetserver"
	"github.com/wailsapp/wails/v2/pkg/options/windows"
	wailsruntime "github.com/wailsapp/wails/v2/pkg/runtime"
)

//go:embed all:frontend/dist
var assets embed.FS

// ── 前端真源直接嵌进来（v1.1.0）────────────────────────────────────────────
// design/ui-prototype.html 是重构 1.1 时的前端本体（浏览器直开就是设计预览），
// frontend/dist/index.html 只是它的副本。两份手工维护必然分叉 —— 2026-10-09 那次
// 「68.58 GB 假数据」就有这个成分：修好的补丁只打在 dist 里，真源里没有；而站长看到的是 exe，
// exe 读的又是 dist。现在改成 exe 直接服务真源，从此不需要「复制到 dist」这一步，
// 也不可能再出现两边不一致。dist 仍嵌着，留给以后可能加的其它静态资源用。

//go:embed design/ui-prototype.html
var pageHTML []byte

/* 本机配置页拆成了独立文件（2026-10-10）：这一页后面要反复改，
   塞在 5000+ 行的大文件里互相干扰、不好回滚。
   pcinfo.js / pcinfo.css 与真源页面放在同一个 design/ 目录，
   通过下面的中间件按路径提供 —— 不走 assets embed，
   这样"真源 + 拆出来的文件"永远在同一处，不会出现改了 A 忘了同步 B。 */
//
//go:embed design/pcinfo.js
var pcinfoJS []byte

//go:embed design/pcinfo.css
var pcinfoCSS []byte

/* 系统优化页也拆成了独立文件（2026-10-11 页面重做时搬出来）：
   这一页 154 项、分类折叠 + 勾选 + 右栏待应用清单，改动频繁，理由同本机配置页。 */
//
//go:embed design/opt.js
var optJS []byte

//go:embed design/opt.css
var optCSS []byte

// 应用卸载页（2026-10-11 独立成模块：design/uninst.js + design/uninst.css）
//go:embed design/uninst.js
var uninstJS []byte

//go:embed design/uninst.css
var uninstCSS []byte

// 磁盘空间页（2026-10-11 独立成模块：design/disk.js + design/disk.css）
//go:embed design/disk.js
var diskJS []byte

//go:embed design/disk.css
var diskCSS []byte

// 启动项管理页（2026-10-12 独立成模块：design/startup.js + design/startup.css）
//go:embed design/startup.js
var startupJS []byte

//go:embed design/startup.css
var startupCSS []byte

// 驱动管理页（2026-10-12 独立成模块：design/driver.js + design/driver.css）
//go:embed design/driver.js
var driverJS []byte

//go:embed design/driver.css
var driverCSS []byte

// 蓝屏分析页（2026-10-12 独立成模块：design/bsod.js + design/bsod.css）
//go:embed design/bsod.js
var bsodJS []byte

//go:embed design/bsod.css
var bsodCSS []byte

// 工具库页（2026-10-12 布局重做并独立成模块：design/tools.js + design/tools.css）
//go:embed design/tools.js
var toolsJS []byte

//go:embed design/tools.css
var toolsCSS []byte

// 服务大厅页（2026-10-13 拆成独立模块：design/svc.js + design/svc.css）
//go:embed design/svc.js
var svcJS []byte

//go:embed design/svc.css
var svcCSS []byte

// 关于页（2026-10-13 拆成独立模块：design/about.js + design/about.css）
//go:embed design/about.js
var aboutJS []byte

//go:embed design/about.css
var aboutCSS []byte

// 设置面板（2026-10-13 拆成独立模块：design/setpanel.js + design/setpanel.css）
//go:embed design/setpanel.js
var setpanelJS []byte

//go:embed design/setpanel.css
var setpanelCSS []byte

// designPage 把真源页面的标题换成软件名（原型文件里保留「UI 原型」标题，便于浏览器预览辨认）。
var designPage = bytes.Replace(pageHTML,
	[]byte("<title>3z9wOpt v1.1.0 · UI 原型</title>"),
	[]byte("<title>无名大叔系统优化工具箱 v1.1.0</title>"), 1)

// serveDesignPage 让 "/" 与 "/index.html" 直接返回真源页面。
// 必须挂在 Wails 资产服务器的 Middleware 里：Wails 会在它之外再包一层，
// 把 runtime.js / ipc.js 注入 <head>（pkg/assetserver/assetserver.go processIndexHTML），
// window.go 才会在页面脚本执行前就位。
func serveDesignPage(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 本机配置页拆出来的独立文件。必须在这里供出去：主页面是用同步 XHR
		// 读 pcinfo.js 的（Wails 只对 /index.html 注入 runtime.js，
		// 外链 async script 会错过注入时序）。
		if r.Method == http.MethodGet {
			switch r.URL.Path {
			case "/pcinfo.js":
				w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
				_, _ = w.Write(pcinfoJS)
				return
			case "/pcinfo.css":
				w.Header().Set("Content-Type", "text/css; charset=utf-8")
				_, _ = w.Write(pcinfoCSS)
				return
			case "/opt.js":
				w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
				_, _ = w.Write(optJS)
				return
			case "/opt.css":
				w.Header().Set("Content-Type", "text/css; charset=utf-8")
				_, _ = w.Write(optCSS)
				return
			case "/uninst.js":
				w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
				_, _ = w.Write(uninstJS)
				return
			case "/uninst.css":
				w.Header().Set("Content-Type", "text/css; charset=utf-8")
				_, _ = w.Write(uninstCSS)
				return
			case "/disk.js":
				w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
				_, _ = w.Write(diskJS)
				return
			case "/disk.css":
				w.Header().Set("Content-Type", "text/css; charset=utf-8")
				_, _ = w.Write(diskCSS)
				return
			case "/startup.js":
				w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
				_, _ = w.Write(startupJS)
				return
			case "/startup.css":
				w.Header().Set("Content-Type", "text/css; charset=utf-8")
				_, _ = w.Write(startupCSS)
				return
			case "/driver.js":
				w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
				_, _ = w.Write(driverJS)
				return
			case "/driver.css":
				w.Header().Set("Content-Type", "text/css; charset=utf-8")
				_, _ = w.Write(driverCSS)
				return
			case "/bsod.js":
				w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
				_, _ = w.Write(bsodJS)
				return
			case "/bsod.css":
				w.Header().Set("Content-Type", "text/css; charset=utf-8")
				_, _ = w.Write(bsodCSS)
				return
			case "/tools.js":
				w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
				_, _ = w.Write(toolsJS)
				return
			case "/tools.css":
				w.Header().Set("Content-Type", "text/css; charset=utf-8")
				_, _ = w.Write(toolsCSS)
				return
			case "/svc.js":
				w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
				_, _ = w.Write(svcJS)
				return
			case "/svc.css":
				w.Header().Set("Content-Type", "text/css; charset=utf-8")
				_, _ = w.Write(svcCSS)
				return
			case "/about.js":
				w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
				_, _ = w.Write(aboutJS)
				return
			case "/about.css":
				w.Header().Set("Content-Type", "text/css; charset=utf-8")
				_, _ = w.Write(aboutCSS)
				return
			case "/setpanel.js":
				w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
				_, _ = w.Write(setpanelJS)
				return
			case "/setpanel.css":
				w.Header().Set("Content-Type", "text/css; charset=utf-8")
				_, _ = w.Write(setpanelCSS)
				return
			}
		}
		if r.Method == http.MethodGet && (r.URL.Path == "/" || r.URL.Path == "/index.html") {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			_, _ = w.Write(designPage)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// noCache 资产响应禁用缓存：保证重编译/发新版后前端立即生效，不被 WebView2 旧缓存坑
func noCache(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")
		w.Header().Set("Pragma", "no-cache")
		next.ServeHTTP(w, r)
	})
}

/* devPage 开发/验收用：3z9wOpt_v1.1.0.exe -devpage <页面id> 启动后直接跳到那一页。
   为什么需要：GUI 截图验收如果靠"点侧栏坐标"，会被侧栏滚动位置影响（同一坐标这次点到这里、
   下次点到别处，2026-10-10 实测踩过两次）。有了这个开关，验证任意一页都是确定性的：
       bin\3z9wOpt_v1.1.0.exe -devpage driver   然后截图即可。
   只接受简单的页面 id（字母数字连字符下划线），拼进 JS 前还会再过滤一次。 */
var devPage string

/* devTab 本机配置页要逐个 Tab 截图验收（2026-10-10 加）。
   这一页有 8 个 Tab，Tab 切换是局部的（不整页重绘），
   用「启动后点某个坐标」或注入脚本切都靠不住 —— 时序一差就截到空白页。
   做成确定性开关：bin\3z9wOpt_v1.1.0.exe -devpage pcinfo -devtab disk
   它在 go('pcinfo') 之后、稳定 800ms 才点目标 Tab，截图必然落在目标 Tab 上。 */
var devTab string

/* devMeasure 开发期测量浮层开关（2026-10-10 加）。
   为什么要单独一个开关：本机配置页要求「一屏装完」，验收时想读到精确的
   溢出像素；但给用户看的截图不该带那块红框。所以拆开：
     bin\3z9wOpt_v1.1.0.exe -devpage pcinfo -devtab disk              → 干净截图
     bin\3z9wOpt_v1.1.0.exe -devpage pcinfo -devtab disk -devmeasure  → 带读数 */
var devMeasure = false

// parseDevPage 从命令行取 -devpage <id> 与 -devtab <id>。
// 只接受简单的 id（字母数字连字符下划线）—— 它会被拼进注入的 JS，
// 过滤掉引号括号分号等一切特殊字符，避免变成一条可执行表达式。
func parseDevPage(args []string) string {
	val := func(flag string) string {
		for i, a := range args {
			if a == flag && i+1 < len(args) {
				id := strings.TrimSpace(args[i+1])
				if id == "" {
					return ""
				}
				for _, r := range id {
					if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_') {
						return ""
					}
				}
				return id
			}
		}
		return ""
	}
	devTab = val("-devtab")
	for _, a := range args {
		if a == "-devmeasure" {
			devMeasure = true
		}
	}
	return val("-devpage")
}

// domReady 页面 DOM 就绪后回调：把 -devpage 指定的页面切过去（前端 go() 是全局函数）。
// 2026-10-09 修 B 级「-devpage 截图落点偶尔不稳」：DOM 就绪早于前端脚本定义 go()，
// 单发一次 try{go()} 会扑空、停在首页。改成注入轮询：每 100ms 试一次，go() 可用即跳，
// 最多等 10 秒（真加载故障时不至于死等）。
func (a *App) domReady(ctx context.Context) {
	if devPage == "" {
		return
	}
	logDev("domReady 触发 devPage=" + devPage + " devTab=" + devTab)
	/* 切 Tab：必须整段拼成**一条合法语句**再注入。
	   ⚠️ 2026-10-10 踩过：原先把 tabJS 用 `setTimeout(...)` 追加在 if 块**里**、
	   `}else if` 之前，拼出来是 `...setTimeout(...) else if(...)` —— else 悬空，
	   整段注入脚本 SyntaxError 直接不执行，表现为「-devpage/-devtab 全都不生效、停在首页」。
	   现在把 Tab 那段包成 if 块内的完整语句（用一次性标记防重复排程）。 */
	tabJS := ""
	if devTab != "" {
		tabJS = "if(!window.__devTabDone){window.__devTabDone=1;setTimeout(function(){" +
			"var m=0;var tt=setInterval(function(){" +
			// 选择器带三个页面的 Tab 属性：本机配置页的 [data-pctab]、应用卸载页的 [data-untab]、
			// 启动项管理页的 [data-su-tab]（2026-10-12 加 —— 光靠改默认值验收不了「计划任务」那一栏）
			"var b=document.querySelector('[data-pctab=\"" + devTab + "\"],[data-untab=\"" + devTab + "\"],[data-su-tab=\"" + devTab + "\"]');" +
			"if(b){clearInterval(tt);b.click()}" +
			"else if(++m>120){clearInterval(tt);console.error('devtab timeout')}},100)},1200)}"
	}
	js := "(function(){var n=0;var t=setInterval(function(){" +
		"if(typeof go==='function'){clearInterval(t);try{go('" + devPage + "')}catch(e){console.error('devpage',e)}" +
		tabJS +
		"}else if(++n>100){clearInterval(t);console.error('devpage timeout')}},100)})()"
	wailsruntime.WindowExecJS(ctx, js)
	if devMeasure {
		wailsruntime.WindowExecJS(ctx, measureJS)
	}
	logDev("WindowExecJS 已调用（该 API 无返回值）")
}

/* measureJS 开发期测量浮层（配合 -devtab 用，2026-10-10 加）。
   为什么需要：本机配置页要求「一屏装完」，但到底超出多少、被谁撑高的，
   靠肉眼看截图猜不准（实测猜错过三次）。这段脚本把 .pc-body 的
   clientHeight / scrollHeight / 溢出量、以及每个子块的实际高度
   直接画成右下角浮层 —— 截一张图就能读数，不用再猜。
   只在带 -devtab 启动时注入，正常启动完全不走这条路。 */
const measureJS = `if(!window.__devMeasure){window.__devMeasure=1;
var tries=0;
var iv=setInterval(function(){
  var b=document.querySelector('.pc-body');
  if(!b&&++tries<60){return}
  clearInterval(iv);
  var L=[];
  if(!b){L.push('等 30s 仍没有 .pc-body')}else{
    L.push('视口 '+window.innerHeight);
    L.push('可视 '+b.clientHeight+'  内容 '+b.scrollHeight+'  溢出 '+(b.scrollHeight-b.clientHeight));
    if(b.children.length===1&&b.children[0].children.length){
      L.push('—— 卡片内部 ——');
      var c=b.children[0].children;
      for(var j=0;j<c.length;j++){
        L.push('  '+(c[j].className||'').split(' ')[0]+'  h='+c[j].offsetHeight);
      }
    }else{
      var k=b.children;
      for(var i=0;i<k.length;i++){
        L.push(i+'. '+(k[i].className||'').split(' ')[0]+'  h='+k[i].offsetHeight);
      }
    }
  }
  var d=document.createElement('div');
  d.style.cssText='position:fixed;right:6px;bottom:6px;z-index:999999;background:rgba(190,0,0,.93);color:#fff;font:11px/1.5 monospace;padding:6px 9px;border-radius:6px;white-space:pre;max-width:360px';
  d.textContent=L.join('\n');
  document.body.appendChild(d);
},500)}`

// logDev 调试期落盘日志（exe 是 windowsgui 子系统，看不到 stderr；只给 -devpage 用）
func logDev(s string) {
	f, e := os.OpenFile("_devtools/_devpage.log", os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if e == nil {
		f.WriteString(s + "\n")
		f.Close()
	}
}

/* 版本信息：由构建时注入，源码里不写死 —— 开源后别人 clone 构建，显示的是他们自己的信息。

   ── 两级版本（2026-10-19 定）──────────────────────────────────────────
   ① appVersion 版本号 1.1.0：只表示「功能集合」。攒够一批要对外宣布时才改，
      改一个 bug 重建发布**不用**动它。
   ② buildNum  构建号 20261019.2：日期 + 当天序号，每次发布自动生成、永不复用。
      它是「同版本号下第几次发」，更新判断和官方指纹都靠它区分。

   为什么必须分开：以前只有版本号一个标识，它同时当发布号、更新比较的键、
   官方指纹的键。于是同一个 1.1.0 重建一次，指纹表只能留一份 —— 要么老用户
   收不到更新，要么老用户自查时报「指纹不一样」，两条必然触发一条。

   正式构建（build.cmd release，自动化生成三个 -X）：
     go build -tags desktop,production -ldflags "-s -w -H windowsgui ^
       -X main.appVersion=1.1.0 -X main.buildDate=2026-10-19 ^
       -X main.buildNum=20261019.2" -o bin\3z9wOpt_v1.1.0.exe .
   ⚠️ appVersion 与 buildDate 必须**一起**注入：只给版本不给日期，关于页会显示「构建 unknown」。
   不带 -X（含 build.cmd 的日常构建）显示 1.1.0-dev / unknown，且不参与更新与指纹校验。 */
var (
	appVersion = "1.1.0-dev"
	buildDate  = "unknown"
	buildNum   = "" // 构建号 YYYYMMDD.N；只有发布构建才注入，日常构建为空
)

func main() {
	/* 更新器模式必须最先分流（2026-10-13）：这时主进程还没退，界面不能起来，
	   否则会出现两个窗口抢同一个 WebView2 用户数据目录。 */
	if len(os.Args) > 1 && os.Args[1] == "-apply-update" {
		runUpdateApplier(os.Args[1:])
		return
	}

	devPage = parseDevPage(os.Args[1:])
	for _, a := range os.Args[1:] {
		if a == "-cli" {
			runCliSelfCheck(os.Args[1:])
			return
		}
		if a == "-perf" {
			runCliPerf()
			return
		}
	}

	// 上次更新留下的临时包 / 更新器，开机顺手清掉（删不掉就留着，下次再说）
	CleanUpdateTemp()

	app := NewApp()
	err := wails.Run(&options.App{
		Title:     "无名大叔系统优化工具箱 v1.1",
		Width:     1220,
		Height:    780,
		MinWidth:  1020,
		MinHeight: 660,
		AssetServer: &assetserver.Options{Assets: assets, Middleware: func(next http.Handler) http.Handler {
			return noCache(serveDesignPage(next))
		}},
		// 窗口创建到页面首帧之间，用户看到的就是这个纯色 —— 对齐前端浅色主题的 --bg0(#dde4f2)，
		// 免得开屏先闪一下白再变底色（默认主题是 light，见 design 里 init() 的 setTheme）。
		BackgroundColour: &options.RGBA{R: 221, G: 228, B: 242, A: 255},
		Frameless:        true,
		OnStartup:        app.startup,
		OnDomReady:       app.domReady,
		Bind:             []interface{}{app},
		Windows: &windows.Options{
			/* ⚠️ 别开透明窗口：本机是 Win10 22H2(19045)，没有 Mica 材质，
			   Wails 会回退 ACCENT_ENABLE_BLURBEHIND 整窗毛玻璃，底色直接被桌面透穿。
			   Win10 也没有系统圆角，窗口外形只能保持直角 —— 圆角需求待 Win11 再议。 */
			WebviewIsTransparent:              false,
			WindowIsTranslucent:               false,
			DisableFramelessWindowDecorations: true,
			WebviewGpuIsDisabled:              false,
		},
	})
	if err != nil {
		log.Fatal(err)
	}
}

// runCliSelfCheck 命令行自检：与 GUI 共用同一套后端，打印真机扫描结果。
//
//	3z9wOpt_v1.1.0.exe -cli           全量自检（含 deepjunk 全盘遍历，约十几秒到几分钟）
//	3z9wOpt_v1.1.0.exe -cli -nofull   跳过 deepjunk，快速迭代用
//
// 另把每项明细写到 %TEMP%\3z9wOpt_cli_items.tsv，便于逐项对照"总量是哪一项抬起来的"。
func runCliSelfCheck(args []string) {
	noFull := false
	for _, a := range args {
		if a == "-nofull" {
			noFull = true
		}
	}
	skipCmd := ""
	if noFull {
		skipCmd = "deepjunk"
	}

	cats := junkItems()
	byGroup := map[string]int{}
	warn := 0
	for _, it := range cats {
		byGroup[it.Group]++
		if it.Risk == "warn" {
			warn++
		}
	}
	groups := make([]string, 0, len(byGroup))
	for g := range byGroup {
		groups = append(groups, g)
	}
	sort.Strings(groups)

	fmt.Printf("垃圾清理项：%d 个 / %d 组（谨慎项 %d 个）\n", len(cats), len(groups), warn)
	for _, g := range groups {
		fmt.Printf("  %-12s %2d 项\n", g, byGroup[g])
	}
	fmt.Printf("\n固定磁盘：%v\n", fixedDrives())
	fmt.Printf("驱动解压残留命中：%v\n", driverResDirs())

	// 系统优化目录自检：真数据到位后这里应是 151 项 / 7 组 / 基本 87 / 深度 127
	optTotal, optCats, optBasic, optDeep := OptCatalogCheck()
	fmt.Printf("\n系统优化项：%d 个 / %d 组（基本预设 %d · 深度预设 %d）\n", optTotal, optCats, optBasic, optDeep)
	if optTotal < 100 {
		fmt.Printf("  ⚠️ 当前是占位目录：在 Git Bash 里跑 _devtools/copy-v10.sh opt 拷入 v1.0 的 optimize_data.go\n")
	}

	list, _, _, ms := ScanJunkSync(nil, skipCmd)
	if noFull {
		fmt.Printf("\n真机扫描耗时：%dms（已跳过 deepjunk 全盘遍历）\n", ms)
	} else {
		fmt.Printf("\n真机扫描耗时：%dms（item 级 6 路 × 目录级 4 路 × 每棵树 %d 路，均过全局目录闸门）\n", ms, scanWorkers())
	}

	var naive int64
	for i := range list {
		naive += list[i].Bytes
	}
	uniq, ufiles := TotalUnique(list, nil)
	_, heavyB, _ := TotalUniqueSplit(list, nil)
	fmt.Printf("直接相加（会重复计数）：%.2f GB\n", float64(naive)/1024/1024/1024)
	fmt.Printf("去重后合计（只算一次）：%.2f GB / %d 个目标\n", float64(uniq)/1024/1024/1024, ufiles)
	if naive > 0 {
		fmt.Printf("  （重复计数占比 %.0f%%，即虚高 %.2f GB）\n",
			float64(naive-uniq)/float64(naive)*100, float64(naive-uniq)/1024/1024/1024)
	}
	// 口径分层（与界面同一套）：跟同类工具对比，看下面第一行
	fmt.Printf("  · 可重建缓存/系统垃圾：%.2f GB\n", float64(uniq-heavyB)/1024/1024/1024)
	fmt.Printf("  · 你的文件 / 整包（默认同样勾选）：%.2f GB\n", float64(heavyB)/1024/1024/1024)

	// 分组小计（组内也按路径去重，与界面同口径）
	byGroupItems := map[string][]JunkItem{}
	for _, it := range list {
		byGroupItems[it.Group] = append(byGroupItems[it.Group], it)
	}
	fmt.Println("\n分组小计（组内去重后）：")
	for _, g := range groups {
		items := byGroupItems[g]
		if len(items) == 0 {
			continue
		}
		gb, gh, gn := TotalUniqueSplit(items, nil)
		fmt.Printf("  %-14s %10.2f MB / %d 个目标（其中你的文件·整包 %.2f MB）\n",
			g, float64(gb)/1024/1024, gn, float64(gh)/1024/1024)
	}

	sorted := make([]JunkItem, len(list))
	copy(sorted, list)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].ScanMs > sorted[j].ScanMs })
	fmt.Println("\n扫描最慢 Top 8（找瓶颈）：")
	for i, it := range sorted {
		if i >= 8 || it.ScanMs == 0 {
			break
		}
		fmt.Printf("  %-28s %7dms  %d 个文件 %s\n", it.Name, it.ScanMs, it.Files, heavyMark(it.Heavy))
	}

	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Bytes > sorted[j].Bytes })
	fmt.Println("\n体积 Top 10：")
	for i, it := range sorted {
		if i >= 10 || it.Bytes == 0 {
			break
		}
		fmt.Printf("  %-28s %10.2f MB  %d 个文件 %s\n", it.Name, float64(it.Bytes)/1024/1024, it.Files, heavyMark(it.Heavy))
	}

	// 每项明细落盘（UTF-8 BOM，Excel 直接打开不乱码）
	lines := []string{"id\t组\t名称\t重型\t字节\t文件数\t目标数\tscanMs\t未安装"}
	for i := range list {
		it := &list[i]
		lines = append(lines, fmt.Sprintf("%s\t%s\t%s\t%v\t%d\t%d\t%d\t%d\t%v",
			it.ID, it.Group, it.Name, it.Heavy, it.Bytes, it.Files, len(it.Paths), it.ScanMs, it.Empty))
	}
	dumpPath := filepath.Join(os.TempDir(), "3z9wOpt_cli_items.tsv")
	if err := os.WriteFile(dumpPath, []byte("\ufeff"+strings.Join(lines, "\r\n")+"\r\n"), 0644); err != nil {
		fmt.Printf("\n每项明细写出失败：%v\n", err)
	} else {
		fmt.Printf("\n每项明细（Excel 可直接打开）：%s\n", dumpPath)
	}
}

// heavyMark 命令行里给「你的文件 / 整包」打标记（口径分层与界面同一套）
func heavyMark(heavy bool) string {
	if heavy {
		return "← 你的文件/整包"
	}
	return ""
}

// runCliPerf 实时采样自检：连采两次（间隔 1.2 秒，CPU/磁盘/网络都是速率类计数器，需要差值）。
// 取不到的项一律 -1 —— 界面显示 "--"，绝不编数。
func runCliPerf() {
	app := NewApp()
	fmt.Println("实时性能采样（第一次作基准，1.2 秒后再采一次）")
	app.GetLivePerf()
	time.Sleep(1200 * time.Millisecond)
	p := app.GetLivePerf()

	show := func(name, unit string, v float64) {
		if v < 0 {
			fmt.Printf("  %-10s 读不到（本机没有对应计数器）\n", name)
			return
		}
		fmt.Printf("  %-10s %.1f %s\n", name, v, unit)
	}
	fmt.Println("占用率：")
	show("CPU", "%", p.CPU)
	show("内存", "%", p.Mem)
	show("磁盘", "%", p.Disk)
	show("GPU", "%", p.GPU)

	total, avail := memPhys()
	fmt.Printf("物理内存：共 %.2f GB / 可用 %.2f GB\n",
		float64(total)/1024/1024/1024, float64(avail)/1024/1024/1024)

	fmt.Println("速率：")
	show("下行", "MB/s", p.NetIn/1024/1024)
	show("上行", "MB/s", p.NetOut/1024/1024)
	show("磁盘读", "MB/s", p.DiskR/1024/1024)
	show("磁盘写", "MB/s", p.DiskW/1024/1024)

	if p.CpuMHz > 0 {
		fmt.Printf("CPU 实测频率：%.0f MHz\n", p.CpuMHz)
	} else {
		fmt.Println("CPU 实测频率：读不到")
	}
	fmt.Printf("温度传感器：%d 个 ｜ 风扇：%d 个 ｜ 电压：%d 个 ｜ 功耗：%d 个\n",
		len(p.Temps), len(p.Fans), len(p.Volts), len(p.Power))
	if len(p.Temps) > 0 {
		for _, t := range p.Temps {
			fmt.Printf("  %s %.1f°C\n", t.Name, t.C)
		}
	}
	fmt.Println("\n说明：这一条与首页/本机配置页同源（同一个方法、同一个时点），不会出现两个页面两个数。")
}
