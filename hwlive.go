package main

// hwlive.go —— 本机配置页的实时监控数据（2026-09-24）
//
// 这一页要给用户看「现在正在发生什么」：占用率、温度、网速、磁盘读写、CPU 当前频率。
// 全部走系统自带通道（PDH 计数器 / WMI / TSC），**不依赖任何第三方软件、不需要驱动**。
//
// 通用性说明（这点很重要，别照着某一台机器写死）：
//   · 每台机器能挂上的计数器不一样：核显机器常没有 GPU Adapter Memory，
//     精简版系统可能连 PDH 计数器都缺 —— 拿不到就返回 -1，前端显示「--」，不编数。
//   · 温度是最难的一项：Windows 没有给用户态的通用 CPU 温度接口。
//     这里按「有就用、没有就降级」的顺序读（见 readTemps），
//     实在读不到就留空 —— 比摆一个编出来的 45℃ 强。

import (
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/go-ole/go-ole"
	"github.com/go-ole/go-ole/oleutil"
)

/* ==================== 温度 ==================== */

// TempItem 一个温度读数
type TempItem struct {
	Name string  `json:"name"` // 传感器名字（照实写来源，不冒称「CPU 温度」）
	C    float64 `json:"c"`    // 摄氏度
}

var tempCache struct {
	mu    sync.Mutex
	at    time.Time
	items []TempItem
}

// readTemps 读温度，5 秒内复用上次结果（WMI 查询有开销，别每秒都问）
//
// 通用降级顺序：
//   ① root\WMI 的 MSAcpi_ThermalZoneTemperature —— ACPI 温度区，大多数机器都有，
//      读出来的是「0.1 开尔文」整数，要减 273.15 才是摄氏度。
//   ② root\LibreHardwareMonitor 的 Sensor 表 —— 仅当用户正在跑 LibreHardwareMonitor
//      且开启了它的 WMI 接口时才有，命中就能拿到真正的 CPU 封装/核心温度、风扇转速。
//      （开源免费，很多装机会用，检测到就用，不强制依赖。）
//   ③ 都没有 → 返回空，页面那一格显示「读不到」。
func readTemps() []TempItem {
	tempCache.mu.Lock()
	defer tempCache.mu.Unlock()
	if time.Since(tempCache.at) < 5*time.Second && tempCache.items != nil {
		return tempCache.items
	}
	var out []TempItem
	out = append(out, readAcpiTemps()...)
	out = append(out, readLHMTemps()...)
	// 同名只留一条，按温度从高到低排（高的更像「需要注意的那个」）
	seen := map[string]bool{}
	var uniq []TempItem
	for _, t := range out {
		if seen[t.Name] {
			continue
		}
		seen[t.Name] = true
		uniq = append(uniq, t)
	}
	sort.SliceStable(uniq, func(i, j int) bool { return uniq[i].C > uniq[j].C })
	tempCache.items, tempCache.at = uniq, time.Now()
	return uniq
}

// readAcpiTemps ACPI 温度区（root\WMI）
func readAcpiTemps() []TempItem {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	_ = ole.CoInitializeEx(0, ole.COINIT_MULTITHREADED)
	defer ole.CoUninitialize()

	loc, err := oleutil.CreateObject("WbemScripting.SWbemLocator")
	if err != nil {
		return nil
	}
	defer loc.Release()
	wmi, err := loc.QueryInterface(ole.IID_IDispatch)
	if err != nil {
		return nil
	}
	defer wmi.Release()
	svcRaw, err := oleutil.CallMethod(wmi, "ConnectServer", nil, `root\WMI`)
	if err != nil {
		return nil
	}
	svc := svcRaw.ToIDispatch()
	defer svc.Release()
	resRaw, err := oleutil.CallMethod(svc, "ExecQuery", "SELECT * FROM MSAcpi_ThermalZoneTemperature")
	if err != nil {
		return nil
	}
	res := resRaw.ToIDispatch()
	defer res.Release()
	cnt, err := oleutil.GetProperty(res, "Count")
	if err != nil {
		return nil
	}
	var out []TempItem
	n := int(cnt.Val)
	for i := 0; i < n; i++ {
		itRaw, err := oleutil.CallMethod(res, "ItemIndex", i)
		if err != nil {
			continue
		}
		it := itRaw.ToIDispatch()
		if it == nil {
			continue
		}
		v, err := oleutil.GetProperty(it, "CurrentTemperature")
		nm, _ := oleutil.GetProperty(it, "InstanceName")
		name := ""
		if nm != nil && nm.VT == ole.VT_BSTR {
			name = nm.ToString()
		}
		it.Release()
		if err != nil || v == nil || v.VT != ole.VT_I4 {
			continue
		}
		raw := int32(uint32(v.Val))
		if raw <= 0 {
			continue
		}
		c := float64(raw)/10 - 273.15
		if c < -20 || c > 150 {
			continue // 明显不是正常温度，别往页面上摆
		}
		// InstanceName 形如 ACPI\ThermalZone\TZ00_0，取最后一段当名字
		if i := strings.LastIndex(name, `\`); i >= 0 {
			name = name[i+1:]
		}
		out = append(out, TempItem{Name: acpiZoneCN(name), C: c})
	}
	return out
}

/* acpiZoneCN ACPI 温度区名字说人话。
   原始名是 TZ00_0 / TZ10_0 这种固件内部编号，摆到界面上没人看得懂。
   ⚠️ 这里只改称呼，**不猜它是谁的 temperature** ——
   它到底是 CPU 封装还是主板某块区域，固件不告诉我们，猜了就是编。
   所以统称「温度区」，后面的编号原样保留（方便对着 HWiNFO 查）。 */
func acpiZoneCN(zone string) string {
	z := strings.TrimSpace(zone)
	if z == "" {
		return "温度区"
	}
	return "温度区 " + z
}

// readLHMTemps LibreHardwareMonitor 的 WMI 接口（用户在跑它时才有）
// 类名是 Sensor，字段：Name / SensorType / Value / Parent
func readLHMTemps() []TempItem {
	rows, err := wmiQuery(`root\LibreHardwareMonitor`,
		"SELECT Name,SensorType,Value FROM Sensor WHERE SensorType='Temperature'",
		[]string{"Name", "SensorType", "Value"})
	if err != nil || len(rows) == 0 {
		return nil
	}
	var out []TempItem
	for _, r := range rows {
		name := strings.TrimSpace(r["Name"])
		var c float64
		if _, e := parseTempValue(r["Value"], &c); e {
			continue
		}
		if c < -20 || c > 150 {
			continue
		}
		out = append(out, TempItem{Name: name, C: c})
	}
	return out
}

// parseTempValue 解析温度字符串（WMI 返回的可能是 "56.5" / "56,5" / "56"）
func parseTempValue(s string, out *float64) (bool, bool) {
	s = strings.TrimSpace(strings.ReplaceAll(s, ",", "."))
	if s == "" {
		return false, true
	}
	v, neg := 0.0, false
	if s[0] == '-' {
		neg, s = true, s[1:]
	}
	i := 0
	for i < len(s) && s[i] >= '0' && s[i] <= '9' {
		v = v*10 + float64(s[i]-'0')
		i++
	}
	if i == 0 {
		return false, true
	}
	if i < len(s) && s[i] == '.' {
		f, p := 0.0, 0.1
		for i++; i < len(s) && s[i] >= '0' && s[i] <= '9'; i++ {
			f += float64(s[i]-'0') * p
			p /= 10
		}
		v += f
	}
	if neg {
		v = -v
	}
	*out = v
	return true, false
}

/* ==================== 风扇转速 ==================== */

// FanItem 一路风扇
type FanItem struct {
	Name string  `json:"name"`
	RPM  float64 `json:"rpm"`
}

// readFans 风扇转速。
// Windows 用户态几乎没有通用风扇接口：Win32_Fan 在绝大多数机器上只给「冷却设备」这个名字，
// DesiredSpeed 是 NULL；转速要么靠主板 SuperIO 芯片（要内核驱动），要么靠 LHM 这类软件。
// 所以这里只读 LHM（用户在跑才有），读不到就返回空 —— 不编转速。
func readFans() []FanItem {
	rows, err := wmiQuery(`root\LibreHardwareMonitor`,
		"SELECT Name,SensorType,Value FROM Sensor WHERE SensorType='Fan'",
		[]string{"Name", "SensorType", "Value"})
	if err != nil || len(rows) == 0 {
		return nil
	}
	var out []FanItem
	for _, r := range rows {
		var v float64
		if ok, bad := parseTempValue(r["Value"], &v); bad || !ok {
			continue
		}
		if v <= 0 {
			continue // 停转/未接的风扇，摆个 0 转只会让人以为坏了
		}
		out = append(out, FanItem{Name: strings.TrimSpace(r["Name"]), RPM: v})
	}
	return out
}

/* ==================== 电压 / 功耗 ====================
   用户要求部件上要能看到「电压、功耗」。先说清楚现实：
     Windows 用户态**没有**通用接口能拿到 CPU 核心电压（Vcore）和实际功耗。
     试过并确认走不通的路（2026-09-24 实测）：
       · Win32_Processor.CurrentVoltage  → 空
       · Win32_VoltageProbe.CurrentReading → 空（本机只报「数字传感器」，无数值）
       · 硬盘温度 DeviceIoControl + StorageDeviceTemperatureProperty(21) → ERROR_INVALID_FUNCTION
     唯一通用的来源是 LibreHardwareMonitor 的 WMI（它自己带驱动，SensorType=Voltage/Power）。
     装了就显示真值；没装就返回空，前端写「读不到」并给出装 LHM 的入口 —— 不编数值。 */

type SensorItem struct {
	Name  string  `json:"name"`
	Value float64 `json:"value"`
	Unit  string  `json:"unit"` // W / V
}

// readVolts 电压（伏特）。除 LHM 外目前没有通用来源
func readVolts() []SensorItem {
	return readLHMSensors("Voltage", "V")
}

// readPower 功耗（瓦特）。除 LHM 外目前没有通用来源
func readPower() []SensorItem {
	return readLHMSensors("Power", "W")
}

/* readLHMSensors 从 LibreHardwareMonitor 的 WMI 读一类传感器。
   LHM 没在跑就是空 —— 这不是错误，是这台机器确实没开放这个数据。 */
func readLHMSensors(kind, unit string) []SensorItem {
	rows, err := wmiQuery(`root\LibreHardwareMonitor`,
		"SELECT Name,SensorType,Value FROM Sensor WHERE SensorType='"+kind+"'",
		[]string{"Name", "SensorType", "Value"})
	if err != nil || len(rows) == 0 {
		return nil
	}
	var out []SensorItem
	for _, r := range rows {
		name := strings.TrimSpace(r["Name"])
		if name == "" {
			continue
		}
		var v float64
		if ok, bad := parseTempValue(r["Value"], &v); bad || !ok {
			continue
		}
		if v <= 0 {
			continue // 0 伏 / 0 瓦是没采到，不是真的为零
		}
		out = append(out, SensorItem{Name: name, Value: v, Unit: unit})
	}
	return out
}

/* cpuPackage 从一堆传感器里挑出「最像 CPU 整体」的那一个。
   LHM 会同时报 CPU Package / CPU Core #1..#N / CPU SoC 一堆，卡片上只放一个总览值，
   明细里再全列。挑不到就返回 nil —— 不拿某个核心的温度冒充整颗 CPU。 */
func pickCPU(items []SensorItem) *SensorItem {
	if len(items) == 0 {
		return nil
	}
	// 优先级：Package > Total > 名字里带 cpu 的 > 第一个
	for _, kw := range []string{"package", "total", "cpu"} {
		for i, it := range items {
			l := strings.ToLower(it.Name)
			if strings.Contains(l, kw) {
				return &items[i]
			}
		}
	}
	if len(items) > 0 {
		return &items[0]
	}
	return nil
}
