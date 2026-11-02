package main

// ============================================================================
// 3z9wOpt v1.1 · 磁盘空间（绑定层）
//
// 引擎 = mftscan.go（NTFS 主文件表直读，几秒出全盘索引）。
// 三个视图（目录分析 / 大文件 / 重复文件）共用**一次**扫描的索引。
// 删除一律进回收站（SHFileOperation + FOF_ALLOWUNDO），误删可还原。
//
// 2026-10-11：这一页从老版本（v2）**按功能 100% 移植** —— 重复文件判定
// （同大小分组 → 抽样哈希 → 全量 MD5）、删除、定位的行为与参数一律照搬，
// 只换了盘列表来源：沿用本版 GetDiskList（带卷标名与固态标记，首页也在用），
// 没有再搬老版 GetSpaceDrives，免得两套盘列表各说各话。
//
// 事件（前端 onGo 监听）：
//   space:progress  pct int                   扫描进度
//   space:done      {ok,drive,files,bytes,dupFree,dupGroups} 扫描完成（索引已就位）
//   dup:progress    {pct,stage}               重复文件比对进度
//   dup:done        {ok,groups,freed,err}     重复文件结果
// ============================================================================

import (
	"crypto/md5"
	"encoding/binary"
	"fmt"
	"os"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf16"
	"unsafe"

	"golang.org/x/sys/windows"
)

// DiskInfo 磁盘卡（首页与本页共用；含卷标名与固态标记）
type DiskInfo struct {
	Drive string `json:"drive"` // "C:"
	Name  string `json:"name"`  // "系统盘 (C:)" / "软件 (D:)"
	Label string `json:"label"` // 卷标（可能为空）
	Total int64  `json:"total"` // 总容量
	Free  int64  `json:"free"`  // 可用空间
	SSD   bool   `json:"ssd"`   // 寻道惩罚判定：true=固态
	Ntfs  bool   `json:"ntfs"`  // 只有 NTFS 才能走 MFT 快扫
}

// ---------- 盘列表 ----------

// diskIsSSDByDrive 用「寻道惩罚」判断固态：IOCTL_STORAGE_QUERY_PROPERTY +
// StorageDeviceSeekPenaltyProperty。读不到就按 false（机械）—— 磁盘卡上只是少一个「固态」标，
// 不影响任何功能，所以这里不纠结。
func diskIsSSDByDrive(drive string) bool {
	h, err := windows.CreateFile(windows.StringToUTF16Ptr(`\\.\`+drive),
		windows.GENERIC_READ, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE, nil, windows.OPEN_EXISTING, 0, 0)
	if err != nil {
		return false
	}
	defer windows.CloseHandle(h)

	const ioctlStorageQueryProperty = 0x002D1400
	const storageDeviceSeekPenaltyProperty = 7
	type propQuery struct {
		PropertyId uint32
		QueryType  uint32
	}
	type seekPenaltyDesc struct {
		Version           uint32
		Size              uint32
		IncursSeekPenalty byte
		_                 [3]byte
	}
	q := propQuery{PropertyId: storageDeviceSeekPenaltyProperty, QueryType: 0}
	var out seekPenaltyDesc
	var ret uint32
	if err := windows.DeviceIoControl(h, ioctlStorageQueryProperty,
		(*byte)(unsafe.Pointer(&q)), uint32(unsafe.Sizeof(q)),
		(*byte)(unsafe.Pointer(&out)), uint32(unsafe.Sizeof(out)), &ret, nil); err != nil {
		return false
	}
	return out.IncursSeekPenalty == 0
}

// GetDiskList 固定盘列表（含卷标 / 容量 / 固态 / 是否 NTFS）
func (a *App) GetDiskList() []DiskInfo {
	out := []DiskInfo{}
	sysDrive := strings.ToUpper(strings.TrimRight(os.Getenv("SystemDrive"), `\`)) // 形如 "C:"
	for c := 'A'; c <= 'Z'; c++ {
		d := string(c) + ":"
		root := d + `\`
		p := windows.StringToUTF16Ptr(root)
		// x/sys 的 GetDriveType 只返回盘符类型（不返回 error）
		if windows.GetDriveType(p) != windows.DRIVE_FIXED {
			continue
		}
		var volName, fsName [128]uint16
		var serial, maxLen, fsFlags uint32
		ntfs := false
		if err := windows.GetVolumeInformation(p, &volName[0], uint32(len(volName)),
			&serial, &maxLen, &fsFlags, &fsName[0], uint32(len(fsName))); err == nil {
			ntfs = strings.EqualFold(windows.UTF16ToString(fsName[:]), "NTFS")
		}
		var freeAvail, total, totalFree uint64
		if err := windows.GetDiskFreeSpaceEx(p, &freeAvail, &total, &totalFree); err != nil {
			continue
		}
		label := windows.UTF16ToString(volName[:])
		name := d
		switch {
		case d == sysDrive:
			name = "系统盘 (" + d + ")"
		case label != "":
			name = label + " (" + d + ")"
		default:
			name = "本地磁盘 (" + d + ")"
		}
		out = append(out, DiskInfo{
			Drive: d, Name: name, Label: label,
			Total: int64(total), Free: int64(freeAvail),
			SSD: diskIsSSDByDrive(d), Ntfs: ntfs,
		})
	}
	return out
}

// ---------- 扫描（异步 + 可停止） ----------

// SpaceScanAsync 异步扫描：进度走 space:progress，完成发 space:done（含重复文件概览）。
// 已经在扫就忽略（前端按钮此时也是禁用的）。
func (a *App) SpaceScanAsync(drive string) {
	d := strings.ToUpper(strings.TrimSpace(drive))
	if d == "" {
		return
	}
	if !strings.HasSuffix(d, ":") {
		d += ":"
	}
	spaceMu.Lock()
	if spaceScanning {
		spaceMu.Unlock()
		return
	}
	spaceScanning = true
	spaceStop = false
	spaceApp = a
	spaceMu.Unlock()

	cleanTrace("SpaceScanAsync " + d)
	go func() {
		idx, err := ScanSpaceMft(d, func(pct int) {
			spaceMu.Lock()
			stopped := spaceStop
			spaceMu.Unlock()
			if !stopped {
				a.emit("space:progress", pct)
			}
		})
		spaceMu.Lock()
		spaceScanning = false
		stopped := spaceStop
		if err == nil && !stopped {
			spaceIdx = idx
		}
		spaceMu.Unlock()

		if stopped {
			a.emit("space:done", map[string]any{"ok": false, "err": "已停止扫描"})
			cleanTrace("SpaceScanAsync stopped")
			return
		}
		if err != nil {
			a.emit("space:done", map[string]any{"ok": false, "err": err.Error()})
			cleanTrace("SpaceScanAsync fail: " + err.Error())
			return
		}
		// 顺手把重复文件概览也预热（同大小分组，纯内存，秒级）
		dupFree, dupGroups := idx.dupPreview(32 << 20)
		cleanTrace(fmt.Sprintf("SpaceScanAsync done %s files=%d bytes=%d dupGroups=%d", d, idx.total, idx.bytes, dupGroups))
		a.emit("space:done", map[string]any{
			"ok": true, "drive": idx.drive, "files": idx.total, "bytes": idx.bytes,
			"dupFree": dupFree, "dupGroups": dupGroups,
		})
	}()
}

// SpaceStopScan 停止本次扫描（引擎按 spaceStop 就地返回）
func (a *App) SpaceStopScan() {
	spaceMu.Lock()
	spaceStop = true
	spaceMu.Unlock()
	cleanTrace("SpaceStopScan: 用户请求停止")
}

// idxReady 取当前索引；扫描中 / 没扫过都如实报错，前端照着提示
func (a *App) idxReady() (*mftIndex, error) {
	spaceMu.Lock()
	defer spaceMu.Unlock()
	if spaceScanning {
		return nil, fmt.Errorf("扫描进行中，请稍候")
	}
	if spaceIdx == nil {
		return nil, fmt.Errorf("尚未扫描，请先选一个磁盘开始分析")
	}
	return spaceIdx, nil
}

// ---------- 下钻 / 大文件 ----------

// SpaceChildren 目录钻取：直接子项（含子树聚合大小）
func (a *App) SpaceChildren(frn uint64) ([]SpRow, error) {
	idx, err := a.idxReady()
	if err != nil {
		return nil, err
	}
	return idx.SpaceChildren(frn), nil
}

// SpaceCrumbs 面包屑链（根目录 → 当前目录）
func (a *App) SpaceCrumbs(frn uint64) ([]SpRow, error) {
	idx, err := a.idxReady()
	if err != nil {
		return nil, err
	}
	return idx.SpaceCrumbs(frn), nil
}

// SpaceTopFiles 全盘大文件 Top（前端给阈值与条数，这里只做上限保护）
func (a *App) SpaceTopFiles(minSize int64, limit int) ([]SpRow, error) {
	idx, err := a.idxReady()
	if err != nil {
		return nil, err
	}
	if limit <= 0 || limit > 2000 {
		limit = 500
	}
	return idx.SpaceTopFiles(minSize, limit), nil
}

// SpacePath frn → 完整路径（打开 / 删除前都要先换成真路径）
func (a *App) SpacePath(frn uint64) (string, error) {
	idx, err := a.idxReady()
	if err != nil {
		return "", err
	}
	return idx.fullPath(frn), nil
}

// ---------- 重复文件 ----------

// DupGroup 一组内容完全相同的文件（全量哈希确认过才会进这里）
type DupGroup struct {
	Size   int64    `json:"size"`
	Hash   string   `json:"hash"`
	Paths  []string `json:"paths"`
	IsFull bool     `json:"isFull"` // true=全量哈希确认 false=仅快速哈希疑似
}

func (g *DupGroup) files() int { return len(g.Paths) }

var (
	dupMu   sync.Mutex
	dupRuns bool
	dupStop bool
)

// SpaceDupScanAsync 重复文件扫描：同大小分组 → 抽样哈希 → 全量 MD5 确认
func (a *App) SpaceDupScanAsync(minSize int64) {
	idx, err := a.idxReady()
	if err != nil {
		a.emit("dup:done", map[string]any{"ok": false, "err": err.Error()})
		return
	}
	dupMu.Lock()
	if dupRuns {
		dupMu.Unlock()
		return
	}
	dupRuns, dupStop = true, false
	dupMu.Unlock()
	go func() {
		defer func() { dupMu.Lock(); dupRuns = false; dupMu.Unlock() }()
		groups, freed, err := idx.findDuplicates(minSize, func(pct int, stage string) {
			dupMu.Lock()
			stopped := dupStop
			dupMu.Unlock()
			if !stopped {
				a.emit("dup:progress", map[string]any{"pct": pct, "stage": stage})
			}
		})
		if err != nil {
			a.emit("dup:done", map[string]any{"ok": false, "err": err.Error()})
			return
		}
		a.emit("dup:done", map[string]any{"ok": true, "groups": groups, "freed": freed})
	}()
}

// SpaceDupStop 停止重复文件扫描
func (a *App) SpaceDupStop() {
	dupMu.Lock()
	dupStop = true
	dupMu.Unlock()
}

// dupPreview 概览（扫描完成时调用，只做同大小分组，不读文件内容）
func (idx *mftIndex) dupPreview(minSize int64) (int64, int) {
	bySize := map[int64][]uint64{}
	for frn, n := range idx.nodes {
		if !n.dir && n.size >= minSize && frn >= 16 {
			bySize[n.size] = append(bySize[n.size], frn)
		}
	}
	var freed int64
	groups := 0
	for sz, l := range bySize {
		if len(l) >= 2 {
			freed += sz * int64(len(l)-1)
			groups++
		}
	}
	return freed, groups
}

// findDuplicates 完整重复检测（老版本原样移植：抽样哈希先筛，再全量 MD5 定案）
func (idx *mftIndex) findDuplicates(minSize int64, progress func(int, string)) ([]DupGroup, int64, error) {
	bySize := map[int64][]uint64{}
	var candidates int64
	for frn, n := range idx.nodes {
		if !n.dir && n.size >= minSize && frn >= 16 {
			bySize[n.size] = append(bySize[n.size], frn)
		}
	}
	var sizes []int64
	for sz, l := range bySize {
		if len(l) >= 2 {
			sizes = append(sizes, sz)
			candidates += int64(len(l))
		}
	}
	sort.Slice(sizes, func(i, j int) bool { return sizes[i] > sizes[j] })
	var done int64
	report := func() {
		if progress != nil && candidates > 0 {
			progress(int(done*100/candidates), "hashing")
		}
	}
	var out []DupGroup
	var freed int64
	buf := make([]byte, 1<<20)
	for _, sz := range sizes {
		// 快速哈希分组
		type qh struct{ head, tail, length uint64 }
		quick := map[qh][]uint64{}
		for _, frn := range bySize[sz] {
			done++
			if done%64 == 0 {
				report()
			}
			p := idx.fullPath(frn)
			h, err := quickHash(p, sz, buf)
			if err != nil {
				continue
			}
			quick[h] = append(quick[h], frn)
		}
		for _, l := range quick {
			if len(l) < 2 {
				continue
			}
			// 全量 MD5 确认
			full := map[string][]uint64{}
			for _, frn := range l {
				done++
				report()
				p := idx.fullPath(frn)
				sum, err := fullMD5(p, buf)
				if err != nil {
					continue
				}
				full[sum] = append(full[sum], frn)
			}
			for sum, l2 := range full {
				if len(l2) < 2 {
					continue
				}
				paths := make([]string, 0, len(l2))
				for _, frn := range l2 {
					paths = append(paths, idx.fullPath(frn))
				}
				sort.Strings(paths)
				out = append(out, DupGroup{Size: sz, Hash: sum, Paths: paths, IsFull: true})
				freed += sz * int64(len(l2)-1)
			}
		}
		dupMu.Lock()
		stopped := dupStop
		dupMu.Unlock()
		if stopped {
			return nil, 0, fmt.Errorf("已停止")
		}
	}
	report()
	sort.Slice(out, func(i, j int) bool { return out[i].Size*int64(out[i].files()) > out[j].Size*int64(out[j].files()) })
	if len(out) > 200 {
		out = out[:200]
	}
	return out, freed, nil
}

// quickHash 抽样哈希：头 256KB + 尾 256KB + 长度
func quickHash(path string, size int64, buf []byte) (qh struct{ head, tail, length uint64 }, err error) {
	f, err := os.Open(path)
	if err != nil {
		return qh, err
	}
	defer f.Close()
	qh.length = uint64(size)
	sample := int64(256 * 1024)
	if size < sample {
		sample = size
	}
	if sample > 0 {
		if _, err = f.ReadAt(buf[:sample], 0); err != nil {
			return qh, err
		}
		qh.head = binary.LittleEndian.Uint64(fnv1a(buf[:sample], 0))
	}
	if size > sample*2 {
		tailOff := size - sample
		if _, err = f.ReadAt(buf[:sample], tailOff); err != nil {
			return qh, err
		}
		qh.tail = binary.LittleEndian.Uint64(fnv1a(buf[:sample], 0))
	}
	return qh, nil
}

func fnv1a(b []byte, seed uint64) []byte {
	h := uint64(14695981039346656037) ^ seed
	for _, c := range b {
		h = (h ^ uint64(c)) * 1099511628211
	}
	var out [8]byte
	binary.LittleEndian.PutUint64(out[:], h)
	return out[:]
}

func fullMD5(path string, buf []byte) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := md5.New()
	for {
		n, err := f.Read(buf)
		if n > 0 {
			h.Write(buf[:n])
		}
		if err != nil {
			break
		}
	}
	return fmt.Sprintf("%x", h.Sum(nil)), nil
}

// ---------- 删除（回收站）与打开 ----------

// SpaceDelResult 一条删除结果（逐条独立，互不影响）
type SpaceDelResult struct {
	Path string `json:"path"`
	Ok   bool   `json:"ok"`
	Err  string `json:"err"`
}

// SpaceDelete 批量删除到回收站（每个路径独立 SHFileOperation，互不影响）
func (a *App) SpaceDelete(paths []string) []SpaceDelResult {
	out := []SpaceDelResult{}
	for _, p := range paths {
		err := recycleDelete(p)
		out = append(out, SpaceDelResult{Path: p, Ok: err == nil, Err: errStr(err)})
	}
	return out
}

// recycleDelete SHFileOperation 进回收站（FOF_ALLOWUNDO：误删可还原）
//
// 2026-11-02 修（用户反馈「重复文件删不掉，最后报调用超时」）：
// 原来这行写的是 windows.StringToUTF16Ptr(path + "\x00")。
// x/sys/windows 的 UTF16FromString 会把内嵌的 NUL 判成非法输入并 panic
// （"windows: string with NUL passed to StringToUTF16"）。panic 在 Wails 的
// dispatcher 里被 recover 成 error，但那条给前端的回调是空的 —— 前端拿不到
// callbackID，Promise 永远悬着，15 秒后判「调用超时」。也就是：
// **SHFileOperation 一次都没被调用过**，任何文件（100MB 的单文件也一样）
// 都删不掉，报错信息还全指向"超时"这个假象。
// 正确做法：用 utf16 自己编码，按 shell 要求的「双 NUL 结尾列表」构造。
// 实测（100MB 文件，D 盘进回收站）：修复前必 panic；修复后 0.06s 完成，返回 0。
func recycleDelete(path string) error {
	src := pathListUTF16([]string{path})
	op := &shFileOp{
		wFunc:  3, // FO_DELETE
		pFrom:  uintptr(unsafe.Pointer(src)),
		fFlags: 0x0040 | 0x0010 | 0x0004 | 0x0400, // ALLOWUNDO | NOCONFIRMATION | SILENT | NOERRORUI
	}
	h, _, _ := shOpProc.Call(uintptr(unsafe.Pointer(op)))
	// pFrom 存的是 uintptr，GC 看不见这个引用；src 必须在调用期间保活，
	// 否则极端情况下（本函数极短）缓冲区可能被回收成悬空指针。
	runtime.KeepAlive(src)
	if h != 0 {
		return fmt.Errorf("%s（错误码 0x%x）", shDelErrText(h), h)
	}
	return nil
}

// pathListUTF16 构造 SHFileOperationW 要求的 UTF-16 双 NUL 结尾列表缓冲区。
// ⚠️ 千万别改成 windows.StringToUTF16Ptr(p + "\x00") —— 那条路会 panic，见上。
// 多路径时同样适用（一次 shell 调用删一批，比逐个调用少一层会话开销）。
func pathListUTF16(paths []string) *uint16 {
	var buf []uint16
	for _, p := range paths {
		buf = append(buf, utf16.Encode([]rune(p))...)
		buf = append(buf, 0) // 每个路径以 \0 结尾
	}
	buf = append(buf, 0) // 整张列表再补一个 \0（双 NUL 结尾）
	return &buf[0]
}

// shDelErrText 把 SHFileOperation 的错误码翻成人话 —— 以前只显示 0x20 这种
// 十六进制，用户和反馈渠道都看不懂，白丢一次排查线索。
func shDelErrText(h uintptr) string {
	switch h {
	case 0x20:
		return "删除失败：文件被占用或没有权限"
	case 0x21:
		return "删除失败：路径层级太深"
	case 0x71:
		return "删除失败：源和目标指向同一个文件"
	case 0x74:
		return "删除失败：不能删除根目录"
	case 0x78:
		return "删除失败：文件名太长"
	case 0x7C:
		return "删除失败：路径无效"
	case 0x7D:
		return "删除失败：路径无效"
	case 0x402:
		return "删除失败：未知错误"
	}
	return "删除失败"
}

type shFileOp struct {
	hwnd                  uintptr
	wFunc                 uint32
	pFrom, pTo            uintptr
	fFlags                uint16
	fAnyOperationsAborted int32
	hNameMappings         uintptr
	lpszProgressTitle     uintptr
}

var shOpProc = windows.NewLazySystemDLL("shell32.dll").NewProc("SHFileOperationW")

// SpaceOpen 打开文件所在位置（资源管理器选中该文件）
// 注意：explorer 的 /select 参数必须是一个整体 token（/select,"路径"），
// 用 exec.Command 拆成两个参数会被各自加引号，explorer 解析失败表现为"点了没反应"。
// 所以走 ShellExecuteW 传参；它再失败才退回 cmd start。
func (a *App) SpaceOpen(path string) {
	p := strings.TrimSuffix(path, `\`)
	if err := windows.ShellExecute(0,
		windows.StringToUTF16Ptr("open"),
		windows.StringToUTF16Ptr("explorer.exe"),
		windows.StringToUTF16Ptr(`/select,"`+p+`"`),
		nil, windows.SW_SHOWNORMAL); err != nil {
		_ = runHidden(10*time.Second, "cmd", "/c", "start", "", "explorer.exe", `/select,"`+p+`"`)
	}
}
