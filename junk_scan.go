package main

// ============================================================================
// 3z9wOpt v1.1 · 扫描引擎（junk scan engine）
//
// 设计目标（用户 2026-10-08 拍板）：
//   1. 快：item 级并发 + 目录级工作池，全盘扫描走 v1.0 实测最优的 4 worker 队列模型
//   2. 准：合计体积按实际路径去重，同一个文件/目录永不计两次（TotalUnique）
//   3. 激进但不伤系统：只扫描，不删除；系统红线目录在清单层就已被排除
//
// deepJunkScan / deepJunkScanDrives 从 v1.0 app.go 复制（真机实测口径未改）。
// ============================================================================

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// scanWorkers 目录级并发 worker 数。
// v1.0 机械盘实测：4 路 18s 最优，8/16/24 路不更快（磁盘是瓶颈）；
// v1.1 按核数取 4~8：SSD 上多几路能压满 IO 队列，机械盘也不会退化（瓶颈仍在磁盘）。
func scanWorkers() int {
	n := runtime.NumCPU()
	if n < 4 {
		n = 4
	}
	if n > 8 {
		n = 8
	}
	return n
}

// ---------------------------------------------------------------------------
// v1.1.0：全局目录读取闸门
//
// 背景：现在三级并发叠在一起 —— item 级 6 路 × 单项目录级 4 路 × 每棵树 scanWorkers() 路，
// 8 核机器上最多 ~192 个 goroutine 同时在枚举目录。而本文件开头写着「v1.0 机械盘实测：
// 4 路 18s 最优，8/16/24 路不更快（磁盘是瓶颈）」—— 192 路正是那条实测结论要避免的情形。
// 闸门只圈住 os.ReadDir 这一下（获取-释放不嵌套，不会死锁），把并发目录读取拉回实测最优档；
// 队列里的其余 goroutine 只是排队，不会丢活。
// ---------------------------------------------------------------------------

var dirReadGate = make(chan struct{}, scanWorkers())

// readDirGated 受闸门保护的 os.ReadDir：所有热路径（全盘遍历 / 体积统计 / "**" 枚举）都走这里。
func readDirGated(d string) ([]os.DirEntry, error) {
	dirReadGate <- struct{}{}
	ents, err := os.ReadDir(d)
	<-dirReadGate
	return ents, err
}

// ---------------------------------------------------------------------------
// 全盘零散垃圾扫描（v1.0 复制：deepJunkScan / deepJunkScanDrives）
// ---------------------------------------------------------------------------

// countGroups 按"条数"计量的组：它们的体积本来就是 0（一堆无效注册表键、失效快捷方式），
// 不能因为 Bytes==0 就把它们当成"没扫到东西"隐藏掉。
var countGroups = map[string]bool{"注册表垃圾": true, "快捷方式": true, "使用痕迹": true}

var deepJunkExts = []string{".tmp", ".log", ".bak", ".old", ".dmp"}

// deepJunkLiveWindow：最后修改时间在这个窗口内的文件视为「活的」—— 有程序正握着它在写。
// 2026-10-13 用户拍板：这类文件报出来、也清不掉（删完人家下一秒就写回来），
// 只会让用户清完再扫看到一大堆、以为软件没清干净。所以扫描阶段直接跳过，不报也不删。
// ⚠️ 这是本轮唯一一处改到「清什么」的地方，改之前已由用户确认（10 分钟）。
const deepJunkLiveWindow = 10 * time.Minute

type djFile struct {
	path string
	size int64
}

// deepJunkScan 全盘遍历 5 类扩展名：目录队列工作池（4 worker）并行遍历。
// v1.0 三轮提速实测：单线程 WalkDir 35s；4 路 18s 最优；8/16/24 路不更快（磁盘才是瓶颈）。
// 关键点：send 带 default，队满就地递归处理 —— 生产者=消费者模型下绝不阻塞（死锁防线）。
func deepJunkScan(ctx context.Context, root string) []djFile {
	root = filepath.Clean(root)
	var mu sync.Mutex
	var out []djFile
	dirs := make(chan string, 4096)
	var pending sync.WaitGroup // 在飞的目录任务数，归零即遍历完
	var wg sync.WaitGroup

	liveSince := time.Now().Add(-deepJunkLiveWindow)
	collect := func(p string, d os.DirEntry) {
		name := strings.ToLower(d.Name())
		for _, e := range deepJunkExts {
			if strings.HasSuffix(name, e) {
				fi, err := d.Info()
				if err != nil {
					return // 连属性都读不到：体积和时间都没法判断，不报（不许编 0 顶上）
				}
				if fi.ModTime().After(liveSince) {
					return // 活的：程序正在写，删了立刻回来 —— 报出来等于给用户制造"清不干净"的错觉
				}
				mu.Lock()
				out = append(out, djFile{p, fi.Size()})
				mu.Unlock()
				break
			}
		}
	}

	var processDir func(d string)
	processDir = func(d string) {
		entries, err := readDirGated(d)
		if err != nil {
			return
		}
		for _, e := range entries {
			if ctx.Err() != nil {
				return
			}
			p := filepath.Join(d, e.Name())
			if e.IsDir() {
				// v1.1：这两个目录 ReadDir 极慢且永远清不出东西，直接跳过（不损失任何清理能力）
				ln := strings.ToLower(e.Name())
				if ln == "$recycle.bin" || ln == "system volume information" {
					continue
				}
				if isReparsePoint(p) {
					continue
				}
				pending.Add(1)
				select {
				case dirs <- p: // 交给 worker 池
				case <-ctx.Done():
					pending.Done()
					return
				default: // 队满：就地递归处理，绝不阻塞
					processDir(p)
					pending.Done()
				}
			} else {
				collect(p, e)
			}
		}
	}

	pending.Add(1)
	select {
	case dirs <- root:
	case <-ctx.Done():
		pending.Done()
		return nil
	}
	// v1.1.0：worker 不再因为 ctx 取消就 return —— 提前退出会让 pending 永远归不了零，
	// 上面那句 close(dirs) 的收尾 goroutine 会永久阻塞（每次取消泄漏一个 goroutine + 整个待处理队列）。
	// 取消后 processDir 内部会就地返回，队列很快被消费完，pending 正常归零、channel 正常关闭。
	worker := func() {
		defer wg.Done()
		for d := range dirs {
			if ctx.Err() == nil {
				processDir(d)
			}
			pending.Done()
		}
	}
	go func() { pending.Wait(); close(dirs) }()
	for i := 0; i < scanWorkers(); i++ {
		wg.Add(1)
		go worker()
	}
	wg.Wait()
	return out
}

// deepJunkScanDrives 全部固定磁盘并行扫描：单盘内部 4 worker，多盘并行让总耗时 ≈ 最慢的盘。
func deepJunkScanDrives(ctx context.Context) []djFile {
	drives := fixedDrives()
	if len(drives) == 0 {
		return nil
	}
	var mu sync.Mutex
	var all []djFile
	var wg sync.WaitGroup
	for _, drv := range drives {
		wg.Add(1)
		go func(drv string) {
			defer wg.Done()
			fs := deepJunkScan(ctx, drv)
			mu.Lock()
			all = append(all, fs...)
			mu.Unlock()
		}(drv)
	}
	wg.Wait()
	return all
}

// ---------------------------------------------------------------------------
// v1.1 新增：多盘路径工具（取代 v1.0 写死 C 盘）
// ---------------------------------------------------------------------------

// allDrivesSub 返回所有固定盘上存在的 rel 子目录（如 inetpub\logs / MSOCache）。
// 不存在的盘直接跳过，UI 不会显示空条目。
func allDrivesSub(rel string) []string {
	var out []string
	for _, d := range fixedDrives() {
		p := filepath.Join(d, rel)
		if fi, err := os.Stat(p); err == nil && fi.IsDir() {
			out = append(out, p)
		}
	}
	return out
}

// driverResDirs 驱动解压残留（v1.1 激进版）：遍历所有固定盘根目录，
// 命中厂商驱动解压目录就整个收录。
//
// 激进 = 整个目录清空，而不是只挑 .tmp/.log。
// 红线（绝不收录）：DriverStore、INF、System32\drivers 以及任何系统目录 —— 已装驱动不受影响。
func driverResDirs() []string {
	hit := map[string]bool{
		"amd": true, "intel": true, "nvidia": true, "ati": true, "amd software": true,
		"drivers": true, "driver": true, "driverfiles": true, "displaydriver": true,
		"nvidiadriver": true, "inteldriver": true,
	}
	block := map[string]bool{ // 系统目录，即便同名也跳过
		"windows": true, "program files": true, "program files (x86)": true,
		"programdata": true, "users": true, "documents and settings": true,
		"system volume information": true, "$recycle.bin": true, "recovery": true,
		"perflogs": true, "boot": true, "onedrivetemp": true, "winsxs": true,
	}
	var out []string
	for _, drv := range fixedDrives() {
		ents, err := os.ReadDir(drv)
		if err != nil {
			continue
		}
		for _, e := range ents {
			if !e.IsDir() {
				continue
			}
			ln := strings.ToLower(e.Name())
			if !hit[ln] || block[ln] {
				continue
			}
			p := filepath.Join(drv, e.Name())
			if isReparsePoint(p) {
				continue
			}
			out = append(out, p)
		}
	}
	return out
}

// listDirsFast 并发枚举 root 及其子目录（junction / 软链接不深入），maxDepth<=0 表示不限。
// v1.1 提速两处：
//
//	① resolveGlob 的 "**" 段原用串行 filepath.WalkDir，实测单项拖到 8.7s
//	② 深度上限：浏览器 / 应用缓存目录都在 1~4 层，继续往下钻只会撞上 node_modules、
//	   WinSxS 这类成千上万的深层目录，纯浪费时间。maxDepth=8 对所有现役规则都够用。
//
// 队列模型与 walkSizeFast 一致（send 带 default，队满就地递归，不会死锁）。
func listDirsFast(ctx context.Context, root string, maxDepth int) []string {
	var mu sync.Mutex
	out := []string{root}
	// v1.1.0：队列必须带深度。原版只把路径塞队列、出队时一律 process(d, 1)，
	// depth 永远从 1 数起，`depth >= maxDepth` 基本不可能成立 → maxDepth 形同虚设，
	// LOCALAPPDATA 整棵树（Packages / node_modules 那种）还是会被全量枚举。
	type dirJob struct {
		p     string
		depth int
	}
	dirs := make(chan dirJob, 4096)
	var pending sync.WaitGroup
	var wg sync.WaitGroup

	var process func(d string, depth int)
	process = func(d string, depth int) {
		if ctx.Err() != nil {
			return
		}
		ents, err := readDirGated(d)
		if err != nil {
			return
		}
		for _, e := range ents {
			if ctx.Err() != nil {
				return
			}
			if !e.IsDir() {
				continue
			}
			p := filepath.Join(d, e.Name())
			if isReparsePoint(p) {
				continue
			}
			mu.Lock()
			out = append(out, p)
			mu.Unlock()
			if maxDepth > 0 && depth >= maxDepth {
				continue
			}
			pending.Add(1)
			select {
			case dirs <- dirJob{p: p, depth: depth + 1}:
			case <-ctx.Done():
				pending.Done()
				return
			default:
				process(p, depth+1)
				pending.Done()
			}
		}
	}

	pending.Add(1)
	dirs <- dirJob{p: root, depth: 1}
	// 同 deepJunkScan：取消后 worker 不提前 return，保证 pending 归零、队列被消费完
	worker := func() {
		defer wg.Done()
		for j := range dirs {
			if ctx.Err() == nil {
				process(j.p, j.depth)
			}
			pending.Done()
		}
	}
	go func() { pending.Wait(); close(dirs) }()
	for i := 0; i < scanWorkers(); i++ {
		wg.Add(1)
		go worker()
	}
	wg.Wait()
	return out
}

// ---------------------------------------------------------------------------
// v1.1 新增：体积统计（并发工作池）
// ---------------------------------------------------------------------------

// walkSizeFast 统计目录体积：目录级工作池 4 路（沿用 v1.0 实测最优），
// 遍历时顺带取体积，免去事后逐个 Stat。junction / 软链接一律不深入。
func walkSizeFast(ctx context.Context, root string) (int64, int) {
	// 2026-10-09 修复：原来只判子目录是不是重解析点，根目录本身不判。
	// Edge/IE 的 INetCache 与 Temporary Internet Files 互指 junction，
	// 同一份缓存会在同一个项里被计两次。这里补上根目录判定。
	if root != "" && isReparsePoint(root) {
		return 0, 0
	}
	var mu sync.Mutex
	var total int64
	var count int
	dirs := make(chan string, 4096)
	var pending sync.WaitGroup
	var wg sync.WaitGroup

	var processDir func(d string)
	processDir = func(d string) {
		if ctx.Err() != nil {
			return
		}
		entries, err := readDirGated(d)
		if err != nil {
			return
		}
		for _, e := range entries {
			if ctx.Err() != nil {
				return
			}
			p := filepath.Join(d, e.Name())
			if e.IsDir() {
				if isReparsePoint(p) {
					continue
				}
				pending.Add(1)
				select {
				case dirs <- p:
				case <-ctx.Done():
					pending.Done()
					return
				default:
					processDir(p) // 队满就地处理，绝不阻塞
					pending.Done()
				}
				continue
			}
			var sz int64
			if fi, err := e.Info(); err == nil {
				sz = fi.Size()
			}
			mu.Lock()
			total += sz
			count++
			mu.Unlock()
		}
	}

	pending.Add(1)
	dirs <- root
	// 取消后 worker 不提前 return（否则 pending 归不了零、收尾 goroutine 永久阻塞）
	worker := func() {
		defer wg.Done()
		for d := range dirs {
			if ctx.Err() == nil {
				processDir(d)
			}
			pending.Done()
		}
	}
	go func() { pending.Wait(); close(dirs) }()
	for i := 0; i < scanWorkers(); i++ {
		wg.Add(1)
		go worker()
	}
	wg.Wait()
	return total, count
}

// ---------------------------------------------------------------------------
// v1.1 新增：并发扫描 + 去重合计
// ---------------------------------------------------------------------------

// ScanAll 并发扫描全部清理项：解析目标 → 统计体积 → 记录 tsize（供 TotalUnique 去重）。
// item 级并发上限 = min(6, NumCPU)；deepjunk 走全盘并发扫描；命令型且无法预估的项体积为 0。
// v1.1.0：ctx 必须一路传下去，否则「取消扫描」只是不再派发新项，正在跑的全盘扫描照样跑完。
func ScanAll(ctx context.Context, items []JunkItem) {
	par := runtime.NumCPU()
	if par > 6 {
		par = 6
	}
	if par < 2 {
		par = 2
	}
	sem := make(chan struct{}, par)
	var wg sync.WaitGroup
	for i := range items {
		if ctx.Err() != nil {
			break
		}
		wg.Add(1)
		sem <- struct{}{}
		go func(it *JunkItem) {
			defer wg.Done()
			defer func() { <-sem }()
			scanOne(ctx, it)
		}(&items[i])
	}
	wg.Wait()
}

// ---------- 回收站：每个盘各有自己的仓库，必须逐盘问系统再累加 ----------

// shQueryRBInfo = SHQUERYRBINFO（shell32）。cbSize 必须先填成结构体大小，
// 否则 SHQueryRecycleBinW 直接返回失败。
type shQueryRBInfo struct {
	cbSize      uint32
	i64Size     int64
	i64NumItems int64
}

// recycleBinTotal 问系统每个盘回收站占多少、多少个文件。
// 2026-10-10：以前这一项扫描阶段根本不量（界面永远显示「—」），清完也不复核，
// 于是**清没清掉界面上一个字都不说** —— 实测那次 C 盘清了、D 盘 160 MB 原封不动，
// 用户完全不知情。回收站没有可枚举的文件目标，只能靠系统接口问。
// 不用遍历 $Recycle.Bin 目录：那里按用户 SID 分子目录，没权限的读不到，数只会偏小。
/* 2026-10-14 换口径：改自己枚举 $Recycle.Bin，不再用 SHQueryRecycleBinW。
   三条实测理由（本机 D 盘）：
     官方 SHQueryRecycleBinW : 1.843s   1965 个 /  332.15 MB
     自己 walk $Recycle.Bin  : 0.195s   2877 个 /  910.06 MB
   ① **慢 9 倍**：官方接口自己要统计一遍，文件越多越慢（曾实测 12363 个文件 2.3 秒）。
      改完实测 2278ms → 79ms。
   ② **数对不上**：清理（case "recycle"）一直是用 walkSize 算 before 的 ——
      扫描报 332 MB、清完按 910 MB 算，用户看到的就是"说一套做一套"。
      清空回收站真正腾出来的是磁盘上的实际占用，所以 **walk 的口径才是对的**。
   ③ **根本没进合计**（独立成组后才暴露）：TotalUniqueSplit 见 tsize 为空就整项跳过，
      于是全场最大的一块（638 MB）在主数字里是 0。现在 tsize 按盘填真实路径。
   兜底：walk 一个都没读到（没权限 / 精简版没这个目录）时退回官方接口，不许显示 0。 */
func recycleBinTotal() (int64, int) {
	_, total, files := recycleBinScan()
	if total == 0 {
		_, t2, f2 := recycleBinTotalAPI()
		return t2, f2
	}
	return total, files
}

// recycleBinScan 并发枚举各固定盘的 $Recycle.Bin。
// 返回：分盘体积（键是真实路径，要给 TotalUniqueSplit 做去重）、总体积、总文件数。
func recycleBinScan() (map[string]int64, int64, int) {
	sizes := map[string]int64{}
	var total int64
	var files int
	var mu sync.Mutex
	var wg sync.WaitGroup
	/* 只问固定盘：光驱 / 网络盘 / 读卡器上没有回收站。各盘并发 —— walk 是纯磁盘 IO，
	   并发有效（不像 shell32 那个接口内部是串行的，并发反而更慢）。 */
	for _, root := range fixedDrives() {
		wg.Add(1)
		go func(root string) {
			defer wg.Done()
			b, c := walkSize(root + `$Recycle.Bin`)
			if c == 0 {
				return
			}
			mu.Lock()
			sizes[root+`$Recycle.Bin`] = b
			total += b
			files += c
			mu.Unlock()
		}(root)
	}
	wg.Wait()
	return sizes, total, files
}

// recycleBinTotalAPI 走 shell32!SHQueryRecycleBinW 逐盘问（旧口径，现在只作兜底）。
// ⚠️ 它报的是文件逻辑大小、不含 $I 元数据与簇对齐，比实际能腾出来的空间偏小。
// 🔴 兜底也必须返回**分盘路径 map**（2026-10-14 补）：TotalUniqueSplit 见 tsize 为空就整项跳过，
//    回收站在顶部合计里就成了 0（"洞③"正常路径修好后，兜底分支又原样踩了一次）。
func recycleBinTotalAPI() (map[string]int64, int64, int) {
	ps := syscall.NewLazyDLL("shell32.dll")
	p := ps.NewProc("SHQueryRecycleBinW")
	if err := p.Find(); err != nil {
		return nil, 0, 0 // 系统没这个接口（极老的系统）→ 如实不量，不许编一个数
	}
	sizes := map[string]int64{}
	var total int64
	var n int
	for _, root := range fixedDrives() {
		var info shQueryRBInfo
		info.cbSize = uint32(unsafe.Sizeof(info))
		ptr, err := windows.UTF16PtrFromString(root)
		if err != nil {
			continue
		}
		r, _, _ := p.Call(uintptr(unsafe.Pointer(ptr)), uintptr(unsafe.Pointer(&info)))
		if r != 0 { // 这个盘没有回收站 / 用不着清，跳过（S_OK=0）
			continue
		}
		sizes[root+`$Recycle.Bin`] = info.i64Size
		total += info.i64Size
		n += int(info.i64NumItems)
	}
	return sizes, total, n
}

func recycleMeasure(it *JunkItem) {
	/* tsize 必须按盘填真实路径：TotalUniqueSplit 见 tsize 为空会整项跳过，
	   那样全场最大的一块（本机 638 MB）在主数字里就成了 0（2026-10-14）。 */
	sizes, total, files := recycleBinScan()
	if total == 0 {
		/* 兜底（没权限 / 精简版没这个目录）：tsize 照样按盘填真实路径，
		   🔴 不能像以前那样留空 map —— 空 map 会被 TotalUniqueSplit 整项跳过，
		   列表显示 600 MB、顶部合计却是 0。 */
		bs, t2, f2 := recycleBinTotalAPI()
		it.tsize = bs
		it.Bytes = t2
		it.Files = f2
		return
	}
	it.tsize = sizes
	it.Bytes = total
	it.Files = files
}

func scanOne(ctx context.Context, it *JunkItem) {
	t0 := time.Now()
	/* ⚠️ 实测必须挂在 defer 上：**所有**分支都要跑到。
	   下面 deepjunk / recycle / reg:* / badlnk 四个分支都会提前 return，
	   写在函数末尾等于漏掉它们 —— deepjunk 那 1 GB 的 Cleanable 会全是 0，
	   账上直接凭空少一大块（2026-10-15 实测踩过）。
	   放在 ScanMs 之前，实测本身花的时间也计入这一项的耗时。 */
	defer func() {
		probeDeletable(it)
		it.ScanMs = time.Since(t0).Milliseconds()
	}()

	it.targets = nil
	it.tsize = map[string]int64{}
	it.tfiles = map[string]int{}
	it.Bytes, it.Files = 0, 0

	// 2026-10-09 修复：注册表垃圾 / 失效快捷方式这两类不是文件，
	// v1.1 里 regItemMeasure / badlnkMeasure 定义了却没有任何调用点 → 界面永远显示 0 条。
	// 这里把它们接回扫描主链路（按「条数」呈现，不按字节，因为删了也省不出磁盘空间）。
	if strings.HasPrefix(it.Cmd, "reg:") {
		regItemMeasure(it)
		applyRequireEmpty(it)
		it.syncPaths()
		return
	}
	if it.Cmd == "badlnk" {
		badlnkMeasure(it)
		applyRequireEmpty(it)
		it.syncPaths()
		return
	}
	// 回收站：走系统接口逐盘问，别再让它在界面上永远是「—」
	if it.Cmd == "recycle" {
		recycleMeasure(it)
		applyRequireEmpty(it)
		it.syncPaths()
		return
	}

	// 全盘零散垃圾：走专门的并发全盘扫描（含各盘根目录，已与 rootjunk 合并）
	if it.Cmd == "deepjunk" {
		fs := deepJunkScanDrives(ctx)
		var total int64
		for _, f := range fs {
			total += f.size
			it.tsize[strings.ToLower(f.path)] = f.size
			it.tfiles[strings.ToLower(f.path)] = 1 // 文件级目标：一条路径就是一个文件
			it.targets = append(it.targets, f.path)
		}
		it.Bytes, it.Files = total, len(fs)
		it.syncPaths()
		return
	}

	dirs, files := resolveTargets(ctx, it)
	var mu sync.Mutex
	var wg sync.WaitGroup
	sem := make(chan struct{}, 4)
	for _, d := range dirs {
		if ctx.Err() != nil {
			break // 用户取消：不再派发新的目录
		}
		wg.Add(1)
		sem <- struct{}{}
		go func(d string) {
			defer wg.Done()
			defer func() { <-sem }()
			b, f := walkSizeFast(ctx, d)
			mu.Lock()
			it.Bytes += b
			it.Files += f
			it.tsize[strings.ToLower(d)] = b
			it.tfiles[strings.ToLower(d)] = f
			it.targets = append(it.targets, d)
			mu.Unlock()
		}(d)
	}
	wg.Wait()
	for _, f := range files {
		if ctx.Err() != nil {
			break
		}
		if fi, err := os.Stat(f); err == nil {
			it.Bytes += fi.Size()
			it.Files++
			it.tsize[strings.ToLower(f)] = fi.Size()
			it.tfiles[strings.ToLower(f)] = 1
			it.targets = append(it.targets, f)
		}
	}
	// 自适应：require 全不存在 → 前端隐藏该行（未安装的软件不占位）
	applyRequireEmpty(it)
	/* 2026-10-15：这里原本还有两条**扫描期过滤**，已连同整套机制删除
	（junk_blocked.go 黑名单 + junk_renew.go 压制，共 777 行，含设置页的重置入口）：
	     dropBlocked  —— 「上次没删掉」就整项藏掉
	     quenchRenew  —— 「上次清过、现在没攒够」就整行不列出
	   删除理由：两条都是拿**上一次的历史**推断这一次，而且都让用户自己决定不了 ——
	     前者本机误判率 100%（实测 1285 个目标里 denied 为 0，黑名单一条都没押对），
	     后者让用户想再清一遍都找不到入口。都与最终口径「全部列出、默认全选、用户自己决定」冲突。
	   替代方案：每次扫描重新问一遍系统（函数顶部的 probeDeletable），项照常列出、照常可勾，
	     删不删得动由数字说话。 */
	/* 一个字节都没扫到的项不占一行：清完了就是清完了，列个"0 B"只会让列表看着还是一大堆。
	   ⚠️ 计数类项（注册表垃圾/快捷方式/使用痕迹按条数算）不算字节，不受影响，照常列出。 */
	if it.Bytes == 0 && !countGroups[it.Group] {
		it.Empty = true
	}
	// 把「路径 -> 体积」下发给前端（前端据此做路径级去重，界面数字才与后端同口径）
	it.syncPaths()
}

// applyRequireEmpty 本机没装 → Empty=true，前端隐藏该行（未安装的软件不占位）
func applyRequireEmpty(it *JunkItem) {
	if len(it.require) == 0 {
		return
	}
	ok := false
	for _, r := range it.require {
		if _, err := os.Stat(r); err == nil {
			ok = true
			break
		}
	}
	it.Empty = !ok
}

// TotalUniqueSplit 全选合计：同一个文件/目录只算一次，并把体积按口径分层。
//
//   - 路径完全相同 → 只算一次
//   - 父子关系（父目录已计入）→ 子路径跳过，只算父目录
//   - 同一 dup 组（如 winsxstmp / dismreset）→ 只取组内最大者
//   - 没有具体路径的命令型项（如 cleanmgr）→ 按无路径处理，不参与去重
//
// 返回：total = 去重后总和；heavy = 其中来自 Heavy 项（你的文件 / 整包）的部分；
// targets = 目标条数。恒有 heavy <= total。
// 归属规则和前端 sumSplit() 完全一致：谁"赢"了这条路径就归谁（父目录赢子路径，先到先得）。
//
// sel 为 nil 表示全部计入；否则只计入 sel[id]==true 的项。
func TotalUniqueSplit(items []JunkItem, sel map[string]bool) (total, heavy int64, targets int) {
	type ent struct {
		p string
		s int64
		h bool
	}
	var all []ent
	dupBest := map[string]int64{}
	dupHeavy := map[string]bool{}
	dupFiles := map[string]int{}

	for i := range items {
		it := &items[i]
		if sel != nil && !sel[it.ID] {
			continue
		}
		if it.dup != "" {
			if it.Bytes > dupBest[it.dup] {
				dupBest[it.dup] = it.Bytes
				dupHeavy[it.dup] = it.Heavy
				dupFiles[it.dup] = it.Files
			}
			continue
		}
		if len(it.tsize) == 0 {
			continue // 没有可枚举的目标（命令型，无法预估体积）
		}
		for p, s := range it.tsize {
			all = append(all, ent{p, s, it.Heavy})
		}
	}

	sort.Slice(all, func(i, j int) bool { return len(all[i].p) < len(all[j].p) })

	var accepted []string
	for _, e := range all {
		dup := false
		for _, a := range accepted {
			// v1.1.0：路径完全相同也算重复。原来只判父子关系（len(e.p) > len(a)），
			// 两个项测到同一个目标时会被算两次。
			if e.p == a || (strings.HasPrefix(e.p, a) && len(e.p) > len(a) && e.p[len(a)] == '\\') {
				dup = true
				break
			}
		}
		if dup {
			continue
		}
		accepted = append(accepted, e.p)
		total += e.s
		if e.h {
			heavy += e.s
		}
		targets++
	}
	for k, v := range dupBest {
		total += v
		if dupHeavy[k] {
			heavy += v
		}
		targets += dupFiles[k]
	}
	return total, heavy, targets
}

// TotalUnique 只要总数（兼容旧调用点与命令行自检）。
func TotalUnique(items []JunkItem, sel map[string]bool) (int64, int) {
	total, _, targets := TotalUniqueSplit(items, sel)
	return total, targets
}

// matchedDrives 供 UI 显示：本次实际扫描到的固定磁盘
func matchedDrives() []string { return fixedDrives() }

// 确保 syscall / unsafe 在本文件被引用（fixedDrives 走 syscall，保留以防将来独立使用）
var _ = unsafe.Pointer(nil)
var _ = syscall.FILE_ATTRIBUTE_REPARSE_POINT
