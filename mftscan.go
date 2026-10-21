package main

// mftscan.go —— MFT 全盘索引引擎（2026-09-20 磁盘空间模块核心）
// 直接解析 NTFS 主文件表（$MFT），每条记录自带文件名 / 父目录 FRN / 文件大小 / 修改时间，
// 不做文件系统递归遍历 —— 百万文件全盘索引秒级完成（对标 WizTree 的做法）。
// 需要管理员权限（卷句柄 GENERIC_READ）。

import (
	"encoding/binary"
	"fmt"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

const (
	frRoot          = uint64(5) // 根目录的 MFT 记录号
	frMetaMask      = uint64(0x0000FFFFFFFFFFFF)
	mftBufSize      = 64 << 20 // 每次 I/O 最多读 64MB
	fsctlGetNtfsVol = 0x00090064
)

// spNode MFT 索引节点
type spNode struct {
	name   string
	parent uint64
	size   int64  // 文件大小；目录为子树聚合大小
	agg    int64  // 子树聚合大小（目录）
	nFiles int64  // 子树文件数（目录）
	mtime  int64  // FILETIME（100ns）
	dir    bool
}

// mftIndex 一次扫描的完整索引
type mftIndex struct {
	drive string
	nodes map[uint64]*spNode
	kids  map[uint64][]uint64 // parent -> children FRN
	total int64               // 文件总数
	bytes int64               // 已用空间合计
}

var (
	spaceMu       sync.Mutex
	spaceIdx      *mftIndex
	spaceScanning bool
	spaceStop     bool
	spaceApp      *App // 用于 emit 事件
)

// ---------- Win32 基础 ----------

func openVolume(drive string) (windows.Handle, error) {
	d := strings.TrimRight(strings.ToUpper(drive), `:`)
	p := `\\.\` + d + ":"
	h, err := windows.CreateFile(windows.StringToUTF16Ptr(p),
		windows.GENERIC_READ, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil, windows.OPEN_EXISTING, 0, 0)
	return h, err
}

type ntfsVolData struct {
	serial                     int64
	sectors, totalClus, freeCl int64
	reserved                   int64
	bytesSector                uint32
	bytesCluster               uint32
	bytesRecord                uint32
	clusPerRecord              int32
	mftValidLen                int64
	mftStartLcn                int64
	_                          int64
	_                          int64
	_                          int64
	_                          int64
}

func readVolData(h windows.Handle) (*ntfsVolData, error) {
	var buf ntfsVolData
	var ret uint32
	err := windows.DeviceIoControl(h, fsctlGetNtfsVol, nil, 0,
		(*byte)(unsafe.Pointer(&buf)), uint32(unsafe.Sizeof(buf)), &ret, nil)
	if err != nil {
		return nil, err
	}
	return &buf, nil
}

func volReadAt(h windows.Handle, off int64, buf []byte) error {
	var done uint32
	hi := int32(uint32(off >> 32))
		if nlo, err := windows.SetFilePointer(h, int32(uint32(off)), &hi, windows.FILE_BEGIN); err != nil {
			return err
		} else if uint64(nlo)|uint64(uint32(hi))<<32 != uint64(off) {
			return fmt.Errorf("卷内寻址失败 @ %d", off)
		}
	return windows.ReadFile(h, buf, &done, nil)
}

// ---------- 记录解析 ----------

// fixupMftRecord 应用更新序列号（update sequence array），校验/修复每个扇区尾 2 字节
func fixupMftRecord(rec []byte) bool {
	if len(rec) < 48 || string(rec[:4]) != "FILE" {
		return false
	}
	usaOff := int(binary.LittleEndian.Uint16(rec[4:6]))
	usaCnt := int(binary.LittleEndian.Uint16(rec[6:8]))
	if usaOff <= 0 || usaCnt <= 1 || usaOff+usaCnt*2 > len(rec) {
		return usaOff > 0 // 无 fixup 的小记录直接放行
	}
	sec := 512
	for i := 1; i < usaCnt; i++ {
		end := sec*i - 2
		if end+2 > len(rec) {
			return false
		}
		// 记录末尾存的正确值覆盖扇区尾（磁盘写入时被 USA 占位）
		rec[end], rec[end+1] = rec[usaOff+i*2], rec[usaOff+i*2+1]
	}
	return true
}

// walkAttrs 遍历记录里的属性
func walkAttrs(rec []byte, at func(typ uint32, nonRes bool, attr []byte)) {
	if len(rec) < 0x20 {
		return
	}
	off := int(binary.LittleEndian.Uint16(rec[0x14:0x16]))
	for off+16 <= len(rec) {
		typ := binary.LittleEndian.Uint32(rec[off:])
		if typ == 0xFFFFFFFF {
			break
		}
		alen := int(binary.LittleEndian.Uint32(rec[off+4:]))
		if alen < 16 || off+alen > len(rec) {
			break
		}
		attr := rec[off : off+alen]
		nonRes := attr[8] != 0
		at(typ, nonRes, attr)
		off += alen
	}
}

// parseMftRecord 解析单条记录，有效返回 (frn, node)
func parseMftRecord(rec []byte, frn uint64) (*spNode, bool) {
	if !fixupMftRecord(rec) {
		return nil, false
	}
	var n spNode
	n.parent = frn // 兜底：找不到 FILE_NAME 时指向自己（会被丢弃）
	hasName := false
	hasData := false
	walkAttrs(rec, func(typ uint32, nonRes bool, attr []byte) {
		switch typ {
		case 0x30: // $FILE_NAME（恒为 resident）
			if nonRes {
				return
			}
			vOff := int(binary.LittleEndian.Uint16(attr[20:22]))
			vLen := int(binary.LittleEndian.Uint32(attr[16:20]))
			if vOff <= 0 || vOff+vLen > len(attr) || vLen < 66 {
				return
			}
			v := attr[vOff : vOff+vLen]
			ns := v[65]
			if hasName && ns == 2 { // 已有更好名字，跳过 DOS 短名
				return
			}
			parent := binary.LittleEndian.Uint64(v[0:8]) & frMetaMask
			n.parent = parent
			n.mtime = int64(binary.LittleEndian.Uint64(v[16:24]))
			nameLen := int(v[64])
			if 66+nameLen*2 > vLen {
				return
			}
			raw := v[66 : 66+nameLen*2]
			n.name = strings.TrimSuffix(utf16ToString(raw), "\x00")
			hasName = true
		case 0x80: // $DATA（无名数据流 = 文件内容）
			if attr[9] != 0 { // 有名字的流（ADS）不算内容
				return
			}
			hasData = true
			if nonRes {
				// 非常驻头：0x20 运行列表偏移，0x28 已分配，0x30 实际大小
				if len(attr) >= 0x38 {
					n.size = int64(binary.LittleEndian.Uint64(attr[0x30:0x38]))
				}
			} else if len(attr) >= 20 {
				n.size = int64(binary.LittleEndian.Uint32(attr[16:20]))
			}
		}
	})
	if !hasName || n.name == "" {
		return nil, false
	}
	n.dir = !hasData
	return &n, true
}

func utf16ToString(b []byte) string {
	u := make([]uint16, len(b)/2)
	for i := range u {
		u[i] = binary.LittleEndian.Uint16(b[i*2:])
	}
	return syscall.UTF16ToString(u)
}

// ---------- $MFT 运行列表 ----------

// parseRunlist 解析非常驻属性的 cluster run 列表
func parseRunlist(b []byte) []struct{ lcn, len int64 } {
	var runs []struct{ lcn, len int64 }
	var lcn int64
	i := 0
	for i < len(b) {
		hdr := b[i]
		if hdr == 0 {
			break
		}
		i++
		lenBytes := int(hdr & 0x0F)
		offBytes := int(hdr >> 4)
		if lenBytes == 0 || lenBytes > 8 || i+lenBytes > len(b) {
			break
		}
		var rl int64
		for j := 0; j < lenBytes; j++ {
			rl |= int64(b[i+j]) << (8 * j)
		}
		i += lenBytes
		var off int64
		if offBytes > 0 {
			if i+offBytes > len(b) {
				break
			}
			off = int64(int8(b[i+offBytes-1])) // 符号扩展
			for j := offBytes - 2; j >= 0; j-- {
				off = off<<8 | int64(b[i+j])
			}
			i += offBytes
		}
		lcn += off
		runs = append(runs, struct{ lcn, len int64 }{lcn, rl})
	}
	return runs
}

// readMftExtents 从记录 0 读出 $MFT 自身的物理运行列表
func readMftExtents(h windows.Handle, vd *ntfsVolData) ([]struct{ lcn, len int64 }, error) {
	recSize := int(vd.bytesRecord)
	if recSize <= 0 {
		recSize = 1024
	}
	buf := make([]byte, recSize)
	if err := volReadAt(h, vd.mftStartLcn*int64(vd.bytesCluster), buf); err != nil {
		return nil, fmt.Errorf("读取 $MFT 记录 0 失败: %w", err)
	}
	if !fixupMftRecord(buf) {
		return nil, fmt.Errorf("$MFT 记录 0 校验失败")
	}
	var runs []struct{ lcn, len int64 }
	found := false
	walkAttrs(buf, func(typ uint32, nonRes bool, attr []byte) {
		if typ != 0x80 || !nonRes || found {
			return
		}
		rlOff := int(binary.LittleEndian.Uint16(attr[0x20:0x22]))
		if rlOff <= 0 || rlOff >= len(attr) {
			return
		}
		runs = parseRunlist(attr[rlOff:])
		found = true
	})
	if !found {
		return nil, fmt.Errorf("$MFT 记录 0 中未找到 $DATA")
	}
	return runs, nil
}

// ---------- 主扫描 ----------

// ScanSpaceMft 扫描一个 NTFS 盘，构建完整索引。progress: 0-100。
func ScanSpaceMft(drive string, progress func(int)) (*mftIndex, error) {
	t0 := time.Now()
	h, err := openVolume(drive)
	if err != nil {
		return nil, fmt.Errorf("打开卷 %s 失败（需要管理员权限）: %w", drive, err)
	}
	defer windows.CloseHandle(h)
	vd, err := readVolData(h)
	if err != nil {
		return nil, fmt.Errorf("读取 NTFS 卷信息失败: %w", err)
	}
	recSize := int(vd.bytesRecord)
	if recSize <= 0 {
		recSize = 1024
	}
	extents, err := readMftExtents(h, vd)
	if err != nil {
		return nil, err
	}

	idx := &mftIndex{
		drive: strings.ToUpper(strings.TrimSuffix(drive, ":")),
		nodes: map[uint64]*spNode{},
		kids:  map[uint64][]uint64{},
	}
	idx.nodes[frRoot] = &spNode{name: idx.drive + ":", parent: frRoot, dir: true}

	total := vd.mftValidLen
	readBuf := make([]byte, mftBufSize)
	var consumed int64
	for _, ext := range extents {
		need := ext.len * int64(vd.bytesCluster)
		if consumed+need > total {
			need = total - consumed // 最后一段可能不满
		}
		if need <= 0 {
			break
		}
		abs := ext.lcn * int64(vd.bytesCluster)
		for done := int64(0); done < need; {
			chunk := need - done
			if chunk > mftBufSize {
				chunk = mftBufSize
			}
			buf := readBuf[:chunk-chunk%int64(recSize)]
			if err := volReadAt(h, abs+done, buf); err != nil {
				return nil, fmt.Errorf("读取 $MFT 失败 @ %d: %w", abs+done, err)
			}
			for off := 0; off+recSize <= len(buf); off += recSize {
				frn := uint64((consumed + done + int64(off)) / int64(recSize))
				if frn < 16 && frn != frRoot {
					continue // 系统元文件（根目录另行播种）
				}
				if frn == frRoot {
					continue
				}
				n, ok := parseMftRecord(buf[off:off+recSize], frn)
				if !ok {
					continue
				}
				idx.nodes[frn] = n
				idx.kids[n.parent] = append(idx.kids[n.parent], frn)
				if !n.dir {
					idx.total++
					idx.bytes += n.size
				}
			}
			if len(buf) == 0 {
				// need 不是记录大小的整数倍时，末段会算出 0 长度缓冲 ——
				// 不 break 的话 done 永远不前进，这是死循环（换台机器换块盘就会撞上）
				break
			}
			done += int64(len(buf))
			if progress != nil && total > 0 {
				progress(int((consumed + done) * 100 / total))
			}
		}
		consumed += ext.len * int64(vd.bytesCluster)
		if consumed >= total {
			break
		}
	}
	// 逐层聚合子树大小（自底向上）
	computeAgg(idx, frRoot)
	if progress != nil {
		progress(100) // 调用方可能不关心进度（传 nil），原来这里会直接 panic
	}
	fmt.Printf("[space] %s scan done: %d nodes, %d files, %.1fGB, %s\n",
		idx.drive, len(idx.nodes), idx.total, float64(idx.bytes)/1e9, time.Since(t0).Round(time.Millisecond))
	return idx, nil
}

// computeAgg 迭代式后序聚合（防深目录递归爆栈）
func computeAgg(idx *mftIndex, root uint64) {
	type ent struct {
		frn  uint64
		deep bool
	}
	stack := []ent{{root, false}}
	for len(stack) > 0 {
		e := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		n := idx.nodes[e.frn]
		if n == nil {
			continue
		}
		if !n.dir { // 文件：直接记自身
			n.agg, n.nFiles = n.size, 1
			continue
		}
		if !e.deep {
			stack = append(stack, ent{e.frn, true})
			for _, k := range idx.kids[e.frn] {
				if kn := idx.nodes[k]; kn != nil && kn.dir {
					stack = append(stack, ent{k, false})
				}
			}
		} else {
			var agg, nf int64
			for _, k := range idx.kids[e.frn] {
				kn := idx.nodes[k]
				if kn == nil {
					continue
				}
				if kn.dir {
					agg += kn.agg
					nf += kn.nFiles
				} else { // 文件节点没有入栈，直接取自身
					agg += kn.size
					nf++
				}
			}
			n.agg, n.nFiles = agg, nf
		}
	}
}

// ---------- 查询 ----------

// fullPath 自底向上拼路径
func (idx *mftIndex) fullPath(frn uint64) string {
	var parts []string
	cur := frn
	for cur != frRoot {
		n := idx.nodes[cur]
		if n == nil {
			break
		}
		parts = append(parts, n.name)
		cur = n.parent
		if len(parts) > 128 {
			break
		}
	}
	d := idx.drive + ":"
	for i := len(parts) - 1; i >= 0; i-- {
		d += `\` + parts[i]
	}
	if len(parts) == 0 {
		return d + `\`
	}
	return d
}

// SpRow 前端展示行
type SpRow struct {
	Frn   uint64 `json:"frn"`
	Name  string `json:"name"`
	Path  string `json:"path"`
	IsDir bool   `json:"isDir"`
	Size  int64  `json:"size"`
	Files int64  `json:"files"`
	Mtime int64  `json:"mtime"`
}

// SpaceChildren 某目录的直接子项（子树聚合大小，按大小降序）
func (idx *mftIndex) SpaceChildren(frn uint64) []SpRow {
	kids := idx.kids[frn]
	out := make([]SpRow, 0, len(kids))
	for _, k := range kids {
		n := idx.nodes[k]
		if n == nil {
			continue
		}
		size, files := n.agg, n.nFiles
		if !n.dir {
			size, files = n.size, 1
		}
		out = append(out, SpRow{Frn: k, Name: n.name, IsDir: n.dir, Size: size, Files: files, Mtime: n.mtime})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Size > out[j].Size })
	return out
}

// SpaceTopFiles 全盘最大文件 Top
func (idx *mftIndex) SpaceTopFiles(minSize int64, limit int) []SpRow {
	out := make([]SpRow, 0, limit)
	for frn, n := range idx.nodes {
		if n.dir || n.size < minSize {
			continue
		}
		if frn < 16 {
			continue
		}
		out = append(out, SpRow{Frn: frn, Name: n.name, Size: n.size, Mtime: n.mtime})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Size > out[j].Size })
	if len(out) > limit {
		out = out[:limit]
	}
	for i := range out {
		out[i].Path = idx.fullPath(out[i].Frn)
	}
	return out
}

// SpaceCrumbs 面包屑链
func (idx *mftIndex) SpaceCrumbs(frn uint64) []SpRow {
	var chain []SpRow
	cur := frn
	for {
		n := idx.nodes[cur]
		if n == nil {
			break
		}
		chain = append(chain, SpRow{Frn: cur, Name: n.name, IsDir: true, Size: n.agg})
		if cur == frRoot {
			break
		}
		cur = n.parent
		if len(chain) > 64 {
			break
		}
	}
	// 反转：根在前
	for i, j := 0, len(chain)-1; i < j; i, j = i+1, j-1 {
		chain[i], chain[j] = chain[j], chain[i]
	}
	return chain
}
