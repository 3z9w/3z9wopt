package main

import (
	"fmt"
	"strings"
	"syscall"
	"unsafe"
)

/* ============================================================================
   DXGI 显存读取（通用路）
   ----------------------------------------------------------------------------
   为什么要有这一路：注册表 `HardwareInformation.qwMemorySize` 是驱动自己写的，
   **不是每个驱动都写**（NVIDIA/AMD/Intel 三代驱动写法还各不相同），换台机器就可能整列读空。
   DXGI 是 Windows 图形栈自己的接口，任务管理器「专用/共享 GPU 内存」就是这一路，
   不依赖驱动心情 —— 所以它排在读取链最前面，注册表降级为兜底。

   这块没有现成的 Go 封装：DXGI 是纯 COM（不是 IDispatch），oleutil 那套自动化接口用不上，
   只能自己按 vtable 下标调。下标是 ABI 定的，改不得：
     IUnknown     : 0 QueryInterface / 1 AddRef / 2 Release
     IDXGIObject  : 3 SetPrivateData / 4 SetPrivateDataInterface / 5 GetPrivateData / 6 GetParent
     IDXGIFactory : 7 EnumAdapters ...
     IDXGIFactory1: 12 EnumAdapters1
     IDXGIAdapter : 8 GetDesc
   ============================================================================ */

const (
	vtblRelease       = 2
	vtblEnumAdapters1 = 12 // IDXGIFactory1::EnumAdapters1(UINT, IDXGIAdapter1**)
	vtblGetDesc       = 8  // IDXGIAdapter::GetDesc(DXGI_ADAPTER_DESC*)
)

// S_OK = 0；DXGI_ERROR_NOT_FOUND = 0x887A0002（枚举结束，正常不是错误）
const dxgiS_OK = 0

var (
	dxgiDLL   = syscall.NewLazyDLL("dxgi.dll")
	ole32DLL  = syscall.NewLazyDLL("ole32.dll")
	pCreateFx = dxgiDLL.NewProc("CreateDXGIFactory1")
	pCoInitEx = ole32DLL.NewProc("CoInitializeEx")
)

// comGUID 内存布局 = Windows GUID（Data1 uint32 / Data2 uint16 / Data3 uint16 / Data4 8 字节）
type comGUID struct {
	Data1 uint32
	Data2 uint16
	Data3 uint16
	Data4 [8]byte
}

// IID_IDXGIFactory1 = {770AAE78-F26F-4DBA-A829-253C83D1B387}
var iidDXGIFactory1 = comGUID{
	Data1: 0x770AAE78, Data2: 0xF26F, Data3: 0x4DBA,
	Data4: [8]byte{0xA8, 0x29, 0x25, 0x3C, 0x83, 0xD1, 0xB3, 0x87},
}

// dxgiAdapterDesc 对应 DXGI_ADAPTER_DESC（64 位下 304 字节）
// 字段顺序照 MSDN 写死，改顺序会静默读错 —— 有单测锁 Sizeof 和偏移量。
type dxgiAdapterDesc struct {
	Description           [128]uint16
	VendorID              uint32
	DeviceID              uint32
	SubSysID              uint32
	Revision              uint32
	DedicatedVideoMemory  uint64 // 专用显存（独立显存）
	DedicatedSystemMemory uint64 // 专用系统内存（一般很小）
	SharedSystemMemory    uint64 // 共享显存（集显吃内存就是这一档）
	AdapterLUID           [8]byte
}

// comCall 按 vtable 下标调用 COM 方法，自动把 this 补到第一个参数
/* COM 接口指针是 Win32 以 uintptr 交出来的。直接 uintptr→unsafe.Pointer 会被 go vet
   判为误用；这里先做一次类型重解释拿到指针，之后全程按指针走（虚表指针也按指针读）。 */
func comCall(obj uintptr, idx int, args ...uintptr) uintptr {
	if obj == 0 {
		return 0x80004003 // E_POINTER
	}
	p := *(*unsafe.Pointer)(unsafe.Pointer(&obj))
	vt := *(*unsafe.Pointer)(p)
	if vt == nil {
		return 0x80004003
	}
	fn := *(*uintptr)(unsafe.Add(vt, uintptr(idx)*unsafe.Sizeof(uintptr(0))))
	if fn == 0 {
		return 0x80004002 // E_NOINTERFACE
	}
	all := make([]uintptr, 0, len(args)+1)
	all = append(all, obj)
	all = append(all, args...)
	ret, _, _ := syscall.SyscallN(fn, all...)
	return ret
}

// readVRAMDxgi 枚举所有 DXGI 适配器，取每个的专用/共享显存。
// 读不到任何一块就返回 nil，交给后面的注册表路兜底 —— 绝不返回 0 冒充"读到了 0 字节"。
func readVRAMDxgi() []vramInfo {
	if err := pCreateFx.Find(); err != nil {
		return nil // 系统没有 dxgi.dll（XP 及更早）
	}
	// COM 初始化：同进程里 WMI 可能已经初始化过，返回 RPC_E_CHANGED_MODE(0x80010106) 属正常，忽略
	if err := pCoInitEx.Find(); err == nil {
		pCoInitEx.Call(0, 2) // COINIT_APARTMENTTHREADED
	}

	var factory uintptr
	hr, _, _ := pCreateFx.Call(
		uintptr(unsafe.Pointer(&iidDXGIFactory1)),
		uintptr(unsafe.Pointer(&factory)),
	)
	if hr != dxgiS_OK || factory == 0 {
		return nil
	}
	defer comCall(factory, vtblRelease)

	var out []vramInfo
	seen := map[string]bool{}
	for i := uint32(0); i < 16; i++ {
		var adapter uintptr
		hr := comCall(factory, vtblEnumAdapters1, uintptr(i), uintptr(unsafe.Pointer(&adapter)))
		if hr != dxgiS_OK || adapter == 0 {
			break // DXGI_ERROR_NOT_FOUND = 枚举完了；其它错误也别硬撑
		}
		var d dxgiAdapterDesc
		if comCall(adapter, vtblGetDesc, uintptr(unsafe.Pointer(&d))) == dxgiS_OK {
			name := strings.TrimSpace(syscall.UTF16ToString(d.Description[:]))
			/* 软件渲染器（Microsoft Basic Render Driver / 远程投屏的虚拟卡）不是真显卡。
			   它们专用显存 = 0，只报共享 —— 放进显卡列表会让用户以为自己有两块卡。
			   判据：专用显存为 0 的一律不收（真卡哪怕集显也会预留一块专用，本机实测 128 MB）。 */
			if d.DedicatedVideoMemory > 0 && !isSoftwareAdapter(name, d.VendorID) {
				/* 一块物理卡会被 DXGI 枚举出多份（本机实测同名的 Intel UHD 610 出了 3 份，
				   三个 LUID 还都不一样，所以**不能按 LUID 去重**）。
				   按「显卡名 + 厂商 + 设备号 + 容量」去重：同一块卡的多份实例这些全一样。
				   两块不同型号（如 iGPU + 独显）厂商/设备号不同，不会被误合并。 */
				key := fmt.Sprintf("%s|%04X|%04X|%d", name, d.VendorID, d.DeviceID, d.DedicatedVideoMemory)
				if !seen[key] {
					seen[key] = true
					it := vramInfo{
						Name:    name,
						Bytes:   d.DedicatedVideoMemory,
						Shared:  d.SharedSystemMemory,
						DedSys:  d.DedicatedSystemMemory,
						Source:  "dxgi",
						Vendor:  d.VendorID,
						Device:  d.DeviceID,
					}
					it.Size = fmtRAM(int64(it.Bytes))
					if it.Shared > 0 {
						it.SharedSize = fmtRAM(int64(it.Shared))
					}
					out = append(out, it)
				}
			}
		}
		comCall(adapter, vtblRelease)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// isSoftwareAdapter 明确的软件/虚拟适配器黑名单。
// 0x1414 = Microsoft（Basic Render Driver 的 VendorID），不靠名字是因为名字会本地化。
func isSoftwareAdapter(name string, vendor uint32) bool {
	if vendor == 0x1414 {
		return true
	}
	low := strings.ToLower(name)
	for _, k := range []string{"basic render", "microsoft basic", "remote display", "mirror"} {
		if strings.Contains(low, k) {
			return true
		}
	}
	return false
}

/* readVRAM 读取链：DXGI（通用）→ 显示类注册表 → DEVICEMAP\VIDEO
   每一路都填 Source，页面上「数据来源」能说清这个数是哪条路来的。 */
func readVRAMDxgiFirst() []vramInfo {
	if v := readVRAMDxgi(); len(v) > 0 {
		return v
	}
	return nil
}
