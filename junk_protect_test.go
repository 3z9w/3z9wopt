package main

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

// TestJunkRuntimeProtected 运行时保护清单自检。
//
// 背景（2026-10-13 事故）：「.NET 程序集缓存 (GAC)」项把
// C:\Windows\Microsoft.NET\assembly\GAC_32 / GAC_64 / GAC_MSIL 当缓存清掉了。
// GAC 不是缓存，是 .NET Framework 强命名程序集的安装区，删了不会自动重建，
// 结果所有依赖 GAC 的 .NET 程序启动即抛 0xE0434352 静默崩溃（表现为"双击 exe 没反应"）。
// 该项已整项移除，并在 junk_clean.go 加了 protectedRuntimePaths 硬拦截兜底。
//
// 这个测试守住两件事：
//  1. 保护清单确实拦得住 GAC（系统盘不在 C 盘也要拦住）
//  2. 保护清单没有误伤该清的目录，且现有清理项里再也出现不了 GAC
func TestJunkRuntimeProtected(t *testing.T) {
	// ---- 1. 必须拦住的 ----
	mustBlock := []string{
		`C:\Windows\Microsoft.NET\assembly\GAC_MSIL`,
		`C:\Windows\Microsoft.NET\assembly\GAC_32`,
		`C:\Windows\Microsoft.NET\assembly\GAC_64`,
		`C:\Windows\assembly\GAC_MSIL`,
		`D:\Windows\Microsoft.NET\assembly\GAC_MSIL`, // 系统盘不在 C 盘也要拦（铁律一）
		`C:\Windows\Microsoft.NET\Framework64\v4.0.30319`,
		`C:\Windows\System32\drivers`,
	}
	for _, p := range mustBlock {
		if !isProtectedRuntimePath(p) {
			t.Errorf("保护失效，该拦没拦住: %s", p)
		}
	}

	// ---- 2. 必须放行的（误伤会让正常清理项失效）----
	mustPass := []string{
		`C:\Windows\System32\LogFiles`,                    // svclogs
		`C:\Windows\WinSxS\Temp`,                          // winsxstmp
		`C:\Windows\assembly\NativeImages_v4.0.30319_32`,  // netnative（NGen 是真缓存，用户拍板保留）
		`C:\Windows\assembly\NativeImages_v4.0.30319_64`,  // netnative
		`C:\Windows\Installer\$PatchCache$`,               // patchcache
		`C:\Program Files\Steam\logs`,                     // steam
	}
	for _, p := range mustPass {
		if isProtectedRuntimePath(p) {
			t.Errorf("误伤，不该保护却保护了: %s", p)
		}
	}

	// ---- 3. 清单里不许再出现 GAC ----
	for _, it := range junkItems() {
		for _, d := range it.dirs {
			ld := strings.ToLower(filepath.Clean(d))
			if strings.Contains(ld, `\microsoft.net\assembly\gac_`) ||
				strings.Contains(ld, `\assembly\gac`) {
				t.Errorf("清理项 [%s] 又把 GAC 当目标了: %s", it.ID, d)
			}
		}
	}

	// ---- 4. 现有所有项的实际目标，不应有任何一个被保护清单拦下 ----
	items := junkItems()
	blocked := 0
	for i := range items {
		it := items[i]
		for _, d := range it.dirs {
			if isProtectedRuntimePath(d) {
				t.Errorf("清理项 [%s] 的目标被保护清单拦下，请确认是否误伤: %s", it.ID, d)
				blocked++
			}
		}
	}
	if blocked == 0 && !testing.Verbose() {
		fmt.Println("运行时保护自检: 拦截/放行/无 GAC/无误伤 全部通过")
	}
}
