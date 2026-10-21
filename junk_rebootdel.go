package main

// ============================================================================
// 3z9wOpt v1.1 · 「重启后再删」—— 被占用文件的兜底删除（2026-10-14 新增）
//
// 【为什么要有这个】
// 2026-10-14 真机实测（见 .workbuddy/memory/2026-10-14.md）：
//   扫描到 965.5 MB / 计划清 262.4 MB / 实际只释放 115.9 MB / 残留 209.0 MB，
//   其中 **134 个文件删不掉，全部是「被占用」**，清掉率只有 44.2%。
// 分类口径、主数字口径都已经修过了，剩下的唯一病根就是「删不掉怎么办」。
//
// 【依据（不是我拍脑袋）】
// 1. 开源项目 LightC v2.16.16 `cleaner/enhanced_delete.rs`：
//    MoveFileExW + MOVEFILE_DELAY_UNTIL_REBOOT，`enable_reboot_delete: true` 默认开，
//    注释原话「这是 Windows 系统存储感知处理顽固文件的标准方式」。
// 2. 某同类清理引擎的导入表里**没有 MoveFileEx**（已用 PE 导入表取证），
//    所以它也不做这件事 —— 做了就是真的比它多一招，不是嘴上说说。
//
// 【安全边界】
// - 只对**用户已勾选、已经被判为垃圾**的文件排队，不扩大任何删除范围；
// - 命中运行时保护清单（protectedRuntimePaths）的**一律不排**，宁可留着；
// - 只排普通文件，不排目录（目录要等里面文件清空才可能删掉，提前排没意义）；
// - 没有管理员/提权时第一次调用就会失败，之后整场不再白试。
// ============================================================================

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"

	"golang.org/x/sys/windows"
)

// moveFileDelayUntilReboot = MOVEFILE_DELAY_UNTIL_REBOOT
//（x/sys/windows 里有同名常量，这里再定义一份是为了让本文件的意图自解释，值必须与系统一致）
const moveFileDelayUntilReboot = 0x00000004

var (
	errRebootDisabled  = errors.New("重启删除未启用")
	errRebootProteced  = errors.New("受保护的系统路径，不排重启删除")
	errRebootNotFile   = errors.New("不是可排队的普通文件")
	errRebootNoPriv    = errors.New("权限不足，无法写入重启删除队列")
)

var (
	rebootMu      sync.Mutex
	rebootWorks   = -1 // -1=还没试过 / 0=这台机器上不可用 / 1=可用
	rebootPlanned = map[string]bool{}
)

// rebootDeleteEnabled 总开关。环境变量 NO_REBOOTDEL=1 可整场关掉
//（给单测和被占用的排查场景留个后门，正常情况下没人会设它）。
func rebootDeleteEnabled() bool { return os.Getenv("NO_REBOOTDEL") == "" }

// markRebootDelete 把删不掉的文件排进 Windows 的重启删除队列
//（HKLM\...\Session Manager\PendingFileRenameOperations，由系统自己写，我们不碰注册表）。
//
// 返回 nil 表示**已排队**：文件现在还在、空间现在没释放，重启后由系统在早启动阶段删掉。
// 调用方必须把它和「已释放」分开计数 —— 混在一起就是虚报释放量。
func markRebootDelete(path string) error {
	if !rebootDeleteEnabled() {
		return errRebootDisabled
	}
	if isProtectedRuntimePath(path) {
		return errRebootProteced
	}
	if fi, err := os.Stat(path); err != nil || fi.IsDir() {
		return errRebootNotFile
	}
	rebootMu.Lock()
	if rebootWorks == 0 {
		rebootMu.Unlock()
		return errRebootNoPriv
	}
	rebootMu.Unlock()

	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return err
	}
	if err := windows.MoveFileEx(p, nil, moveFileDelayUntilReboot); err != nil {
		/* 只有「权限/特权」类失败才是整场都救不回来的，要立刻闭嘴别再一个个白调 API；
		   路径过长、文件刚好被别的动作挪走这类是偶发的，下次该试还得试。 */
		if isRebootPrivError(err) {
			rebootMu.Lock()
			rebootWorks = 0
			rebootMu.Unlock()
			return errRebootNoPriv
		}
		return err
	}
	rebootMu.Lock()
	rebootWorks = 1
	rebootPlanned[strings.ToLower(filepath.Clean(path))] = true
	rebootMu.Unlock()
	return nil
}

// isRebootPrivError 权限/特权类失败：ERROR_ACCESS_DENIED(5) / ERROR_PRIVILEGE_NOT_HELD(1314)
func isRebootPrivError(err error) bool {
	var en syscall.Errno
	if errors.As(err, &en) {
		return en == 5 || en == 1314
	}
	return false
}

// isRebootPlanned 这个路径是不是本次会话排进重启队列了。
// 用途：清后复扫会看到它还在，原因得如实说「已排到重启后删除」，
// 而不是含糊地报成「被占用」让用户再点一次（点了还是一样）。
func isRebootPlanned(path string) bool {
	rebootMu.Lock()
	defer rebootMu.Unlock()
	return rebootPlanned[strings.ToLower(filepath.Clean(path))]
}

// rebootPlannedReset 清空本会话的排队记录（单测用）
func rebootPlannedReset() {
	rebootMu.Lock()
	rebootPlanned = map[string]bool{}
	rebootWorks = -1
	rebootMu.Unlock()
}
