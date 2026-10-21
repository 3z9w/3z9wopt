package main

// ============ 应用图标提取（HICON / UWP Assets → PNG → 内存缓存）============
// 原则：绝不把图标塞进列表 JSON。列表先出，前端按 iconKey 逐个异步取 32px PNG。

import (
	"bytes"
	"crypto/sha1"
	"encoding/base64"
	"encoding/hex"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"unsafe"
)

var (
	u32icon = syscall.NewLazyDLL("user32.dll")
	g32icon = syscall.NewLazyDLL("gdi32.dll")
)

// ExtractIconExW 是 Shell API，在 shell32.dll（optimize.go 已有 shell32lazy）
var pExtractIconEx = shell32lazy.NewProc("ExtractIconExW")

// SHGetFileInfoW：兜底取图标用（见 shfiIconPNG）
var pSHGetFileInfo = shell32lazy.NewProc("SHGetFileInfoW")

const (
	shgfiIcon      = 0x00000100 // SHGFI_ICON
	shgfiLargeIcon = 0x00000000 // SHGFI_LARGEICON（32x32）
)

// SHFILEINFOW：只用到前三个字段，后面两个缓冲区必须按真实大小占位，
// 否则 cbFileInfo 传小了系统会拒绝调用（结构体大小必须 696 字节）。
type shFileInfoT struct {
	hIcon         uintptr
	iIcon         int32
	dwAttributes  uint32
	szDisplayName [260]uint16
	szTypeName    [80]uint16
}

// shfiIconPNG 用 SHGetFileInfo 取「系统眼里这个文件长什么样」的图标。
// 兜底用，ExtractIconEx 抽不出来时上：
//
//	· 后台服务程序（lsass / sppsvc / ssh-agent / 各类 Agent）—— 编译时根本没嵌图标资源，
//	  资源里没有 RT_GROUP_ICON，ExtractIconEx 老老实实返回 0（实测文件能打开，不是权限问题）；
//	· .lnk 快捷方式（启动文件夹项）—— ExtractIconEx 不认 lnk，SHGetFileInfo 认。
//
// 系统至少会给一个通用的应用程序图标，比前端画首字母头像更像「图标」。
func shfiIconPNG(path string) ([]byte, bool) {
	if path == "" {
		return nil, false
	}
	p16, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return nil, false
	}
	var fi shFileInfoT
	r, _, _ := pSHGetFileInfo.Call(
		uintptr(unsafe.Pointer(p16)),
		0,
		uintptr(unsafe.Pointer(&fi)),
		unsafe.Sizeof(fi),
		shgfiIcon|shgfiLargeIcon,
	)
	if r == 0 || fi.hIcon == 0 {
		return nil, false
	}
	defer pDestroyIcon.Call(fi.hIcon)
	return hiconPNG(fi.hIcon)
}

var (
	pGetIconInfo    = u32icon.NewProc("GetIconInfo")
	pDestroyIcon    = u32icon.NewProc("DestroyIcon")
	pCreateCompatDC = g32icon.NewProc("CreateCompatibleDC")
	pDeleteDC       = g32icon.NewProc("DeleteDC")
	pGetDIBits      = g32icon.NewProc("GetDIBits")
	pDeleteObj      = g32icon.NewProc("DeleteObject")
	pGetObjectW     = g32icon.NewProc("GetObjectW")
)

type iconInfoT struct {
	fIcon    int32
	xHotspot uint32
	yHotspot uint32
	hbmMask  uintptr
	hbmColor uintptr
}

type bitmapT struct {
	bmType       int32
	bmWidth      int32
	bmHeight     int32
	bmWidthBytes int32
	bmPlanes     uint16
	bmBitsPixel  uint16
	bmBits       uintptr
}

type bmiHeaderT struct {
	biSize          uint32
	biWidth         int32
	biHeight        int32
	biPlanes        uint16
	biBitCount      uint16
	biCompression   uint32
	biSizeImage     uint32
	biXPelsPerMeter int32
	biYPelsPerMeter int32
	biClrUsed       uint32
	biClrImportant  uint32
}

type bmiT struct {
	bmiHeader bmiHeaderT
	bmiColors [4]uint32
}

// 从可执行文件 / dll 中抽取图标并转成 PNG（32bpp 反预乘）
func loadIconPNG(path string, idx int) ([]byte, bool) {
	p16, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return nil, false
	}
	var large, small uintptr
	n, _, _ := pExtractIconEx.Call(
		uintptr(unsafe.Pointer(p16)),
		uintptr(int32(idx)),
		uintptr(unsafe.Pointer(&large)),
		uintptr(unsafe.Pointer(&small)),
		1,
	)
	if n == 0 {
		return nil, false
	}
	if small != 0 {
		pDestroyIcon.Call(small)
	}
	if large == 0 {
		return nil, false
	}
	defer pDestroyIcon.Call(large)
	return hiconPNG(large)
}

func hiconPNG(h uintptr) ([]byte, bool) {
	var ii iconInfoT
	if r, _, _ := pGetIconInfo.Call(h, uintptr(unsafe.Pointer(&ii))); r == 0 {
		return nil, false
	}
	defer func() {
		if ii.hbmColor != 0 {
			pDeleteObj.Call(ii.hbmColor)
		}
		if ii.hbmMask != 0 {
			pDeleteObj.Call(ii.hbmMask)
		}
	}()
	hb := ii.hbmColor
	colored := hb != 0
	if hb == 0 {
		hb = ii.hbmMask
	}
	if hb == 0 {
		return nil, false
	}
	var bm bitmapT
	if r, _, _ := pGetObjectW.Call(hb, unsafe.Sizeof(bm), uintptr(unsafe.Pointer(&bm))); r == 0 {
		return nil, false
	}
	w, hgt := int(bm.bmWidth), int(bm.bmHeight)
	if !colored {
		// 单色图标：mask 高度是两倍（AND+AND），宽度按 1bpp 折算
		w = w * int(bm.bmBitsPixel)
		hgt = hgt / 2
	}
	if w <= 0 || hgt <= 0 || w > 512 || hgt > 512 {
		return nil, false
	}

	hdc, _, _ := pCreateCompatDC.Call(0)
	if hdc == 0 {
		return nil, false
	}
	defer pDeleteDC.Call(hdc)

	buf := make([]byte, w*hgt*4)
	var bi bmiT
	bi.bmiHeader.biSize = uint32(unsafe.Sizeof(bi.bmiHeader))
	bi.bmiHeader.biWidth = int32(w)
	bi.bmiHeader.biHeight = int32(-hgt) // top-down
	bi.bmiHeader.biPlanes = 1
	bi.bmiHeader.biBitCount = 32
	bi.bmiHeader.biCompression = 0 // BI_RGB
	r, _, _ := pGetDIBits.Call(
		hdc, hb, 0, uintptr(hgt),
		uintptr(unsafe.Pointer(&buf[0])),
		uintptr(unsafe.Pointer(&bi)),
		0,
	)
	if r == 0 {
		return nil, false
	}

	img := image.NewNRGBA(image.Rect(0, 0, w, hgt))
	for y := 0; y < hgt; y++ {
		for x := 0; x < w; x++ {
			i := (y*w + x) * 4
			b, g, rr, a := buf[i], buf[i+1], buf[i+2], buf[i+3]
			if a == 0 {
				continue
			}
			if a != 255 && int(rr) <= int(a) && int(g) <= int(a) && int(b) <= int(a) {
				// 预乘 alpha 位图 → 反预乘
				rr = uint8(int(rr) * 255 / int(a))
				g = uint8(int(g) * 255 / int(a))
				b = uint8(int(b) * 255 / int(a))
			}
			img.SetNRGBA(x, y, color.NRGBA{R: rr, G: g, B: b, A: a})
		}
	}

	if w > 64 || hgt > 64 {
		img = scaleNRGBA(img, 48)
	}
	var out bytes.Buffer
	if err := png.Encode(&out, img); err != nil {
		return nil, false
	}
	return out.Bytes(), true
}

// 均值缩放，图标够用，避免额外依赖
func scaleNRGBA(src *image.NRGBA, size int) *image.NRGBA {
	sb := src.Bounds()
	sw, sh := sb.Dx(), sb.Dy()
	dst := image.NewNRGBA(image.Rect(0, 0, size, size))
	fx := float64(sw) / float64(size)
	fy := float64(sh) / float64(size)
	for y := 0; y < size; y++ {
		for x := 0; x < size; x++ {
			var sr, sg, sb2, sa, n float64
			x0, x1 := int(float64(x)*fx), int(float64(x+1)*fx)
			y0, y1 := int(float64(y)*fy), int(float64(y+1)*fy)
			if x1 <= x0 {
				x1 = x0 + 1
			}
			if y1 <= y0 {
				y1 = y0 + 1
			}
			for yy := y0; yy < y1 && yy < sh; yy++ {
				for xx := x0; xx < x1 && xx < sw; xx++ {
					c := src.NRGBAAt(xx, yy)
					a := float64(c.A) / 255
					sr += float64(c.R) * a
					sg += float64(c.G) * a
					sb2 += float64(c.B) * a
					sa += a
					n++
				}
			}
			if n == 0 {
				continue
			}
			if sa > 0 {
				dst.SetNRGBA(x, y, color.NRGBA{
					R: uint8(clamp255(sr / sa)),
					G: uint8(clamp255(sg / sa)),
					B: uint8(clamp255(sb2 / sa)),
					A: uint8(clamp255(sa / n * 255)),
				})
			}
		}
	}
	return dst
}

func clamp255(v float64) int {
	i := int(v + 0.5)
	if i < 0 {
		return 0
	}
	if i > 255 {
		return 255
	}
	return i
}

// 老式包的通用命名（按显示质量排序）
var uwpIconNames = []string{
	"Square44x44Logo.targetsize-48_altform-unplated.png",
	"Square44x44Logo.targetsize-48.png",
	"Square44x44Logo.targetsize-32_altform-unplated.png",
	"Square44x44Logo.targetsize-32.png",
	"Square44x44Logo.scale-200.png",
	"Square44x44Logo.scale-100.png",
	"Square44x44Logo.png",
	"Logo.scale-200.png",
	"Logo.scale-100.png",
	"Logo.png",
	"SmallTile.scale-200.png",
}

// UWP 图标解析：
// ① 通用命名（Square44x44Logo.* 等）—— 老式包
// ② 包自报家门：读 AppxManifest.xml 里声明的 Square44x44Logo/Logo 文件名，按
//
//	targetsize/scale 家族逐个试 —— 2026-09-21 实测：计算器/Store/Xbox 等用自家
//	品牌名（如 CalculatorAppList.*），通用命名和关键词扫描都匹配不上
//
// ③ 目录扫描兜底（applist/logo/icon 等关键词）
func uwpIconPNG(dir string) ([]byte, bool) {
	if dir == "" {
		return nil, false
	}
	// ① 通用命名
	for _, sub := range []string{"Assets", "assets", ""} {
		for _, nm := range uwpIconNames {
			p := filepath.Join(dir, sub, nm)
			if b, err := os.ReadFile(p); err == nil && len(b) > 0 {
				return shrinkPNG(b)
			}
		}
	}
	// ② 清单声明的 Logo 名（Assets\CalculatorAppList.png 这类），剥掉扩展名后试变体
	if base := manifestLogoBase(dir); base != "" {
		for _, suffix := range uwpLogoSuffixes {
			if b, err := os.ReadFile(base + suffix); err == nil && len(b) > 0 {
				return shrinkPNG(b)
			}
		}
		// 声明名的前缀（目录名匹配）再扫一遍 Assets
		prefix := strings.ToLower(filepath.Base(base))
		if prefix != "" {
			if b, ok := scanAssetsPNG(dir, prefix); ok {
				return b, true
			}
		}
	}
	// ③ 关键词兜底
	if b, ok := scanAssetsPNG(dir, ""); ok {
		return b, true
	}
	// ④ 主 exe 图标兜底：MSIX 桥接的桌面应用（Edge/Office 等）目录里没有
	//    Assets 和清单，直接抽目录里主程序的 exe 图标 —— 与具体机器无关。
	if ents, err := os.ReadDir(dir); err == nil {
		for _, en := range ents {
			if !en.IsDir() && strings.EqualFold(filepath.Ext(en.Name()), ".exe") {
				if b, ok := loadIconPNG(filepath.Join(dir, en.Name()), 0); ok {
					return shrinkPNG(b)
				}
			}
		}
	}
	return nil, false
}

// 声明 Logo 常见的尺寸/变形后缀（按显示质量排序）
var uwpLogoSuffixes = []string{
	".targetsize-48_altform-unplated.png",
	".targetsize-48.png",
	".targetsize-32_altform-unplated.png",
	".targetsize-32.png",
	".scale-200.png",
	".scale-150.png",
	".scale-100.png",
	".png",
}

// manifestLogoBase 从 AppxManifest.xml 提取应用 Logo 引用，返回不带扩展名的绝对路径。
// 优先 Square44x44Logo，其次 VisualElements 的 Logo。
func manifestLogoBase(dir string) string {
	b, err := os.ReadFile(filepath.Join(dir, "AppxManifest.xml"))
	if err != nil || len(b) == 0 {
		return ""
	}
	s := string(b)
	val := ""
	for _, attr := range []string{"Square44x44Logo", "Square150x150Logo", "Logo"} {
		if i := strings.Index(s, attr+`="`); i > 0 {
			rest := s[i+len(attr)+2:]
			if j := strings.Index(rest, `"`); j > 0 {
				val = rest[:j]
				break
			}
		}
	}
	val = strings.TrimSpace(val)
	if val == "" || strings.HasPrefix(val, "ms-resource") {
		return ""
	}
	val = strings.ReplaceAll(val, `\`, string(os.PathSeparator))
	p := filepath.Join(dir, filepath.FromSlash(val))
	return strings.TrimSuffix(p, filepath.Ext(p))
}

// scanAssetsPNG 在 Assets（限两层深）里找 png。kw 非空时要求文件名含该关键词；
// 为空时接受 applist/logo/icon/square44x44 任一关键词。优先 targetsize-48。
func scanAssetsPNG(dir, kw string) ([]byte, bool) {
	keywords := []string{kw}
	if kw == "" {
		keywords = []string{"applist", "logo", "appicon", "square44x44", "icon"}
	}
	var found []string
	assets := filepath.Join(dir, "Assets")
	ents, err := os.ReadDir(assets)
	if err != nil {
		assets = dir
		ents, err = os.ReadDir(assets)
		if err != nil {
			return nil, false
		}
	}
	for _, en := range ents {
		if en.IsDir() {
			// 个别包多一层子目录
			subs, err := os.ReadDir(filepath.Join(assets, en.Name()))
			if err != nil {
				continue
			}
			for _, s2 := range subs {
				if !s2.IsDir() && matchIconName(s2.Name(), keywords) {
					found = append(found, filepath.Join(assets, en.Name(), s2.Name()))
				}
			}
			continue
		}
		if matchIconName(en.Name(), keywords) {
			found = append(found, filepath.Join(assets, en.Name()))
		}
	}
	if len(found) == 0 {
		return nil, false
	}
	sort.Strings(found)
	best := found[0]
	for _, p := range found {
		if strings.Contains(strings.ToLower(p), "targetsize-48") {
			best = p
			break
		}
	}
	b, err := os.ReadFile(best)
	if err != nil || len(b) == 0 {
		return nil, false
	}
	return shrinkPNG(b)
}

func matchIconName(name string, keywords []string) bool {
	lm := strings.ToLower(name)
	if !strings.HasSuffix(lm, ".png") {
		return false
	}
	if strings.Contains(lm, "contrast-") {
		return false // 高对比度变体，正常人用的不是这个
	}
	for _, kw := range keywords {
		if kw != "" && strings.Contains(lm, kw) {
			return true
		}
	}
	return false
}

// 把过大的 PNG 统一压到 48px，减少前端传输量
func shrinkPNG(b []byte) ([]byte, bool) {
	img, _, err := image.Decode(bytes.NewReader(b))
	if err != nil {
		return b, true // 解不了就原样给，浏览器自己能显示
	}
	bd := img.Bounds()
	if bd.Dx() <= 64 && bd.Dy() <= 64 {
		return b, true
	}
	src := image.NewNRGBA(image.Rect(0, 0, bd.Dx(), bd.Dy()))
	draw.Draw(src, src.Bounds(), img, bd.Min, draw.Src)
	dst := scaleNRGBA(src, 48)
	var out bytes.Buffer
	if err := png.Encode(&out, dst); err != nil {
		return b, true
	}
	return out.Bytes(), true
}

// ---------- 磁盘缓存（%LOCALAPPDATA%\3z9wOpt\iconcache）----------

// iconCacheDir 图标磁盘缓存目录。
//
// ⚠️ 原来直接拼 os.Getenv("LOCALAPPDATA")：这个变量取不到时（SYSTEM 账户、
// 被改过的用户环境）路径会退化成相对的 `3z9wOpt\iconcache`，写进进程工作目录 ——
// 换一种启动方式（计划任务 / 服务 / 右键管理员运行）缓存就全丢，图标每次重取。
// 统一走 appDataDir()（LOCALAPPDATA → UserCacheDir → TempDir 三层兜底）。
func iconCacheDir() string {
	d := filepath.Join(appDataDir(), "iconcache")
	_ = os.MkdirAll(d, 0o755)
	return d
}

func hashName(s string) string {
	h := sha1.Sum([]byte(s))
	return hex.EncodeToString(h[:])
}

func iconCacheGet(key string) ([]byte, bool) {
	b, err := os.ReadFile(filepath.Join(iconCacheDir(), hashName(key)+".png"))
	if err != nil || len(b) == 0 {
		return nil, false
	}
	return b, true
}

func iconCachePut(key string, b []byte) {
	_ = os.WriteFile(filepath.Join(iconCacheDir(), hashName(key)+".png"), b, 0o644)
}

// ---------- 对外接口 ----------

// GetAppIcon 返回 data URL。key 形如：
//
//	w|<exe路径>|<iconIndex>    —— Win32 程序
//	u|<InstallLocation>        —— UWP 应用
func (a *App) GetAppIcon(key string) string {
	key = strings.TrimSpace(key)
	if key == "" || len(key) > 1024 {
		return ""
	}
	if b, ok := iconCacheGet(key); ok {
		return "data:image/png;base64," + base64.StdEncoding.EncodeToString(b)
	}
	var pngBytes []byte
	var ok bool
	switch {
	case strings.HasPrefix(key, "w|"):
		rest := key[2:]
		idx := 0
		if i := strings.LastIndex(rest, "|"); i > 1 {
			idx, _ = strconv.Atoi(rest[i+1:])
			rest = rest[:i]
		}
		pngBytes, ok = loadIconPNG(rest, idx)
		if !ok {
			// dll/ico 抽不出来时，退到同目录主程序图标
			if e := strings.ToLower(filepath.Ext(rest)); e == ".ico" || e == ".dll" {
				dir := filepath.Dir(rest)
				if ents, err := os.ReadDir(dir); err == nil {
					for _, en := range ents {
						if !en.IsDir() && strings.EqualFold(filepath.Ext(en.Name()), ".exe") {
							if b2, ok2 := loadIconPNG(filepath.Join(dir, en.Name()), 0); ok2 {
								pngBytes, ok = b2, true
								break
							}
						}
					}
				}
			}
		}
		if !ok {
			// 最后一道兜底：问系统要关联图标（没嵌图标的服务程序、.lnk 快捷方式靠它救回来）
			pngBytes, ok = shfiIconPNG(rest)
		}
	case strings.HasPrefix(key, "u|"):
		pngBytes, ok = uwpIconPNG(key[2:])
	}
	if !ok || len(pngBytes) == 0 {
		return ""
	}
	iconCachePut(key, pngBytes)
	return "data:image/png;base64," + base64.StdEncoding.EncodeToString(pngBytes)
}
