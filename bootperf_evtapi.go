package main

/* ══════════════════════════════════════════════════════════════════════════
   开机耗时 —— 原生事件查询（wevtapi.dll）
   ---------------------------------------------------------------------------
   为什么绕开 wevtutil 读数据（实测对比得出的结论，不是设计洁癖）：

   wevtutil 是控制台程序，输出跟随**控制台代码页**，而不是它自己 XML 声明里写的
   UTF-8。本机实测同一批数据：

       原生 wevtapi : FriendlyName="Windows 服务主进程"   ProductName="Microsoft® ..."
       wevtutil     : B7 FE CE F1 D6 F7 BD F8 B3 CC      ← 这是"服务主进程"的 GBK
                      整段 utf8.Valid == false

   也就是说 wevtutil 给我们的是本地代码页字节，得靠 decodeANSI 猜着解 —— 在
   中文机器上是 GBK、日文机器上是 932、英文机器上是 1252，猜错了就是一屏乱码。
   国内用户的机器上一堆中文软件名，这个风险不能留。

   换 wevtapi 之后一举解决四个跨机器问题：
     ① 编码：EvtRender 返回 UTF-16，中文和 ® 这类符号都无损，无需猜测代码页
     ② 不闪窗：不再起子进程（GUI 进程下 exec 会被分配新控制台）
     ③ 不依赖 PATH：不再依赖 wevtutil 能否被找到
     ④ 快一个量级：没有进程启动开销

   ⚠️ 附一条排查时踩的坑，别再踩第二次：本机 10-17 那条驱动记录显示成 "??exe"，
      `<Data Name='NameLength'>7</Data>` 却只有 5 个字符 —— 一开始以为是转码
      损失，但原生路径拿到的**同样是** "??exe"，说明事件源头就是坏的，
      两条路都救不回来。这种情况在 code 里标 NameBroken，前端显示"名称无法识别"，
      别把 "??exe" 直接甩给用户看。

   wevtutil 只保留用于「查通道开关状态」（`gl` 的输出是纯 ASCII 的
   enabled="true"，不涉及中文，没有编码风险）。

   仍需要管理员权限 —— 这是日志通道的 ACL 决定的，换任何读法都绕不开。
   ══════════════════════════════════════════════════════════════════════════ */

import (
	"context"
	"encoding/xml"
	"errors"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unsafe"
)

const (
	evtQueryChannelPath      = 0x1   // Path 参数是通道名（而非 XPath 到某个文件）
	evtQueryReverseDirection = 0x200 // 倒序：最新的在前（与 wevtutil /rd:true 同义）
	evtRenderEventXml        = 1     // 渲染成 XML 字符串

	errNoMoreItems        syscall.Errno = 259    // ERROR_NO_MORE_ITEMS：正常迭代结束
	errInsufficientBuffer syscall.Errno = 122    // ERROR_INSUFFICIENT_BUFFER
	errAccessDenied       syscall.Errno = 5      // ERROR_ACCESS_DENIED
	errEvtChannelNotFound syscall.Errno = 15007  // ERROR_EVT_CHANNEL_NOT_FOUND
)

var (
	wevtapiLazy = syscall.NewLazyDLL("wevtapi.dll")
	pEvtQuery   = wevtapiLazy.NewProc("EvtQuery")
	pEvtNext    = wevtapiLazy.NewProc("EvtNext")
	pEvtRender  = wevtapiLazy.NewProc("EvtRender")
	pEvtClose   = wevtapiLazy.NewProc("EvtClose")
)

// utf16PtrZ 把 Go 字符串转成 Windows API 要的 LPCWSTR（以 NUL 结尾的 UTF-16）。
func utf16PtrZ(s string) uintptr {
	p, err := syscall.UTF16PtrFromString(s)
	if err != nil {
		return 0
	}
	return uintptr(unsafe.Pointer(p))
}

/* bootQueryNative 用 EvtQuery/EvtNext/EvtRender 读事件，返回与 wevtutil 方案
   完全相同的 []xmlEvent，下游解析逻辑一行都不用改。 */
func bootQueryNative(query string, max int) ([]xmlEvent, error) {
	if err := pEvtQuery.Find(); err != nil {
		return nil, err // DLL 或函数缺失（极老的系统）
	}
	if max <= 0 {
		max = 1
	}

	ch, q := utf16PtrZ(bootLogName), utf16PtrZ(query)
	if ch == 0 || q == 0 {
		return nil, errors.New("通道名或查询串含非法字符")
	}
	h, _, err := pEvtQuery.Call(0, ch, q, uintptr(evtQueryChannelPath|evtQueryReverseDirection))
	if h == 0 {
		// EvtQuery 失败时的 errno 就是判断成因的唯一可靠依据（跨语言稳定）
		return nil, errnoOf(err)
	}
	defer pEvtClose.Call(h)

	// 一次把要的条数全取回来（max 最多几十条，不用分批）
	handles := make([]uintptr, max)
	var returned uint32
	ok, _, err := pEvtNext.Call(h, uintptr(max), uintptr(unsafe.Pointer(&handles[0])),
		0, 0, uintptr(unsafe.Pointer(&returned)))
	if ok == 0 {
		e := errnoOf(err)
		if errors.Is(e, errNoMoreItems) {
			return nil, nil // 通道开着但一条记录都没有 —— 不是错误
		}
		return nil, e
	}
	defer func() {
		for i := uint32(0); i < returned; i++ {
			pEvtClose.Call(handles[i])
		}
	}()

	var sb strings.Builder
	got := 0
	for i := uint32(0); i < returned; i++ {
		x, err := evtRenderXml(handles[i])
		if err != nil {
			continue // 单条渲染失败不该拖垮整批
		}
		if x != "" {
			sb.WriteString(x)
			got++
		}
	}
	if got == 0 {
		return nil, nil
	}
	return bootParseEvents(sb.String())
}

// evtRenderXml 把一个事件句柄渲染成 XML 字符串（UTF-16 → UTF-8）。
func evtRenderXml(evt uintptr) (string, error) {
	var used, propCount uint32
	// 第一遍：Size 传 0，让系统把需要的字节数写进 used
	// （必定以 ERROR_INSUFFICIENT_BUFFER 失败，这是规定动作不是异常）
	r, _, err := pEvtRender.Call(0, evt, evtRenderEventXml, 0, 0,
		uintptr(unsafe.Pointer(&used)), uintptr(unsafe.Pointer(&propCount)))
	if r != 0 || used == 0 {
		return "", nil // 空渲染
	}
	buf := make([]byte, used)
	r, _, err = pEvtRender.Call(0, evt, evtRenderEventXml, uintptr(used),
		uintptr(unsafe.Pointer(&buf[0])),
		uintptr(unsafe.Pointer(&used)), uintptr(unsafe.Pointer(&propCount)))
	if r == 0 {
		return "", errnoOf(err)
	}
	return utf16BytesToString(buf), nil
}

// utf16BytesToString 把 UTF-16LE 字节切片转成 Go 字符串。
// 渲染结果以 NUL 结尾，syscall.UTF16ToString 会自动在 NUL 处截断。
func utf16BytesToString(b []byte) string {
	if len(b) < 2 {
		return ""
	}
	n := len(b) / 2
	u16 := unsafe.Slice((*uint16)(unsafe.Pointer(&b[0])), n)
	return syscall.UTF16ToString(u16)
}

/* bootParseEvents 解析事件 XML。原生路径和 wevtutil 回退路径共用这一套：
   抹掉 xmlns（Go 的 xml 包匹配不上带命名空间的元素名）+ 洗掉控制字符
   （实测遇到过名字里带 0x03 的记录，会让**整批**解析失败）。 */
func bootParseEvents(raw string) ([]xmlEvent, error) {
	clean := bootSanitize(reXmlns.ReplaceAllString(raw, ""))
	if strings.TrimSpace(clean) == "" {
		return nil, nil
	}
	var out xmlEvents
	if err := xml.Unmarshal([]byte("<Events>"+clean+"</Events>"), &out); err != nil {
		return nil, err
	}
	return out.Events, nil
}

// errnoOf 把 syscall 返回的 error 规整成 syscall.Errno，便于用 errors.Is 判断。
func errnoOf(err error) error {
	if err == nil {
		return errors.New("wevtapi call failed")
	}
	if errno, ok := err.(syscall.Errno); ok {
		return errno
	}
	return err
}

/* bootQuery 统一入口：优先原生 API，失败回退 wevtutil。
   回退不是摆设 —— 万一某台机器上 wevtapi 行为异常，至少功能还能用（代价是
   中文名可能受损），总比整个卡片空白好。 */
func bootQueryWithFallback(query string, max int) ([]xmlEvent, bool, error) {
	evs, err := bootQueryNative(query, max)
	if err == nil {
		return evs, false, nil
	}
	// 通道不存在 / 权限不足：换 wevtutil 也是同样结果，不必再试，直接把准确原因报上去
	if errors.Is(err, errEvtChannelNotFound) || errors.Is(err, errAccessDenied) {
		return nil, false, err
	}
	// 其余（DLL 缺失、查询语法、渲染异常）再走一次老路
	evs2, timedOut, err2 := bootQueryWevtutil(query, max)
	if err2 == nil {
		return evs2, timedOut, nil
	}
	return nil, timedOut, err // 两个都不行，报原生那个错误（信息更准）
}

// bootQueryWevtutil 原来的 wevtutil 实现，作为回退保留。
func bootQueryWevtutil(query string, max int) ([]xmlEvent, bool, error) {
	exe := bootWevtutil()
	if exe == "" {
		return nil, false, errBootNoTool
	}
	args := []string{
		"qe", bootLogName,
		"/q:" + query,
		"/f:xml",
		"/rd:true",
		"/c:" + strconv.Itoa(max),
	}
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()

	cmd := bootCmd(ctx, exe, args...)
	b, err := cmd.Output()
	if err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return nil, true, err
		}
		return nil, false, err
	}
	evs, err := bootParseEvents(bootDecode(b))
	if err != nil {
		return nil, false, err
	}
	return evs, false, nil
}
