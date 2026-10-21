package main

// ============================================================================
// 3z9wOpt v1.1 · Win32 小工具
//
// 1) isElevated：MFT 直读、停服务、写 HKLM 都要提权，单测里先问一句，没提权就跳过
//    （照 v1.0 appclean_uwp.go 的实现）。exe 清单要求 requireAdministrator，正式跑恒为 true；
//    这个函数主要给「以别的身份跑单测/调试」兜底。
// 2) shell32lazy：shell32.dll 的懒加载句柄。v1.0 把它放在 optimize.go 里，而 1.1 的 optimize.go
//    是自己写的（那边直接 NewLazyDLL），但搬来的 appclean / appicon 需要这个包级变量，
//    所以统一放这里 —— Win32 句柄集中一处，谁要谁用。
// ============================================================================

import (
	"syscall"

	"golang.org/x/sys/windows"
)

// isElevated 当前进程令牌是否是提权令牌（UAC 视角）
func isElevated() bool {
	return windows.GetCurrentProcessToken().IsElevated()
}

// shell32lazy shell32.dll 懒加载句柄（CommandLineToArgvW / ExtractIconExW / SHChangeNotify 都用它）
var shell32lazy = syscall.NewLazyDLL("shell32.dll")
