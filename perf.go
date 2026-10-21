package main

// perf.go —— 实时性能采样：CPU / 内存 / 磁盘 / GPU（前端 Hero 右侧那四条曲线）
// 数据源跟任务管理器「性能」页同源：
//   CPU  = GetSystemTimes 两次采样的差值（kernel 时间含 idle，busy = total - idle）
//   内存 = GlobalMemoryStatusEx（已用 / 总量）
//   磁盘 = PDH 计数器 PhysicalDisk(_Total)\% Idle Time 反算
//   GPU  = PDH 计数器 GPU Engine(*)\Utilization Percentage（多实例取最大）
// 取不到的项一律返回 -1：精简版系统可能没有 PDH 计数器，GPU 计数器在核显 /
// 老系统上常缺失。宁可这一行不显示，也不摆一个编出来的数。

import (
	"math"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"
)

// LivePerf 一次实时采样。占用率单位是百分比 0-100；速率单位是字节/秒；-1 = 这一项取不到
type LivePerf struct {
	CPU  float64 `json:"cpu"`
	Mem  float64 `json:"mem"`
	Disk float64 `json:"disk"`
	GPU  float64 `json:"gpu"`

	// 下面这些是本页新增的「正在发生什么」，拿不到一律 -1 / 空，前端显示 --
	NetIn  float64 `json:"netIn"`  // 下行（收到）字节/秒
	NetOut float64 `json:"netOut"` // 上行（发出）字节/秒
	NetBw  float64 `json:"netBw"`  // 网卡链路带宽，位/秒（1 Gbps 的卡 = 1e9）
	DiskR  float64 `json:"diskR"`  // 磁盘读，字节/秒
	DiskW  float64 `json:"diskW"`  // 磁盘写，字节/秒
	GPUMem float64 `json:"gpuMem"` // 显存已用，字节
	GPUTot float64 `json:"gpuTot"` // 显存总量，字节
	CpuMHz float64 `json:"cpuMHz"` // CPU 当前实测频率 MHz
	Temps  []TempItem `json:"temps"`
	// 风扇转速：Windows 用户态基本拿不到（要主板 SuperIO 驱动 / 内核驱动）。
	// 拿不到就是空数组，前端会写清「读不到」，绝不编一个转速出来。
	Fans []FanItem `json:"fans"`
	/* 电压 / 功耗：用户要求部件上要看到这两个。
	   但 Windows 用户态没有通用来源（实测 Win32_Processor.CurrentVoltage、
	   Win32_VoltageProbe.CurrentReading 全空），只有装了 LibreHardwareMonitor 才有真值。
	   没装就是空数组 —— 前端显示「读不到」并给装 LHM 的入口，不许拿 0 或估算值顶上。 */
	Volts []SensorItem `json:"volts"`
	Power []SensorItem `json:"power"`
}

var perfMu sync.Mutex

/* ---------------- CPU：GetSystemTimes 差值 ---------------- */

var (
	krn32Perf   = syscall.NewLazyDLL("kernel32.dll")
	procSysTime = krn32Perf.NewProc("GetSystemTimes")
)

var cpuPrev struct{ idle, kernel, user uint64 }

func cpuPercent() float64 {
	var idle, kernel, user syscall.Filetime
	if r, _, _ := procSysTime.Call(
		uintptr(unsafe.Pointer(&idle)),
		uintptr(unsafe.Pointer(&kernel)),
		uintptr(unsafe.Pointer(&user))); r == 0 {
		return -1
	}
	ft := func(f syscall.Filetime) uint64 { return uint64(f.HighDateTime)<<32 | uint64(f.LowDateTime) }
	i, k, u := ft(idle), ft(kernel), ft(user)
	first := cpuPrev.kernel == 0 && cpuPrev.user == 0
	defer func() { cpuPrev.idle, cpuPrev.kernel, cpuPrev.user = i, k, u }()
	if first {
		return -1 // 第一次采样没有差值可比，前端等下一秒
	}
	dk, du, di := k-cpuPrev.kernel, u-cpuPrev.user, i-cpuPrev.idle
	total := dk + du // kernel 时间里已经包含 idle
	if total == 0 {
		return 0
	}
	return clampPct(float64(total-di) / float64(total) * 100)
}

func memPercent() float64 {
	var ms memStatusEx // 与 optimize.go 共用同一个 MEMORYSTATUSEX 定义
	ms.length = uint32(unsafe.Sizeof(ms))
	p := krn32Perf.NewProc("GlobalMemoryStatusEx")
	if p.Find() != nil {
		return -1
	}
	if r, _, _ := p.Call(uintptr(unsafe.Pointer(&ms))); r == 0 || ms.totalPhys == 0 {
		return -1
	}
	return clampPct(float64(ms.totalPhys-ms.availPhys) / float64(ms.totalPhys) * 100)
}

// memPhys 物理内存总量 / 可用量（GlobalMemoryStatusEx）。
// 本机配置页的内存条必须和顶栏芯片同源：一边走 WMI、一边走 Win32，
// 取到的不是同一时点，同屏就会冒出「顶栏 63% / 本机配置 65%」两个数
//（2026-09-23 走查点名过这个现象，别再引入第二条路）。
func memPhys() (total, avail uint64) {
	var ms memStatusEx
	ms.length = uint32(unsafe.Sizeof(ms))
	p := krn32Perf.NewProc("GlobalMemoryStatusEx")
	if p.Find() != nil {
		return 0, 0
	}
	if r, _, _ := p.Call(uintptr(unsafe.Pointer(&ms))); r == 0 {
		return 0, 0
	}
	return ms.totalPhys, ms.availPhys
}

func clampPct(v float64) float64 {
	if math.IsNaN(v) {
		return -1
	}
	return math.Max(0, math.Min(100, v))
}

/* ---------------- 磁盘 / GPU：PDH 性能计数器 ---------------- */

var (
	pdhDLL          = syscall.NewLazyDLL("pdh.dll")
	procPdhOpenQ    = pdhDLL.NewProc("PdhOpenQueryW")
	procPdhAddEng   = pdhDLL.NewProc("PdhAddEnglishCounterW")
	procPdhCollect  = pdhDLL.NewProc("PdhCollectQueryData")
	procPdhGetFmt   = pdhDLL.NewProc("PdhGetFormattedCounterValue")
	procPdhExpand   = pdhDLL.NewProc("PdhExpandCounterPathW")
	procPdhCloseQ   = pdhDLL.NewProc("PdhCloseQuery")
)

const (
	pdhFmtDouble = 0x00000200
	pdhMoreData  = 0x800007D2 // PDH_MORE_DATA：缓冲区不够，sz 已填上需要的长度
)

// pdhFmtValue 对应 PDH_FMT_COUNTERVALUE（64 位：DWORD + 4 字节对齐 + double）
type pdhFmtValue struct {
	CStatus uint32
	_       [4]byte
	Double  float64
}

type pdhQuery struct {
	q        uintptr
	counters []uintptr
	// sum=true：多个引擎是并行占用的，加起来才是整块 GPU 的负载（故任务管理器）
	// sum=false：取最大值（磁盘只有 _Total 一个实例，无所谓）
	sum bool
}

// pdhNew 打开一个查询并挂上这些计数器；全部挂不上就返回 nil
func pdhNew(paths []string) *pdhQuery {
	var q uintptr
	if r, _, _ := procPdhOpenQ.Call(0, 0, uintptr(unsafe.Pointer(&q))); r != 0 {
		return nil
	}
	pq := &pdhQuery{q: q}
	for _, path := range paths {
		up, err := syscall.UTF16PtrFromString(path)
		if err != nil {
			continue
		}
		var c uintptr
		// AddEnglishCounter：中文系统上的计数器显示名是中文，用英文名才挂得上
		if r, _, _ := procPdhAddEng.Call(q, uintptr(unsafe.Pointer(up)), 0, uintptr(unsafe.Pointer(&c))); r == 0 {
			pq.counters = append(pq.counters, c)
		}
	}
	if len(pq.counters) == 0 {
		procPdhCloseQ.Call(q)
		return nil
	}
	return pq
}

// sample 采集一次；多实例（GPU 各个引擎）取最大值
func (pq *pdhQuery) sample() float64 {
	if pq == nil || len(pq.counters) == 0 {
		return -1
	}
	if r, _, _ := procPdhCollect.Call(pq.q); r != 0 {
		return -1
	}
	best, total := -1.0, 0.0
	for _, c := range pq.counters {
		var v pdhFmtValue
		if r, _, _ := procPdhGetFmt.Call(c, pdhFmtDouble, 0, uintptr(unsafe.Pointer(&v))); r == 0 && v.CStatus == 0 && v.Double >= 0 {
			total += v.Double
			if v.Double > best {
				best = v.Double
			}
		}
	}
	if pq.sum && total > 0 {
		return total
	}
	return best
}

// pdhExpandPaths 展开含通配符的计数器路径（GPU 引擎是 per-process 实例，必须先展开）
func pdhExpandPaths(wild string) []string {
	p, err := syscall.UTF16PtrFromString(wild)
	if err != nil {
		return nil
	}
	var sz uint32
	// 第一次调用传空缓冲区，PDH 返回 PDH_MORE_DATA 并把需要的长度写进 sz ——
	// 这是正常流程不是失败，别把它当错误（当初就是在这里直接 return nil，
	// 结果磁盘和 GPU 的实例一个都展开不出来）。
	r, _, _ := procPdhExpand.Call(uintptr(unsafe.Pointer(p)), 0, uintptr(unsafe.Pointer(&sz)))
	if r != 0 && r != pdhMoreData {
		return nil
	}
	if sz == 0 {
		return nil
	}
	buf := make([]uint16, sz)
	if r, _, _ := procPdhExpand.Call(uintptr(unsafe.Pointer(p)), uintptr(unsafe.Pointer(&buf[0])), uintptr(unsafe.Pointer(&sz))); r != 0 {
		return nil
	}
	// 返回的是多字符串：以 \0 分隔，结尾两个 \0
	var out []string
	start := 0
	for i := 0; i < len(buf); i++ {
		if buf[i] != 0 {
			continue
		}
		if i == start { // 连续两个 0 = 结束
			break
		}
		out = append(out, syscall.UTF16ToString(buf[start:i]))
		start = i + 1
	}
	return out
}

// sampleEach 逐个计数器取值（网速的收/发、磁盘的读/写必须分开读，
// 不能混在一个「取最大值」里 —— 当初就是这么写的，上行下行永远一样）
func (pq *pdhQuery) sampleEach() []float64 {
	if pq == nil || len(pq.counters) == 0 {
		return nil
	}
	if r, _, _ := procPdhCollect.Call(pq.q); r != 0 {
		return nil
	}
	out := make([]float64, len(pq.counters))
	for i, c := range pq.counters {
		var v pdhFmtValue
		if r, _, _ := procPdhGetFmt.Call(c, pdhFmtDouble, 0, uintptr(unsafe.Pointer(&v))); r == 0 && v.CStatus == 0 && v.Double >= 0 {
			out[i] = v.Double
		} else {
			out[i] = -1
		}
	}
	return out
}

// 记录每个查询里各计数器的顺序，方便按名字取值
type pdhNamed struct {
	q      *pdhQuery
	labels []string
}

func (pn *pdhNamed) get(label string) float64 {
	if pn == nil || pn.q == nil {
		return -1
	}
	vals := pn.q.sampleEach()
	for i, l := range pn.labels {
		if l == label && i < len(vals) {
			return vals[i]
		}
	}
	return -1
}

// pdhNamedNew 按「标签 → 计数器路径」建查询；路径挂不上的标签会被丢掉
func pdhNamedNew(pairs map[string]string) *pdhNamed {
	var labels []string
	var paths []string
	// 顺序不确定会让标签错位，先排序保证稳定
	keys := make([]string, 0, len(pairs))
	for k := range pairs {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		labels = append(labels, k)
		paths = append(paths, pairs[k])
	}
	q := pdhNew(paths)
	if q == nil {
		return nil
	}
	return &pdhNamed{q: q, labels: labels}
}

var (
	perfDiskQ   *pdhQuery
	perfGPUQ    *pdhQuery
	perfNetQ    *pdhNamed
	perfDiskBQ  *pdhNamed
	perfGPUMemQ *pdhNamed
	perfNetName string
	perfOnce    sync.Once
)

/* netVirtualKeys 虚拟 / 不该拿来表示"网速"的网卡名字关键字（小写匹配）。
   覆盖：VMware、VirtualBox、Hyper-V、Docker、WSL、VPN（深信服 / ExpressVPN）、
   Teredo / ISATAP / 6to4 隧道、蓝牙、WAN 迷你口、抓包用的 Npcap / TAP、Radmin。
   这些要么恒 0 流量、要么只是虚拟隧道，拿给用户当「网速」一定是错的。
   ⚠️ 也顺手排掉 "_Total" 汇总实例 —— 显示成网卡名会让人以为真有这么块网卡。 */
var netVirtualKeys = []string{
	"vmware", "vmnet", "virtualbox", "vbox", "hyper-v", "vethernet",
	"teredo", "isatap", "6to4", "loopback", "pseudo", "bluetooth",
	"miniport", "wan ", "tap-", "npcap", "sangfor", "expressvpn",
	"docker", "wsl", "radmin", "_total",
}

// pickNetPath 从 PDH 展开出来的网卡计数器路径里挑一个**真实在用的**。
// 策略：先按名字排除虚拟网卡，剩下的按 PDH 枚举顺序取第一个（通常是系统主网卡）。
// 极端情况（纯虚拟机里只剩虚拟网卡）退化为取第一个 —— 总比整个读不出来强。
func pickNetPath(paths []string) string {
	if len(paths) == 0 {
		return ""
	}
	for _, p := range paths {
		if !isVirtualNetPath(p) {
			return p
		}
	}
	return paths[0]
}

func isVirtualNetPath(path string) bool {
	low := strings.ToLower(path)
	for _, k := range netVirtualKeys {
		if strings.Contains(low, k) {
			return true
		}
	}
	return false
}

// initNetQuery 挂网速计数器：网卡实例名带中文/空格，必须先用通配符展开再逐个挂
func initNetQuery() {
	pairs := map[string]string{}
	for _, spec := range []struct{ label, wild string }{
		{"in", `\Network Interface(*)\Bytes Received/sec`},
		{"out", `\Network Interface(*)\Bytes Sent/sec`},
		{"bw", `\Network Interface(*)\Current Bandwidth`},
	} {
		ex := pdhExpandPaths(spec.wild)
		if len(ex) == 0 {
			continue
		}
		/* ⚠️ 旧实现直接取枚举到的**第一个**网卡实例。装了 VMware / VirtualBox /
		   Hyper-V / VPN / Docker 的机器上，第一个往往是虚拟网卡 ——
		   网速恒为 0、网卡名显示成 "VMnet1" / "Teredo"，用户只会觉得「网速读不出来」。
		   → 先按名字排除虚拟网卡，再取第一个真实网卡。 */
		best := pickNetPath(ex)
		if best == "" {
			continue
		}
		pairs[spec.label] = best
		if spec.label == "in" {
			// 从路径里抠出网卡名：\机器名\Network Interface(Realtek ...)\Bytes Received/sec
			s := best
			if i := strings.Index(s, "Network Interface("); i >= 0 {
				rest := s[i+len("Network Interface("):]
				if j := strings.LastIndex(rest, ")"); j > 0 {
					perfNetName = rest[:j]
				}
			}
		}
	}
	if len(pairs) > 0 {
		perfNetQ = pdhNamedNew(pairs)
	}
}

func initDiskByteQuery() {
	perfDiskBQ = pdhNamedNew(map[string]string{
		"read":  `\PhysicalDisk(_Total)\Disk Read Bytes/sec`,
		"write": `\PhysicalDisk(_Total)\Disk Write Bytes/sec`,
	})
}

// initGPUMemQuery 显存占用：优先整机适配器实例，取不到就逐个展开。
// 核显机器上这个计数器常常整个没有（本机实测 GPU Adapter Memory 只有 2 个 luid 实例
// 且 Dedicated Usage 恒为 0），那时候页面显存那格显示「--」，不编一个。
func initGPUMemQuery() {
	pairs := map[string]string{}
	for _, spec := range []struct{ label, wild string }{
		{"used", `\GPU Adapter Memory(*)\Dedicated Usage`},
		{"total", `\GPU Adapter Memory(*)\Dedicated Limit`},
	} {
		var paths []string
		for _, p := range pdhExpandPaths(spec.wild) {
			paths = append(paths, p)
		}
		// 多块显卡：每块挂一条，取名 used/total、used1/total1 …
		for i, p := range paths {
			suffix := ""
			if i > 0 {
				suffix = strconv.Itoa(i)
			}
			pairs[spec.label+suffix] = p
		}
	}
	if len(pairs) > 0 {
		perfGPUMemQ = pdhNamedNew(pairs)
	}
}

// gpuMemSum 把多块显卡的显存加起来（有的机器独显+核显各一个实例）
func gpuMemSum(prefix string) float64 {
	if perfGPUMemQ == nil {
		return -1
	}
	vals := perfGPUMemQ.q.sampleEach()
	total, any := 0.0, false
	for i, l := range perfGPUMemQ.labels {
		if !strings.HasPrefix(l, prefix) || i >= len(vals) || vals[i] < 0 {
			continue
		}
		total += vals[i]
		any = true
	}
	if !any || total <= 0 {
		return -1
	}
	return total
}

// pdhNewGPU 挂 GPU 占用：优先整块适配器（部分机器有，最准），没有再退到 3D 引擎实例求和
func pdhNewGPU() *pdhQuery {
	if q := pdhNew([]string{`\GPU Adapter(*)\Utilization Percentage`}); q != nil {
		return q
	}
	// GPU Engine 是 per-process × per-engine 的实例，整机有几百个；
	// 只取 engtype_3D（最能代表 GPU 负载），再多就截断，别让查询过重。
	var eng []string
	for _, p := range pdhExpandPaths(`\GPU Engine(*)\Utilization Percentage`) {
		if strings.Contains(p, "engtype_3D") {
			eng = append(eng, p)
			if len(eng) >= 32 {
				break
			}
		}
	}
	if len(eng) == 0 {
		return nil
	}
	q := pdhNew(eng)
	if q != nil {
		q.sum = true
	}
	return q
}

func initPerf() {
	perfDiskQ = pdhNew([]string{`\PhysicalDisk(_Total)\% Idle Time`})
	perfGPUQ = pdhNewGPU()
	initNetQuery()
	initDiskByteQuery()
	initGPUMemQuery()
}

// GetLivePerf 拍一次实时性能：CPU / 内存 / 磁盘 / GPU 占用率，
// 外加网速、磁盘读写速度、显存占用、CPU 实测频率、温度。拿不到的项返回 -1。
//
// 本机配置页和首页 Hero 都调这一个方法 —— 同一个来源、同一个时点，
// 才不会出现「顶栏 63%、本机配置页 65%」这种两个数打架的情况。
func (a *App) GetLivePerf() LivePerf {
	perfMu.Lock()
	defer perfMu.Unlock()
	perfOnce.Do(initPerf)

	p := LivePerf{CPU: -1, Mem: -1, Disk: -1, GPU: -1,
		NetIn: -1, NetOut: -1, NetBw: -1, DiskR: -1, DiskW: -1, GPUMem: -1, GPUTot: -1, CpuMHz: -1}
	p.CPU = cpuPercent()
	p.Mem = memPercent()
	// % Idle Time 是速率类计数器：第一次 Collect 还没差值，PDH 会返回忙/无数据，
	// 等下一秒自然就有值了 —— 期间前端这一行显示 --。
	if idle := perfDiskQ.sample(); idle >= 0 {
		p.Disk = clampPct(100 - idle)
	}
	if g := perfGPUQ.sample(); g >= 0 {
		p.GPU = clampPct(g)
	}
	if perfNetQ != nil {
		p.NetIn = perfNetQ.get("in")
		p.NetOut = perfNetQ.get("out")
		p.NetBw = perfNetQ.get("bw")
	}
	if perfDiskBQ != nil {
		p.DiskR = perfDiskBQ.get("read")
		p.DiskW = perfDiskBQ.get("write")
	}
	p.GPUMem = gpuMemSum("used")
	p.GPUTot = gpuMemSum("total")

	// CPU 实测频率：忙等 30ms 量一次，太频繁会自己把 CPU 顶上去，所以 2 秒才刷一次
	if mhz, ok := cpuMHzCached(); ok {
		p.CpuMHz = mhz
	}
	p.Temps = readTemps()
	p.Fans = readFans()
	p.Volts = readVolts()
	p.Power = readPower()
	return p
}

// GetNetName 正在监测的那块网卡名（网速读数对应的卡，避免用户以为是另一块）
func (a *App) GetNetName() string {
	perfMu.Lock()
	defer perfMu.Unlock()
	perfOnce.Do(initPerf)
	return perfNetName
}

/* ---------------- CPU 实测频率缓存 ---------------- */

var cpuMHzMu sync.Mutex
var cpuMHzVal float64
var cpuMHzAt time.Time

// cpuMHzCached 2 秒内复用上次实测结果
func cpuMHzCached() (float64, bool) {
	cpuMHzMu.Lock()
	defer cpuMHzMu.Unlock()
	if time.Since(cpuMHzAt) < 2*time.Second && cpuMHzVal > 0 {
		return cpuMHzVal, true
	}
	cpuMHzMu.Unlock()
	v := measureCPUMHz()
	cpuMHzMu.Lock()
	cpuMHzAt = time.Now()
	if v > 0 {
		cpuMHzVal = v
	}
	return cpuMHzVal, cpuMHzVal > 0
}
