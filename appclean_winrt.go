package main

// ============ 免 PowerShell 的 UWP 移除（WinRT COM 直调）============
// 2026-09-21：精简版系统大量缺失 PowerShell 托管引擎（System.Management.Automation.dll），
// Remove-AppxPackage 路线彻底不可用。本文件直接从 Go 调 Windows 部署引擎
// （Windows.Management.Deployment.PackageManager.RemovePackageAsync），
// 这是 Win8+ 系统自带的原生 API，精简系统也躲不掉。
//
// 调用链（全部经过本机探针实证，错误码 0x80073CF1 与预期一致）：
//   RoInitialize → RoGetActivationFactory(PackageManager) → IActivationFactory.ActivateInstance
//   → QI IPackageManager → vtable[8].RemovePackageAsync(pfn)
//   → QI IAsyncInfo → 轮询 Status(0=Started) → get_ErrorCode
//
// 关键事实（本机 HKCR\Interface 登记）：
//   IPackageManager IID = {9A7D4B65-5E8F-4FC7-A2E5-7F6925CB8B53}
//   ActivateInstance 在 IActivationFactory vtable 槽 6（IUnknown3 + IInspectable3）
//   RemovePackageAsync 在 IPackageManager 槽 8（add=6, update=7, remove=8）
//   IAsyncInfo: get_Status 槽 7、get_ErrorCode 槽 8

import (
	"fmt"
	"runtime"
	"syscall"
	"time"
	"unsafe"
	"unicode/utf16"
)

var (
	combaseRT = syscall.NewLazyDLL("combase.dll")
	pRoInitRT = combaseRT.NewProc("RoInitialize")
	pRoUninit = combaseRT.NewProc("RoUninitialize")
	pRoGetAct = combaseRT.NewProc("RoGetActivationFactory")
	pWinCrStr = combaseRT.NewProc("WindowsCreateString")
	pWinDelSt = combaseRT.NewProc("WindowsDeleteString")
)

var (
	guidActivationFactory = guidRT(0x00000035, 0x0000, 0x0000, [8]byte{0xC0, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x46})
	guidAsyncInfo         = guidRT(0x00000036, 0x0000, 0x0000, [8]byte{0xC0, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x46})
	guidPackageManager    = guidRT(0x9A7D4B65, 0x5E8F, 0x4FC7, [8]byte{0xA2, 0xE5, 0x7F, 0x69, 0x25, 0xCB, 0x8B, 0x53})
)

func guidRT(d1 uint32, d2, d3 uint16, rest [8]byte) syscall.GUID {
	return syscall.GUID{Data1: d1, Data2: d2, Data3: d3, Data4: rest}
}

func hstringRT(s string) (uintptr, error) {
	u16 := utf16.Encode([]rune(s))
	if len(u16) == 0 {
		var h uintptr
		return h, nil
	}
	var h uintptr
	r, _, _ := pWinCrStr.Call(uintptr(unsafe.Pointer(&u16[0])), uintptr(len(u16)), uintptr(unsafe.Pointer(&h)))
	if r != 0 {
		return 0, syscall.Errno(r)
	}
	return h, nil
}

/* vtable 取 COM 接口的虚表。p 是 Win32 以 uintptr 交出来的接口指针：
   先做一次类型重解释拿到指针（与 uintptr→unsafe.Pointer 转换二进制等价，但 go vet 认可），
   再读它第一个字（虚表指针）。 */
func vtable(p uintptr) *[24]uintptr {
	pp := *(*unsafe.Pointer)(unsafe.Pointer(&p))
	return (*[24]uintptr)(*(*unsafe.Pointer)(pp))
}

func comRelease(p uintptr) {
	if p != 0 {
		syscall.Syscall(vtable(p)[2], 1, p, 0, 0) // IUnknown::Release
	}
}

// AsyncStatus: 0=Started 1=Completed 2=Canceled 3=Error
const (
	asyncStarted   = 0
	asyncCompleted = 1
	asyncCanceled  = 2
	asyncError     = 3
)

// removeUwpViaWinRT 免 PS 移除当前用户的 UWP 包。返回 (部署引擎 HRESULT, error)。
// hr=0 表示移除成功；hr!=0 时 error 携带可读信息。
func removeUwpViaWinRT(packageFullName string, timeout time.Duration) (uint32, error) {
	if packageFullName == "" {
		return 0, fmt.Errorf("包全名为空")
	}

	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	if r, _, _ := pRoInitRT.Call(1); r != 0 && r != 1 { // S_OK / S_FALSE
		return uint32(r), fmt.Errorf("COM 初始化失败 (0x%08X)", uint32(r))
	}
	defer pRoUninit.Call()

	cid, err := hstringRT("Windows.Management.Deployment.PackageManager")
	if err != nil {
		return 0, err
	}
	defer pWinDelSt.Call(cid)

	var factory uintptr
	if r, _, _ := pRoGetAct.Call(cid, uintptr(unsafe.Pointer(&guidActivationFactory)), uintptr(unsafe.Pointer(&factory))); r != 0 {
		return uint32(r), fmt.Errorf("部署引擎不可用 (0x%08X)", uint32(r))
	}
	defer comRelease(factory)

	var inst uintptr
	if hr, _, _ := syscall.Syscall(vtable(factory)[6], 2, factory, uintptr(unsafe.Pointer(&inst)), 0); hr != 0 {
		return uint32(hr), fmt.Errorf("部署引擎激活失败 (0x%08X)", uint32(hr))
	}
	defer comRelease(inst)

	var pm uintptr
	if hr, _, _ := syscall.Syscall(vtable(inst)[0], 3, inst, uintptr(unsafe.Pointer(&guidPackageManager)), uintptr(unsafe.Pointer(&pm))); hr != 0 || pm == 0 {
		return uint32(hr), fmt.Errorf("包管理接口不可用 (0x%08X)", uint32(hr))
	}
	defer comRelease(pm)

	pfn, err := hstringRT(packageFullName)
	if err != nil {
		return 0, err
	}
	defer pWinDelSt.Call(pfn)

	var op uintptr
	if hr, _, _ := syscall.Syscall(vtable(pm)[8], 3, pm, pfn, uintptr(unsafe.Pointer(&op))); hr != 0 {
		return uint32(hr), fmt.Errorf("移除请求被拒绝 (0x%08X)", uint32(hr))
	}
	defer comRelease(op)

	var ai uintptr
	if hr, _, _ := syscall.Syscall(vtable(op)[0], 3, op, uintptr(unsafe.Pointer(&guidAsyncInfo)), uintptr(unsafe.Pointer(&ai))); hr != 0 || ai == 0 {
		return uint32(hr), fmt.Errorf("异步状态不可用 (0x%08X)", uint32(hr))
	}
	defer comRelease(ai)

	atab := vtable(ai)
	deadline := time.Now().Add(timeout)
	var status int32
	for {
		if hr, _, _ := syscall.Syscall(atab[7], 2, ai, uintptr(unsafe.Pointer(&status)), 0); hr != 0 {
			return uint32(hr), fmt.Errorf("状态查询失败 (0x%08X)", uint32(hr))
		}
		if status != asyncStarted {
			break
		}
		if time.Now().After(deadline) {
			// 超时取消，避免悬挂
			syscall.Syscall(atab[9], 1, ai, 0, 0) // IAsyncInfo::Cancel
			return 0, fmt.Errorf("移除超时（已请求取消），请稍后在列表刷新后确认结果")
		}
		time.Sleep(400 * time.Millisecond)
	}

	if status == asyncCompleted {
		return 0, nil
	}
	var hrCode int32
	syscall.Syscall(atab[8], 2, ai, uintptr(unsafe.Pointer(&hrCode)), 0)
	return uint32(hrCode), deployErrText(uint32(hrCode), status)
}

// isDeployResultHR 判断是否部署引擎的真实错误（facility=WIN32，即 0x8007xxxx）。
// 这类错误是部署服务对本次请求的裁决（包不存在/被占用等），应反馈用户；
// 否则是 COM 激活/初始化类失败（0x8004xxxx 等），调用方可降级 PS 重试。
func isDeployResultHR(hr uint32) bool {
	return hr&0xFFFF0000 == 0x80070000 && hr != 0
}

// deployErrText 常见部署错误码 → 用户可读文案
func deployErrText(hr uint32, status int32) error {
	if status == asyncCanceled {
		return fmt.Errorf("移除操作被取消")
	}
	switch hr {
	case 0x80073CF1:
		return fmt.Errorf("该应用已不存在（可能已被卸载）")
	case 0x80073D02:
		return fmt.Errorf("应用正在运行或资源被占用，请先关闭它再试")
	case 0x80073CFE:
		return fmt.Errorf("系统包仓库损坏，请先运行『系统维护』的 DISM/SFC 修复")
	}
	return fmt.Errorf("系统返回错误码 0x%08X", hr)
}
