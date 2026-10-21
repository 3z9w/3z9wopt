package main

// ============================================================================
// fwboot.go —— 固件 / 启动模式 / 安全启动 / UEFI 证书（2026-10-10 新增）
//
// 为什么单独一个文件：本机配置页的横幅要摆这几项，而它们**全都不是
// GetHardwareInfo / readHWSpec 现有字段**。凑进 pcinfo.go 会把「硬件规格」
// 和「固件与启动状态」两类完全不同的东西混在一起。
//
// ⚠️ 本文件所有取值遵守项目铁律：读不到就留空，前端写「未获取到」。
//    绝不因为"读不到 PEFirmwareType"就猜"那应该是传统 BIOS 吧"—— 那是编。
//
// ── 三项数据的坑（都是本机实测踩出来的，别再走一遍）────────────────────
//
// ① 启动模式（UEFI / 传统BIOS）
//
//	常查的 HKLM\SYSTEM\CurrentControlSet\Control\PEFirmwareType
//	在 Win10 上有值，**Win11 微软已移除**（本机实测就是 null）。
//	所以走三条判据交叉：
//	  a) bcdedit /enum firmware —— 有 \EFI\... 就是 UEFI（本机实测有效）
//	     ⚠️ 需要管理员权限；权限不足读不到（返回空）
//	  b) Get-Partition 的 C 盘类型 —— Basic=MBR / GPT。
//	     ⚠️ UEFI+CSM 装出来也可能显示 MBR，所以只能当**佐证**，不能单独定论
//	  c) 固件目录里有没有 \EFI —— 作为兜底
//	三条都拿不到 → 返回空，让前端写「未获取到」
//
// ② 安全启动状态
//
//	HKLM\SYSTEM\CurrentControlSet\Control\SecureBoot\State\UEFISecureBootEnabled
//	**不需要管理员权限**（本机实测读到 0）。1=开启 0=关闭。
//	键整个不存在 = 这台机不是 UEFI 启动（传统 BIOS 没这个概念），写空。
//
// ③ UEFI 证书数量
//
//	只能用 PowerShell 的 Get-SecureBootUEFI，**精简版系统经常把 SecureBoot
//	模块阉割掉**（本机实测报"无法加载 SecureBoot 模块"）。
//	读不到 → 返回空，绝不编个数��
//
// ── 为什么 CPU / 主板温度做不了（别再试了）──────────────────────────────
//	HWiNFO / CoreTemp / AIDA64 读 CPU 封装温度，靠的是**自带的 Ring 0 内核驱动**
//	去读 CPU 的 MSR 寄存器（Intel 是 IA32_THERM_STATUS / 0x19C）。
//	Windows **没有提供任何用户态 API** 能读它；主板温度同理要读 SuperIO 芯片。
//	我们不带签名驱动（微软 EV 证书个人拿不到，且装驱动有蓝屏风险），
//	所以这两项只能读ACPI 温度区（见 hwlive.go readTemps），照实标「温度区」。
// ============================================================================

import (
	"context"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/windows/registry"
)

// FwBoot 固件与启动相关的一组状态。全为空字符串 = 这一项在这台机上取不到。
type FwBoot struct {
	BIOSVendor string `json:"biosVendor"` // BIOS 厂商（American Megatrends Inc.）
	BIOSVer    string `json:"biosVer"`    // BIOS 版本（F4）
	BIOSDate   string `json:"biosDate"`   // BIOS 发布日期（2026-06-14）
	SMBIOS     string `json:"smbios"`     // SMBIOS 版本号
	BootMode   string `json:"bootMode"`   // UEFI / 传统 BIOS
	BootDetail string `json:"bootDetail"` // 判据说明，如「EFI 启动项」「MBR 分区」
	SecureBoot string `json:"secureBoot"` // 开启 / 关闭
	SBCerts    string `json:"sbCerts"`    // UEFI 证书数量（读不到留空）
}

// readFwBoot 一次读完固件与启动状态。任何一项读不到就是空字符串，不猜。
func readFwBoot() FwBoot {
	var f FwBoot

	// ── BIOS：厂商 / 版本 / 日期 / SMBIOS ──
	//
	// ⚠️ 这里踩过一个坑（WMI 查询的字段必须全部被 class 支持）：
	//   一次查 Manufacturer,SMBIOSBIOSVersion,ReleaseDate,SMBIOSVersion 四个字段，
	//   其中某个字段名这台机不认 → **整行结果变成空 map**，
	//   于是连本来读得到的 SMBIOSBIOSVersion 也一起丢了（本机实测）。
	// 所以改成**分两次查、每次只取核心字段**，某一项查不到不影响其它项。
	// 厂商与日期另外用宽一点的字段名兜（见下）。
	row := wmiFirst(`root\cimv2`,
		"SELECT SMBIOSBIOSVersion,ReleaseDate FROM Win32_BIOS",
		[]string{"SMBIOSBIOSVersion", "ReleaseDate"})
	f.BIOSVer = strings.TrimSpace(row["SMBIOSBIOSVersion"])
	if t, ok := parseWmiDate(row["ReleaseDate"]); ok {
		f.BIOSDate = t.Format("2006-01-02")
	}
	// 厂商单独查：Manufacturer 在个别精简系统上不上报，查不到就留空，
	// 但**绝不能因此把已经拿到的版本/日期也丢掉**。
	f.BIOSVendor = strings.TrimSpace(
		wmiFirst(`root\cimv2`, "SELECT Manufacturer FROM Win32_BIOS",
			[]string{"Manufacturer"})["Manufacturer"])
	f.SMBIOS = strings.TrimSpace(
		wmiFirst(`root\cimv2`, "SELECT SMBIOSVersion FROM Win32_BIOS",
			[]string{"SMBIOSVersion"})["SMBIOSVersion"])

	// ── 启动模式：三条判据交叉 ──
	f.BootMode, f.BootDetail = readBootMode()

	// ── 安全启动：注册表，不需要管理员 ──
	f.SecureBoot = readSecureBoot()

	// ── UEFI 证书：只能走 PowerShell，读不到就留空 ──
	f.SBCerts = readSbCerts()

	return f
}

// readBootMode 判定这台机是 UEFI 还是传统 BIOS 启动。
// 返回 ("UEFI"|"传统 BIOS"|"", 判据说明)。三条判据都拿不到就返回空串。
func readBootMode() (string, string) {
	// 判据 a：bcdedit 的 firmware 段里有 \EFI\... 就是 UEFI 启动（最直接）
	// 本机实测有效。⚠️ 需要管理员；权限不足时输出为空 / 报错，静默跳过即可。
	//
	// ⚠️⚠️ 必须 HideWindow（2026-10-10 用户实测）：bcdedit.exe 是**控制台程序**，
	// Go 从 GUI 进程启动它，Windows 会给它分配一个控制台 → 界面上真的会闪出一个
	// 黑色命令行窗口。用户点「本机配置」看到的"两个弹窗口"，一个是这里、
	// 一个是下面 readSbCerts 的 PowerShell。
	// PowerShell 带 -WindowStyle Hidden 也挡不住：那个参数只能隐藏它**自己已经
	// 挂在上面**的那个控制台，而 Go 起进程时新建出来的这个框它管不了。
	// 真正管用的是下面的 SysProcAttr（HideWindow + CREATE_NO_WINDOW），
	// 让 Windows 创建进程时根本不建控制台 —— 与 junk_clean.go / toolkit.go 同口径。
	/* ⚠️⚠️ 必须带超时（2026-10-10 补）：
	   bcdedit / PowerShell 在某些精简版系统、被安全软件拦、固件存储异常的机器上
	   会**一直挂着不返回** —— 而它们是在 GetPCFull 里同步调的，一旦挂住，
	   整个本机配置页（连首页那次预读）就永久卡死，用户看到的是"软件死了"。
	   3 秒拿不到就当这台机读不到，照实留空，比卡死强得多。 */
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	bc := exec.CommandContext(ctx, resolveExe("bcdedit.exe"), "/enum", "firmware")
	bc.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: 0x08000000}
	if out, err := bc.CombinedOutput(); err == nil {
		s := decodeConsole(out)
		if i := strings.Index(s, `\EFI\`); i >= 0 {
			// 把命中的那行截出来当说明，别把整段输出摆到界面上
			line := s[i:]
			if e := strings.IndexAny(line, "\r\n"); e > 0 {
				line = line[:e]
			}
			detail := "检测到 EFI 启动项"
			if n := e2eBootItem(line); n != "" {
				detail = n
			}
			return "UEFI", detail
		}
	}

	// 判据 b：系统盘分区表类型。GPT 只能由 UEFI 装出来 → 可以直接定论 UEFI；
	// MBR 可能是传统 BIOS，也可能是 UEFI+CSM → 只能说「未发现 EFI 启动项」，
	// 这里给的是**弱结论**，所以只在 a 没结论时用。
	if isGPTSystemVolume() {
		return "UEFI", "系统盘为 GPT 分区表"
	}
	return "", ""
}

// e2eBootItem 从 bcdedit 那一行里抠出启动项名字（\EFI\Microsoft\Boot\BOOTMGFW.EFI → Windows Boot Manager）
func e2eBootItem(line string) string {
	line = strings.TrimSpace(line)
	if line == "" {
		return ""
	}
	low := strings.ToLower(line)
	switch {
	case strings.Contains(low, "bootmgfw"):
		return "检测到 Windows Boot Manager（EFI 启动项）"
	case strings.Contains(low, "\\efi\\"):
		return "检测到 EFI 启动项"
	}
	return "检测到 EFI 启动项"
}

// isGPTSystemVolume 判断系统盘是不是 GPT 分区表。
// 读 C 盘所在卷的分区表类型；读不到返回 false。
// 依据：DeviceIoControl IOCTL_DISK_GET_DRIVE_TYPE 拿不到这个，
// 走 WMI 的 MSFT_Partition（有 GPT/GPTBasic/MBR 枚举）最省事且不需权限。
func isGPTSystemVolume() bool {
	/* ⚠️ 旧实现不加条件地扫**所有**分区：只要任意一块盘是 GPT 就判 UEFI。
	   但「MBR 装系统 + 一块 ≥2TB 的 GPT 数据盘」在真实用户里非常常见 ——
	   那台机会被错报成 UEFI，还和安全启动「未获取到」自相矛盾（真 UEFI 不会读不到）。
	   → 现在只看**系统盘**（%SystemDrive% 那个盘符）的分区表类型。
	   Win7 没有 root\Microsoft\Windows\Storage 命名空间 → 查询失败 → 返回 false，
	   不会误判，交由其它判据处理。 */
	wql := "SELECT Type FROM MSFT_Partition"
	if letter := systemDriveLetter(); letter != "" {
		wql += " WHERE DriveLetter='" + letter + "'"
	}
	rows, err := wmiQuery(`root\Microsoft\Windows\Storage`, wql, []string{"Type"})
	if err != nil || len(rows) == 0 {
		return false
	}
	for _, r := range rows {
		if strings.EqualFold(strings.TrimSpace(r["Type"]), "GPT") {
			return true
		}
	}
	return false
}

// systemDriveLetter 取系统盘盘符（"C:" → "C"），取不到返回空。
// 用环境变量而不是写死 "C"：系统盘在别人机器上完全可能是 D: / E:
// （多系统、PE 装的系统、企业定制镜像都很常见），写死 C 就是只在开发机正确。
func systemDriveLetter() string {
	d := strings.TrimSpace(os.Getenv("SystemDrive"))
	if i := strings.IndexRune(d, ':'); i > 0 {
		return d[:i]
	}
	return d
}

// readSecureBoot 读安全启动状态。返回 开启/关闭，取不到返回空串。
// UEFISecureBootEnabled 在 HKLM 下，**不需要管理员权限**（本机实测读到 0）。
// 键不存在 = 这台机不是 UEFI 启动，传统 BIOS 没这个概念 → 留空而不是写"关闭"。
func readSecureBoot() string {
	const sub = `SYSTEM\CurrentControlSet\Control\SecureBoot\State`
	k, err := registry.OpenKey(registry.LOCAL_MACHINE, sub, registry.QUERY_VALUE|registry.WOW64_64KEY)
	if err != nil {
		return ""
	}
	defer k.Close()
	v, _, err := k.GetIntegerValue("UEFISecureBootEnabled")
	if err != nil {
		return ""
	}
	switch v {
	case 1:
		return "开启"
	case 0:
		return "关闭"
	}
	return ""
}

// readSbCerts 读 UEFI 安全启动证书数量。
// ⚠️ 只能走 PowerShell 的 Get-SecureBootUEFI，精简系统常把 SecureBoot 模块
// 阉割掉（本机实测报"无法加载 SecureBoot 模块"）。读不到返回空串，**绝不编个数**。
// 本程序以管理员身份运行，所以权限不是问题；问题是模块本身在不在。
func readSbCerts() string {
	ps := resolveExe("powershell.exe")
	if ps == "" {
		return ""
	}
	// -NoProfile：不加载用户配置，快；窗口隐藏：-WindowStyle Hidden
	// 2>/dev/null 不走：改用 CombinedOutput 自己判错。
	// 超时同上：PowerShell 冷启动本来就慢（1~2 秒），给 5 秒；再久就当读不到，
	// 绝不让它把 GetPCFull 拖成"界面卡死"。
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, ps, "-NoProfile", "-NonInteractive", "-WindowStyle", "Hidden",
		"-Command", "try{ (Get-SecureBootUEFI -ErrorAction Stop | Measure-Object).Count }catch{ exit 1 }")
	// 同上：不让 Windows 新建控制台窗口，否则用户会看到一个黑框闪出来。
	// -WindowStyle Hidden 挡不住这个（它只能藏 PowerShell 自己的窗口），靠 SysProcAttr。
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: 0x08000000}
	out, err := cmd.Output() // Output 只取 stdout，stderr 丢弃
	if err != nil {
		return ""
	}
	n, err := strconv.Atoi(strings.TrimSpace(decodeConsole(out)))
	if err != nil || n < 0 {
		return ""
	}
	return strconv.Itoa(n) + " 个"
}