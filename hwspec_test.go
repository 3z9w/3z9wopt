package main

import (
	"encoding/json"
	"regexp"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
	"unsafe"
)

// ============================================================================
// 本机配置（PC Info）读取链路回归测试
//
// 背景：这一页要跑在任意一台用户机器上，所以这里钉住的**不是**「我这台机器读到了什么」，
// 而是「解析逻辑本身对不对」。凡是靠拼表/猜值能过、但换台机器就错的写法，都会被下面的
// 反证断言打回来。
//
// 每个测试都带 FALSIFY 注释，写明它要挡住的那个具体 bug。
// ============================================================================

// buildSMBIOS 按 SMBIOS 规范拼一张最小固件表：头部 4 字节（Type/Length/Handle）+ 格式化区 + 字符串表。
func buildSMBIOS(blocks ...[]byte) []byte {
	var out []byte
	for _, b := range blocks {
		out = append(out, b...)
	}
	return out
}

// smbiosBlock 拼一个结构体：typ/len + 格式化区 + 字符串表（双 NUL 收尾）
func smbiosBlock(typ byte, fields map[int][]byte, fixedLen int, strs ...string) []byte {
	buf := make([]byte, fixedLen)
	buf[0] = typ
	buf[1] = byte(fixedLen)
	buf[2], buf[3] = 0x01, 0x00 // handle
	for off, v := range fields {
		if off+len(v) <= len(buf) {
			copy(buf[off:], v)
		}
	}
	for _, s := range strs {
		buf = append(buf, []byte(s)...)
		buf = append(buf, 0)
	}
	buf = append(buf, 0) // 字符串表结束标记
	return buf
}

// ---------------------------------------------------------------------------
// 1. SMBIOS 结构头：Data 必须保留 Type/Length/Handle 4 字节
//
// FALSIFY：早期实现写的是 Data = b[i+4 : i+l]（砍掉头 4 字节），
// 但字段偏移仍照规范写 → 整表错位 4 字节。实测后果：
//   BIOS 厂商读成日期「06/14/2018」（拿的是 0x08 ReleaseDate 的字符串索引）
//   内存读成「FLASH 1541 MHz 0.5 GB」
// ---------------------------------------------------------------------------
func TestSMBIOSKeeps4ByteHeader(t *testing.T) {
	// Type 0（BIOS Information），规范长度 0x18
	t0 := smbiosBlock(0x00, map[int][]byte{
		0x04: {0x01}, // Vendor       -> 字符串 #1
		0x05: {0x02}, // Version      -> 字符串 #2
		0x08: {0x03}, // Release Date -> 字符串 #3
		0x14: {0x03}, // System BIOS Major Release
		0x15: {0x03}, // System BIOS Minor Release
	}, 0x18, "American Megatrends Inc.", "F4", "06/14/2018")

	tbl := buildSMBIOS(t0)
	st := parseSMBIOS(tbl)
	if len(st) != 1 {
		t.Fatalf("期望解析出 1 个结构体，实际 %d", len(st))
	}
	got := st[0]

	// 结构头必须还在 Data 里
	if len(got.Data) == 0 || got.Data[0] != 0x00 {
		t.Fatalf("Data 丢失了结构头：Data[0]=%#x，Data=%v", got.Data[0], got.Data)
	}
	if len(got.Data) != 0x18 {
		t.Fatalf("Data 长度应等于规范 Length=0x18，实际 %d（若差 4 说明头被砍了）", len(got.Data))
	}

	// 三个字符串必须各就各位，不能整体错位
	if v := got.str(got.u8(0x04)); v != "American Megatrends Inc." {
		t.Errorf("BIOS 厂商错位：got=%q want=%q（错位时会读到日期字符串）", v, "American Megatrends Inc.")
	}
	if v := got.str(got.u8(0x05)); v != "F4" {
		t.Errorf("BIOS 版本错位：got=%q want=%q", v, "F4")
	}
	if v := got.str(got.u8(0x08)); v != "06/14/2018" {
		t.Errorf("BIOS 日期错位：got=%q want=%q", v, "06/14/2018")
	}
	if v := normalizeBIOSDate(got.str(got.u8(0x08))); v != "2018-06-14" {
		t.Errorf("BIOS 日期归一化错误：got=%q want=%q", v, "2018-06-14")
	}
	if v := got.u8(0x14); v != 0x03 {
		t.Errorf("SMBIOS 主版本错位：got=%d want=3", v)
	}
}

// ---------------------------------------------------------------------------
// 2. SMPBIOS Type 16：最大支持容量偏移是 0x07，槽位数偏移是 0x0D
//
// FALSIFY：早期把 Maximum Capacity 读在 0x0F（那是 2.7+ 的 Extended Maximum Capacity），
// 在 2.x 长度的条目上会读到 0 或越界；槽位数也曾读错位置。
// ---------------------------------------------------------------------------
func TestSMBIOSType16Offsets(t *testing.T) {
	// 64 GB = 67108864 KB = 0x04000000，小端
	t16 := smbiosBlock(0x10, map[int][]byte{
		0x04: {0x03},                      // Location
		0x05: {0x03},                      // Use
		0x06: {0x06},                      // Error Correction
		0x07: {0x00, 0x00, 0x00, 0x04},    // Maximum Capacity = 64 GB(KB)
		0x0D: {0x04, 0x00},                // Number of Memory Devices = 4
	}, 0x17)

	st := parseSMBIOS(buildSMBIOS(t16))
	if len(st) != 1 {
		t.Fatalf("期望 1 个结构体，实际 %d", len(st))
	}
	got := st[0]
	if got.Type != 0x10 {
		t.Fatalf("Type 解析错误：got=%#x want=0x10", got.Type)
	}
	if v := got.u32(0x07); v != 67108864 {
		t.Errorf("Maximum Capacity 偏移错误：got=%d want=67108864", v)
	}
	if v := got.u16(0x0D); v != 4 {
		t.Errorf("Number of Memory Devices 偏移错误：got=%d want=4", v)
	}
}

// ---------------------------------------------------------------------------
// 3. SMBIOS Type 17：容量/频率/实跑频率/电压/Rank 偏移
//
// FALSIFY：Configured Voltage 早期读在 0x11（其实是 Bank Locator 字符串索引），
// 导致电压列整列为空或出现乱码字符串。
// ---------------------------------------------------------------------------
func TestSMBIOSType17Offsets(t *testing.T) {
	t17 := smbiosBlock(0x11, map[int][]byte{
		0x08: {0x40, 0x00},             // Total Width = 64
		0x0A: {0x40, 0x00},             // Data Width = 64
		0x0C: {0x00, 0x20},             // Size = 8192 MB (8 GB)
		0x0E: {0x0B},                   // Form Factor = 11 (DIMM)
		0x10: {0x01},                   // Device Locator -> 字符串 #1
		0x11: {0x02},                   // Bank Locator   -> 字符串 #2
		0x12: {0x1A},                   // Memory Type = 26 (DDR4)
		0x15: {0x60, 0x09},             // Speed = 2400 MT/s
		0x17: {0x03},                   // Manufacturer  -> 字符串 #3
		0x18: {0x04},                   // Serial        -> 字符串 #4
		0x19: {0x05},                   // Asset Tag     -> 字符串 #5
		0x1A: {0x06},                   // Part Number   -> 字符串 #6
		0x1B: {0x01},                   // Attributes: Rank = 1
		0x20: {0x60, 0x09},             // Configured Memory Speed = 2400
		0x26: {0xB0, 0x04},             // Configured Voltage = 1200 mV
	}, 0x28, "ChannelA-DIMM0", "BANK 0", "Kingston", "SN0001", "Asset", "99P5732-001.A00G")

	st := parseSMBIOS(buildSMBIOS(t17))
	if len(st) != 1 {
		t.Fatalf("期望 1 个结构体，实际 %d", len(st))
	}
	got := st[0]

	if v := got.str(got.u8(0x10)); v != "ChannelA-DIMM0" {
		t.Errorf("槽位(Device Locator)错误：got=%q", v)
	}
	if v := got.u16(0x0C); v != 8192 {
		t.Errorf("容量偏移错误：got=%d MB want=8192", v)
	}
	if v := got.u8(0x12); v != 26 {
		t.Errorf("内存类型偏移错误：got=%d want=26(DDR4)", v)
	}
	if v := got.u16(0x15); v != 2400 {
		t.Errorf("标称频率偏移错误：got=%d want=2400", v)
	}
	if v := got.u16(0x20); v != 2400 {
		t.Errorf("实跑频率(Configured Speed)偏移错误：got=%d want=2400", v)
	}
	if v := got.u16(0x26); v != 1200 {
		t.Errorf("工作电压偏移错误：got=%d mV want=1200（读到别的值说明还在用旧的 0x11）", v)
	}
	if v := got.u8(0x1B) & 0x0F; v != 1 {
		t.Errorf("Rank 解析错误：got=%d want=1", v)
	}
	if v := got.u16(0x08); v != 64 {
		t.Errorf("Total Width 偏移错误：got=%d want=64", v)
	}
}

// ---------------------------------------------------------------------------
// 4. fmtKB 单位分档
//
// FALSIFY：≥1 GB 那一档早期单位写成 MB，把 64 GB 最大支持内存显示成「65536 MB」；
// 另有一处又额外乘了一次 1024。这里直接反证这两个输出。
// ---------------------------------------------------------------------------
func TestFmtKBUnits(t *testing.T) {
	cases := []struct {
		kb   int
		want string
	}{
		{512, "512 KB"},
		{1024, "1 MB"},
		{1536, "1.5 MB"},
		{4096, "4 MB"},
		{1024 * 1024, "1 GB"},
		{64 * 1024 * 1024, "64 GB"},
	}
	for _, c := range cases {
		got := fmtKB(c.kb)
		// 允许小数写法（64.0 GB / 64 GB 都算对），但单位必须是 want 里的那个
		if !strings.HasSuffix(got, strings.Fields(c.want)[1]) {
			t.Errorf("fmtKB(%d) = %q，单位应为 %q", c.kb, got, strings.Fields(c.want)[1])
		}
	}
	// 反证：不许再出现裸 MB 数字的超大值
	if v := fmtKB(64 * 1024 * 1024); strings.Contains(v, "65536") {
		t.Errorf("fmtKB(64GB) 出现了旧 bug 输出「65536」：%q", v)
	}
	if v := fmtKB(32 * 1024); strings.Contains(v, "GB") {
		t.Errorf("fmtKB(32MB) 不该进位到 GB：%q", v)
	}
}

// ---------------------------------------------------------------------------
// 5. CPUID 拓扑：核数/线程数
//
// FALSIFY：早期用 leaf 0x04 的子叶数量数核 → 本机被数成「8 核 16 线程」/「4 核」；
// 正确做法是 leaf 0x0B 扩展拓扑（level type 1=SMT, 2=Core）。
// 断言只检查「任何机器都必须成立」的不变量，不写死具体数值。
// ---------------------------------------------------------------------------
func TestCPUIDTopologySane(t *testing.T) {
	c := readCPUSpec()
	if c.Threads <= 0 {
		t.Fatalf("逻辑处理器数读不出来：threads=%d", c.Threads)
	}
	if c.Cores <= 0 {
		t.Fatalf("物理核心数读不出来：cores=%d", c.Cores)
	}
	if c.Cores > c.Threads {
		t.Errorf("物理核(%d) 不该多于逻辑线程(%d)", c.Cores, c.Threads)
	}
	// 线程数必须是核心数的整数倍（SMT 1/2，个别机器 4）
	if c.Threads%c.Cores != 0 {
		t.Errorf("线程数(%d) 不是核心数(%d) 的整数倍，拓扑解析大概率错了", c.Threads, c.Cores)
	}
	if c.Threads != runtime.NumCPU() {
		t.Errorf("CPUID 线程数(%d) 与 runtime.NumCPU()(%d) 不一致", c.Threads, runtime.NumCPU())
	}
	if c.Threads > 1024 || c.Cores > 512 {
		t.Errorf("核/线程数离谱：cores=%d threads=%d", c.Cores, c.Threads)
	}
}

// ---------------------------------------------------------------------------
// 6. 缓存层级
//
// FALSIFY：cpuCacheKB 早期把 level<<5 塞进 ECX（应该只传子叶序号），全核返回 0；
// 修好取值后又把 level/type 映射反了（L1D 读成 64KB、L1I 读成 256KB、L2 读成 4MB）。
// ---------------------------------------------------------------------------
func TestCPUCacheHierarchy(t *testing.T) {
	c := readCPUSpec()
	if c.CacheL1DKB <= 0 {
		t.Errorf("L1D 缓存读不到：%d KB", c.CacheL1DKB)
	}
	if c.CacheL1IKB <= 0 {
		t.Errorf("L1I 缓存读不到：%d KB", c.CacheL1IKB)
	}
	if c.CacheL2KB <= 0 {
		t.Errorf("L2 缓存读不到：%d KB", c.CacheL2KB)
	}
	// 容量必须单调递增：L1 < L2 < L3
	if c.CacheL2KB <= c.CacheL1DKB {
		t.Errorf("L2(%dKB) 不该 ≤ L1D(%dKB) —— level/type 映射可能又反了", c.CacheL2KB, c.CacheL1DKB)
	}
	if c.CacheL3KB > 0 && c.CacheL3KB <= c.CacheL2KB {
		t.Errorf("L3(%dKB) 不该 ≤ L2(%dKB)", c.CacheL3KB, c.CacheL2KB)
	}
	// 单份 L1 常见 16~64 KB，超出这个范围说明单位或乘算错了
	if c.CacheL1DKB < 8 || c.CacheL1DKB > 256 {
		t.Errorf("L1D 单份容量离谱：%d KB", c.CacheL1DKB)
	}
}

// ---------------------------------------------------------------------------
// 7. 硬盘介质判定必须是通用接口（DeviceIoControl），不能靠接口名字符串
//
// FALSIFY：Win32_DiskDrive.InterfaceType 在 Win10+ 上恒为 "IDE"，
// MSFT_PhysicalDisk 的 MediaType/BusType 在不少机器上是空的 —— 靠它们会把
// SATA 固态判成「机械硬盘 / IDE」。
// ---------------------------------------------------------------------------
func TestDiskMediaGeneric(t *testing.T) {
	ds := readDiskDrives()
	if len(ds) == 0 {
		t.Skip("本机没枚举到物理硬盘，跳过")
	}
	for _, d := range ds {
		if d.Model == "" {
			t.Errorf("硬盘型号为空：%+v", d)
		}
		// 介质只能是这两类；判不出来时留空（页面显示「读不到」），不许瞎猜
		if d.Media != "" && d.Media != "固态 SSD" && d.Media != "机械硬盘" {
			t.Errorf("硬盘介质取值非法：%q（%s）", d.Media, d.Model)
		}
	}
	// DeviceIoControl 路径本身要能返回合法值
	isSSD, bus := diskIsSSD(0)
	if isSSD < -1 || isSSD > 1 {
		t.Errorf("diskIsSSD(0) 返回值越界：%d", isSSD)
	}
	if isSSD == -1 && bus != "" {
		t.Errorf("判不出来时总线类型也应为空，实际 bus=%q", bus)
	}
}

// ---------------------------------------------------------------------------
// 8. 实时采样数值范围
//
// FALSIFY：PDH 计数器名 / 单位换算写错时会出现负数或天文数字（例如把 B/s 当 b/s、
// 或把百分比乘了 100）。这里卡住「任何机器都必须成立」的范围。
// ---------------------------------------------------------------------------
func TestLivePerfRanges(t *testing.T) {
	a := &App{}
	/* 速率类 PDH 计数器（% Idle Time / 网络 / 磁盘读写）必须两次采样才有差值，
	   首帧返回 -1 是设计好的哨兵值（前端显示 --），不是 bug。
	   所以这里先热机一帧，等 1.2s 再取正式样本。 */
	a.GetLivePerf()
	time.Sleep(1200 * time.Millisecond)
	lp := a.GetLivePerf()

	if lp.CPU < 0 || lp.CPU > 100 {
		t.Errorf("CPU 占用越界：%.1f", lp.CPU)
	}
	if lp.Mem < 0 || lp.Mem > 100 {
		t.Errorf("内存占用越界：%.1f", lp.Mem)
	}
	// 以下各项允许 -1（读不到/首帧），但只要读到了就必须在合理范围
	chk := func(name string, v float64, max float64) {
		if v < -1 {
			t.Errorf("%s 出现非法负值：%.1f（只有 -1 表示读不到）", name, v)
		}
		if v > max {
			t.Errorf("%s 数值离谱：%.1f（上限 %g）", name, v, max)
		}
	}
	chk("磁盘占用", lp.Disk, 100)
	chk("GPU 占用", lp.GPU, 100)
	chk("下行网速", lp.NetIn, 100e9)  // 100 GB/s 已远超任何民用网卡
	chk("上行网速", lp.NetOut, 100e9)
	chk("磁盘读", lp.DiskR, 100e9)
	chk("磁盘写", lp.DiskW, 100e9)
	chk("网卡带宽", lp.NetBw, 1000e9)
	// 主频：读不到是 -1（显示 --），读到就不该离谱
	if lp.CpuMHz > 0 && (lp.CpuMHz < 300 || lp.CpuMHz > 8000) {
		t.Errorf("CPU 主频离谱：%.0f MHz", lp.CpuMHz)
	}
	// 温度：ACPI 给的十分之一开尔文，换算错会出现 -200℃ 或 300℃
	for _, x := range lp.Temps {
		if x.C < -20 || x.C > 150 {
			t.Errorf("温度换算错误：%s = %.1f℃", x.Name, x.C)
		}
		if x.Name == "" {
			t.Errorf("温度项缺少名称")
		}
	}
}

// ---------------------------------------------------------------------------
// 8b. 首帧哨兵值语义
//
// FALSIFY：有人看到第一帧全是 -1 会以为「接口坏了」，然后改成返回 0 兜底。
// 返回 0 会让前端把「还没采到」显示成「网速 0 B/s、占用 0%」—— 假数据比空数据更糟。
// 这里钉死：首帧必须是 -1，绝不能是 0。
// ---------------------------------------------------------------------------
func TestLivePerfSentinelNotZero(t *testing.T) {
	var pn *pdhNamed // 计数器没建起来（机器上没有这类网卡/磁盘）
	if v := pn.get("in"); v != -1 {
		t.Errorf("取不到值时必须返回 -1，不能返回 %.1f —— 返回 0 会让前端把「没采到」显示成真实读数", v)
	}
	// clampPct 也要保留「算不出来 = -1」的语义，不能把 NaN 夹成 0
	if v := clampPct(math.NaN()); v != -1 {
		t.Errorf("clampPct(NaN) 应为 -1，实际 %.1f", v)
	}
	if v := clampPct(150); v != 100 {
		t.Errorf("clampPct(150) 应为 100，实际 %.1f", v)
	}
	if v := clampPct(-5); v != 0 {
		t.Errorf("clampPct(-5) 应为 0，实际 %.1f", v)
	}
}

// ---------------------------------------------------------------------------
// 9. GetPCFull 聚合：关键字段不许整块为空
//
// FALSIFY：早期 VRAM 读不到时整列空、网卡名退化成「以太网」。
// 这里只卡「必须有值」的项；读不到的项允许为空（页面显示「读不到」），
// 但**不允许**用猜测值填充。
// ---------------------------------------------------------------------------
func TestPCFullNoPlaceholder(t *testing.T) {
	a := &App{}
	f := a.GetPCFull()
	if f.Spec.CPUBrandID == "" {
		t.Errorf("CPU 型号为空")
	}
	if f.Spec.CPUCore == "" {
		t.Errorf("核/线程为空")
	}
	if f.Spec.MemType == "" {
		t.Errorf("内存类型为空")
	}
	if len(f.PhysDisks) == 0 {
		t.Errorf("硬盘列表为空")
	}
	if len(f.Nets) == 0 {
		t.Errorf("网卡列表为空")
	}
	for _, n := range f.Nets {
		if n.MAC == "" {
			t.Errorf("网卡缺少 MAC：%+v", n)
		}
		// 连接名（「以太网」）不能当设备名用
		if n.Name == "以太网" || n.Name == "WLAN" || n.Name == "本地连接" {
			t.Errorf("网卡名退化成连接名了：%q", n.Name)
		}
	}
}

// ============================================================================
// 10. DXGI 显存（hwvram.go）
//
// FALSIFY：DXGI 是纯 COM，没有 Go 封装，结构体靠手写镜像 Win32 布局。
// 布局一旦错位，API **不报错**，而是静默返回错的字节数（专用显存读到共享、读到 0）。
// 所以 Sizeof / Offsetof 必须用单测钉死。
// ---------------------------------------------------------------------------
func TestDXGIAdapterDescLayout(t *testing.T) {
	if got, want := unsafe.Sizeof(dxgiAdapterDesc{}), uintptr(304); got != want {
		t.Errorf("DXGI_ADAPTER_DESC 大小错误：got=%d want=%d（字段错位会静默读错显存）", got, want)
	}
	if got, want := unsafe.Offsetof(dxgiAdapterDesc{}.Description), uintptr(0); got != want {
		t.Errorf("Description 偏移错误：got=%d want=0", got)
	}
	if got, want := unsafe.Offsetof(dxgiAdapterDesc{}.DedicatedVideoMemory), uintptr(272); got != want {
		t.Errorf("DedicatedVideoMemory 偏移错误：got=%d want=272", got)
	}
	if got, want := unsafe.Offsetof(dxgiAdapterDesc{}.SharedSystemMemory), uintptr(288); got != want {
		t.Errorf("SharedSystemMemory 偏移错误：got=%d want=288", got)
	}
	// GUID 布局也是手写的，错了 CreateDXGIFactory1 直接返回 E_NOINTERFACE
	if got, want := unsafe.Sizeof(comGUID{}), uintptr(16); got != want {
		t.Errorf("GUID 大小错误：got=%d want=16", got)
	}
	if iidDXGIFactory1.Data1 != 0x770AAE78 || iidDXGIFactory1.Data2 != 0xF26F || iidDXGIFactory1.Data3 != 0x4DBA {
		t.Errorf("IID_IDXGIFactory1 常量写错：%+v", iidDXGIFactory1)
	}
}

// comCall 对空指针必须安全返回错误码，不能 panic（COM 调用失败是常态，不是异常）
func TestComCallNilSafe(t *testing.T) {
	if hr := comCall(0, vtblRelease); hr == 0 {
		t.Errorf("comCall(0) 不该返回 S_OK")
	}
}

// 软件渲染器过滤：Microsoft Basic Render Driver（VendorID 0x1414）必须被挡掉，
// 否则页面会多出一「块」显卡，用户以为自己有两张卡。
func TestSoftwareAdapterFiltered(t *testing.T) {
	if !isSoftwareAdapter("Microsoft Basic Render Driver", 0x1414) {
		t.Errorf("Microsoft Basic Render Driver 没被判成软件适配器")
	}
	if !isSoftwareAdapter("Some Remote Display Adapter", 0x8086) {
		t.Errorf("远程/虚拟显示适配器没被过滤")
	}
	if isSoftwareAdapter("Intel(R) UHD Graphics 610", 0x8086) {
		t.Errorf("真显卡被误判成软件适配器")
	}
	if isSoftwareAdapter("NVIDIA GeForce RTX 4060", 0x10DE) {
		t.Errorf("NVIDIA 独显被误判成软件适配器")
	}
}

// 显存读取链：DXGI 优先。读到的每一条都必须带 Source，不能出现「有容量但不知道哪来的」。
func TestVRAMChainHasSource(t *testing.T) {
	v := readVRAM()
	if len(v) == 0 {
		t.Skip("本机一条显存都没读到（DXGI + 注册表三路全空），跳过")
	}
	for _, x := range v {
		if x.Source == "" {
			t.Errorf("显存条目缺少来源标记：%+v", x)
		}
		if x.Source != "dxgi" && x.Source != "reg" && x.Source != "devicemap" {
			t.Errorf("来源取值非法：%q", x.Source)
		}
		if x.Bytes > 0 && x.Size == "" {
			t.Errorf("有字节数却没人话字串：%+v", x)
		}
	}
	// DXGI 那一路只要拿到数据，就不该再有「专用显存 = 0」的条目（那是软件渲染器）
	for _, x := range v {
		if x.Source == "dxgi" && x.Bytes == 0 {
			t.Errorf("DXGI 路混进了专用显存为 0 的条目（多半是软件渲染器）：%q", x.Name)
		}
	}
}

// ============================================================================
// 11. LHM 探测（hwlive_lhm.go）
//
// FALSIFY：引导入口一旦在「已经能读到真实传感器」时还弹出来，就是噪音；
// 反过来，读不到时又必须给一条出路，不能只干写「读不到」。
// 另有一条红线：**只探测，绝不自动安装**。
// ---------------------------------------------------------------------------
func TestLHMStatusNoInstallAction(t *testing.T) {
	a := &App{}
	s := a.GetLHMStatus()
	if s.Site == "" {
		t.Errorf("LHM 下载页地址为空，前端按钮没地方跳")
	}
	if !strings.HasPrefix(s.Site, "https://github.com/LibreHardwareMonitor") {
		t.Errorf("下载页不是 LHM 官方仓库：%q", s.Site)
	}
	if s.Hint == "" {
		t.Errorf("缺一句人话提示，用户不知道该怎么办")
	}
	// 状态之间不能自相矛盾
	if s.HasReal && !s.Running {
		t.Errorf("拿到真实传感器数据却说 LHM 没在跑：%+v", s)
	}
	if s.Running && !s.Install {
		t.Errorf("说 LHM 在跑却说没装：%+v", s)
	}
}

// 探测函数不许有副作用：连续两次调用结果必须一致（不写文件、不起进程、不改注册表）
func TestLHMProbeIsReadOnly(t *testing.T) {
	a := &App{}
	s1 := a.GetLHMStatus()
	s2 := a.GetLHMStatus()
	if s1.Running != s2.Running || s1.Install != s2.Install || s1.Path != s2.Path {
		t.Errorf("LHM 探测有副作用，两次结果不一致：%+v vs %+v", s1, s2)
	}
}

/* isSoftwareAdapter 是「专用显存 > 0」之外的第二道防线（软件渲染器专用显存恰好也是 0，
   本机测不出来第二层到底拦没拦住），所以只能用源码接线测试防它被删。 */
func TestSoftwareFilterWired(t *testing.T) {
	b, err := os.ReadFile("hwvram.go")
	if err != nil {
		t.Fatalf("读不到 hwvram.go：%v", err)
	}
	if !strings.Contains(string(b), "!isSoftwareAdapter(name, d.VendorID)") {
		t.Errorf("readVRAMDxgi 里没调到 isSoftwareAdapter —— 软件渲染器的第二道防线被拆了")
	}
	if !strings.Contains(string(b), "d.DedicatedVideoMemory > 0") {
		t.Errorf("readVRAMDxgi 里没有「专用显存 > 0」这道主过滤")
	}
}

/* 查「装没装某个软件」不许走 Win32_Product：那个类每次查询都会触发 Windows Installer
   一致性检查，实测把本机配置页首屏拖慢十几秒（2026-09-24 实测卡住整页渲染）。
   这个坑很容易被下一个人踩回去，所以直接扫全仓库钉死。 */
func TestNoWin32ProductQuery(t *testing.T) {
	fs, _ := filepath.Glob("*.go")
	if len(fs) == 0 {
		t.Fatalf("没找到 go 源文件")
	}
	for _, f := range fs {
		if strings.HasSuffix(f, "_test.go") {
			continue // 本文件自己的注释里就有这个字符串，别把自己扫中
		}
		b, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		s := string(b)
		if strings.Contains(s, "FROM Win32_Product") || strings.Contains(s, "Win32_Product\"") {
			t.Errorf("%s 里出现了 Win32_Product 查询，会触发 msiexec 重配置拖慢页面 —— 改走注册表卸载表", f)
		}
	}
}

// LHM 探测必须快：它在页面首屏路径上，慢了整页都要等
func TestLHMProbeIsFast(t *testing.T) {
	a := &App{}
	a.GetLHMStatus() // 预热
	t0 := time.Now()
	a.GetLHMStatus()
	if d := time.Since(t0); d > 1500*time.Millisecond {
		t.Errorf("LHM 探测耗时 %.0f ms，超过 1.5s 预算（会拖慢页面首屏）", float64(d.Milliseconds()))
	}
}

/* 反证：2026-09-24 实测，vramInfo 没打小写 json tag，Go 默认输出 "Size"，
   前端 v.size 读到 undefined → 共享显存 / 显存来源两行直接消失、专用显存悄悄退回 s.vram 兜底。
   页面上看不出错（值还是 128 MB），但新加的 DXGI 三档展示等于白做。
   这条断言锁住 JSON 的字段名，前端就能一直按小写取。 */
func TestVRAMJSONKeysAreLowercase(t *testing.T) {
	v := vramInfo{Name: "Intel(R) UHD Graphics 610", Size: "128 MB", Bytes: 134217728,
		SharedSize: "3.9 GB", Shared: 4222900224, Source: "dxgi"}
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("序列化失败：%v", err)
	}
	var m map[string]interface{}
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("反序列化失败：%v", err)
	}
	for _, k := range []string{"name", "size", "bytes", "sharedSize", "shared", "source", "vendor", "device"} {
		if _, ok := m[k]; !ok {
			t.Errorf("vramInfo 的 JSON 里没有小写字段 %q（前端按小写取值，缺了就整行消失）实际字段：%v", k, keysOf(m))
		}
	}
	/* 只禁「首字母大写」——驼峰（sharedSize / dedSys）是前端约定好的字段名，放行 */
	for k := range m {
		if k[0] >= 'A' && k[0] <= 'Z' {
			t.Errorf("vramInfo 的 JSON 字段 %q 首字母大写，前端按小写 v.%s%s 取会读到 undefined",
				k, strings.ToLower(k[:1]), k[1:])
		}
	}
	if m["size"] != "128 MB" || m["sharedSize"] != "3.9 GB" {
		t.Errorf("值不对：size=%v sharedSize=%v", m["size"], m["sharedSize"])
	}
}

func keysOf(m map[string]interface{}) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

/* 反证：配置清单是给用户复制出去贴给别人看的，最怕两件事：
   ① 出现半吊子英文（"Notebook" / "Docking Station"）—— 用户点名要中文表述
   ② 出现「读不到」以外的占位（空行、0、估算值）—— 那是编的
   这条直接跑一遍真实生成，把结果打出来人工看，同时断言这两类问题不存在。 */
func TestPCSummaryIsChineseAndHonest(t *testing.T) {
	a := &App{}
	s := a.GetPCSummary()
	if strings.TrimSpace(s) == "" {
		t.Fatalf("配置清单是空的")
	}
	t.Logf("\n---- 配置清单全文 ----\n%s----------------------", s)

	for _, bad := range []string{"Notebook", "Docking Station", "低profile", "(R)", "(TM)"} {
		if strings.Contains(s, bad) {
			t.Errorf("清单里出现了没翻干净的英文/商标符 %q", bad)
		}
	}
	for _, need := range []string{"【本机配置】", "操作系统", "处理器", "内存"} {
		if !strings.Contains(s, need) {
			t.Errorf("清单里缺了 %q", need)
		}
	}
	/* 「读不到」出现是允许的（诚实），但绝不允许把空值写成 0 单位。
	   注意要用「独立的 0」去匹配：467.0 GB 里含 "0 GB" 子串，直接 Contains 会误报。 */
	zeroUnit := regexp.MustCompile(`(^|[^0-9.])0\s*(V|W|GB|MHz)`)
	if m := zeroUnit.FindString(s); m != "" {
		t.Errorf("清单里出现了 0 值占位 %q，说明拿空值冒充读数了", m)
	}
}

/* 反证：机型判断不能只靠 SMBIOS 机箱类型 —— 很多主板报 1(其他)/2(未知)，
   只认机箱类型的话那些机器会一律判成「判不出」。这里钉住三条降级证据都被用上。 */
func TestFormFactorChain(t *testing.T) {
	cases := []struct {
		ct      byte
		name    string
		battery bool
		sticks  []hwMemStick
		wantFac string
		wantNm  string
	}{
		{3, "台式机", false, nil, "desktop", "台式机"},
		{7, "塔式机箱", false, nil, "desktop", "台式机"},
		{9, "笔记本", true, nil, "laptop", "笔记本"},
		{10, "笔记本", true, nil, "laptop", "笔记本"},
		{30, "平板", true, nil, "tablet", "平板"},
	}
	for _, c := range cases {
		got := formOf(chassisGroup[c.ct], "机箱类型写着「"+c.name+"」")
		if got.Factor != c.wantFac || got.Name != c.wantNm {
			t.Errorf("机箱类型 %d(%s) 归类错：got %s/%s want %s/%s",
				c.ct, c.name, got.Factor, got.Name, c.wantFac, c.wantNm)
		}
	}
	// SODIMM 必须是降级证据之一：机箱类型没说人话时靠它判笔记本
	if !soDimm([]hwMemStick{{Form: "SODIMM"}}) {
		t.Errorf("SODIMM 没被认出来，笔记本会误判成台式机")
	}
	if soDimm([]hwMemStick{{Form: "DIMM"}}) {
		t.Errorf("普通 DIMM 被当成了笔记本条")
	}
	// 判不出时不能硬说台式机
	f := readFormFactor(2, "未知", nil)
	if f.Factor == "desktop" {
		t.Errorf("机箱类型「未知」且没电池，不该直接判成台式机：%v", f)
	}
}

/* 反证：品牌中译是翻译，不是编规格。
   型号本体（G5400 / H310M DS2）必须原样保留 —— 翻成中文反而认不出。 */
func TestBrandTranslateKeepsModel(t *testing.T) {
	if got := brandCN("Gigabyte Technology Co., Ltd."); got != "技嘉" {
		t.Errorf("技嘉没翻对：%q", got)
	}
	if got := brandCN("Kingston"); got != "金士顿" {
		t.Errorf("金士顿没翻对：%q", got)
	}
	if got := brandCN("WDC WD5000AZLX-00ZR6A0"); got != "西部数据" {
		t.Errorf("西部数据没翻对：%q", got)
	}
	if got := cpuBrand("Intel(R) Pentium(R) Gold G5400 CPU @ 3.70GHz"); got != "英特尔" {
		t.Errorf("英特尔没翻对：%q", got)
	}
	m := cpuModelCN("Intel(R) Pentium(R) Gold G5400 CPU @ 3.70GHz")
	if m != "英特尔 奔腾 金牌 G5400" {
		t.Errorf("CPU 型号中译不对：%q", m)
	}
	if strings.Contains(m, "(R)") || strings.Contains(m, "GHz") {
		t.Errorf("型号里还留着商标符/频率：%q", m)
	}
	// 认不出来的厂商不许乱翻
	if got := brandCN("LuminouTek"); got != "LuminouTek" {
		t.Errorf("陌生厂商被乱翻了：%q", got)
	}
	// 型号数字必须原样保留，翻了就认不出是哪颗 CPU
	if got := cpuModelCN("Intel(R) Core(TM) i5-9400F CPU @ 2.90GHz"); got != "英特尔 酷睿 i5-9400F" {
		t.Errorf("型号里的数字/系列被改了：%q", got)
	}
	// 显卡品牌：DXGI VendorID 兜底
	if got := gpuBrand("Some Unknown Card", 0x10DE); got != "英伟达" {
		t.Errorf("VendorID 兜底没生效：%q", got)
	}
	/* 品牌那格不能空着：查不到中译时退回型号里的原样厂商词。
	   空着用户会以为「没读到」，而首词本来就是厂商自己写的名字，可信。 */
	if got := brandOf("LuminouTek 256GB"); got != "LuminouTek" {
		t.Errorf("陌生厂商没退回原样首词：%q", got)
	}
	if got := brandOf("Realtek Gaming GbE Family Controller"); got != "瑞昱" {
		t.Errorf("网卡品牌没中译：%q", got)
	}
	// 首词带数字的是容量/型号，不是厂商名 —— 不许拿它当品牌
	if got := brandLoose("256GB SSD"); got != "" {
		t.Errorf("带数字的首词被当厂商名了：%q", got)
	}
}

/* FALSIFY：机型/品牌都中译了，温度区名字却还是固件内部编号（TZ00_0），
   页面上一半中文一半机器码 —— 用户点名要「一眼看懂」。
   同时：只能改称呼，不许猜它是「CPU 温度」。 */
func TestAcpiZoneNameIsChinese(t *testing.T) {
	got := acpiZoneCN("TZ00_0")
	if !strings.Contains(got, "温度区") {
		t.Errorf("温度区名字没中文化：%q", got)
	}
	if strings.Contains(got, "ACPI") {
		t.Errorf("还留着 ACPI 前缀：%q", got)
	}
	// 不许把 ACPI 温度区说成 CPU 温度 —— 固件没告诉我们它测的是谁
	for _, banned := range []string{"CPU", "处理器", "核心"} {
		if strings.Contains(got, banned) {
			t.Errorf("温度区被说成了 %s 温度（这是编的）：%q", banned, got)
		}
	}
}
