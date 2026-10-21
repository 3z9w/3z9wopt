package main

// 蓝屏分析：扫描本机 Windows 蓝屏转储（C:\Windows\Minidump\*.dmp 小转储 + MEMORY.DMP），
// 解析 DUMP_HEADER64 得到 BugCheck 代码与 4 个参数 —— 纯 Go 读文件头，不需要 WinDbg、
// 不需要符号服务器、不需要 PowerShell。
//
// 分层设计（2026-09-22，用户拍板「BlueScreenView 这种」）：
//   1. 头部解析（必成）：代码 + 参数 + 架构 + 系统版本，格式公开且 20 年不变；
//   2. 嫌疑驱动归属（尽力而为）：经 DirectoryTableBase 做四级页表翻译 → 走
//      PsLoadedModuleList 模块链表 → 读崩溃线程栈（CONTEXT 在头部 0x348），
//      落在模块地址区间内的即嫌疑驱动（BlueScreenView 同款思路）。
//      该步依赖转储内部布局，解析失败时优雅降级（只显示代码结论），绝不能报错。
//   3. 内置常见码表给中文结论与处理建议；「完整知识库」深链到 3z9w.com/bsod。
//
// 已知限制（交接单有记录）：
//   - 模块归属的偏移按 Win10/11 x64 布局解析，拿到真实蓝屏样本前未做过实证，
//     归属失败时 UI 显示「未定位到栈内驱动」，不影响主结论。

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unsafe"

	"github.com/wailsapp/wails/v2/pkg/runtime"
	"golang.org/x/sys/windows"
)

// BsodDriver 嫌疑驱动（崩溃栈里出现的）
type BsodDriver struct {
	Name string `json:"name"` // 模块名，如 nvlddmkm.sys
	Base string `json:"base"` // 基址（十六进制）
	Size string `json:"size"` // 镜像大小（十六进制）
	Hit  string `json:"hit"`  // 栈里命中的地址（十六进制）
	Off  string `json:"off"`  // 命中偏移，如 nvlddmkm.sys+0x2651（BlueScreenView 的「导致崩溃的驱动程序」同一口径）
	Desc string `json:"desc"` // 文件描述（读驱动文件的版本资源）
	Comp string `json:"comp"` // 公司
	Ver  string `json:"ver"`  // 文件版本
	Time string `json:"time"` // 文件时间戳（PE TimeDateStamp → 日期）

	// VerLocal：公司/描述/版本这几列是拿本机磁盘上的同名文件读出来的，但那份文件跟
	// 转储里的镜像不是同一个版本（多半在看别人机器或旧系统的转储）。界面上要标出来，
	// 别让用户以为那就是出事那台机器上的版本。
	VerLocal bool `json:"verLocal"`
}

// BsodModule 该转储装载的一个内核模块（BlueScreenView 下方那张驱动表的口径）
type BsodModule struct {
	Name    string `json:"name"`    // 文件名，如 dxgkrnl.sys
	Path    string `json:"path"`    // 完整路径（模块链里的 FullDllName；取不到留空，不猜）
	Base    string `json:"base"`    // 起始地址
	End     string `json:"end"`     // 结束地址
	Size    string `json:"size"`    // 大小
	Stamp   string `json:"stamp"`   // 文件时间戳（PE TimeDateStamp）
	Desc    string `json:"desc"`    // 文件描述
	Product string `json:"product"` // 产品名称
	Comp    string `json:"comp"`    // 公司
	Ver     string `json:"ver"`     // 文件版本
	InStack bool   `json:"inStack"` // 是否出现在崩溃栈里（表格里高亮 —— 就是「嫌疑驱动」）
	Hit     string `json:"hit"`     // 栈内命中的地址
	Off     string `json:"off"`     // 命中偏移（模块名+0x…）

	// VerLocal 见 BsodDriver 同名字段：版本几列来自本机同名文件、且与转储里的镜像对不上
	VerLocal bool `json:"verLocal"`
}

// BsodFrame 调用栈的每一层（WinDbg 的 kb / BlueScreenView 的栈口径）
type BsodFrame struct {
	No     int    `json:"no"`     // 层号（0 = 最内层，和 WinDbg 一致）
	Addr   string `json:"addr"`   // 返回地址
	Module string `json:"module"` // 落在哪个模块，如 nvlddmkm.sys
	Off    string `json:"off"`    // 模块+偏移，如 nvlddmkm+0x108BE00
	Call   string `json:"call"`   // 校验方式：直接调用 / 间接调用
	Sure   bool   `json:"sure"`   // 是否通过「上一条指令是 call」的校验
}

// BsodDump 一条蓝屏转储记录
type BsodDump struct {
	File    string       `json:"file"`    // 完整路径
	Name    string       `json:"name"`    // 文件名
	Size    int64        `json:"size"`    // 字节
	Time    string       `json:"time"`    // 文件时间（≈蓝屏发生时间）
	Valid   bool         `json:"valid"`   // 是否识别为有效内核转储
	Err     string       `json:"err"`     // 解析失败原因（Valid=false 时）
	Build   string       `json:"build"`   // 系统内部版本号，如 19045
	OS      string       `json:"os"`      // 版本人话，如 Windows 10 22H2
	Arch    string       `json:"arch"`    // x64 / x86
	Code    uint32       `json:"code"`    // BugCheck 代码
	CodeHex string       `json:"codeHex"` // 0x…显示
	P1      uint64       `json:"p1"`
	P2      uint64       `json:"p2"`
	P3      uint64       `json:"p3"`
	P4      uint64       `json:"p4"`
	Title   string       `json:"title"`   // 符号名，如 DRIVER_IRQL_NOT_LESS_OR_EQUAL
	Zh      string       `json:"zh"`      // 中文一句话
	Cause   string       `json:"cause"`   // 常见原因
	Advice  []string     `json:"advice"`  // 处理建议
	Params  []string     `json:"params"`  // 参数逐条解读
	Drivers []BsodDriver `json:"drivers"` // 嫌疑驱动（栈内命中的，可能为空）
	Modules []BsodModule `json:"modules"` // 该转储装载的全部内核模块（栈内的高亮）
	CrashAt string       `json:"crashAt"` // 产生崩溃的代码地址
	CrashIn string       `json:"crashIn"` // 崩溃点归属，如 dxgkrnl.sys+0x2651
	Image   string       `json:"image"`   // 被点名的驱动文件，如 nvlddmkm.sys（WinDbg 的 IMAGE_NAME）
	Symbol  string       `json:"symbol"`  // 模块+偏移，如 nvlddmkm+0x108BE00（无符号，只到这一层）
	Bucket  string       `json:"bucket"`  // 故障桶，如 0x116_IMAGE_nvlddmkm.sys
	Uptime  string       `json:"uptime"`  // 从开机到崩溃的运行时长（读不到留空，不编）
	Stack   []BsodFrame  `json:"stack"`   // 调用栈（按校验过的返回地址还原）
	Note    string       `json:"note"`    // 归属环节备注
}

// dumpDirs 转储位置
func bsodDumpLocations() (dir, full string) {
	root := os.Getenv("SystemRoot")
	if root == "" {
		root = `C:\Windows`
	}
	return filepath.Join(root, "Minidump"), filepath.Join(root, "MEMORY.DMP")
}

// BsodList 扫描转储目录，逐个做头部解析（轻量，不读大数据）。
func (a *App) BsodList() []BsodDump {
	dir, full := bsodDumpLocations()
	var list []BsodDump
	if ents, err := os.ReadDir(dir); err == nil {
		for _, e := range ents {
			if e.IsDir() || !strings.EqualFold(filepath.Ext(e.Name()), ".dmp") {
				continue
			}
			p := filepath.Join(dir, e.Name())
			list = append(list, bsodParse(p, false))
		}
	}
	if st, err := os.Stat(full); err == nil && !st.IsDir() {
		list = append(list, bsodParse(full, false))
	}
	sort.Slice(list, func(i, j int) bool { return list[i].Time > list[j].Time })
	return list
}

// BsodDumpDir 本机转储目录（C:\Windows\Minidump 之类，由 SystemRoot 推出来，不写死盘符）。
// 前端提示条要显示真实路径 —— 系统盘不一定是 C:。
func (a *App) BsodDumpDir() string {
	dir, _ := bsodDumpLocations()
	return dir
}

// BsodPickFile 打开系统文件选择框，让用户挑一个 .dmp 来分析
// （BlueScreenView 的「打开转储文件」同款：朋友发来的、从旧硬盘拷出来的都能看）。
// 返回选中路径；取消 / 取不到返回空串（前端据此静默返回，不算错误）。
func (a *App) BsodPickFile() string {
	if a.ctx == nil {
		return ""
	}
	p, err := runtime.OpenFileDialog(a.ctx, runtime.OpenDialogOptions{
		Title: "打开蓝屏转储文件",
		Filters: []runtime.FileFilter{
			{DisplayName: "转储文件 (*.dmp)", Pattern: "*.dmp"},
			{DisplayName: "所有文件 (*.*)", Pattern: "*.*"},
		},
	})
	if err != nil || p == "" {
		return ""
	}
	return p
}

// BsodReveal 在资源管理器里定位某个转储文件（explorer /select,）。
// ShellExecute 直接调，不走 CreateProcess —— 不会闪黑框，也不依赖 cmd。
func (a *App) BsodReveal(file string) string {
	file = filepath.Clean(file)
	if st, err := os.Stat(file); err != nil || st.IsDir() {
		return "文件不存在"
	}
	err := windows.ShellExecute(0,
		windows.StringToUTF16Ptr("open"),
		windows.StringToUTF16Ptr("explorer.exe"),
		windows.StringToUTF16Ptr(`/select,"`+file+`"`),
		nil, windows.SW_SHOWNORMAL)
	if err != nil {
		return "打开失败：" + err.Error()
	}
	return ""
}

// BsodDetail 深度分析单个转储（含嫌疑驱动归属）。
// 2026-10-12 放开「只限系统转储目录」：加了「打开蓝屏文件」后要能分析任意位置的 .dmp，
// 但仍挡住非 .dmp 与不存在/读不到的路径（解析只按头部走，不会把别的文件当转储）。
func (a *App) BsodDetail(file string) *BsodDump {
	file = filepath.Clean(file)
	if !strings.EqualFold(filepath.Ext(file), ".dmp") {
		return &BsodDump{File: file, Valid: false, Err: "只支持 .dmp 转储文件"}
	}
	st, err := os.Stat(file)
	if err != nil {
		return &BsodDump{File: file, Valid: false, Err: "文件不存在或读不到"}
	}
	if st.IsDir() {
		return &BsodDump{File: file, Valid: false, Err: "这是个文件夹，不是转储文件"}
	}
	d := bsodParse(file, true)
	return &d
}

// ---- 头部解析 ----

func bsodParse(path string, deep bool) BsodDump {
	d := BsodDump{File: path, Name: filepath.Base(path)}
	if st, err := os.Stat(path); err == nil {
		d.Size = st.Size()
		d.Time = st.ModTime().Format("2006-01-02 15:04")
	}
	f, err := os.Open(path)
	if err != nil {
		d.Err = "无法读取（需要管理员权限）: " + shortErr(err)
		return d
	}
	defer f.Close()
	head := make([]byte, 0x2000)
	if n, err := f.ReadAt(head, 0); err != nil && n < 0x60 {
		d.Err = "文件过小，不是内核转储"
		return d
	}
	if string(head[0:4]) != "PAGE" {
		d.Err = "不是 Windows 内核转储（缺 PAGE 签名）"
		return d
	}
	switch string(head[4:8]) {
	case "DU64":
		d.Arch = "x64"
	case "DUMP":
		d.Arch = "x86"
	default:
		d.Err = "未知转储子签名: " + string(head[4:8])
		return d
	}
	d.Valid = true
	// 发生时间优先用转储里记的那一个（拷来拷去文件时间会变，只有转储里的才是真时间）
	if t := bsodHeaderTime(head); t != "" {
		d.Time = t
	}
	machine := binary.LittleEndian.Uint32(head[0x30:])
	if d.Arch == "" {
		if machine == 0x8664 {
			d.Arch = "x64"
		} else {
			d.Arch = "x86"
		}
	}
	build := binary.LittleEndian.Uint32(head[0x0C:])
	d.Build = fmt.Sprintf("%d", build)
	d.OS = bsodBuildName(build)
	if d.Arch == "x64" {
		d.Code = binary.LittleEndian.Uint32(head[0x38:])
		d.P1 = binary.LittleEndian.Uint64(head[0x40:])
		d.P2 = binary.LittleEndian.Uint64(head[0x48:])
		d.P3 = binary.LittleEndian.Uint64(head[0x50:])
		d.P4 = binary.LittleEndian.Uint64(head[0x58:])
	} else {
		d.Code = binary.LittleEndian.Uint32(head[0x38:])
		d.P1 = uint64(binary.LittleEndian.Uint32(head[0x40:]))
		d.P2 = uint64(binary.LittleEndian.Uint32(head[0x44:]))
		d.P3 = uint64(binary.LittleEndian.Uint32(head[0x48:]))
		d.P4 = uint64(binary.LittleEndian.Uint32(head[0x4C:]))
	}
	d.CodeHex = fmt.Sprintf("0x%08X", d.Code)
	bi := bsodLookup(d.Code)
	d.Title, d.Zh, d.Cause, d.Advice = bi.title, bi.zh, bi.cause, bi.advice
	d.Params = bsodParams(d.Code, d.P1, d.P2, d.P3, d.P4)
	if deep {
		if d.Arch == "x64" {
			bsodAttribute(f, head, &d)
		} else {
			// 32 位转储的页表与栈布局是另一套（还分 PAE / 非 PAE），没把握就不做 ——
			// 但错误码、参数、系统版本这些头部信息照给，并写清楚哪部分没做。
			d.Note = "这份是 " + d.Arch + "（32 位）转储：错误码与参数已解析；栈回溯、调用栈与驱动表暂只做 x64"
		}
	}
	return d
}

// bsodHeaderTime 蓝屏发生时间。DUMP_HEADER 里时间字段的偏移各版本并不一致，写死会错，
// 所以改成在头部里找「像 FILETIME 的值」（2000 年 ~ 明年），并且**同一个值至少出现两次**
// 才采信 —— 实测 041024-6312-01.dmp 里 0x508 与 0xFA8 存着同一份时间，靠这条把撞运气的值滤掉。
func bsodHeaderTime(head []byte) string {
	minT := time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC).Unix()
	maxT := time.Now().AddDate(1, 0, 0).Unix()
	cnt := map[uint64]int{}
	for o := 0; o+8 <= len(head); o += 8 {
		v := binary.LittleEndian.Uint64(head[o:])
		if v == 0 {
			continue
		}
		if sec := int64(v)/1e7 - 11644473600; sec < minT || sec > maxT {
			continue
		}
		cnt[v]++
	}
	var best uint64
	bestN := 0
	for v, n := range cnt {
		if n > bestN || (n == bestN && v < best) {
			best, bestN = v, n
		}
	}
	if bestN < 2 {
		return ""
	}
	return time.Unix(int64(best)/1e7-11644473600, 0).Format("2006-01-02 15:04")
}

// bsodBuildName 版本号 → 人话
func bsodBuildName(b uint32) string {
	switch {
	case b >= 26100:
		return "Windows 11 24H2+"
	case b >= 22631:
		return "Windows 11 23H2"
	case b >= 22621:
		return "Windows 11 22H2"
	case b >= 22000:
		return "Windows 11 21H2"
	case b >= 19045:
		return "Windows 10 22H2"
	case b >= 19041:
		return "Windows 10 2004~21H2"
	case b >= 17763:
		return "Windows 10 1809"
	case b >= 17134:
		return "Windows 10 1803"
	case b >= 16299:
		return "Windows 10 1709"
	case b >= 15063:
		return "Windows 10 1703"
	case b >= 10586:
		return "Windows 10 1511"
	case b >= 10240:
		return "Windows 10 1507"
	case b >= 9600:
		return "Windows 8.1"
	case b >= 9200:
		return "Windows 8"
	case b >= 7601:
		return "Windows 7 SP1"
	case b >= 7600:
		return "Windows 7"
	default:
		return fmt.Sprintf("内部版本 %d", b)
	}
}

// ---- 码表（覆盖日常蓝屏 90%+）----

type bugInfo struct {
	title  string
	zh     string
	cause  string
	advice []string
}

func bsodLookup(code uint32) bugInfo {
	// 0x10000000 高位 = 用户态起源变体（如 0x1000007E），查表前剥掉
	key := code
	if key&0xF0000000 == 0x10000000 {
		key &^= 0x10000000 // 0x10000000 高位 = 用户态起源变体（如 0x1000007E）
	}
	tbl := map[uint32]bugInfo{
		0x0A:  {"IRQL_NOT_LESS_OR_EQUAL", "内核以过高的中断级别访问了无效内存", "绝大多数是驱动问题，其次是内存条", []string{"优先排查最近新装的驱动（结合下方嫌疑驱动）", "运行 Windows 内存诊断或 MemTest86 检查内存", "卸载最近装的安全软件 / 虚拟机工具类驱动"}},
		0x19:  {"BAD_POOL_HEADER", "内存池头部损坏", "驱动写坏内存池，杀软驱动是常见元凶", []string{"卸载或更新杀毒 / 安全软件", "检查最近装过的驱动与外设管理软件", "有内存报错先修内存"}},
		0x1A:  {"MEMORY_MANAGEMENT", "内存管理器遇到无法处理的错误", "内存条故障或驱动越界写内存", []string{"内存诊断优先（Win+R 输 mdsched.exe）", "重新插拔内存条、单条轮换测试", "结合嫌疑驱动更新或回滚"}},
		0x1E:  {"KMODE_EXCEPTION_NOT_HANDLED", "内核模式程序产生了未被处理的异常", "驱动故障，偶发于硬件", []string{"结合嫌疑驱动更新或卸载", "检查磁盘与内存"}},
		0x23:  {"FAT_FILE_SYSTEM", "FAT 文件系统出错", "U 盘 / 移动硬盘损坏或其驱动问题", []string{"拔掉所有 U 盘 / 移动硬盘再开机试试", "对相关盘做 chkdsk 修复"}},
		0x24:  {"NTFS_FILE_SYSTEM", "NTFS 文件系统出错", "磁盘坏道、文件系统损坏或杀软过滤驱动", []string{"chkdsk /r 修复系统盘", "检查硬盘健康（SMART）", "卸载磁盘加密 / 杀软类驱动"}},
		0x2E:  {"DATA_BUS_ERROR", "内存或总线奇偶校验错误", "内存故障为主，也可能是显卡显存", []string{"内存诊断 / 重新插拔内存", "清洗内存金手指、更换插槽"}},
		0x35:  {"NO_MORE_IRP_STACK_LOCATIONS", "驱动请求包层数不足", "多个过滤驱动互相打架（常见杀软 / 虚拟化软件）", []string{"卸载一个安全软件再试", "更新主板存储驱动"}},
		0x3B:  {"SYSTEM_SERVICE_EXCEPTION", "系统服务发生异常", "驱动或系统组件故障，显卡驱动常见", []string{"更新显卡驱动（DDU 干净卸载后重装）", "结合嫌疑驱动处理", "运行 sfc /scannow 修系统文件"}},
		0x44:  {"MULTIPLE_IRP_COMPLETE_REQUESTS", "同一个请求被多个驱动重复完成", "过滤驱动冲突，杀软 / 网络过滤最常见", []string{"卸载多余的安全软件 / 网络加速器", "更新网卡驱动"}},
		0x4A:  {"IRQL_GT_ZERO_AT_SYSTEM_SERVICE", "带着过高中断级别进入系统服务", "驱动问题", []string{"结合嫌疑驱动更新或卸载"}},
		0x50:  {"PAGE_FAULT_IN_NONPAGED_AREA", "访问了不存在的内存页", "内存故障或驱动越界，最常见蓝屏之一", []string{"内存诊断优先", "结合嫌疑驱动更新 / 回滚", "检查最近装过的软件与驱动"}},
		0x6B:  {"PROCESS1_INITIALIZATION_FAILED", "系统进程初始化失败", "启动文件损坏或磁盘问题", []string{"进恢复环境做启动修复", "检查系统盘 SMART"}},
		0x73:  {"CONFIG_LIST_FAILED", "注册表配置单元读写失败", "注册表损坏或磁盘问题", []string{"chkdsk 检查系统盘", "从恢复环境还原注册表"}},
		0x74:  {"BAD_SYSTEM_CONFIG_INFO", "系统配置信息错误", "注册表 SYSTEM 蜂巢损坏 / BCD 配置错误", []string{"进恢复环境做启动修复（bootrec /rebuildbcd）", "最近改过 BIOS 磁盘模式的改回原样", "本软件工具库的「系统引导修复」可尝试"}},
		0x77:  {"KERNEL_STACK_INPAGE_ERROR", "内核栈换页读取失败", "磁盘坏道或内存故障", []string{"chkdsk /r 修磁盘", "内存诊断"}},
		0x7A:  {"KERNEL_DATA_INPAGE_ERROR", "页面文件数据读入失败", "磁盘坏道、数据线松动或内存故障", []string{"检查磁盘 SMART 与数据线", "chkdsk /r", "内存诊断"}},
		0x7B:  {"INACCESSIBLE_BOOT_DEVICE", "启动时访问不到系统盘", "改过 BIOS 磁盘模式（AHCI/RAID）、存储驱动缺失", []string{"BIOS 里把磁盘模式改回装机时的方式", "进恢复环境卸载最近装存储驱动", "换数据线 / 接口试试"}},
		0x7E:  {"SYSTEM_THREAD_EXCEPTION_NOT_HANDLED", "系统线程产生未处理异常", "驱动故障（显卡 / 声卡驱动常见）", []string{"结合嫌疑驱动更新或回滚", "DDU 干净重装显卡驱动"}},
		0x7F:  {"UNEXPECTED_KERNEL_MODE_TRAP", "内核发生意外陷阱", "硬件问题为主：超频、内存、CPU", []string{"BIOS 恢复默认频率，去掉超频", "内存诊断", "清灰检查散热"}},
		0x8E:  {"KERNEL_MODE_EXCEPTION_NOT_HANDLED", "内核模式异常未处理", "驱动或内存问题", []string{"结合嫌疑驱动处理", "内存诊断"}},
		0x9F:  {"DRIVER_POWER_STATE_FAILURE", "驱动电源状态切换超时", "驱动在睡眠 / 唤醒 / 关机时卡死", []string{"回忆是否睡眠 / 关机时蓝屏，更新对应设备驱动", "显卡 / 网卡驱动优先", "关闭快速启动试试"}},
		0xA0:  {"INTERNAL_POWER_ERROR", "电源管理内部错误", "电源策略、电池驱动或页面文件太小", []string{"重置电源计划为默认", "加大页面文件（虚拟内存）", "更新电池 / 电源驱动"}},
		0xBE:  {"ATTEMPTED_WRITE_TO_READONLY_MEMORY", "驱动试图写只读内存", "驱动越权写内存", []string{"结合嫌疑驱动更新或卸载"}},
		0xC2:  {"BAD_POOL_CALLER", "驱动以错误方式申请内存池", "驱动缺陷", []string{"结合嫌疑驱动更新或卸载", "卸载最近装的软件驱动"}},
		0xC4:  {"DRIVER_VERIFIER_DETECTED_VIOLATION", "驱动验证器抓到违规驱动", "开启了驱动验证器后由问题驱动触发", []string{"看嫌疑驱动一栏，它就是元凶", "更新或卸载该驱动", "命令 verifier /reset 关闭验证器"}},
		0xC5:  {"DRIVER_CORRUPTED_EXPOOL", "驱动损坏了内存池", "驱动越界写内存", []string{"结合嫌疑驱动更新或卸载"}},
		0xC9:  {"DRIVER_VERIFIER_IOMANAGER_VIOLATION", "驱动验证器发现 IO 违规", "驱动违规操作 IO", []string{"看嫌疑驱动，更新或卸载", "verifier /reset 关闭验证器"}},
		0xD1:  {"DRIVER_IRQL_NOT_LESS_OR_EQUAL", "驱动在过高中断级别访问了无效内存", "最常见的驱动蓝屏，嫌疑驱动一栏基本就是元凶", []string{"更新或回滚嫌疑驱动（网络 / 显卡驱动最常见）", "网络蓝屏可先禁用网卡高级特性试试", "查不到就用驱动验证器定位"}},
		0xED:  {"UNMOUNTABLE_BOOT_VOLUME", "系统卷无法挂载", "磁盘故障或文件系统损坏", []string{"chkdsk /r", "检查数据线与 SMART", "严重时先备份再换盘"}},
		0xEF:  {"CRITICAL_PROCESS_DIED", "关键系统进程死亡", "系统文件损坏、磁盘坏道或驱动问题", []string{"sfc /scannow + DISM 修复系统", "chkdsk 检查磁盘", "结合嫌疑驱动处理"}},
		0xF5:  {"FLTMGR_FILE_SYSTEM", "文件系统过滤管理器出错", "安全软件 / 杀软过滤驱动故障", []string{"卸载或更新杀毒软件"}},
		0xFC:  {"ATTEMPTED_EXECUTE_OF_NOEXECUTE_MEMORY", "试图执行不可执行内存", "驱动或内存故障", []string{"结合嫌疑驱动处理", "内存诊断"}},
		0x101: {"CLOCK_WATCHDOG_TIMEOUT", "多核时钟同步超时", "CPU / 超频 / 主板问题，也可由驱动引起", []string{"去掉超频，BIOS 恢复默认", "更新 BIOS 与芯片组驱动", "检查 CPU 散热"}},
		0x109: {"CRITICAL_STRUCTURE_CORRUPTION", "关键内核结构被篡改", "问题驱动、内存故障或超频", []string{"结合嫌疑驱动更新或卸载", "内存诊断", "去掉超频与内核级「优化」软件"}},
		0x116: {"VIDEO_TDR_FAILURE", "显卡驱动响应超时，系统重置显卡也没救回来", "显卡驱动崩溃、供电/散热不足，或显卡本身有问题", []string{
		"装一个和显卡型号匹配的驱动；要是更新后才开始蓝屏，就回退到上一个版本",
		"重装时优先用显卡厂商安装程序里的「清洁安装 / 自定义清洁安装」",
		"显卡和系统的设置恢复默认（去掉超频、降压、强制高功耗）",
		"检查显卡散热与供电，确认供电线和显示器线插紧",
		"以上都做完还蓝屏，再跑显卡 / 整机硬件检测"}},
		0x117: {"VIDEO_TDR_TIMEOUT_DETECTED", "显卡超时未恢复", "显卡驱动或硬件问题", []string{"同 0x116：重装显卡驱动、检查散热供电"}},
		0x119: {"VIDEO_SCHEDULER_INTERNAL_ERROR", "显卡调度器内部错误", "显卡驱动 / 硬件", []string{"重装显卡驱动", "检查显卡硬件"}},
		0x124: {"WHEA_UNCORRECTABLE_ERROR", "硬件报告了不可纠正的错误", "硬件级故障：CPU / 电压 / 超频 / 内存", []string{"去掉一切超频，BIOS 恢复默认", "更新 BIOS", "检查供电与散热", "内存诊断"}},
		0x133: {"DPC_WATCHDOG_VIOLATION", "DPC 看门狗超时", "驱动长时间占用调度，SSD 固件 / 驱动常见", []string{"更新 SSD 固件与主板存储驱动", "更新显卡 / 网卡驱动", "Win10 老版本先打满系统补丁"}},
		0x139: {"KERNEL_SECURITY_CHECK_FAILURE", "内核安全检查失败（栈损坏）", "驱动内存越界", []string{"结合嫌疑驱动更新或卸载", "内存诊断"}},
		0x15F: {"CONNECTED_STANDBY_WATCHDOG_TIMEOUT", "现代待机看门狗超时", "睡眠 / 待机相关驱动问题", []string{"更新显卡与网卡驱动", "暂时改用传统睡眠模式"}},
		0x192: {"KERNEL_AUTO_BOOST_LOCK_ACQUISITION_WITH_RAISED_IRQL", "驱动在过高中断级别获取锁", "驱动问题", []string{"结合嫌疑驱动处理"}},
		0x1C7: {"STORE_DATA_STRUCTURE_CORRUPTION", "存储数据结构损坏", "内存或驱动问题", []string{"内存诊断", "结合嫌疑驱动处理"}},
		0x1D3: {"WFP_INVALID_OPERATION", "网络过滤平台操作无效", "网络过滤驱动（杀软 / 防火墙）问题", []string{"卸载或更新安全软件", "重置网络组件"}},
	}
	if v, ok := tbl[key]; ok {
		return v
	}
	return bugInfo{
		title:  fmt.Sprintf("UNKNOWN BugCheck 0x%X", code),
		zh:     "罕见蓝屏代码，内置码表未收录",
		cause:  "请在在线知识库查询该代码的完整释义",
		advice: []string{"点击「在线查完整释义」到 3z9w.com/bsod 查询", "通用排查：内存诊断 + 更新嫌疑驱动 + 系统修复"},
	}
}

// bsodStatus 常见 NTSTATUS → 中文（只收录有把握的，认不出来的返回空串，绝不硬编）
var bsodStatus = map[uint32]string{
	0xC0000001: "未指定的失败（STATUS_UNSUCCESSFUL）",
	0xC0000002: "功能未实现（STATUS_NOT_IMPLEMENTED）",
	0xC0000004: "传入的信息长度不匹配（STATUS_INFO_LENGTH_MISMATCH）",
	0xC0000005: "访问违规 —— 读了不该读的内存（STATUS_ACCESS_VIOLATION）",
	0xC0000006: "页面读入失败 —— 磁盘坏道或文件损坏（STATUS_IN_PAGE_ERROR）",
	0xC0000008: "句柄无效（STATUS_INVALID_HANDLE）",
	0xC000000D: "参数无效（STATUS_INVALID_PARAMETER）",
	0xC000000E: "设备不存在（STATUS_NO_SUCH_DEVICE）",
	0xC000000F: "文件不存在（STATUS_NO_SUCH_FILE）",
	0xC0000010: "设备请求无效（STATUS_INVALID_DEVICE_REQUEST）",
	0xC0000022: "访问被拒绝（STATUS_ACCESS_DENIED）",
	0xC000009A: "系统资源不足（STATUS_INSUFFICIENT_RESOURCES）",
	0xC000009C: "设备数据错误 —— 磁盘坏道 / 数据线松动（STATUS_DEVICE_DATA_ERROR）",
	0xC000009D: "设备未连接（STATUS_DEVICE_NOT_CONNECTED）",
	0xC00000A3: "设备未就绪（STATUS_DEVICE_NOT_READY）",
	0xC00000B5: "IO 超时（STATUS_IO_TIMEOUT）",
	0xC0000120: "操作被取消（STATUS_CANCELLED）",
	0xC0000185: "IO 设备错误（STATUS_IO_DEVICE_ERROR）",
	0xC0000374: "堆已损坏（STATUS_HEAP_CORRUPTION）",
	0xC0000409: "栈缓冲区溢出（STATUS_STACK_BUFFER_OVERRUN）",
}

// bsodStatusText 值看着像 NTSTATUS 才翻译（0xC0000000 段），认不出来返回空串
func bsodStatusText(v uint64) string {
	var code uint32
	switch {
	case v>>32 == 0xFFFFFFFF: // 符号扩展成 64 位，蓝屏参数里最常见的写法
		code = uint32(v)
	case v>>32 == 0 && v >= 0xC0000000:
		code = uint32(v)
	default:
		return ""
	}
	return bsodStatus[code]
}

// bsodBlameArg 哪个参数里装的是「责任驱动」的指针（MSDN 公开定义，只对有把握的代码填）。
// 返回 0 = 不用参数定位，以崩溃点（RIP）为准。
func bsodBlameArg(code uint32) int {
	key := code
	if key&0xF0000000 == 0x10000000 {
		key &^= 0x10000000
	}
	switch key {
	case 0x116, 0x117: // Arg2 = 指向责任驱动的指针
		return 2
	case 0x7E, 0x8E, 0x3B: // Arg2 = 异常发生地址
		return 2
	case 0x50: // Arg3 = 引用该地址的代码地址
		return 3
	case 0x0A, 0xD1: // Arg4 = 引用内存的那条指令地址
		return 4
	}
	return 0
}

// bsodParams 高频代码的参数翻译（只翻译有把握的）
func bsodParams(code uint32, p1, p2, p3, p4 uint64) []string {
	H := func(v uint64) string { return fmt.Sprintf("0x%016X", v) }
	key := code
	if key&0xF0000000 == 0x10000000 {
		key &^= 0x10000000
	}
	type pi struct{ idx int; text string }
	var hints []pi
	switch key {
	case 0x0A, 0xD1:
		rw := "读"
		if p3 == 1 {
			rw = "写"
		}
		hints = []pi{{1, "被访问的无效地址"}, {2, fmt.Sprintf("当时的中断级别 IRQL=%d", p2)}, {3, "操作类型（" + rw + "）"}, {4, "出错指令的地址 ← 归属驱动的关键线索"}}
	case 0x50:
		hints = []pi{{1, "出错的虚拟地址"}, {2, "访问类型（0=读 1=写 8=执行）"}, {3, "引用该地址的代码地址 ← 归属驱动的关键线索"}}
	case 0x7E, 0x8E:
		hints = []pi{{1, "异常代码"}, {2, "异常发生地址 ← 归属驱动的关键线索"}, {3, "异常记录地址"}, {4, "上下文记录地址"}}
	case 0x3B:
		hints = []pi{{1, "异常代码"}, {2, "异常发生地址 ← 归属驱动的关键线索"}}
	case 0x116, 0x117:
		hints = []pi{{1, "内部 TDR 恢复上下文指针"}, {2, "指向责任驱动的指针 ← 关键线索"}, {3, "最后一次失败操作的 NTSTATUS 错误码"}, {4, "内部上下文数据"}}
	case 0x139:
		hints = []pi{{2, "陷阱帧地址"}, {3, "异常帧地址"}}
	case 0xC2, 0xC5:
		hints = []pi{{1, "内存池操作 / 出错地址"}}
	}
	out := []string{"参数1 = " + H(p1), "参数2 = " + H(p2), "参数3 = " + H(p3), "参数4 = " + H(p4)}
	if len(hints) == 0 {
		return out
	}
	res := make([]string, 4)
	for i := 1; i <= 4; i++ {
		txt := ""
		for _, h := range hints {
			if h.idx == i {
				txt = h.text
			}
		}
		var v uint64 = p1
		if i == 2 {
			v = p2
		} else if i == 3 {
			v = p3
		} else if i == 4 {
			v = p4
		}
		if txt != "" {
			res[i-1] = fmt.Sprintf("参数%d = %s —— %s", i, H(v), txt)
		} else {
			res[i-1] = fmt.Sprintf("参数%d = %s", i, H(v))
		}
	}
	return res
}

// ---- 嫌疑驱动归属（尽力而为，失败必须优雅降级）----

type bsodRun struct{ base, count uint64 }

type bsodReader struct {
	f          *os.File
	runs       []bsodRun
	dataOffset int64
	dtb        uint64 // DirectoryTableBase（页表翻译用）
}

// newBsodReader 解析物理内存布局：头部 0x88 起是 PHYSICAL_MEMORY_DESCRIPTOR64
// （NumberOfRuns + 每项 16 字节），位图紧随 0x2000 头部，然后是按序存放的页数据。
func newBsodReader(f *os.File, head []byte) (*bsodReader, error) {
	runsN := binary.LittleEndian.Uint64(head[0x88:])
	if runsN == 0 || runsN > 503 {
		return nil, fmt.Errorf("物理内存区段数异常: %d", runsN)
	}
	var runs []bsodRun
	var total uint64
	for i := uint64(0); i < runsN; i++ {
		off := 0x90 + i*16
		if off+16 > 0x2000 {
			break
		}
		bp := binary.LittleEndian.Uint64(head[off:])
		pc := binary.LittleEndian.Uint64(head[off+8:])
		if pc == 0 || bp+pc > 1<<40 {
			return nil, fmt.Errorf("区段不合理")
		}
		runs = append(runs, bsodRun{bp, pc})
		total += pc
	}
	// 合理性：数据区大小不能比文件大太多
	if total < 1 || int64(total)*0x1000 > 4*1024*1024*1024 {
		return nil, fmt.Errorf("内存总量异常")
	}
	bitmapBytes := (total + 7) / 8
	bitmapBytes = (bitmapBytes + 7) / 8 * 8
	return &bsodReader{f: f, runs: runs, dataOffset: 0x2000 + int64(bitmapBytes)}, nil
}

// readPhys 读物理地址
func (r *bsodReader) readPhys(pa uint64, n int) ([]byte, error) {
	page := pa / 0x1000
	off := pa % 0x1000
	var before uint64
	for _, run := range r.runs {
		if page >= run.base && page < run.base+run.count {
			idx := before + (page - run.base)
			buf := make([]byte, n)
			fo := r.dataOffset + int64(idx)*0x1000 + int64(off)
			if m, err := r.f.ReadAt(buf, fo); err != nil && m < n {
				return nil, fmt.Errorf("读转储数据失败")
			}
			return buf, nil
		}
		before += run.count
	}
	return nil, fmt.Errorf("物理页 0x%X 不在转储内", page)
}

// readVirtual x64 四级页表翻译后读内存（支持 1GB/2MB/4KB 页）
func (r *bsodReader) readVirtual(va uint64, n int) ([]byte, error) {
	pml4 := func(tab uint64, idx uint64) (uint64, error) {
		b, err := r.readPhys(tab&0x000FFFFFFFFFF000+idx*8, 8)
		if err != nil {
			return 0, err
		}
		e := binary.LittleEndian.Uint64(b)
		if e&1 == 0 {
			return 0, fmt.Errorf("页表项不存在")
		}
		return e, nil
	}
	e, err := pml4(r.dtb, va>>39&0x1FF)
	if err != nil {
		return nil, err
	}
	e, err = pml4(e, va>>30&0x1FF)
	if err != nil {
		return nil, err
	}
	if e&0x80 != 0 { // 1GB 大页
		pa := e&0x000FFFFFC0000000 + va&0x3FFFFFFF
		return r.readPhys(pa, n)
	}
	e, err = pml4(e, va>>21&0x1FF)
	if err != nil {
		return nil, err
	}
	if e&0x80 != 0 { // 2MB 大页
		pa := e&0x000FFFFFFFE00000 + va&0x1FFFFF
		return r.readPhys(pa, n)
	}
	e, err = pml4(e, va>>12&0x1FF)
	if err != nil {
		return nil, err
	}
	pa := e&0x000FFFFFFFFFF000 + va&0xFFF
	if n <= 0x1000-int(pa&0xFFF) {
		return r.readPhys(pa, n)
	}
	// 跨页：分两段
	first := 0x1000 - int(pa&0xFFF)
	b1, err := r.readPhys(pa, first)
	if err != nil {
		return nil, err
	}
	b2, err := r.readVirtual(va+uint64(first), n-first)
	if err != nil {
		return nil, err
	}
	return append(b1, b2...), nil
}

// bsodAttribute 深度归属：模块链表 + 崩溃栈扫描。任何一步失败都只写 Note，不报错。
func bsodAttribute(f *os.File, head []byte, d *BsodDump) {
	defer func() { recover() }() // 归属失败绝不拖垮主流程
	dtb := binary.LittleEndian.Uint64(head[0x10:])
	lml := binary.LittleEndian.Uint64(head[0x20:])
	// 崩溃上下文：头部 0x348 是 CONTEXT（RSP=+0x98, RIP=+0xF8），0xF00 是 EXCEPTION_RECORD64（Address=+0x18）
	rsp := binary.LittleEndian.Uint64(head[0x348+0x98:])
	rip := binary.LittleEndian.Uint64(head[0x348+0xF8:])
	exc := binary.LittleEndian.Uint64(head[0xF00+0x18:])

	r, err := newBsodReader(f, head)
	if err == nil && dtb != 0 && lml != 0 {
		r.dtb = dtb
		if mods := bsodWalkModules(r, lml); len(mods) > 0 {
			// 运行时长：KUSER_SHARED_DATA（内核固定 VA）里的 InterruptTime，单位 100ns
			if b, e := r.readVirtual(0xFFFFF78000000000+0x8, 0x10); e == nil && len(b) == 0x10 {
				// LowPart@+0x8 / High1Time@+0xC / High2Time@+0x10，取 High2Time 那一份
				tk := uint64(binary.LittleEndian.Uint32(b[8:]))<<32 | uint64(binary.LittleEndian.Uint32(b[0:]))
				if sec := tk / 1e7; sec > 0 && sec < 3600*24*730 {
					d.Uptime = fmt.Sprintf("%d 天 %02d:%02d:%02d", sec/86400, sec%86400/3600, sec%3600/60, sec%60)
				}
			}
			bsodBlame(d, mods, bsodStackBytes(r, rsp), rip, exc,
				func(va uint64) (string, bool) { return bsodCallSite(r, va) },
				func(m bsodMod) uint32 { return bsodImageStamp(r, m.base) })
			return
		}
	}
	/* ── 走不了页表 → 退到「文件自带的模块表 + 崩溃栈」这一路 ──────────────
	   ⚠️ 真实机器上最常见的小转储（Minidump 目录里那种）头部**没有**物理内存段表
	   （实测 041024-6312-01.dmp：0x88 起是 "PAGEPAGE" 填充，不是段描述符），
	   原来在这里直接放弃 → 真实文件只能出错误码，出不了「被点名的驱动」和驱动表。
	   这类转储把模块表和崩溃栈直接摆在文件里，扫出来即可，逻辑见 bsodAttributeFlat。 */
	bsodAttributeFlat(f, head, d)
}

// bsodStackBytes 从 RSP 往上读 32KB 栈（缺页即止）
func bsodStackBytes(r *bsodReader, rsp uint64) []byte {
	if rsp < 0xFFFF800000000000 {
		return nil
	}
	const span = 0x8000
	buf := make([]byte, 0, span)
	got := 0
	for off := uint64(0); off < span && got < span; off += 0x1000 {
		n := span - got
		if n > 0x1000 {
			n = 0x1000
		}
		chunk, err := r.readVirtual(rsp+off, n)
		if err != nil {
			break
		}
		buf = append(buf, chunk...)
		got += n
	}
	return buf
}

// bsodBlame 通用归属：模块表 + 崩溃栈有了之后，剩下的产出逻辑两种转储共用。
//   buf     崩溃栈的原始字节（按 8 字节扫；没有就传 nil）
//   rip     崩溃时的指令地址（0 = 没取到）；exc 异常记录里的地址（0 = 没有）
//   verify  校验「返回地址前面确实是 call」的函数；传 nil = 这份转储取不到指令页，不校验
//   stampOf 取模块自带的 PE 时间戳（0 = 取不到）
func bsodBlame(d *BsodDump, mods []bsodMod, buf []byte, rip, exc uint64,
	verify func(uint64) (string, bool), stampOf func(bsodMod) uint32) {
	// 落点直接命中：命中地址一并记下来（「模块名+偏移」是 BlueScreenView 最有用的一列）
	hits := map[string]uint64{}
	hitFor := func(va uint64) (bsodMod, bool) {
		for _, m := range mods {
			if va >= m.base && va < m.base+m.size {
				return m, true
			}
		}
		return bsodMod{}, false
	}
	mark := func(va uint64) {
		m, ok := hitFor(va)
		// va == m.base：正好等于模块基址的一律不算 —— 栈里不会有「返回到模块开头」这种事，
		// 这类值只可能是旁边那张模块表自己的基址字段（小转储的栈窗口会和模块表挨着，实测跑出 42 个假嫌疑）。
		if !ok || va == m.base {
			return
		}
		if _, dup := hits[m.name]; !dup {
			hits[m.name] = va
		}
	}
	d.CrashAt = fmt.Sprintf("0x%X", rip)
	if m, ok := hitFor(rip); ok {
		d.CrashIn = bsodModOff(m, rip)
	}
	mark(rip)
	if exc != 0 {
		mark(exc)
	}
	// 栈扫描：顺序即「栈内出现顺序」——先命中的排前面（栈顶 = 最后出事的那层）
	for i := 0; i+8 <= len(buf); i += 8 {
		v := binary.LittleEndian.Uint64(buf[i:])
		if v >= 0xFFFF800000000000 {
			mark(v)
		}
	}
	// 调用栈：再扫一遍，只留落在模块区间的地址。
	// 能校验就校验「上一条指令是不是 call」（栈里躺着的远不止返回地址：局部变量、句柄、
	// 字符串指针都可能长得像内核地址）—— 宁可少列，也不列假的；取不到指令页就照实标注。
	seenAddr := map[uint64]bool{}
	// 校验过的帧可以放心多列；没校验的只列前 16 层 —— 栈里除了返回地址还压着参数和
	// 局部变量，越往深处真帧占比越低，列太多等于把噪声排在前面（实测列 40 层时后 20 层全是数据）。
	maxFrames := 16
	if verify != nil {
		maxFrames = 40
	}
	for i := 0; i+8 <= len(buf) && len(d.Stack) < maxFrames; i += 8 {
		v := binary.LittleEndian.Uint64(buf[i:])
		if v < 0xFFFF800000000000 || seenAddr[v] {
			continue
		}
		m, ok := hitFor(v)
		if !ok || v == m.base {
			// v == m.base：正好等于模块基址的几乎都是模块表里的条目，不是返回地址
			continue
		}
		kind, sure := "未校验（这份转储不含指令页）", true
		if verify != nil {
			kind, sure = verify(v)
		}
		if !sure {
			continue
		}
		seenAddr[v] = true
		d.Stack = append(d.Stack, BsodFrame{
			No:     len(d.Stack),
			Addr:   fmt.Sprintf("0x%X", v),
			Module: m.name,
			Off:    strings.TrimSuffix(m.name, filepath.Ext(m.name)) + fmt.Sprintf("+0x%X", v-m.base),
			Call:   kind,
			Sure:   verify != nil,
		})
	}
	// 模块表：全量列出（对齐 BlueScreenView 下方那张表），栈内命中的高亮
	for _, m := range mods {
		hit := hits[m.name]
		mo := BsodModule{
			Name:    m.name,
			Path:    m.full,
			Base:    fmt.Sprintf("0x%X", m.base),
			End:     fmt.Sprintf("0x%X", m.base+m.size),
			Size:    fmt.Sprintf("0x%X", m.size),
			InStack: hit != 0,
		}
		if hit != 0 {
			mo.Hit = fmt.Sprintf("0x%X", hit)
			mo.Off = bsodModOff(m, hit)
		}
		// 版本信息只读磁盘上真有的文件（转储里没有这些字段，BlueScreenView 也是从磁盘读的）。
		// ⚠️ 看别的机器的转储时，本机同名文件通常不是同一个版本 —— 先拿转储里镜像自带的
		// PE 时间戳跟本地文件对一下，对不上就在界面标出来，绝不拿本机的冒充出事那台的。
		if p := bsodModuleFile(m); p != "" {
			var dumpTS uint32
			if stampOf != nil {
				dumpTS = stampOf(m)
			}
			mo.Stamp = bsodStampText(dumpTS)
			if mo.Stamp == "" {
				mo.Stamp = peTimeStamp(p) // 转储里读不到（精简转储常见），退回本地文件那份
			}
			mo.VerLocal = dumpTS != 0 && peStampRaw(p) != 0 && dumpTS != peStampRaw(p)
			vi := fileVersionInfo(p)
			mo.Desc, mo.Product, mo.Comp, mo.Ver = vi.desc, vi.product, vi.company, vi.version
		}
		d.Modules = append(d.Modules, mo)
		if hit != 0 {
			d.Drivers = append(d.Drivers, BsodDriver{
				Name: m.name, Base: mo.Base, Size: mo.Size, Hit: mo.Hit, Off: mo.Off,
				Desc: mo.Desc, Comp: mo.Comp, Ver: mo.Ver, Time: mo.Stamp,
				VerLocal: mo.VerLocal,
			})
		}
	}
	// ── 被点名的驱动（WinDbg 的 IMAGE_NAME / SYMBOL_NAME / FAILURE_BUCKET_ID）──
	// 优先用「责任驱动」那个参数（0x116 就是 Arg2），其次才是崩溃点 RIP ——
	// 0x116 的 RIP 落在 nt!KeBugCheckEx 里，拿它当元凶会指错人。
	imgMod, imgVA := bsodMod{}, uint64(0)
	if bi := bsodBlameArg(d.Code); bi > 0 {
		var pv uint64
		switch bi {
		case 2:
			pv = d.P2
		case 3:
			pv = d.P3
		case 4:
			pv = d.P4
		}
		if m, ok := hitFor(pv); ok {
			imgMod, imgVA = m, pv
		}
	}
	if imgVA == 0 {
		if m, ok := hitFor(rip); ok {
			imgMod, imgVA = m, rip
		}
	}
	if imgMod.name != "" {
		base := strings.TrimSuffix(imgMod.name, filepath.Ext(imgMod.name))
		d.Image = imgMod.name
		d.Symbol = fmt.Sprintf("%s+0x%X", base, imgVA-imgMod.base)
		d.Bucket = fmt.Sprintf("0x%X_IMAGE_%s", d.Code, imgMod.name)
	}

	// ── 参数二次解读：像 NTSTATUS 的翻成中文，落在模块区间的补「模块+偏移」──
	d.Params = bsodParams(d.Code, d.P1, d.P2, d.P3, d.P4)
	ps := []uint64{d.P1, d.P2, d.P3, d.P4}
	for i := range d.Params {
		var extra []string
		if s := bsodStatusText(ps[i]); s != "" {
			extra = append(extra, s)
		}
		if m, ok := hitFor(ps[i]); ok {
			extra = append(extra, "→ "+bsodModOff(m, ps[i]))
		}
		if len(extra) > 0 {
			d.Params[i] += " —— " + strings.Join(extra, "；")
		}
	}

	n := len(d.Stack)
	switch {
	case len(d.Drivers) == 0:
		d.Note = "已解析模块链表，但栈内未命中驱动地址（小转储未含完整栈属正常）"
	case n == 0:
		d.Note = fmt.Sprintf("共解析 %d 个内核模块，栈内命中 %d 个嫌疑驱动；但没找到可确认的返回地址，调用栈不列（不猜）", len(mods), len(d.Drivers))
	default:
		d.Note = fmt.Sprintf("共解析 %d 个内核模块，栈内命中 %d 个嫌疑驱动；还原出 %d 层调用栈（未加载符号，只到模块+偏移）", len(mods), len(d.Drivers), n)
	}
}

// ── 小转储（无物理内存段表）：直接从文件里扫模块表与崩溃栈 ──────────────────
//
// 真实机器上最常见的小转储头部没有段描述符，没法做页表翻译，但系统把「模块表」和
// 「崩溃栈」原样摆在文件里 —— 实测（041024-6312-01.dmp，与 WinDbg 报告逐条对过）：
//   · 模块表：144 字节一条，+0x00 名字 RVA、+0x38 基址、+0x48 镜像大小、+0x88 PE 时间戳
//     → 183 个模块；参数 2 落进 nvlddmkm，偏移 0x108BE00，与 WinDbg 的 nvlddmkm+108be00 一致
//   · 崩溃栈：栈上必然压着「BugCheck 代码 + 参数 1~4」这 40 个字节，拿它当锚点定位栈区
//     → 还原出的顺序与 WinDbg 的 STACK_TEXT 一致
// 两处都用「自校验 + 取最长连续段」来找，不写死偏移（换台机器 / 换个版本也不至于全废）。

const bsodFlatRec = 144

func bsodAttributeFlat(f *os.File, head []byte, d *BsodDump) {
	st, err := f.Stat()
	if err != nil {
		d.Note = "读不到转储大小，未能定位驱动（不影响错误码结论）"
		return
	}
	mods, tabLo, tabHi := bsodScanModTable(f, st.Size(), d.Arch)
	if len(mods) == 0 {
		d.Note = "这份转储里没找到模块表，定位不到驱动（错误码与参数不受影响）"
		return
	}
	rip := binary.LittleEndian.Uint64(head[0x348+0xF8:])
	buf := bsodStackWindow(f, st.Size(), d, tabLo, tabHi)
	bsodBlame(d, mods, buf, rip, 0, nil, func(m bsodMod) uint32 { return m.stamp })
	if len(d.Drivers) == 0 && len(d.Stack) == 0 {
		d.Note = fmt.Sprintf("读出 %d 个内核模块；这份转储没存下崩溃栈，嫌疑驱动与调用栈列不出来（不猜）", len(mods))
	}
}

// bsodScanModTable 自定位模块表：按 8 字节步进试，每个候选点往后数连续合法的 144 字节记录，
// 取最长的一段。单条记录必须同时满足「名字是内核路径、基址在内核空间、大小合理」，
// 否则立刻断 —— 靠这个把误命中压到最低（要至少 20 条连续才认）。
func bsodScanModTable(f *os.File, fsz int64, arch string) ([]bsodMod, int64, int64) {
	limit := fsz
	if limit > 32<<20 {
		limit = 32 << 20
	}
	buf := make([]byte, limit)
	m, err := f.ReadAt(buf, 0)
	if (err != nil && m < 0x1000) || m < 0 {
		return nil, 0, 0
	}
	buf = buf[:m]
	low := uint64(0xFFFF800000000000)
	if arch == "x86" {
		low = 0x80000000
	}
	var best []bsodMod
	bestAt := int64(0)
	for off := 0; off+bsodFlatRec <= len(buf); off += 8 {
		var run []bsodMod
		for o := off; o+bsodFlatRec <= len(buf) && len(run) < 4096; o += bsodFlatRec {
			nrva := binary.LittleEndian.Uint32(buf[o:])
			base := binary.LittleEndian.Uint64(buf[o+0x38:])
			size := binary.LittleEndian.Uint64(buf[o+0x48:])
			if base < low || size < 0x1000 || size > 0x8000000 || nrva == 0 || uint64(nrva)+4 >= uint64(fsz) {
				break
			}
			nm := bsodFlatName(f, uint64(nrva)+4)
			if nm == "" {
				break
			}
			run = append(run, bsodMod{name: filepath.Base(nm), full: nm, base: base, size: size,
				stamp: binary.LittleEndian.Uint32(buf[o+0x88:])})
		}
		if len(run) > len(best) {
			best, bestAt = run, int64(off)
		}
	}
	if len(best) < 20 {
		return nil, 0, 0
	}
	return best, bestAt, bestAt + int64(len(best))*bsodFlatRec
}

// bsodFlatName 读内核路径串（UNICODE_STRING 内联缓冲的写法，读到 NUL 为止）。
// 只认以反斜杠开头的 —— 内核模块路径一定是 \SystemRoot\… 或 \??\C:\… 这种，
// 拿它当校验条件能把扫到的杂音几乎全滤掉。
func bsodFlatName(f *os.File, off uint64) string {
	b := make([]byte, 520)
	n, err := f.ReadAt(b, int64(off))
	if n < 4 {
		return ""
	}
	_ = err
	var sb strings.Builder
	for i := 0; i+1 < n; i += 2 {
		c := binary.LittleEndian.Uint16(b[i:])
		if c == 0 {
			break
		}
		if c < 0x20 || c > 0x7E || sb.Len() > 300 {
			return ""
		}
		sb.WriteRune(rune(c))
	}
	s := sb.String()
	if !strings.HasPrefix(s, `\`) {
		return ""
	}
	return s
}

// bsodStackWindow 拿「BugCheck 代码 + 参数 1~4」这 40 个字节当锚点，在文件里找崩溃栈，
// 返回栈区附近的一段原始字节（锚点前 1KB 起、后 8KB）。tabLo/tabHi 是模块表占的文件区间，
// 那段要整段避开 —— 它和栈区常常挨着，扫进去会把模块表的字段当成返回地址列出一屏假帧。
// 找不到返回 nil。
func bsodStackWindow(f *os.File, fsz int64, d *BsodDump, tabLo, tabHi int64) []byte {
	pat := make([]byte, 40)
	binary.LittleEndian.PutUint64(pat[0:], uint64(d.Code))
	binary.LittleEndian.PutUint64(pat[8:], d.P1)
	binary.LittleEndian.PutUint64(pat[16:], d.P2)
	binary.LittleEndian.PutUint64(pat[24:], d.P3)
	binary.LittleEndian.PutUint64(pat[32:], d.P4)
	const chunk = 1 << 20
	buf := make([]byte, chunk+40)
	for off := int64(0); off < fsz; {
		n, err := f.ReadAt(buf, off)
		if n <= 0 {
			break
		}
		if i := bytes.Index(buf[:n], pat); i >= 0 {
			// 锚点那 40 个字节就是压在栈上的「BugCheck 代码 + 参数 1~4」，它们紧跟在
			// 第一层的返回地址后面 —— 所以 RSP 正好对应锚点前 8 个字节。从 RSP 起读，
			// 别往前读：栈下面没清的旧数据长得也像返回地址，读进来就是假帧。
			a := off + int64(i) - 8
			lo, hi := a, a+0x1000
			if lo < 0 {
				lo = 0
			}
			if hi > fsz {
				hi = fsz
			}
			if tabHi > tabLo && tabLo < hi && tabHi > lo {
				if a < tabLo {
					hi = tabLo
				} else {
					lo = tabHi
				}
			}
			if hi <= lo {
				return nil
			}
			out := make([]byte, hi-lo)
			if k, _ := f.ReadAt(out, lo); k > 0 {
				return out[:k]
			}
			return nil
		}
		next := off + int64(n) - 40
		if next <= off {
			break
		}
		off = next
		if err != nil {
			break
		}
	}
	return nil
}

// bsodCallSite 校验「这个地址是返回地址吗」：往前看一条指令是不是 call。
// E8 rel32 = 直接调用；FF /2（FF 15、FF D0…）= 间接调用。两条都不是就不认。
func bsodCallSite(r *bsodReader, va uint64) (string, bool) {
	b, err := r.readVirtual(va-6, 6)
	if err != nil {
		return "", false
	}
	if b[1] == 0xE8 {
		return "直接调用", true
	}
	if b[0] == 0xFF && b[1]&0x38 == 0x10 {
		return "间接调用", true
	}
	return "", false
}

// bsodModOff 命中地址 → 「模块名+0x偏移」（如 dxgkrnl.sys+0x2651）
func bsodModOff(m bsodMod, va uint64) string {
	if va < m.base {
		return m.name
	}
	return fmt.Sprintf("%s+0x%X", m.name, va-m.base)
}

// bsodModuleFile 模块名 → 磁盘上的驱动文件真实路径。
// 优先用模块链里的 FullDllName（\SystemRoot\… 换成真实系统目录），
// 取不到再按常见位置找；都找不到返回空串（**不猜**，宁可这一列空着）。
func bsodModuleFile(m bsodMod) string {
	var cands []string
	if m.full != "" {
		cands = append(cands, bsodResolvePath(m.full))
	}
	if m.name != "" {
		cands = append(cands,
			filepath.Join(nativeSystem32(), "drivers", m.name),
			filepath.Join(nativeSystem32(), m.name))
	}
	for _, p := range cands {
		if p == "" {
			continue
		}
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			return p
		}
	}
	return ""
}

// bsodResolvePath 把转储里的内核路径（\SystemRoot\…、\??\C:\…）换成当前系统能访问的路径
func bsodResolvePath(p string) string {
	low := strings.ToLower(p)
	switch {
	case strings.HasPrefix(low, `\systemroot\`):
		return filepath.Join(suSystemRoot(), p[len(`\SystemRoot\`):])
	case strings.HasPrefix(low, `\??\`):
		p = p[len(`\??\`):]
		if len(p) > 1 && p[1] == ':' {
			return p
		}
	}
	return ""
}

type bsodMod struct {
	name  string
	full  string // 完整路径（FullDllName，如 \SystemRoot\system32\drivers\nvlddmkm.sys）
	base  uint64
	size  uint64
	stamp uint32 // 模块自带的 PE 时间戳（小转储那张模块表里就有；0 = 没读到）
}

// bsodWalkModules 走 PsLoadedModuleList（KLDR_DATA_TABLE_ENTRY 链表）。
// 偏移在不同构建间有漂移，用启发式：DllBase=首个内核地址，SizeOfImage=基址后第 3 个 8 字节，
// 名字=链表项里可读的 UTF-16 串（取较短者为 BaseDllName）。
func bsodWalkModules(r *bsodReader, listHead uint64) []bsodMod {
	var mods []bsodMod
	seen := map[uint64]bool{}
	cur := listHead
	// 列表头本身是 LIST_ENTRY，第一个条目地址 = listHead 的 Flink
	b, err := r.readVirtual(cur, 8)
	if err != nil {
		return nil
	}
	cur = binary.LittleEndian.Uint64(b)
	for i := 0; i < 512; i++ {
		if cur == 0 || cur == listHead || seen[cur] {
			break
		}
		seen[cur] = true
		ent, err := r.readVirtual(cur, 0x80)
		if err != nil || len(ent) < 0x80 {
			break
		}
		next := binary.LittleEndian.Uint64(ent[0:])
		var m bsodMod
		// DllBase：0x10~0x40 内首个内核地址
		for off := 0x10; off <= 0x40; off += 8 {
			v := binary.LittleEndian.Uint64(ent[off:])
			if v >= 0xFFFF800000000000 && v < 0xFFFFFFFFFFF00000 {
				m.base = v
				// SizeOfImage：常见在 DllBase+0x10
				if off+0x10+8 <= len(ent) {
					sz := binary.LittleEndian.Uint64(ent[off+0x10:])
					if sz > 0x1000 && sz < 0x8000000 {
						m.size = sz
					}
				}
				break
			}
		}
		// BaseDllName：0x28~0x78 内收集所有可读 UTF-16 候选（FullDllName 带路径也合法），
		// 取最短者 —— 短的一定是纯文件名的 BaseDllName
		var cands []string
		for off := 0x28; off+16 <= 0x78; off += 8 {
			length := int(binary.LittleEndian.Uint16(ent[off:]))
			maxl := int(binary.LittleEndian.Uint16(ent[off+2:]))
			bufPtr := binary.LittleEndian.Uint64(ent[off+8:])
			if length <= 0 || length > 520 || maxl < length || bufPtr < 0xFFFF800000000000 {
				continue
			}
			nb, err := r.readVirtual(bufPtr, length+2)
			if err != nil {
				continue
			}
			if name := bsodUtf16(nb[:length]); name != "" {
				cands = append(cands, name)
			}
		}
		for _, c := range cands {
			if m.name == "" || len(c) < len(m.name) {
				m.name = c
			}
			// 带路径的那个是 FullDllName：留作模块完整路径（读版本信息时直接照着找文件）
			if strings.Contains(c, `\`) && len(c) > len(m.full) {
				m.full = c
			}
		}
		m.name = strings.TrimPrefix(m.name, `\??\`)
		if m.name != "" && m.base != 0 && m.size != 0 {
			mods = append(mods, m)
		}
		cur = next
	}
	return mods
}

// ---- 驱动文件元数据（文件描述 / 产品 / 公司 / 版本 / 时间戳）----
//
// 转储文件里没有这些字段 —— BlueScreenView 也是拿模块名去磁盘上找那个 .sys 文件，
// 再读它的版本资源。既然要显示给用户看，就照它同样的办法读，读不到就留空（不编）。

type bsodFileVer struct{ company, desc, product, version string }

var (
	bsodVersionDLL              = windows.NewLazySystemDLL("version.dll")
	procGetFileVersionInfoSizeW = bsodVersionDLL.NewProc("GetFileVersionInfoSizeW")
	procGetFileVersionInfoW     = bsodVersionDLL.NewProc("GetFileVersionInfoW")
	procVerQueryValueW          = bsodVersionDLL.NewProc("VerQueryValueW")
)

// fileVersionInfo 读文件的版本资源（公司 / 描述 / 产品 / 版本）。
// 任何一步失败都返回零值 —— 调用方看到空串就显示「—」，不会误报。
func fileVersionInfo(path string) bsodFileVer {
	var out bsodFileVer
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return out
	}
	size, _, _ := procGetFileVersionInfoSizeW.Call(uintptr(unsafe.Pointer(p)), 0)
	if size == 0 || size > 16<<20 {
		return out
	}
	buf := make([]byte, size)
	if r, _, _ := procGetFileVersionInfoW.Call(uintptr(unsafe.Pointer(p)), 0,
		uintptr(size), uintptr(unsafe.Pointer(&buf[0]))); r == 0 {
		return out
	}
	// 语言/代码页要先问 \VarFileInfo\Translation，不能写死 040904B0（中文系统驱动常是中文资源）
	sub := windows.StringToUTF16Ptr(`\VarFileInfo\Translation`)
	var trans unsafe.Pointer
	var tlen uint32
	if ok, _, _ := procVerQueryValueW.Call(uintptr(unsafe.Pointer(&buf[0])),
		uintptr(unsafe.Pointer(sub)), uintptr(unsafe.Pointer(&trans)), uintptr(unsafe.Pointer(&tlen))); ok == 0 || trans == nil || tlen < 4 {
		return out
	}
	arr := unsafe.Slice((*byte)(trans), 4)
	lang := binary.LittleEndian.Uint16(arr[0:2])
	code := binary.LittleEndian.Uint16(arr[2:4])
	get := func(key string) string {
		q := windows.StringToUTF16Ptr(fmt.Sprintf(`\StringFileInfo\%04X%04X\%s`, lang, code, key))
		var val unsafe.Pointer
		var vlen uint32
		if ok, _, _ := procVerQueryValueW.Call(uintptr(unsafe.Pointer(&buf[0])),
			uintptr(unsafe.Pointer(q)), uintptr(unsafe.Pointer(&val)), uintptr(unsafe.Pointer(&vlen))); ok == 0 || val == nil || vlen == 0 {
			return ""
		}
		return strings.TrimSpace(windows.UTF16PtrToString((*uint16)(val)))
	}
	out.company = get("CompanyName")
	out.desc = get("FileDescription")
	out.product = get("ProductName")
	out.version = get("FileVersion")
	return out
}

// peTimeStamp 读 PE 头里的 TimeDateStamp（驱动编译时间），转成日期。
// 蓝屏工具都是拿它当「文件时间戳」列 —— 比文件系统时间可靠（拷来拷去不改它）。
// peTimeStamp 本地文件的 PE TimeDateStamp → 日期字符串（读不到 / 是构建哈希就留空）
func peTimeStamp(path string) string {
	return bsodStampText(peStampRaw(path))
}

// peStampRaw 读 PE 头的 TimeDateStamp（磁盘文件）。0 = 读不到。
func peStampRaw(path string) uint32 {
	f, err := os.Open(path)
	if err != nil {
		return 0
	}
	defer f.Close()
	h := make([]byte, 0x400)
	if n, err := f.ReadAt(h, 0); err != nil && n < 0x100 {
		return 0
	}
	return peStampFrom(h)
}

// peStampFrom 从一段已读进来的 PE 头里取 TimeDateStamp（磁盘文件和转储里的镜像共用）
func peStampFrom(h []byte) uint32 {
	if len(h) < 0x100 || string(h[0:2]) != "MZ" {
		return 0
	}
	off := binary.LittleEndian.Uint32(h[0x3C:])
	if off == 0 || int(off)+24 > len(h) || string(h[off:off+4]) != "PE\x00\x00" {
		return 0
	}
	return binary.LittleEndian.Uint32(h[off+8:])
}

// bsodStampText TimeDateStamp → 日期字符串。
// ⚠️ Win10 1903 起微软把系统自带驱动的 PE 时间戳换成了「可复现构建」哈希，
// 解出来是 2069 年这种未来日期 —— 那不是时间，宁可留空也别显示给用户看。
func bsodStampText(ts uint32) string {
	if ts == 0 {
		return ""
	}
	tm := time.Unix(int64(ts), 0)
	if tm.Year() < 1990 || tm.Year() > time.Now().Year()+1 {
		return ""
	}
	return tm.Format("2006-01-02 15:04")
}

// bsodImageStamp 从转储内存里读模块镜像自带的 PE 时间戳 —— 这才是「出事那台机器」
// 上那个驱动的时间戳。看别人的转储时，本机磁盘上的同名文件多半不是同一个版本，
// 只有拿这个跟本地文件对得上方能确认（对不上就在界面上标出来，不拿本地的冒充）。
func bsodImageStamp(r *bsodReader, base uint64) uint32 {
	b, err := r.readVirtual(base, 0x400)
	if err != nil || len(b) < 0x400 {
		return 0
	}
	return peStampFrom(b)
}

func bsodUtf16(b []byte) string {
	var sb strings.Builder
	for i := 0; i+1 < len(b); i += 2 {
		c := binary.LittleEndian.Uint16(b[i:])
		if c == 0 {
			break
		}
		if c < 0x20 || c > 0x7E {
			return ""
		}
		sb.WriteRune(rune(c))
	}
	return sb.String()
}
