package main

// 蓝屏解析测试：本机没有真实蓝屏样本（2026-09-22 实测 C:\Windows\Minidump 为空），
// 所以按 DUMP_HEADER64 公开格式**手工构造**一份 64 位小转储做端到端验证：
// 头部解析 → 四级页表翻译 → 模块链表走链 → 崩溃栈扫描 → 嫌疑驱动归属。
// 拿到真实 .dmp 样本后，应追加一个真实样本回归（届时放进 _proto 样本目录）。

import (
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const (
	synPageSize   = 0x1000
	synModulesVA  uint64 = 0xFFFFF80000000000
	synStackVA    uint64 = 0xFFFFD00000000000
	synTotalPages uint64 = 64
	synNtosBase   uint64 = synModulesVA + 0x100000
	synGpuBase    uint64 = synModulesVA + 0x4000
	synGpuSize    uint64 = 0x1800000
	synNtosSize   uint64 = 0x400000
	synGpuName           = "nvlddmkm.sys"
	synNtosName          = "ntoskrnl.exe"
	synRipOffset  uint64 = 0x1234
	synExcOffset  uint64 = 0x2000
	synStackOff   uint64 = 0x100
)

// synMapKuser 把 KUSER_SHARED_DATA（内核固定 VA）单独映射一页，写上 InterruptTime。
// 页表页用 30/31/32、数据页 33 —— 避开模块区（10~19）和栈区（20~29）。
func synMapKuser(m synMem, dtbPage uint64) {
	va := uint64(0xFFFFF78000000000)
	l4, l3, l2 := (va>>39)&0x1FF, (va>>30)&0x1FF, (va>>21)&0x1FF
	pml4, pdpt, pd, pt := m.need(dtbPage), m.need(30), m.need(31), m.need(32)
	binary.LittleEndian.PutUint64(pml4[l4*8:], 30*synPageSize|1)
	binary.LittleEndian.PutUint64(pdpt[l3*8:], 31*synPageSize|1)
	binary.LittleEndian.PutUint64(pd[l2*8:], 32*synPageSize|1)
	binary.LittleEndian.PutUint64(pt[(va>>12)&0x1FF*8:], 33*synPageSize|1)
	pg := m.need(33)
	tk := uint64(1338) * 10000000 // 100ns 单位 → 0 天 00:22:18
	binary.LittleEndian.PutUint32(pg[0x8:], uint32(tk))
	binary.LittleEndian.PutUint32(pg[0x10:], uint32(tk>>32))
}

// synMem 物理页内容（页号 → 4096 字节）
type synMem map[uint64][]byte

func (m synMem) need(pn uint64) []byte {
	if b, ok := m[pn]; ok {
		return b
	}
	b := make([]byte, synPageSize)
	m[pn] = b
	return b
}

// synMap 把一组物理页连续映射到虚拟地址（建 PML4→PDPT→PD→PT 全链）
// slot：区域编号（0/1），页表页固定用 2~9 号物理页，避开 10+ 的数据区
func synMap(m synMem, dtbPage, va uint64, pages []uint64, slot int) {
	l4 := (va >> 39) & 0x1FF
	l3 := (va >> 30) & 0x1FF
	l2 := (va >> 21) & 0x1FF
	pml4 := m.need(dtbPage)
	pdpt := m.need(uint64(2 + slot))
	binary.LittleEndian.PutUint64(pml4[l4*8:], uint64(2+slot)*synPageSize|1)
	pd := m.need(uint64(4 + slot))
	binary.LittleEndian.PutUint64(pdpt[l3*8:], uint64(4+slot)*synPageSize|1)
	pt := m.need(uint64(6 + slot))
	binary.LittleEndian.PutUint64(pd[l2*8:], uint64(6+slot)*synPageSize|1)
	for i, pn := range pages {
		binary.LittleEndian.PutUint64(pt[uint64(i)*8:], pn*synPageSize|1)
	}
}

// buildSynDump 构造一份可解析的 64 位小转储（默认 0xD1）
func buildSynDump(t *testing.T) []byte {
	return buildSynDumpEx(t, 0xD1, 0x10, 2, 0, synGpuBase+synRipOffset)
}

// buildSynDumpEx 指定 BugCheck 代码与 4 个参数
func buildSynDumpEx(t *testing.T, code uint32, p1, p2, p3, p4 uint64) []byte {
	t.Helper()
	m := synMem{}
	dtbPage := uint64(1)
	modPages := []uint64{10, 11, 12, 13, 14, 15, 16, 17, 18, 19}
	synMap(m, dtbPage, synModulesVA, modPages, 0)
	stkPages := []uint64{20, 21, 22, 23, 24, 25, 26, 27, 28, 29}
	synMap(m, dtbPage, synStackVA, stkPages, 1)

	nameFull1 := `\SystemRoot\system32\ntoskrnl.exe`
	nameBase1 := synNtosName
	nameFull2 := `\SystemRoot\system32\drivers\` + synGpuName
	nameBase2 := synGpuName
	bufFull1 := synModulesVA + 0x1000
	bufBase1 := synModulesVA + 0x2000
	bufFull2 := synModulesVA + 0x2400
	bufBase2 := synModulesVA + 0x3000

	listHead := synModulesVA
	ent1 := synModulesVA + 0x100
	ent2 := synModulesVA + 0x200
	p10, p11, p12, p13 := m.need(10), m.need(11), m.need(12), m.need(13)

	putStr := func(page []byte, off uint64, s string) {
		for i := 0; i < len(s); i++ {
			page[off+uint64(i)*2] = s[i]
		}
	}
	putStr(p11, 0, nameFull1)
	putStr(p12, 0, nameBase1)
	putStr(p12, 0x400, nameFull2)
	putStr(p13, 0, nameBase2)

	// 列表头 Flink → ent1
	binary.LittleEndian.PutUint64(p10[0:], ent1)
	// ent1 @ 页10 偏移 0x100（布局与启发式解析器对齐：DllBase+0x30, Size+0x40, 名字+0x48/+0x58）
	e1 := ent1 - synModulesVA
	binary.LittleEndian.PutUint64(p10[e1:], ent2)
	binary.LittleEndian.PutUint64(p10[e1+0x30:], synNtosBase)
	binary.LittleEndian.PutUint64(p10[e1+0x40:], synNtosSize)
	binary.LittleEndian.PutUint16(p10[e1+0x48:], uint16(len(nameFull1)*2))
	binary.LittleEndian.PutUint16(p10[e1+0x4A:], uint16(len(nameFull1)*2+2))
	binary.LittleEndian.PutUint64(p10[e1+0x50:], bufFull1)
	binary.LittleEndian.PutUint16(p10[e1+0x58:], uint16(len(nameBase1)*2))
	binary.LittleEndian.PutUint16(p10[e1+0x5A:], uint16(len(nameBase1)*2+2))
	binary.LittleEndian.PutUint64(p10[e1+0x60:], bufBase1)
	// ent2 @ 页10 偏移 0x200（Flink 回列表头 = 链尾）
	e2 := ent2 - synModulesVA
	binary.LittleEndian.PutUint64(p10[e2:], listHead)
	binary.LittleEndian.PutUint64(p10[e2+0x30:], synGpuBase)
	binary.LittleEndian.PutUint64(p10[e2+0x40:], synGpuSize)
	binary.LittleEndian.PutUint16(p10[e2+0x48:], uint16(len(nameFull2)*2))
	binary.LittleEndian.PutUint16(p10[e2+0x4A:], uint16(len(nameFull2)*2+2))
	binary.LittleEndian.PutUint64(p10[e2+0x50:], bufFull2)
	binary.LittleEndian.PutUint16(p10[e2+0x58:], uint16(len(nameBase2)*2))
	binary.LittleEndian.PutUint16(p10[e2+0x5A:], uint16(len(nameBase2)*2+2))
	binary.LittleEndian.PutUint64(p10[e2+0x60:], bufBase2)

	// 栈内容：RSP 起连续命中 nvlddmkm 区间，且每个返回地址前面都摆一条 call 指令
	// （真实栈就是这样 —— 校验不出来的就是数据，不该当栈帧列出来）
	rspVA := synStackVA + synStackOff
	rspPage := m.need(20)
	for i := 0; i < 8; i++ {
		ret := synGpuBase + 0x4000 + uint64(i)*8
		binary.LittleEndian.PutUint64(rspPage[synStackOff+uint64(i)*8:], ret)
		cp := m.need((ret - 5 - synModulesVA) / synPageSize + 10)
		cp[(ret-5)%synPageSize] = 0xE8
	}

	// KUSER_SHARED_DATA（运行时长从这里读）：InterruptTime = 1338 秒 = 0 天 00:22:18
	synMapKuser(m, dtbPage)

	// ---- 拼 DUMP_HEADER64 + 位图 + 页数据 ----
	head := make([]byte, 0x2000)
	copy(head[0x00:], "PAGE")
	copy(head[0x04:], "DU64")
	binary.LittleEndian.PutUint32(head[0x08:], 15)
	binary.LittleEndian.PutUint32(head[0x0C:], 19045)
	binary.LittleEndian.PutUint64(head[0x10:], dtbPage*synPageSize)
	binary.LittleEndian.PutUint64(head[0x20:], listHead)
	binary.LittleEndian.PutUint32(head[0x30:], 0x8664)
	binary.LittleEndian.PutUint32(head[0x34:], 8)
	binary.LittleEndian.PutUint32(head[0x38:], code)
	binary.LittleEndian.PutUint64(head[0x40:], p1)
	binary.LittleEndian.PutUint64(head[0x48:], p2)
	binary.LittleEndian.PutUint64(head[0x50:], p3)
	binary.LittleEndian.PutUint64(head[0x58:], p4)
	binary.LittleEndian.PutUint64(head[0x88:], 1) // 1 个物理内存区段
	binary.LittleEndian.PutUint64(head[0x90:], 0)
	binary.LittleEndian.PutUint64(head[0x98:], synTotalPages)
	binary.LittleEndian.PutUint64(head[0x348+0x98:], rspVA)
	binary.LittleEndian.PutUint64(head[0x348+0xF8:], synGpuBase+synRipOffset)
	binary.LittleEndian.PutUint64(head[0xF00+0x18:], synGpuBase+synExcOffset)

	bitmapBytes := (synTotalPages + 7) / 8
	bitmapBytes = (bitmapBytes + 7) / 8 * 8
	out := make([]byte, 0, int(0x2008+synTotalPages*synPageSize))
	out = append(out, head...)
	out = append(out, make([]byte, bitmapBytes)...)
	data := make([]byte, synTotalPages*synPageSize)
	for pn, content := range m {
		copy(data[pn*synPageSize:], content)
	}
	out = append(out, data...)
	return out
}

func TestBsodSynDumpEndToEnd(t *testing.T) {
	raw := buildSynDump(t)
	p := filepath.Join(t.TempDir(), "092122-8437-01.dmp")
	if err := os.WriteFile(p, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	a := &App{}
	d := a.BsodDetail(p)
	if !d.Valid {
		t.Fatalf("应为有效转储, got err=%s", d.Err)
	}
	if d.Arch != "x64" {
		t.Fatalf("Arch=%s", d.Arch)
	}
	if d.Build != "19045" || !strings.Contains(d.OS, "22H2") {
		t.Fatalf("版本解析错 build=%s os=%s", d.Build, d.OS)
	}
	if d.CodeHex != "0x000000D1" {
		t.Fatalf("CodeHex=%s", d.CodeHex)
	}
	if d.Title != "DRIVER_IRQL_NOT_LESS_OR_EQUAL" {
		t.Fatalf("Title=%s", d.Title)
	}
	if d.P4 != synGpuBase+synRipOffset {
		t.Fatalf("P4=%X", d.P4)
	}
	if len(d.Params) != 4 || !strings.Contains(d.Params[3], "归属驱动") {
		t.Fatalf("参数解读错: %v", d.Params)
	}
	if len(d.Drivers) == 0 {
		t.Fatalf("嫌疑驱动为空, note=%s", d.Note)
	}
	if d.Drivers[0].Name != synGpuName {
		t.Fatalf("首个嫌疑驱动=%s, 期望 %s", d.Drivers[0].Name, synGpuName)
	}
	if !strings.Contains(d.Note, "命中") {
		t.Fatalf("Note=%s", d.Note)
	}
	// 模块表（BlueScreenView 下方那张表）：全量列出，栈内命中的高亮
	if len(d.Modules) != 2 {
		t.Fatalf("模块表应有 2 个, got %d (%+v)", len(d.Modules), d.Modules)
	}
	var gpuMod *BsodModule
	for i := range d.Modules {
		if d.Modules[i].Name == synGpuName {
			gpuMod = &d.Modules[i]
		}
	}
	if gpuMod == nil {
		t.Fatalf("模块表里没有 %s: %+v", synGpuName, d.Modules)
	}
	if !gpuMod.InStack {
		t.Fatalf("栈内命中的模块应标记 InStack: %+v", gpuMod)
	}
	if gpuMod.End != fmt.Sprintf("0x%X", synGpuBase+synGpuSize) || gpuMod.Size != fmt.Sprintf("0x%X", synGpuSize) {
		t.Fatalf("起止地址/大小错: %+v", gpuMod)
	}
	if !strings.Contains(gpuMod.Hit, "0x") || !strings.HasPrefix(gpuMod.Off, synGpuName+"+0x") {
		t.Fatalf("命中地址/偏移错: hit=%s off=%s", gpuMod.Hit, gpuMod.Off)
	}
	if d.Drivers[0].Off != gpuMod.Off {
		t.Fatalf("嫌疑驱动的偏移应来自命中地址: %+v", d.Drivers[0])
	}
	// 崩溃点：RIP 落在 nvlddmkm 里
	if !strings.HasPrefix(d.CrashIn, synGpuName+"+0x") || d.CrashAt == "" {
		t.Fatalf("崩溃点归属错: at=%s in=%s", d.CrashAt, d.CrashIn)
	}
	if gpuMod.Path != `\SystemRoot\system32\drivers\`+synGpuName {
		t.Fatalf("完整路径应取自模块链的 FullDllName: %q", gpuMod.Path)
	}
	t.Logf("OK: code=%s crashIn=%s drivers=%v modules=%d", d.CodeHex, d.CrashIn, d.Drivers, len(d.Modules))
}

// TestBsodRealLike116 拿一份真实公开报告（041024-6312-01.dmp）里的数字做回归：
// 0x116 VIDEO_TDR_FAILURE、参数 2 指向 nvlddmkm、偏移 0x108BE00、
// 故障桶 0x116_IMAGE_nvlddmkm.sys、参数 3 = 0xC000009A、运行时长 0 天 00:22:18。
func TestBsodRealLike116(t *testing.T) {
	raw := buildSynDumpEx(t, 0x116, 0xFFFFE402EC1F6010, synGpuBase+0x108BE00, 0xFFFFFFFFC000009A, 4)
	p := filepath.Join(t.TempDir(), "041024-6312-01.dmp")
	if err := os.WriteFile(p, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	a := &App{}
	d := a.BsodDetail(p)
	if !d.Valid {
		t.Fatalf("应为有效转储: %s", d.Err)
	}
	if d.Image != synGpuName {
		t.Fatalf("Image=%q 期望 %q（0x116 的责任驱动来自参数 2，不是 RIP）", d.Image, synGpuName)
	}
	if d.Symbol != "nvlddmkm+0x108BE00" {
		t.Fatalf("Symbol=%q", d.Symbol)
	}
	if d.Bucket != "0x116_IMAGE_"+synGpuName {
		t.Fatalf("Bucket=%q", d.Bucket)
	}
	if !strings.Contains(d.Params[2], "资源不足") {
		t.Fatalf("参数 3 应把 NTSTATUS 0xC000009A 翻成中文: %v", d.Params)
	}
	if !strings.Contains(d.Params[1], "nvlddmkm.sys+0x") {
		t.Fatalf("参数 2 应归属到模块: %v", d.Params)
	}
	if len(d.Stack) == 0 {
		t.Fatalf("调用栈应还原出帧: note=%s", d.Note)
	}
	if !strings.HasPrefix(d.Stack[0].Off, "nvlddmkm+0x") || d.Stack[0].Sure == false {
		t.Fatalf("栈首帧错: %+v", d.Stack[0])
	}
	if d.Uptime != "0 天 00:22:18" {
		t.Fatalf("Uptime=%q", d.Uptime)
	}
	t.Logf("OK: image=%s symbol=%s bucket=%s uptime=%s 栈 %d 层",
		d.Image, d.Symbol, d.Bucket, d.Uptime, len(d.Stack))
}

// TestBsodStatusText NTSTATUS 翻译：认得出来的翻中文，认不出来的必须留空（不硬编）
func TestBsodStatusText(t *testing.T) {
	if got := bsodStatusText(0xFFFFFFFFC000009A); !strings.Contains(got, "资源不足") {
		t.Fatalf("got=%q", got)
	}
	if got := bsodStatusText(0xFFFFF80001000000); got != "" {
		t.Fatalf("普通内核地址不该当状态码: %q", got)
	}
	if got := bsodStatusText(0xC0009999); got != "" {
		t.Fatalf("认不出来的状态码应留空: %q", got)
	}
}

// TestBsodVersionInfoRealFile 版本资源读取（拿本机真存在的驱动文件读一次）。
// 精简系统里文件可能不在 → 跳过，不算失败。
func TestBsodVersionInfoRealFile(t *testing.T) {
	var target string
	for _, n := range []string{"disk.sys", "tcpip.sys", "usbhub.sys"} {
		p := filepath.Join(nativeSystem32(), "drivers", n)
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			target = p
			break
		}
	}
	if target == "" {
		t.Skip("本机没有可用的驱动文件样本")
	}
	vi := fileVersionInfo(target)
	stamp := peTimeStamp(target)
	t.Logf("%s → 公司=%q 描述=%q 产品=%q 版本=%q 时间戳=%q", filepath.Base(target), vi.company, vi.desc, vi.product, vi.version, stamp)
	if vi.company == "" && vi.desc == "" && vi.version == "" {
		t.Fatalf("读不出任何版本信息: %s", target)
	}
	// 时间戳允许为空：Win10 1903 起系统自带驱动的 PE 时间戳是「可复现构建」哈希，
	// 解出来是 2069 年这种未来日期 —— 我们按「宁可留空不编」处理。
	// 但**绝不允许把未来日期当真时间显示出去**。
	if stamp != "" {
		y := 0
		fmt.Sscanf(stamp, "%d-", &y)
		if y == 0 || y > time.Now().Year()+1 {
			t.Fatalf("时间戳不合理（未来日期不该外显）: %q", stamp)
		}
	}
	// 不存在的文件必须安静返回空，不能编
	if v := fileVersionInfo(filepath.Join(t.TempDir(), "nope.sys")); v.company != "" || v.version != "" {
		t.Fatalf("不存在的文件应返回空: %+v", v)
	}
	if s := peTimeStamp(filepath.Join(t.TempDir(), "nope.sys")); s != "" {
		t.Fatalf("不存在的文件时间戳应为空: %s", s)
	}
}

func TestBsodListEmptyMachine(t *testing.T) {
	// 本机 Minidump 为空也不许崩，返回空列表
	a := &App{}
	list := a.BsodList()
	t.Logf("本机转储数: %d", len(list))
	for _, d := range list {
		if !d.Valid {
			t.Errorf("列出的 %s 应可解析: %s", d.Name, d.Err)
		}
	}
}

func TestBsodPathGuard(t *testing.T) {
	a := &App{}
	d := a.BsodDetail(`D:\somewhere\evil.dmp`)
	if d.Valid || d.Err == "" {
		t.Fatalf("路径守卫失效")
	}
}

func TestBsodVariantCode(t *testing.T) {
	if got := bsodLookup(0x1000007E).title; got != "SYSTEM_THREAD_EXCEPTION_NOT_HANDLED" {
		t.Fatalf("变体码剥高位失败: %s", got)
	}
	if got := bsodLookup(0xD1).zh; got == "" {
		t.Fatal("0xD1 应有中文释义")
	}
	if got := bsodLookup(0x777).title; !strings.Contains(got, "UNKNOWN") {
		t.Fatalf("未收录码应返回 UNKNOWN, got %s", got)
	}
}

func TestBsodCorruptDumpGraceful(t *testing.T) {
	// 布局描述被毁 → 归属失败但主结论保留，绝不能 panic
	raw := buildSynDump(t)
	for i := 0x88; i < 0x2000; i++ {
		raw[i] = 0xFF
	}
	p := filepath.Join(t.TempDir(), "bad.dmp")
	os.WriteFile(p, raw, 0o644)
	a := &App{}
	d := a.BsodDetail(p)
	if !d.Valid || d.Title != "DRIVER_IRQL_NOT_LESS_OR_EQUAL" {
		t.Fatalf("头部主结论应保留: %+v", d)
	}
	if len(d.Drivers) != 0 {
		t.Fatalf("布局坏了不该出驱动: %v", d.Drivers)
	}
	fmt.Println("corrupt note:", d.Note)
}

// TestBsodWriteSample 探针辅助：BSOD_WRITE_SAMPLE=路径 时把合成样本写到该路径
// （给 UI 探针演示用；平时跑测试不会写任何文件）
func TestBsodWriteSample(t *testing.T) {
	dst := os.Getenv("BSOD_WRITE_SAMPLE")
	if dst == "" {
		t.Skip("未设置 BSOD_WRITE_SAMPLE，跳过")
	}
	raw := buildSynDump(t)
	if err := os.WriteFile(dst, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	t.Logf("样本已写入 %s (%d B)", dst, len(raw))
}
