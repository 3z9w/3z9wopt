package main

/* ----------------------------------------------------------------------------------
   窗口几何记忆（2026-09-30 用户拍板：标题栏加回最大化按钮 + 记住大小和位置）
   ----------------------------------------------------------------------------------
   背景：窗口原本是可最大化的（样式位 WS_MAXIMIZEBOX / WS_THICKFRAME 实测都在），
   后来把自绘标题栏上的那个按钮删了 —— 结果变成了「能力还在、入口没了」：
   只有会按 Win+↑ 或会拖边框的人才用得上，其余人永远停在 1220×780。
   再加上窗口大小以前从不落盘，就算手动拖大，下次打开照样回到默认尺寸，等于白拖。

   这里把两件事补齐：
     1. 每次窗口变化（拖边框、最大化、还原）都把几何存进
        %LOCALAPPDATA%\3z9wOpt\winstate.json（跟 oplog / pending_reboot 同一目录）；
     2. 下次启动时按记录恢复：最大化过的直接最大化，否则恢复位置和大小。

   两条护栏（不然「记忆」反而会把人坑住）：
     · 拔掉外接屏 / 换小显示器后，旧坐标可能已经跑到屏幕外 —— 查不到所属显示器就居中，
       查得到就把尺寸夹到可用区以内、左上角往里收，保证标题栏拖拽区永远够得着；
     · 最小化时窗口尺寸读出来是 0，绝不能拿它覆盖上一次的好记录。
   ---------------------------------------------------------------------------------- */

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"syscall"
	"unsafe"

	wailsruntime "github.com/wailsapp/wails/v2/pkg/runtime"
)

// 这三个常量必须跟 main.go 里的 Width / Height / MinWidth / MinHeight 保持一致，
// 改任何一方都要同步另一方（下面是 retestingly 测试用不到 Min 的，仅作兜底下限）。
const (
	winMinW = 1020
	winMinH = 660
)

// winState 一次记住的窗口几何。Max 为真时 W/H/X/Y 存的是最大化之前的样子（Windows 的行为），
// 恢复时只看 Max：为真就直接 WindowMaximise，不去硬套那几个数，避免还原后位置跑偏。
type winState struct {
	W   int  `json:"w"`
	H   int  `json:"h"`
	X   int  `json:"x"`
	Y   int  `json:"y"`
	Max bool `json:"max"`
}

// winStatePath 记录文件位置：%LOCALAPPDATA%\3z9wOpt\winstate.json
func winStatePath() string {
	base := os.Getenv("LOCALAPPDATA")
	if base == "" {
		base = os.TempDir() // 跑在没 LOCALAPPDATA 的环境（服务/计划任务）时不至于崩
	}
	return filepath.Join(base, "3z9wOpt", "winstate.json")
}

func loadWinState() (winState, error) {
	var s winState
	b, err := os.ReadFile(winStatePath())
	if err != nil {
		return s, err
	}
	if err = json.Unmarshal(b, &s); err != nil {
		return winState{}, err
	}
	if s.W <= 0 || s.H <= 0 {
		return winState{}, os.ErrInvalid
	}
	return s, nil
}

// saveWinState 落盘。写坏了不影响主流程（窗口照样能用），所以错误只往上抛、不打扰用户。
func saveWinState(s winState) error {
	p := winStatePath()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	b, err := json.Marshal(s)
	if err != nil {
		return err
	}
	return os.WriteFile(p, b, 0o644)
}

// fitWinState 把「上次记住的几何」修正成「这次一定能用」：
//   - 尺寸不超过目标工作区，也不小于设计最小值；
//   - 左上角往工作区里收 —— 宁可贴边，也不能让标题栏跑出屏幕让人拖不回来。
//
// 纯函数，单测直接覆盖（见 winstate_test.go）。
func fitWinState(s winState, wa winRect32) winState {
	if s.Max {
		return s
	}
	waW, waH := int(wa.Right-wa.Left), int(wa.Bottom-wa.Top)
	if waW <= 0 || waH <= 0 {
		return s
	}
	if s.W > waW {
		s.W = waW
	}
	if s.H > waH {
		s.H = waH
	}
	if s.W < winMinW {
		s.W = winMinW
	}
	if s.H < winMinH {
		s.H = winMinH
	}
	if s.X < int(wa.Left) {
		s.X = int(wa.Left)
	}
	if s.Y < int(wa.Top) {
		s.Y = int(wa.Top)
	}
	if right := s.X + s.W; right > int(wa.Right) {
		s.X = int(wa.Right) - s.W
	}
	if bottom := s.Y + s.H; bottom > int(wa.Bottom) {
		s.Y = int(wa.Bottom) - s.H
	}
	if s.X < int(wa.Left) {
		s.X = int(wa.Left)
	}
	if s.Y < int(wa.Top) {
		s.Y = int(wa.Top)
	}
	return s
}

// ---------- Win32：查「某个坐标落在哪块显示器的可用区里」 ----------

var u32win = syscall.NewLazyDLL("user32.dll")

type winRect32 struct{ Left, Top, Right, Bottom int32 }

type monitorInfoWin struct {
	CbSize    uint32
	RcMonitor winRect32
	RcWork    winRect32
	DwFlags   uint32
}

// workAreaAt 返回 (x, y) 所在显示器的可用区（已排除任务栏）。
// ok=false 表示这个点已经不在任何显示器上了 —— 拔掉外接屏 / 换了显示器布局时的常见后果。
func workAreaAt(x, y int) (winRect32, bool) {
	r := winRect32{int32(x), int32(y), int32(x) + 1, int32(y) + 1}
	h, _, _ := u32win.NewProc("MonitorFromRect").Call(uintptr(unsafe.Pointer(&r)), 0) // MONITOR_DEFAULTTONULL
	if h == 0 {
		return winRect32{}, false
	}
	var mi monitorInfoWin
	mi.CbSize = uint32(unsafe.Sizeof(mi))
	ok, _, _ := u32win.NewProc("GetMonitorInfoW").Call(h, uintptr(unsafe.Pointer(&mi)))
	if ok == 0 {
		return winRect32{}, false
	}
	return mi.RcWork, true
}

// primaryWorkArea 主显示器可用区，用于找不到归属显示器时兜底。
func primaryWorkArea() (winRect32, bool) {
	var r winRect32
	ret, _, _ := u32win.NewProc("SystemParametersInfoW").Call(0x0030, 0, uintptr(unsafe.Pointer(&r)), 0) // SPI_GETWORKAREA
	return r, ret != 0
}

// ---------- 给 App 用的三个动作 ----------

// SaveWindowState 存当前窗口几何。前端拖完边框 / 点完最大化会喊一声，
// 退出时 main.go 的 OnBeforeClose 还会再存一次（那一刻最准）。
func (a *App) SaveWindowState() bool {
	if a.ctx == nil {
		return false
	}
	// 最小化时量出来的是 0 尺寸，绝不能让它把上一次的好记录冲掉。
	if wailsruntime.WindowIsMinimised(a.ctx) {
		return false
	}
	w, h := wailsruntime.WindowGetSize(a.ctx)
	x, y := wailsruntime.WindowGetPosition(a.ctx)
	if w <= 0 || h <= 0 {
		return false
	}
	return saveWinState(winState{
		W:   w,
		H:   h,
		X:   x,
		Y:   y,
		Max: wailsruntime.WindowIsMaximised(a.ctx),
	}) == nil
}

// IsWindowMaximised 只给前端标题栏按钮用：决定画「最大化方框」还是「向下还原双框」。
// wails 的前端 runtime 里没有这个方法（实测 v2.16.0 的 runtime.js 没有导出），只能从后端问。
func (a *App) IsWindowMaximised() bool {
	if a.ctx == nil {
		return false
	}
	return wailsruntime.WindowIsMaximised(a.ctx)
}

// restoreWindowState 在 OnStartup 里按上次记录恢复窗口。
// 没有任何记录（首次运行 / 用户清过配置）就什么都不做，老老实实用 main.go 里的默认 1220×780。
func restoreWindowState(ctx context.Context) {
	s, err := loadWinState()
	if err != nil {
		return
	}
	if s.Max {
		wailsruntime.WindowMaximise(ctx)
		return
	}
	wa, ok := workAreaAt(s.X, s.Y)
	if !ok {
		// 上次的位置已经不在任何屏幕上（外接屏拔了）—— 别硬套，居中给用户看
		if pwa, pok := primaryWorkArea(); pok {
			s = fitWinState(s, pwa)
		}
		wailsruntime.WindowSetSize(ctx, s.W, s.H)
		wailsruntime.WindowCenter(ctx)
		return
	}
	s = fitWinState(s, wa)
	wailsruntime.WindowSetSize(ctx, s.W, s.H)
	wailsruntime.WindowSetPosition(ctx, s.X, s.Y)
}
