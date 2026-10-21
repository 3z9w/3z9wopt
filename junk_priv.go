package main

// ============================================================================
// 「删得动」的地基：备份/还原特权 + 带备份语义的删除（2026-10-15 补）
//
// 【这话是被真机数据逼出来的】
// 当天二连清取证（见 .workbuddy/memory/2026-10-15.md）：
//   C:\Windows\WinSxS\Temp 报了 2.09 GB / 10166 个文件，一轮清下来**一个都没删掉** ——
//   那一处属 TrustedInstaller，ACL 里压根没给 Administrators 写的份儿，
//   os.Remove（内部就是 DeleteFileW）必然 ERROR_ACCESS_DENIED，
//   MoveFileEx 排队同样被拒。于是「扫描敢报 2.09 GB，清理只到账 226 MB」，
//   用户第二次扫描看到的还是同一个数字 —— 那不是垃圾又长出来了，是压根没删动。
//
// 【依据（不是拍脑袋）】
//   1. SeBackupPrivilege / SeRestorePrivilege 是给备份操作员**绕过目标 ACL** 用的
//      （Windows 访问控制模型里写明），绝大多数磁盘清理类工具处理 TrustedInstaller
//      文件用的就是这两个特权；
//   2. 打开句柄带 FILE_FLAG_BACKUP_SEMANTICS，才能以「系统文件的语义」打开目录与
//      受保护文件；
//   3. 删除走 SetFileInformationByHandle(FileDispositionInfo)：持有 DELETE 句柄直接
//      标记删除，比 DeleteFileW 少一次打开、也少一次 ACL 检查。
//
// 【安全边界】
//   - 只对**已经过 junkItems 清单判定为垃圾**的路径使用，不扩大任何删除范围；
//   - protectedRuntimePaths 命中的路径到不了这里（调用方先拦）；
//   - 特权只在本进程内启用一次，进程退出即失效，**不改系统任何设置或 ACL**。
//
// 【铁律对齐】判不出来就说判不出来：特权启用失败只允许静默退化（回到 os.Remove），
//   不允许假装成功、更不许把这块能力算进「能清理」的额度里。
// ============================================================================

import (
	"errors"
	"fmt"
	"os"
	"sync"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

// luidAttr / tokenPrivBuf：自己拼 AdjustTokenPrivileges 的输入缓冲。
// 不用 windows.Tokenprivileges 是因为它只允许 1 条特权，而我们要一次给两条。
type luidAttr struct {
	Luid  windows.LUID
	Attrs uint32
}

type tokenPrivBuf struct {
	Count uint32
	Privs [8]luidAttr
}

var (
	// SetFileInformationByHandle 不走 x/sys 的包装 —— 它的签名在各版本里换过，
	// 直接 Call 省得编译期扯皮，成功与否看返回值（BOOL）。
	setFileInfoProc = syscall.NewLazyDLL("kernel32.dll").NewProc("SetFileInformationByHandle")
	fileDispositionInfo = uintptr(4) // FILE_INFO_BY_HANDLE_CLASS::FileDispositionInfo
)

var (
	privOnce sync.Once
	privHeld bool
)

// deletePrivilegesReady 本进程是否已拿到「绕过 ACL」的两把钥匙。
// 幂等：一辈子只试一次，失败就永远退化到普通删除（不许重试刷日志）。
func deletePrivilegesReady() bool {
	privOnce.Do(func() {
		err := enableDeletePrivileges()
		privHeld = err == nil
		if err != nil {
			cleanTrace(fmt.Sprintf("删除特权不可用（退回普通删除，TrustedInstaller 那类文件仍删不掉）: %v", err))
		} else {
			cleanTrace("删除特权已启用（SeBackupPrivilege + SeRestorePrivilege）")
		}
	})
	return privHeld
}

// enableDeletePrivileges 给本进程开启 SeBackupPrivilege + SeRestorePrivilege。
func enableDeletePrivileges() error {
	var tok windows.Token
	if err := windows.OpenProcessToken(windows.CurrentProcess(),
		windows.TOKEN_ADJUST_PRIVILEGES|windows.TOKEN_QUERY, &tok); err != nil {
		return fmt.Errorf("打不开本进程令牌：%w", err)
	}
	defer tok.Close()

	var buf tokenPrivBuf
	for _, name := range []string{"SeBackupPrivilege", "SeRestorePrivilege"} {
		w, err := syscall.UTF16PtrFromString(name)
		if err != nil {
			return err
		}
		var luid windows.LUID
		/* 精简版 / 被组策略削过的系统可能压根没有这个特权 —— 少一个不算失败，
		   剩下的照用；但如果一个都没有，那就明确回调11失败。 */
		if err := windows.LookupPrivilegeValue(nil, w, &luid); err != nil {
			cleanTrace(fmt.Sprintf("本机没有 %s: %v", name, err))
			continue
		}
		buf.Privs[buf.Count] = luidAttr{Luid: luid, Attrs: windows.SE_PRIVILEGE_ENABLED}
		buf.Count++
	}
	if buf.Count == 0 {
		return errors.New("本机没有可启用的备份/还原特权")
	}
	/* ⚠️ 同一个非零返回 ≠ 同一种错（铁律一）：
	   ERROR_NOT_ALL_ASSIGNED(1300) 的含义是「有一部分特权没批准」，并非全盘失败 ——
	   已经批下来的那些依然生效。这里当成成功，但不能假装全都拿到了。 */
	if err := windows.AdjustTokenPrivileges(tok, false,
		(*windows.Tokenprivileges)(unsafe.Pointer(&buf)), 0, nil, nil); err != nil {
		var en syscall.Errno
		if errors.As(err, &en) && en == 1300 {
			return nil
		}
		return err
	}
	return nil
}

// removeNow 按「普通删除 → 备份语义删除」两档试一次。
// 返回第一档的错误给上层做分类/留证（那才是系统说的原话），成功就算成功。
//
// 为什么要在 os.Remove 之外再补一刀：这两档在系统眼里是**两件事**——
// os.Remove 走的是「我是用户，求 ACL 让我删」，第二档走的是「我是备份操作员，ACL 让开」。
func removeNow(p string) (bool, error) {
	err := os.Remove(p)
	if err == nil {
		return true, nil
	}
	if deletePrivilegesReady() {
		if err2 := deleteByDisposition(p); err2 == nil {
			return true, nil
		}
	}
	return false, err
}

// deleteByDisposition 用 DELETE 句柄 + FileDispositionInfo 标记删除，
// 这是处理 ACL 不让删的文件的标准写法。
func deleteByDisposition(p string) error {
	w, err := syscall.UTF16PtrFromString(p)
	if err != nil {
		return err
	}
	/* 只读文件必须先摘掉只读属性 —— 否则设置删除标记会直接回绝，
	   跟"有没有权限"是两回事（同一条设计错误会对上一个以上的原因，铁律一）。 */
	if attr, err := windows.GetFileAttributes(w); err == nil && attr&windows.FILE_ATTRIBUTE_READONLY != 0 {
		_ = windows.SetFileAttributes(w, attr&^windows.FILE_ATTRIBUTE_READONLY)
	}
	h, err := windows.CreateFile(w, windows.DELETE, 0, nil, windows.OPEN_EXISTING,
		windows.FILE_FLAG_BACKUP_SEMANTICS, 0)
	if err != nil {
		return err
	}
	defer windows.CloseHandle(h)
	var disp byte = 1 // FILE_DISPOSITION_INFO.DeleteFile = TRUE（BOOLEAN，1 字节）
	r, _, e := setFileInfoProc.Call(uintptr(h), fileDispositionInfo,
		uintptr(unsafe.Pointer(&disp)), 1)
	if r == 0 {
		if en, ok := e.(syscall.Errno); ok && en != 0 {
			return en
		}
		return syscall.Errno(5)
	}
	return nil
}

// traceRemoveFail 删除失败留证（每轮最多前 20 条，多了只计数）。
// 2026-10-15：以前 os.Remove 失败既不写明原因也不落日志，事后只能靠「复扫残留」反推，
// 排一次障要跑一整轮清理。现在把系统说的原话直接留在 %TEMP%\3z9wOptV11_clean_trace.log。
func traceRemoveFail(p string, err error, n *int) {
	if *n >= 20 {
		return
	}
	*n++
	cleanTrace(fmt.Sprintf("remove fail %s err=%v", p, err))
}

// probeCanDelete 扫描期的「这条现在删不删得动」，判据与 removeNow **同一把钥匙**：
// 以 DELETE 访问权 + 备份语义试开一次。
//
// 为什么必须换掉原来那把（原来试的是"能不能独占打开"）：
// 独占打开成功 ≠ 能删掉。WinSxS\Temp 那一万个文件全都"打得开"，于是被判成 2.09 GB
// 「能清」，删的时候才发现一个都删不动 —— 扫描报的额度就这么虚掉了。
// 判据和实际动作对不上，比不判更糟：用户正是照着这个数字决定要不要点的。
//
// ⚠️ 调用方必须**先开过 deletePrivilegesReady()**：扫描期与清理期的判定要用同一套权限，
// 否则会出现"扫描说能删、清理说没权限"这种自己打自己脸的结果。
func probeCanDelete(p string) string {
	w, err := syscall.UTF16PtrFromString(p)
	if err != nil {
		return "other"
	}
	h, err := windows.CreateFile(w, windows.DELETE, 0, nil, windows.OPEN_EXISTING,
		windows.FILE_FLAG_BACKUP_SEMANTICS, 0)
	if err == nil {
		windows.CloseHandle(h) // 只是开一下看看能不能拿到 DELETE，别真删
		return "new"
	}
	var en syscall.Errno
	if errors.As(err, &en) {
		switch en {
		case 32: // ERROR_SHARING_VIOLATION：有人占着，关掉它或重启就好 —— 可解
			return "locked"
		case 5: // ERROR_ACCESS_DENIED：系统保护（TrustedInstaller 那类），删不动就是删不动
			return "denied"
		case 2, 3: // 找不到：扫描过程里它自己没了
			return "gone"
		}
	}
	return "other"
}
