package main

// pcshare.go —— 「复制配置」和「保存截图」（2026-09-24 用户要求：机器信息要能复制或截图保存）

import (
	"fmt"
	"image"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
	"unsafe"

	wailsruntime "github.com/wailsapp/wails/v2/pkg/runtime"
	"golang.org/x/sys/windows/registry"
)

// GetPCSummary 生成一份**中文、能直接贴给别人看**的配置清单。
// 规矩照旧：读不到就写「读不到」，绝不拿估算值填空。
func (a *App) GetPCSummary() string {
	f := a.GetPCFull()
	s := f.Spec
	var b strings.Builder

	/* 机型（台式机/笔记本）不再出现在导出里 —— 用户 2026-10-10：机箱信息不需要读。
	   标题行直接「品牌 型号」，读不到就照实写「未获取到」。 */
	board := joinNonEmpty(" ", firstNonEmpty(s.BoardBrand, s.SysVendor), s.SysModel)
	if board == "" {
		board = f.Board
	}
	b.WriteString("【本机配置】" + dashIfEmpty(board) + "\n")
	b.WriteString("读取时间：" + time.Now().Format("2006-01-02 15:04") + "\n")

	// ---- 操作系统 ----
	b.WriteString("\n操作系统\n")
	b.WriteString("  " + dashIfEmpty(joinNonEmpty(" ", f.OS, f.OSVer)) + "\n")
	line := joinNonEmpty(" · ", wrap("安装于", f.Install), wrap("已开机", f.Uptime))
	if line != "" {
		b.WriteString("  " + line + "\n")
	}

	// ---- 处理器 ----
	b.WriteString("\n处理器")
	if s.CPUBrand != "" {
		b.WriteString("　" + s.CPUBrand)
	}
	b.WriteString("\n")
	cpuModel := s.CPUModelCn
	if cpuModel == "" {
		cpuModel = f.CPU
	}
	b.WriteString("  " + dashIfEmpty(cpuModel) + "\n")
	cpuBits := joinNonEmpty(" · ", s.CPUCore, wrap("基准", s.CPUBase), wrap("最大睿频", s.CPUTurbo), wrap("缓存", s.CPUCache))
	if cpuBits != "" {
		b.WriteString("  " + cpuBits + "\n")
	}
	if s.CPUCode != "" || s.CPUTech != "" || s.CPUSocket != "" {
		b.WriteString("  " + joinNonEmpty(" · ", wrap("代号", s.CPUCode), wrap("工艺", s.CPUTech), wrap("插槽", s.CPUSocket)) + "\n")
	}

	// ---- 内存 ----
	b.WriteString("\n内存")
	if s.MemBrand != "" {
		b.WriteString("　" + s.MemBrand)
	}
	b.WriteString("\n")
	memBits := joinNonEmpty(" ", f.MemTotal, s.MemType, s.MemSpeed)
	b.WriteString("  " + dashIfEmpty(strings.TrimSpace(memBits)) + "\n")
	/* 插槽位置是用户点名要的：「内存插槽是插在哪几个槽上」。
	   逐条列出槽位 + 容量，插了几根一目了然。 */
	if len(s.MemSticks) > 0 {
		for _, m := range s.MemSticks {
			slot := m.Slot
			if slot == "" {
				slot = "未知槽位"
			}
			b.WriteString(fmt.Sprintf("  %s：%s %s", slot, dashIfEmpty(m.Size), strings.TrimSpace(joinNonEmpty(" ", m.Maker, m.Part))))
			if m.Conf != "" && m.Conf != m.Speed {
				b.WriteString("（实跑 " + m.Conf + "）")
			}
			b.WriteString("\n")
		}
	}
	mbits := joinNonEmpty(" · ", wrap("工作电压", s.MemVolt), s.MemChan, wrap("插槽", s.MemSlots), wrap("最大支持", s.MemMaxCap))
	if mbits != "" {
		b.WriteString("  " + mbits + "\n")
	}

	// ---- 显卡 ----
	if len(f.GPUs) > 0 {
		/* 显存必须按**同一块卡**配（2026-10-10 修）：
		   f.GPUs 来自 WMI（按「像不像真卡」排过序），f.VRAMs 来自 DXGI（按枚举顺序），
		   两套顺序互不相干。双显卡笔记本上直接取 VRAMs[0]，会导出成
		   「英特尔 Iris Xe（专用显存 8 GB）」—— 两个数各自都真，凑一起是假的。 */
		v0 := vramForGPU(f.GPUs[0], f.VRAMs)
		b.WriteString("\n显卡")
		if v0 != nil && v0.Name != "" {
			if gb := gpuBrand(v0.Name, v0.Vendor); gb != "" {
				b.WriteString("　" + gb)
			}
		}
		b.WriteString("\n")
		b.WriteString("  " + dashIfEmpty(cleanModel(f.GPUs[0])) + "\n")
		if v0 != nil {
			vr := ""
			if v0.Size != "" && v0.SharedSize != "" {
				vr = fmt.Sprintf("专用显存 %s · 共享显存 %s", v0.Size, v0.SharedSize)
			} else if v0.Size != "" {
				vr = "专用显存 " + v0.Size
			}
			if vr != "" && v0.Source != "" {
				vr += "（" + pcSourceTextGo(v0.Source) + " 口径）"
			}
			if vr != "" {
				b.WriteString("  " + vr + "\n")
			}
		}
		if len(f.Display) > 0 && f.Display[0] != "" {
			b.WriteString("  " + wrap("当前分辨率", f.Display[0]) + "\n")
		}
		if len(f.GPUs) > 1 {
			b.WriteString("  " + wrap("其它显卡", strings.Join(cleanAll(f.GPUs[1:]), "、")) + "\n")
		}
	}

	// ---- 主板 ----
	boardLine := joinNonEmpty(" · ", board, wrap("芯片组", f.Chipset))
	if boardLine != "" {
		b.WriteString("\n主板")
		if s.BoardBrand != "" {
			b.WriteString("　" + s.BoardBrand)
		}
		b.WriteString("\n  " + boardLine + "\n")
	}
	bios := joinNonEmpty(" ", s.BIOSVendor, s.BIOSVer, s.BIOSDate)
	if bios == "" {
		bios = f.BIOS
	}
	if bios != "" {
		b.WriteString("  " + wrap("BIOS", bios) + "\n")
	}

	// ---- 硬盘 ----
	if len(f.PhysDisks) > 0 {
		b.WriteString("\n硬盘\n")
		for _, d := range f.PhysDisks {
			modelTxt := reSpace.ReplaceAllString(strings.TrimSpace(d.Model), " ")
			brand := diskBrand(d.Model)
			/* 品牌没查到中译时，diskBrand 会退回型号串里的原样厂商词，
			   这时括号里再写一遍就是复读（"LuminouTek 256GB（LuminouTek）"）—— 干脆不写。 */
			if brand != "" && (brand == modelTxt || strings.HasPrefix(modelTxt, brand)) {
				brand = ""
			}
			tail := joinNonEmpty(" · ", d.Size, d.Media, d.Interface, wrap("固件", d.Firmware), d.Status)
			b.WriteString(fmt.Sprintf("  %s%s\n", modelTxt, tailBrand(brand)))
			if tail != "" {
				b.WriteString("    " + tail + "\n")
			}
		}
	}
	if len(f.Disks) > 0 {
		b.WriteString("\n分区\n")
		for _, d := range f.Disks {
			used := ""
			if d.Total > 0 {
				used = fmt.Sprintf("%.0f%%", float64(d.Total-d.Free)/float64(d.Total)*100)
			}
			b.WriteString(fmt.Sprintf("  %s　%s　剩 %s / 共 %s\n",
				d.Drive, d.Label, fmtBytes(d.Free), fmtBytes(d.Total)) + usedMark(used))
		}
	}

	// ---- 网络 ----
	if len(f.Nets) > 0 {
		b.WriteString("\n网卡\n")
		for _, n := range f.Nets {
			b.WriteString("  " + joinNonEmpty(" · ", cleanModel(n.Name), n.Speed, n.Link, n.MAC) + "\n")
		}
	}
	if len(f.Sounds) > 0 {
		b.WriteString("\n声卡\n  " + strings.Join(cleanAll(f.Sounds), "、") + "\n")
	}

	if s.Sources != "" {
		b.WriteString("\n数据来源：" + pcSourceTextGo(s.Sources) + "\n")
	}
	b.WriteString("（未获取到的项一律写「未获取到」，不填估算值）\n")
	return b.String()
}

// GetPCSummaryJSON 前端要的是字符串，Wails 直接返 string 也行，这里留个包装免得前端拿成对象
func (a *App) GetPCSummaryJSON() map[string]string {
	return map[string]string{"text": a.GetPCSummary()}
}

/* ExportPCInfo 「导出配置」—— 把整份配置写成 .txt。

   落盘位置**交给用户选**（2026-10-10 用户：「导出配置位置由客户自行选择保存位置」）：
   弹系统「另存为」对话框，默认目录给桌面（用户重定向过桌面 / OneDrive 接管也能对上），
   默认文件名带时间戳，用户想存哪就存哪。

   返回三个口径：
     · 路径    —— 存成功，前端提示「已导出到：xxx」
     · "CANCEL" —— 用户在对话框里点了取消，前端静默处理，不当失败报（取消不是错）
     · "ERR:xx" —— 真出错，前端照实播报，绝不假装成功

   ⚠️ 无 UI 上下文时（a.ctx == nil，即单测 / 无头调用）不能弹对话框 —— 会崩。
   这时退回「直接写桌面」的老行为，回归测试靠这条路径验真写盘。 */
func (a *App) ExportPCInfo() string {
	txt := a.GetPCSummary()
	if strings.TrimSpace(txt) == "" {
		return "ERR:配置内容为空"
	}
	def := "本机配置-" + time.Now().Format("20060102-150405") + ".txt"

	/* 默认目录：能取到桌面就给桌面，取不到退回用户主目录 */
	dir, err := desktopDirFn()
	if err != nil || dir == "" {
		dir, _ = os.UserHomeDir()
	}

	p := ""
	if a.ctx != nil {
		sel, derr := wailsruntime.SaveFileDialog(a.ctx, wailsruntime.SaveDialogOptions{
			Title:            "导出本机配置",
			DefaultFilename:  def,
			DefaultDirectory: dir,
			Filters: []wailsruntime.FileFilter{
				{DisplayName: "文本文件 (*.txt)", Pattern: "*.txt"},
				{DisplayName: "所有文件 (*.*)", Pattern: "*.*"},
			},
		})
		if derr != nil {
			return "ERR:打不开保存对话框：" + derr.Error()
		}
		if strings.TrimSpace(sel) == "" {
			return "CANCEL"
		}
		p = strings.TrimSpace(sel)
		/* 用户在对话框里把扩展名删了就补一个 —— 否则存出来一个双击打不开的文件，
		   用户只会觉得「导出坏了」 */
		if filepath.Ext(p) == "" {
			p += ".txt"
		}
	} else {
		if dir == "" {
			return "ERR:找不到保存位置"
		}
		p = filepath.Join(dir, def)
	}

	/* Windows 记事本（Win7 / LTSC 等系统自带的老版本）不认裸 LF：中文会挤成一行；
	   不带 BOM 的 UTF-8 在中文 Windows 上会被按 ANSI 解 → 全文乱码。
	   所以统一写成 CRLF + UTF-8 BOM，和项目里导出 CSV 的口径一致。
	   用户是成千上万台机器，老机器占相当比例，这条不能省。 */
	body := strings.ReplaceAll(txt, "\r\n", "\n")
	body = strings.ReplaceAll(body, "\n", "\r\n")
	if err := os.WriteFile(p, []byte("\ufeff"+body), 0o644); err != nil {
		return "ERR:没法写文件：" + err.Error()
	}
	return p
}

/* vramForGPU 按**显卡名**把显存配对（2026-10-10 修）。
   f.GPUs 来自 WMI（排序是「像不像真卡」），f.VRAMs 来自 DXGI（DXGI 自己的枚举顺序）——
   两套顺序没有任何关系，双显卡机器直接取 VRAMs[0] 会把 A 卡的名字配上 B 卡的显存。
   两个数各自都真、凑在一起是假的，正是铁律一最防的那种。
   所以名字互相包含才算同一块；一块都配不上（名字本地化、驱动改过名）才退回第一块。 */
func vramForGPU(name string, vrams []vramInfo) *vramInfo {
	if len(vrams) == 0 {
		return nil
	}
	low := strings.ToLower(cleanModel(name))
	if low != "" {
		for i := range vrams {
			n := strings.ToLower(strings.TrimSpace(vrams[i].Name))
			if n != "" && (strings.Contains(low, n) || strings.Contains(n, low)) {
				return &vrams[i]
			}
		}
	}
	return &vrams[0]
}

func tailBrand(brand string) string {
	if brand == "" {
		return ""
	}
	return "（" + brand + "）"
}

func usedMark(used string) string {
	if used == "" {
		return ""
	}
	return "    已用 " + used + "\n"
}

func wrap(label, v string) string {
	v = strings.TrimSpace(v)
	if v == "" {
		return ""
	}
	return label + " " + v
}

func dashIfEmpty(s string) string {
	if strings.TrimSpace(s) == "" {
		return "未获取到"
	}
	return strings.TrimSpace(s)
}

func joinNonEmpty(sep string, parts ...string) string {
	var out []string
	for _, p := range parts {
		if strings.TrimSpace(p) != "" {
			out = append(out, strings.TrimSpace(p))
		}
	}
	return strings.Join(out, sep)
}

func pcSourceTextGo(src string) string {
	/* 值要跟前端 pcSourceOne 的口径一致：导出给用户看的，写「dxgi」没人看得懂 */
	m := map[string]string{"cpuid": "CPUID 指令", "smbios": "SMBIOS 固件表", "wmi": "WMI",
		"dxgi": "DirectX 接口", "reg": "注册表", "devicemap": "设备映射",
		"nvidia": "NVIDIA 驱动", "amd": "AMD 驱动", "intel": "Intel 驱动"}
	var out []string
	for _, x := range strings.Split(src, "+") {
		if v, ok := m[x]; ok {
			out = append(out, v)
		} else if x != "" {
			out = append(out, x)
		}
	}
	return strings.Join(out, " + ")
}

/* ==================== 截图保存 ==================== */

/* SaveWindowShot 把当前窗口截成 PNG 存到桌面。

   ⚠️ 三个都是实测踩出来的坑（2026-09-24，本机 1220×780 主窗口上复现）：

   ① **GetDIBits 在大窗口上会返回 0**，而 PrintWindow 明明成功、GetLastError 也是 0，
      于是页面弹「取不到像素」。小窗口（160×28）上反而能过 —— 所以别拿小窗当回归证据。
      改成 CreateDIBSection 自建画布：PrintWindow 直接画进我们自己的内存，
      根本不需要再 GetDIBits 拷一遍。实测同一扇窗：旧法缓冲全 0，新法 100% 有像素。

   ② **窗口最小化时 GetWindowRect 给的是任务栏按钮的尺寸**（实测 160×28），
      照那个尺寸截出来是一条边。所以先 IsIconic 判一下，是最小化的就先 SW_RESTORE。

   ③ **WebView2 必须用 PrintWindow 的 PW_RENDERFULLCONTENT(2)**，
      普通 PrintWindow(hdc, 0) 会抓出一张全白（这个坑在别的页面实证时踩过，别改回去）。 */
func (a *App) SaveWindowShot() string {
	hwnd := findOwnWindow()
	if hwnd == 0 {
		return "ERR:没找到程序窗口"
	}
	img, err := captureWindow(hwnd)
	if err != nil {
		return "ERR:" + err.Error()
	}
	dir, err := desktopDirFn()
	if err != nil {
		dir, _ = os.UserHomeDir()
	}
	if dir == "" {
		return "ERR:找不到保存位置"
	}
	name := "本机配置-" + time.Now().Format("20060102-150405") + ".png"
	p := filepath.Join(dir, name)
	fh, err := os.Create(p)
	if err != nil {
		return "ERR:没法写文件：" + err.Error()
	}
	defer fh.Close()
	if err := png.Encode(fh, img); err != nil {
		return "ERR:存图失败：" + err.Error()
	}
	return p
}

var (
	user32Shot = syscall.NewLazyDLL("user32.dll")
	gdi32Shot  = syscall.NewLazyDLL("gdi32.dll")
	krn32Shot  = syscall.NewLazyDLL("kernel32.dll")
)

/* findOwnWindow 找本进程的主窗口（按进程 ID 匹配，比按标题匹配稳）。

   ⚠️ 别「找到第一个可见的就停」：同进程可能有好几个可见窗口，枚举顺序是 Z 序，
   第一个不一定是最外面那扇主窗 —— 抓错了就截出一条边或者一张小图。
   所以**全收进来，按面积取最大的那个**。

   取到之后还要判最小化：最小化窗口的 GetWindowRect 是任务栏按钮那点尺寸
   （实测 160×28），直接截会截出一条边，所以先恢复再截。 */
func findOwnWindow() uintptr {
	pid, _, _ := krn32Shot.NewProc("GetCurrentProcessId").Call()
	type cand struct {
		hw   uintptr
		area int
	}
	type findCtx struct {
		pid  uintptr
		list []cand
	}
	ctx := &findCtx{pid: pid}
	cb := syscall.NewCallback(func(h uintptr, l uintptr) uintptr {
		var p uintptr
		user32Shot.NewProc("GetWindowThreadProcessId").Call(h, uintptr(unsafe.Pointer(&p)))
		if p == ctx.pid {
			if v, _, _ := user32Shot.NewProc("IsWindowVisible").Call(h); v != 0 {
				var r rectShot
				user32Shot.NewProc("GetWindowRect").Call(h, uintptr(unsafe.Pointer(&r)))
				ctx.list = append(ctx.list,
					cand{hw: h, area: int(r.Right-r.Left) * int(r.Bottom-r.Top)})
			}
		}
		return 1 // 全收，不停
	})
	user32Shot.NewProc("EnumWindows").Call(cb, uintptr(unsafe.Pointer(ctx)))
	if len(ctx.list) == 0 {
		return 0
	}
	best := 0
	for i := range ctx.list {
		if ctx.list[i].area > ctx.list[best].area {
			best = i
		}
	}
	hw := ctx.list[best].hw

	/* 最小化 → 先恢复（SW_RESTORE = 9），等着窗口真回来再截。
	   最多等 1 秒：恢复是异步的，不等的话拿到的还是任务栏那点尺寸。 */
	if v, _, _ := user32Shot.NewProc("IsIconic").Call(hw); v != 0 {
		user32Shot.NewProc("ShowWindow").Call(hw, 9)
		for i := 0; i < 10; i++ {
			time.Sleep(100 * time.Millisecond)
			if v2, _, _ := user32Shot.NewProc("IsIconic").Call(hw); v2 == 0 {
				break
			}
		}
	}
	return hw
}

// lastErrShot 取上一次 Win32 错误码 —— 报错时带上它，下次排查不用再猜
func lastErrShot() uintptr {
	e, _, _ := krn32Shot.NewProc("GetLastError").Call()
	return e
}

type rectShot struct{ Left, Top, Right, Bottom int32 }

type bmiHeaderShot struct {
	Size          uint32
	Width         int32
	Height        int32
	Planes        uint16
	BitCount      uint16
	Compression   uint32
	SizeImage     uint32
	XPelsPerMeter int32
	YPelsPerMeter int32
	ClrUsed       uint32
	ClrImportant  uint32
}

/* captureWindow 抓一扇窗口的内容。

   走 CreateDIBSection：**画布就是我们自己的那段内存**，PrintWindow 画完直接读，
   不再经过 GetDIBits（旧法在大窗口上返回 0、缓冲全 0，页面就报「取不到像素」）。
   bi.Height 给负号 = 自上而下，省掉一次翻转。 */
func captureWindow(hwnd uintptr) (*image.RGBA, error) {
	var r rectShot
	if v, _, _ := user32Shot.NewProc("GetWindowRect").Call(hwnd, uintptr(unsafe.Pointer(&r))); v == 0 {
		return nil, fmt.Errorf("拿不到窗口大小")
	}
	w, h := int(r.Right-r.Left), int(r.Bottom-r.Top)
	/* 100×100 以下基本就是「还缩在任务栏里 / 抓到别的窗口」了，别硬截 */
	if w < 100 || h < 100 {
		return nil, fmt.Errorf("窗口尺寸不对（%d×%d）—— 窗口可能还缩在任务栏里，请把它打开再试", w, h)
	}

	hdc, _, _ := user32Shot.NewProc("GetWindowDC").Call(hwnd)
	if hdc == 0 {
		return nil, fmt.Errorf("拿不到窗口画布（GetLastError=%d）", lastErrShot())
	}
	defer user32Shot.NewProc("ReleaseDC").Call(hwnd, hdc)
	mdc, _, _ := gdi32Shot.NewProc("CreateCompatibleDC").Call(hdc)
	if mdc == 0 {
		return nil, fmt.Errorf("建不了内存画布（GetLastError=%d）", lastErrShot())
	}
	defer gdi32Shot.NewProc("DeleteDC").Call(mdc)

	var bi bmiHeaderShot
	bi.Size = uint32(unsafe.Sizeof(bi))
	bi.Width = int32(w)
	bi.Height = -int32(h) // 负号 = 自上而下
	bi.Planes = 1
	bi.BitCount = 32
	var bits uintptr
	dib, _, _ := gdi32Shot.NewProc("CreateDIBSection").Call(mdc, uintptr(unsafe.Pointer(&bi)),
		0 /*DIB_RGB_COLORS*/, uintptr(unsafe.Pointer(&bits)), 0, 0)
	if dib == 0 || bits == 0 {
		return nil, fmt.Errorf("建不了位图（GetLastError=%d）", lastErrShot())
	}
	defer gdi32Shot.NewProc("DeleteObject").Call(dib)
	old, _, _ := gdi32Shot.NewProc("SelectObject").Call(mdc, dib)

	// PW_RENDERFULLCONTENT = 2 —— WebView2 只有这个模式能抓到内容
	ok := false
	for i := 0; i < 2; i++ {
		if v, _, _ := user32Shot.NewProc("PrintWindow").Call(hwnd, mdc, 2); v != 0 {
			ok = true
			break
		}
		/* 刚从最小化恢复时第一次可能扑空，等一拍再试一次 */
		time.Sleep(150 * time.Millisecond)
	}
	gdi32Shot.NewProc("GdiFlush").Call() // GDI 会攒批处理，不 flush 可能读到半张
	gdi32Shot.NewProc("SelectObject").Call(mdc, old)
	if !ok {
		return nil, fmt.Errorf("抓不到窗口内容（GetLastError=%d）", lastErrShot())
	}

	/* GetDIBits 把位图数据指针当参数回填到 bits（uintptr）。按指针读出来再切片，
	   避免 uintptr→unsafe.Pointer 的往返被 go vet 判为误用。 */
	bp := *(*unsafe.Pointer)(unsafe.Pointer(&bits))
	raw := unsafe.Slice((*byte)(bp), w*h*4)
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	dst, src := img.Pix, raw
	for i := 0; i < w*h; i++ {
		// DIB 是 BGRA，Go 的 image.RGBA 要 RGBA
		dst[i*4+0] = src[i*4+2]
		dst[i*4+1] = src[i*4+1]
		dst[i*4+2] = src[i*4+0]
		dst[i*4+3] = 255
	}
	return img, nil
}

/* desktopDirFn 供调用方取「桌面」（导出配置、保存截图都用它）。
   做成变量只为**测试**能把它指到临时目录 —— 打死也不能让回归测试
   往用户真实桌面写文件（见 pcshare_export_test.go）。生产路径就是 desktopDir。 */
var desktopDirFn = desktopDir

/* desktopDir 桌面路径。
   顺序（2026-10-10 修）：
     ① 注册表 User Shell Folders\Desktop —— **重定向过的桌面只有这里是对的**。
        Ghost/精简系统把桌面挪到 D 盘、企业镜像把桌面指到网络盘、OneDrive 接管桌面，
        这些机器上 `%USERPROFILE%\Desktop` 要么不存在，要么是个残留空壳目录，
        硬拼出来的路径就是错的（旧实现只拼 USERPROFILE，踩的就是这个）。
     ② USERPROFILE\Desktop / OneDrive\Desktop 兜底：注册表项被删过或读不到时用。
     ③ 全都没有就退回用户主目录，绝不返回空串让调用方去拼。
   取到的路径一律先 Stat 确认是目录，不存在就不认。 */
func desktopDir() (string, error) {
	if p := desktopFromRegistry(); p != "" {
		return p, nil
	}
	if d := os.Getenv("USERPROFILE"); d != "" {
		p := filepath.Join(d, "Desktop")
		if st, err := os.Stat(p); err == nil && st.IsDir() {
			return p, nil
		}
	}
	if d := os.Getenv("OneDrive"); d != "" {
		p := filepath.Join(d, "Desktop")
		if st, err := os.Stat(p); err == nil && st.IsDir() {
			return p, nil
		}
	}
	h, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	p := filepath.Join(h, "Desktop")
	if st, err := os.Stat(p); err == nil && st.IsDir() {
		return p, nil
	}
	return h, nil
}

// desktopFromRegistry 读「用户真实的桌面路径」（桌面被重定向/OneDrive 接管时也准）。
// 值为 REG_EXPAND_SZ，常带 %USERPROFILE% 之类的变量，必须自己展开。
// 读不到 / 路径不存在都返回空串，交给 desktopDir 继续兜底。
func desktopFromRegistry() string {
	k, err := registry.OpenKey(registry.CURRENT_USER,
		`Software\Microsoft\Windows\CurrentVersion\Explorer\User Shell Folders`,
		registry.QUERY_VALUE)
	if err != nil {
		return ""
	}
	defer k.Close()
	v, _, err := k.GetStringValue("Desktop")
	if err != nil {
		return ""
	}
	v = strings.TrimSpace(v)
	if v == "" {
		return ""
	}
	if strings.Contains(v, "%") {
		v = os.ExpandEnv(v)
	}
	if v == "" {
		return ""
	}
	if st, err := os.Stat(v); err == nil && st.IsDir() {
		return v
	}
	return ""
}

// cleanAll 一批名字统一去噪（去 (R)、尾部频率）
func cleanAll(list []string) []string {
	out := make([]string, 0, len(list))
	for _, v := range list {
		out = append(out, cleanModel(v))
	}
	return out
}
