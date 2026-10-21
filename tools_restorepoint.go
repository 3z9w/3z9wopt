package main

// 创建系统还原点 —— 走 Windows 原生 Win32 API（srclient.dll!SRSetRestorePointW）
//
// 为什么不继续用 PowerShell 的 Checkpoint-Computer（2026-09-23 重写）：
//   工具库设计原则第 1 条写死了「全部走 Windows 原生命令，**一个都不依赖 PowerShell**」——
//   目标用户群大量使用精简版系统，PowerShell 托管引擎被精简是常态。
//   本机实测：powershell.exe 报 80070002（退出码 0xFFFF0002）彻底起不来，
//   也就是说这个「改坏能回退」的关键按钮在开发机上必然失败，且报错只会指向
//   「系统保护没开」让用户白找一圈。SRSetRestorePointW 是系统自带 API，零外部依赖。

import (
	"fmt"
	"strings"
	"syscall"
	"unicode/utf16"
	"unsafe"
)

// RESTOREPOINTINFO / STATEMGRSTATUS 的 Go 镜像（Win32 结构体布局，务必保持一一对应）
//
//	typedef struct _RESTOREPOINTINFOW {
//	    DWORD  dwEventType;        // BEGIN_SYSTEM_CHANGE 等
//	    DWORD  dwRestorePtType;    // MODIFY_SETTINGS 等
//	    INT64  llSequenceNumber;   // 只有 END_NESTED 之类的操作才用得上
//	    WCHAR  szDescription[256];
//	} RESTOREPOINTINFOW;                                     // 4+4+8+512 = 528 字节
//
//	typedef struct _STATEMGRSTATUS {
//	    DWORD  nStatus;            // 操作状态（ERROR_*）
//	    INT64  llSequenceNumber;   // 创建成功时回填的还原点序号
//	} STATEMGRSTATUS;                                        // 4+4(pad)+8 = 16 字节
type restorePointInfoW struct {
	EventType      uint32
	RestorePtType  uint32
	SequenceNumber int64
	Description    [256]uint16
}

type stateMgrStatus struct {
	Status         uint32
	SequenceNumber int64
}

// 还原点事件类型 / 类型常量（winreg.h / srrestoreptapi.h）
const (
	rpBeginSystemChange = 100 // BEGIN_SYSTEM_CHANGE：调用即创建，无需配对 END
	rpModifySettings    = 12  // MODIFY_SETTINGS：系统设置被修改前的快照
	rpMaxDesc           = 256 // 描述最大字符数（含结尾 \0）
)

const rpDefaultDesc = "3z9wOpt 优化前快照"

// createSystemRestorePoint 调用 SRSetRestorePointW 创建还原点。
// 成功返回 nil；失败返回一句用户看得懂、并指出下一步怎么办的话。
func createSystemRestorePoint(desc string) error {
	proc := syscall.NewLazyDLL("srclient.dll").NewProc("SRSetRestorePointW")
	if err := proc.Find(); err != nil {
		return fmt.Errorf("找不到 srclient.dll 里的 SRSetRestorePointW（%w）——这台机器的系统还原组件可能已被精简，请改用 Windows 自带的「创建还原点」界面", err)
	}

	var rpi restorePointInfoW
	rpi.EventType = rpBeginSystemChange
	rpi.RestorePtType = rpModifySettings

	if strings.TrimSpace(desc) == "" {
		desc = rpDefaultDesc
	}
	u := utf16.Encode([]rune(desc))
	if len(u) > rpMaxDesc-1 { // 留一位给结尾的 \0
		u = u[:rpMaxDesc-1]
	}
	copy(rpi.Description[:], u)

	var sms stateMgrStatus
	ret, _, errno := proc.Call(
		uintptr(unsafe.Pointer(&rpi)),
		uintptr(unsafe.Pointer(&sms)),
	)
	if ret == 0 {
		return fmt.Errorf("创建失败：%s", rpErrText(errno, sms.Status))
	}
	if sms.Status != 0 {
		return fmt.Errorf("创建失败：%s", rpErrText(syscall.Errno(sms.Status), sms.Status))
	}
	return nil
}

// rpErrText 把常见错误码翻译成人话 —— 用户看到的是这些字，写清楚能省一次来回。
// 参考既有错误码 mapping，未知的一律带上原始码，方便用户反馈时贴过来。
func rpErrText(errno error, status uint32) string {
	code := uint32(0)
	if errno != nil {
		if e, ok := errno.(syscall.Errno); ok {
			code = uint32(e)
		}
	}
	if code == 0 {
		code = status
	}
	switch code {
	case 5: // ERROR_ACCESS_DENIED
		return "权限不足（需要以管理员身份运行本软件）"
	case 1058: // ERROR_SERVICE_DISABLED
		return "系统保护没开启 —— C 盘右键 → 属性 → 系统保护 → 启用，然后再跑一次"
	case 1460: // ERROR_TIMEOUT
		return "系统卷影服务响应超时，过一会儿再试；还不行就重启一次电脑"
	case 1610: // ERROR_INSTALL_FAILURE
		return "距离上一次创建太近了，Windows 有最短间隔限制（通常24小时内不重复创建同名快照）"
	case 1056, 1057: // ERROR_SERVICE_ALREADY_RUNNING / ERROR_INVALID_SERVICE_ACCOUNT
		return "卷影复制服务状态异常，建议重启一次电脑再试"
	}
	return fmt.Sprintf("系统返回错误 %d：%v", code, strings.TrimSpace(errno.Error()))
}
