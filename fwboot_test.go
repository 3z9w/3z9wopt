package main

// ============================================================================
// fwboot_test.go —— 固件 / 启动模式 / 安全启动 的回归哨兵（2026-10-10）
//
// 这些函数全是「读不到就返回空」的形状，很容易在后续改动里被写坏：
//   · 有人图省事，把空串填成默认值（"UEFI"/"开启"）→ 变成编数据
//   · 有人改字段名/JSON tag → 前端拿不到，前功尽弃
// 所以在这里钉死：**取不到必须是空，绝不允许是编出来的值**。
// ============================================================================

import (
	"strings"
	"testing"
)

// 1) 启动模式：要么是三种合法值之一，要么空字符串；绝不能是别的
func TestBootModeIsHonest(t *testing.T) {
	mode, detail := readBootMode()
	switch mode {
	case "UEFI", "传统 BIOS":
		// 合法。detail 应该有说明（说明判据来源，便于用户对照）
		if strings.TrimSpace(detail) == "" {
			t.Errorf("启动模式=%s 但没给判据说明：界面要告诉用户凭什么这么判的", mode)
		}
	case "":
		// 合法：取不到就是取不到，不猜
	default:
		t.Errorf("启动模式返回了非法值 %q —— 只允许 UEFI / 传统 BIOS / 空", mode)
	}
}

// 2) 安全启动：只允许 开启 / 关闭 / 空
func TestSecureBootIsHonest(t *testing.T) {
	switch v := readSecureBoot(); v {
	case "开启", "关闭", "":
	default:
		t.Errorf("安全启动返回了非法值 %q —— 只允许 开启 / 关闭 / 空", v)
	}
}

// 3) UEFI 证书数：读不到必须是空串，绝不能编一个数字出来
func TestSbCertsNotFaked(t *testing.T) {
	v := readSbCerts()
	if v == "" {
		return // 取不到，允许
	}
	if !strings.HasSuffix(v, " 个") {
		t.Errorf("UEFI 证书数格式不对：%q（应形如「3个」或空串）", v)
	}
	// 绝不允许出现"未知/0 个"这种编出来的说法：读不到就是空
	if strings.Contains(v, "未知") || strings.HasPrefix(v, "0") {
		t.Errorf("UEFI 证书数疑似编造：%q —— 读不到应返回空串", v)
	}
}

// 4) readFwBoot 必须能跑通，且不会 panic（本机跑一把真数据）
func TestReadFwBootNoPanic(t *testing.T) {
	f := readFwBoot()
	// 不做具体值断言 —— 换一台机器值就变了。这里只保证：
	// ① 不panic ② 各字段组合起来不会自相矛盾
	if f.BootMode == "传统 BIOS" && f.SecureBoot == "开启" {
		t.Error("自相矛盾：传统 BIOS 启动不可能开着 UEFI 安全启动")
	}
	if f.BiosVendorSeen() && f.BootMode != "" && f.BIOSVer == "" {
		// 有厂商却没版本，可能是 WMI 只返回了一半 —— 允许，但记一笔提醒
		t.Log("提示：读到 BIOS 厂商但没读到版本号，界面上会有一格是「未获取到」")
	}
}

// 5) ★ 关键哨兵：HWDisk 必须带 SSD 字段，否则所有盘都会被标成「机械」
//
// 这条是被真实 bug 逼出来的：前端 `ssd: !!d.ssd`，而 HWDisk 当时没有 ssd 字段，
// `!!undefined` = false，于是**所有机器的固态盘都被显示成机械**。
func TestHWDiskHasSSDBool(t *testing.T) {
	// GetDiskList 拿真实枚举值；有盘就验标记能带过来
	disks := (&App{}).GetDiskList()
	if len(disks) == 0 {
		t.Skip("本机没读到固定盘，跳过（不枚举不等于坏了）")
	}
	h := (&App{}).GetHardwareInfo()
	if len(h.Disks) == 0 {
		t.Fatal("GetDiskList 有盘，但 GetHardwareInfo().Disks 是空的 —— 转抄丢了")
	}
	if len(h.Disks) != len(disks) {
		t.Errorf("盘数对不上：DiskList=%d HWInfo.Disks=%d", len(disks), len(h.Disks))
	}
	// 逐盘核对 SSD 标记有没有原样带过来（至少不能全是"机械"却声称接口正常）
	anySSD := false
	for i, d := range h.Disks {
		if i < len(disks) {
			if d.SSD != disks[i].SSD {
				t.Errorf("第%d块盘 SSD 标记在转抄时变了：%v → %v", i, disks[i].SSD, d.SSD)
			}
		}
		if d.SSD {
			anySSD = true
		}
	}
	if anySSD {
		t.Log("本机有固态盘，SSD 标记传递正常")
	} else {
		t.Log("本机盘全是机械/未识别（或读不到介质类型），跳过 SSD 真值核对")
	}
}

// 辅助：读 BIOS 厂商是否存在
func (f FwBoot) BiosVendorSeen() bool { return strings.TrimSpace(f.BIOSVendor) != "" }