package main

import (
	"os"
	"testing"
	"unsafe"
)

// ---------- 创建系统还原点（tools_restorepoint.go）----------
// 两类验证：
//   1) 结构体布局 —— Win32 ABI 对字节数很敏感，Go 镜像结构一旦错位，
//      SRSetRestorePointW 会静默失败甚至崩溃，必须锁死；
//   2) 真跑一遍 —— 默认跳过（创建还原点有副作用），只在 RP_LIVE=1 时执行，
//      用来在本机实证 API 真能建成功，而不是"看起来对"。

func TestRestorePointStructLayout(t *testing.T) {
	var rpi restorePointInfoW
	var sms stateMgrStatus

	// RESTOREPOINTINFOW = DWORD(4) + DWORD(4) + INT64(8) + WCHAR[256](512) = 528
	if got, want := unsafe.Sizeof(rpi), uintptr(528); got != want {
		t.Errorf("restorePointInfoW 大小 = %d，Win32 要求 %d 字节（结构体字段顺序或类型错了吗）", got, want)
	}
	// szDescription 起始偏移必须是 16（4+4+8），错位会导致描述写到别的结构字段上
	if got, want := unsafe.Offsetof(rpi.Description), uintptr(16); got != want {
		t.Errorf("Description 偏移 = %d，应为 %d", got, want)
	}
	// STATEMGRSTATUS = DWORD(4) + 4 对齐填充 + INT64(8) = 16
	if got, want := unsafe.Sizeof(sms), uintptr(16); got != want {
		t.Errorf("stateMgrStatus 大小 = %d，Win32 要求 %d 字节", got, want)
	}
	if got, want := unsafe.Offsetof(sms.SequenceNumber), uintptr(8); got != want {
		t.Errorf("SequenceNumber 偏移 = %d，应为 %d", got, want)
	}
	t.Logf("布局核对通过：RESTOREPOINTINFOW %d 字节 / STATEMGRSTATUS %d 字节",
		unsafe.Sizeof(rpi), unsafe.Sizeof(sms))
}

// TestRestorePointToolWired 确认工具已注册、且不再依赖 PowerShell（工具库设计原则第 1 条）。
func TestRestorePointToolWired(t *testing.T) {
	def, ok := toolImpls["fix-restore-point"]
	if !ok {
		t.Fatal("工具库里没有注册 fix-restore-point")
	}
	if def.Run == nil {
		t.Error("fix-restore-point 没有 Run 实现（应为原生 API 调用）")
	}
	if len(def.Steps) > 0 {
		t.Errorf("fix-restore-point 还在走外部命令 %v —— 工具库设计上不应依赖 PowerShell", def.Steps)
	}
}

// TestCreateRestorePointLive 真刀真枪建一次还原点。默认跳过，需要时：
//   RP_LIVE=1 go test -run TestCreateRestorePointLive -v .
func TestCreateRestorePointLive(t *testing.T) {
	if os.Getenv("RP_LIVE") != "1" {
		t.Skip("跳过实测（设置 RP_LIVE=1 才会真创建系统还原点）")
	}
	err := createSystemRestorePoint(rpDefaultDesc)
	if err != nil {
		t.Fatalf("创建失败：%v", err)
	}
	t.Log("创建成功，返回 nil")
}
