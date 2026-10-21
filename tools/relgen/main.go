// relgen —— 发布台账与 version.json 生成器（2026-10-19）
//
// 解决的问题：以前发一次版要手工改三处数字（size / sha256 / buildDate），
// 手抄一次错一次；而且同一个版本号重建时，旧指纹会被新指纹顶掉，
// 老用户自查「是不是官方原版」必然报不一样。
//
// 现在所有事实只记在一个地方：发布/构建台账.json（只追加，不修改、不删除）。
// version.json 由它生成，指纹由 exe 现算 —— 人手不碰任何数字。
//
// 用法（build.cmd release 会自动调用，平时不用手敲）：
//
//	go run ./tools/relgen next --ver 1.1.0
//	    打印今天该用的构建号，如 20261019.2
//
//	go run ./tools/relgen publish --ver 1.1.0 --build 20261019.2 --exe bin\3z9wOpt_v1.1.0.exe
//	    算指纹 → 追加台账 → 生成两份 version.json → 同步官网页面上的体积与日期
//
// 全部只用标准库，不引依赖（GOPROXY=off 也能跑）。
package main

import (
	"archive/zip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	ledgerPath  = "发布/构建台账.json" // 唯一事实来源（只追加）
	notesPath   = "发布/本次更新.txt"  // 每次发版前改这里，一行一条
	readmePath  = "发布/使用说明.txt"  // 打进下载包的说明文件（包里会改名成 使用说明.txt）
	siteDir     = "opt.3z9w.com"
	releaseDir  = "release"
	downloadDir = "opt.3z9w.com/download"
	urlFmt      = "https://opt.3z9w.com/download/3z9wOpt_v%s.exe"
)

// build 一次对外构建。Build 是主键，永不复用。
type build struct {
	Version   string   `json:"version"`
	Build     string   `json:"build"`     // YYYYMMDD.N
	BuildDate string   `json:"buildDate"` // YYYY-MM-DD
	SHA256    string   `json:"sha256"`
	Size      int64    `json:"size"`
	Notes     []string `json:"notes,omitempty"`
	URL       string   `json:"url,omitempty"`
}

type ledger struct {
	Note   string  `json:"note"`
	Builds []build `json:"builds"`
}

// 生成的 version.json（字段与 Go 端 updateManifest 对齐；history 里带着所有旧构建）
type manifest struct {
	Version   string        `json:"version"`
	Build     string        `json:"build"`
	BuildDate string        `json:"buildDate"`
	Notes     []string      `json:"notes"`
	URL       string        `json:"url"`
	SHA256    string        `json:"sha256"`
	Size      int64         `json:"size"`
	History   []historyItem `json:"history"`
}

type historyItem struct {
	Version   string `json:"version"`
	Build     string `json:"build"`
	BuildDate string `json:"buildDate"`
	SHA256    string `json:"sha256"`
	Size      int64  `json:"size"`
}

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "next":
		err = cmdNext(os.Args[2:])
	case "publish":
		err = cmdPublish(os.Args[2:])
	case "-h", "--help", "help":
		usage()
		return
	default:
		err = fmt.Errorf("不认识的命令 %q", os.Args[1])
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "relgen:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Println(`relgen —— 发布台账 / version.json 生成器

  relgen next    --ver 1.1.0
  relgen publish --ver 1.1.0 --build 20261019.2 --exe bin\3z9wOpt_v1.1.0.exe [--notes 路径]`)
}

// ---------- next ----------

func cmdNext(args []string) error {
	fs := flag.NewFlagSet("next", flag.ContinueOnError)
	ver := fs.String("ver", "", "版本号，如 1.1.0")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if strings.TrimSpace(*ver) == "" {
		return errors.New("缺少 --ver")
	}
	lg, err := loadLedger()
	if err != nil {
		return err
	}
	fmt.Println(nextBuildNum(lg, time.Now()))
	return nil
}

/*
nextBuildNum 今天的下一个构建号：日期 + 当天已有条数 + 1。

	取「当天最大序号 + 1」而不是「条数 + 1」：手工补录过一条时也不会撞号。
*/
func nextBuildNum(lg ledger, now time.Time) string {
	day := now.Format("20060102")
	max := 0
	for _, b := range lg.Builds {
		d, n, ok := splitBuild(b.Build)
		if !ok || d != day {
			continue
		}
		if n > max {
			max = n
		}
	}
	return fmt.Sprintf("%s.%d", day, max+1)
}

// splitBuild "20261019.2" → ("20261019", 2, true)
func splitBuild(s string) (string, int, bool) {
	s = strings.TrimSpace(s)
	i := strings.IndexByte(s, '.')
	if i < 0 {
		if len(s) != 8 {
			return "", 0, false
		}
		return s, 0, true
	}
	d, seq := s[:i], s[i+1:]
	n, err := strconv.Atoi(seq)
	if err != nil || len(d) != 8 {
		return "", 0, false
	}
	return d, n, true
}

// ---------- publish ----------

func cmdPublish(args []string) error {
	fs := flag.NewFlagSet("publish", flag.ContinueOnError)
	ver := fs.String("ver", "", "版本号，如 1.1.0")
	bno := fs.String("build", "", "构建号，如 20261019.2")
	exe := fs.String("exe", "", "要发布的 exe 路径")
	notesFile := fs.String("notes", notesPath, "更新说明（一行一条）")
	if err := fs.Parse(args); err != nil {
		return err
	}
	for k, v := range map[string]string{"--ver": *ver, "--build": *bno, "--exe": *exe} {
		if strings.TrimSpace(v) == "" {
			return fmt.Errorf("缺少 %s", k)
		}
	}
	if _, _, ok := splitBuild(*bno); !ok {
		return fmt.Errorf("构建号格式不对：%q（应该是 20261019.2）", *bno)
	}

	sum, size, err := hashFile(*exe)
	if err != nil {
		return fmt.Errorf("算不出指纹：%w", err)
	}

	/* 先重打下载包，再算体积：页面上的 MB 数是照 zip 写的，顺序反了就会差一版。
	   zip 也交给这里打，而不是让 build.cmd 调 tar/Compress-Archive ——
	   PowerShell 的 Compress-Archive 在本机把路径分隔符写成反斜杠（3z9wOpt_v1.1.0\3z9wOpt.exe），
	   unzip 直接报错；Go 的 archive/zip 分隔符、UTF-8 标志都是对的。 */
	zipPath := filepath.Join(downloadDir, fmt.Sprintf("3z9wOpt_v%s.zip", *ver))
	zipSize, zerr := packZip(*ver, *exe, zipPath, readmePath)
	if zerr != nil {
		fmt.Fprintln(os.Stderr, "relgen: 打包 zip 失败（不影响清单，但官网体积可能显示不准）：", zerr)
		zipSize = fileSize(zipPath)
	}

	lg, err := loadLedger()
	if err != nil {
		return err
	}
	notes := readNotes(*notesFile)
	if len(notes) == 0 {
		notes = lastNotes(lg)
	}
	day, _, _ := splitBuild(*bno)
	date := day[:4] + "-" + day[4:6] + "-" + day[6:]

	item := build{
		Version:   *ver,
		Build:     *bno,
		BuildDate: date,
		SHA256:    sum,
		Size:      size,
		Notes:     notes,
		URL:       fmt.Sprintf(urlFmt, *ver),
	}
	lg = upsert(lg, item)
	sort.SliceStable(lg.Builds, func(i, j int) bool {
		return buildKey(lg.Builds[i].Build) < buildKey(lg.Builds[j].Build)
	})
	if err := saveLedger(lg); err != nil {
		return err
	}

	// 顶层 = 台账里最后一条；history = 其余全部（含同版本号的历史构建，这是关键）
	top := lg.Builds[len(lg.Builds)-1]
	if top.Build != item.Build {
		return fmt.Errorf("台账顺序异常：最后一条是 %s，刚写入的是 %s", top.Build, item.Build)
	}
	m := manifest{
		Version:   top.Version,
		Build:     top.Build,
		BuildDate: top.BuildDate,
		Notes:     top.Notes,
		URL:       top.URL,
		SHA256:    top.SHA256,
		Size:      top.Size,
	}
	for _, b := range lg.Builds[:len(lg.Builds)-1] {
		m.History = append(m.History, historyItem{
			Version: b.Version, Build: b.Build, BuildDate: b.BuildDate,
			SHA256: b.SHA256, Size: b.Size,
		})
	}
	if m.History == nil {
		m.History = []historyItem{}
	}

	for _, dir := range []string{siteDir, releaseDir} {
		if err := writeJSON(filepath.Join(dir, "version.json"), m); err != nil {
			return err
		}
	}

	touched, err := patchSite(top, size, zipSize)
	if err != nil {
		// 页面同步失败不该让整个发布失败：清单已经落盘了，页面手工改也行
		fmt.Fprintln(os.Stderr, "relgen: 官网页面同步失败（不影响 version.json）：", err)
	}

	fmt.Printf("构建号      %s（v%s，%s）\n", top.Build, top.Version, top.BuildDate)
	fmt.Printf("主程序      %d 字节（%.1f MB）\n", size, float64(size)/(1<<20))
	if zipSize > 0 {
		fmt.Printf("下载包      %d 字节（%.1f MB，已重打，含使用说明）\n", zipSize, float64(zipSize)/(1<<20))
	}
	fmt.Printf("SHA256      %s\n", sum)
	fmt.Printf("台账        %s（共 %d 条）\n", ledgerPath, len(lg.Builds))
	fmt.Printf("清单        %s/version.json、%s/version.json\n", siteDir, releaseDir)
	if touched > 0 {
		fmt.Printf("官网页面    已同步 %d 处体积/日期\n", touched)
	}
	fmt.Println()
	fmt.Println("接下来只剩两件事：")
	fmt.Println("  1. 上传顺序：download/ 里的包 → version.json → index.html / sitemap.xml")
	fmt.Println("     （先传清单会让中间那段时间点更新的人拿到 404）")
	fmt.Println("  2. 更新记录.md 里补一句这次的改动")
	return nil
}

func upsert(lg ledger, item build) ledger {
	for i := range lg.Builds {
		if lg.Builds[i].Build == item.Build {
			lg.Builds[i] = item // 同一个构建号重新发布：覆盖它自己，不动别人
			return lg
		}
	}
	lg.Builds = append(lg.Builds, item)
	return lg
}

// buildKey 排序键：日期 * 10000 + 序号，够用到一天一万次
func buildKey(s string) int {
	d, n, ok := splitBuild(s)
	if !ok {
		return -1
	}
	x, _ := strconv.Atoi(d)
	return x*10000 + n
}

// ---------- 官网页面同步 ----------

/*
patchSite 只改几处**有明确上下文**的数字/日期，别的一概不碰。

	⚠️ 绝不能用「全页面所有 N MB / 所有日期」那种粗暴正则：页面上还有
	「首发 2026-10-01」「© 2026」这些固定文案，一处替换错就是整页内容错乱。
	匹配不上就返回 0（页面手工改也行），不猜。
*/
func patchSite(top build, exeSize, zipSize int64) (int, error) {
	path := filepath.Join(siteDir, "index.html")
	raw, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	s := string(raw)
	n := 0
	repl := func(re *regexp.Regexp, fn func(string) string) {
		s = re.ReplaceAllStringFunc(s, func(m string) string {
			n++
			return fn(m)
		})
	}

	exeMB := fmt.Sprintf("%.1f", float64(exeSize)/(1<<20))
	if zipSize > 0 {
		zipMB := fmt.Sprintf("%.1f", float64(zipSize)/(1<<20))
		repl(regexp.MustCompile(`立即下载 · [\d.]+ MB`), func(string) string {
			return "立即下载 · " + zipMB + " MB"
		})
		repl(regexp.MustCompile(`压缩包 [\d.]+ MB，解压后主程序 [\d.]+ MB`), func(string) string {
			return "压缩包 " + zipMB + " MB，解压后主程序 " + exeMB + " MB"
		})
		repl(regexp.MustCompile(`压缩包 [\d.]+ MB · 解压后主程序 [\d.]+ MB`), func(string) string {
			return "压缩包 " + zipMB + " MB · 解压后主程序 " + exeMB + " MB"
		})
		// 结构化数据里的 fileSize —— downloadUrl 指向 zip，所以填 zip 体积
		repl(regexp.MustCompile(`"fileSize":"[\d.]+ MB"`), func(string) string {
			return `"fileSize":"` + zipMB + ` MB"`
		})
	}
	// 结构化数据的 dateModified（datePublished 是首发日，永远不碰）
	repl(regexp.MustCompile(`"dateModified":"20\d{2}-\d{2}-\d{2}"`), func(string) string {
		return `"dateModified":"` + top.BuildDate + `"`
	})
	// 版本号（结构化数据 + 页头那行「v1.1.0 · 2026-10-19 更新」）
	repl(regexp.MustCompile(`"softwareVersion":"[^"]*"`), func(string) string {
		return `"softwareVersion":"` + top.Version + `"`
	})
	repl(regexp.MustCompile(`<span class="ver">v[^<]*</span>`), func(string) string {
		return `<span class="ver">v` + top.Version + ` · ` + top.BuildDate + ` 更新</span>`
	})

	if s != string(raw) {
		if err := os.WriteFile(path, []byte(s), 0o644); err != nil {
			return n, err
		}
	}
	// sitemap 的 lastmod
	smp := filepath.Join(siteDir, "sitemap.xml")
	if b, err := os.ReadFile(smp); err == nil {
		out := regexp.MustCompile(`<lastmod>20\d{2}-\d{2}-\d{2}</lastmod>`).
			ReplaceAllString(string(b), "<lastmod>"+top.BuildDate+"</lastmod>")
		if out != string(b) {
			_ = os.WriteFile(smp, []byte(out), 0o644)
			n++
		}
	}
	return n, nil
}

// ---------- 文件 ----------

func loadLedger() (ledger, error) {
	var lg ledger
	b, err := os.ReadFile(ledgerPath)
	if err != nil {
		if os.IsNotExist(err) {
			return ledger{Note: "发布台账：每条 = 一次对外构建。只追加，不修改、不删除；version.json 由 tools/relgen 从这里生成。"}, nil
		}
		return lg, err
	}
	if err := json.Unmarshal(b, &lg); err != nil {
		return lg, fmt.Errorf("台账不是合法 JSON（%s）：%w", ledgerPath, err)
	}
	return lg, nil
}

func saveLedger(lg ledger) error {
	if err := os.MkdirAll(filepath.Dir(ledgerPath), 0o755); err != nil {
		return err
	}
	return writeJSON(ledgerPath, lg)
}

func writeJSON(path string, v any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path) // 同目录原子替换：写到一半断掉也不会留下半个清单
}

func hashFile(path string) (string, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}

func fileSize(path string) int64 {
	if fi, err := os.Stat(path); err == nil {
		return fi.Size()
	}
	return 0
}

/*
packZip 重打下载包：3z9wOpt_v<ver>/3z9wOpt.exe + 3z9wOpt_v<ver>/使用说明.txt。

	用 Go 的 archive/zip 而不是调系统工具：
	  · PowerShell 5.1 的 Compress-Archive 会把分隔符写成 `\`，unzip 报错；
	  · 中文文件名靠 UTF-8 标志位（Go 自动置位），资源管理器 / 7-Zip 都认。
	先写 .tmp 再改名：中途失败不会留下一个半截的 zip 让用户下走。
*/
func packZip(ver, exePath, outPath, readme string) (int64, error) {
	rb, err := os.ReadFile(readme)
	if err != nil {
		return 0, fmt.Errorf("读不到说明文件（%s）：%w", readme, err)
	}
	if err := os.MkdirAll(filepath.Dir(outPath), 0o755); err != nil {
		return 0, err
	}
	tmp := outPath + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return 0, err
	}
	zw := zip.NewWriter(f)
	dir := "3z9wOpt_v" + ver

	src, err := os.Open(exePath)
	if err != nil {
		zw.Close()
		f.Close()
		os.Remove(tmp)
		return 0, err
	}
	now := time.Now()
	/* ⚠️ Flags 必须自己置 0x800（UTF-8 文件名标志）。
	   实测 go 的 zip.Writer 不会自动置位：打出来的包 general purpose flag 只有 0x0008
	   （数据描述符位），unzip / 7-Zip 会把「使用说明.txt」当 CP437 解成乱码，
	   只有中文 Windows 的资源管理器因为按本地码页猜才看着正常。
	   顺带补 Modified：不填就是 1980-00-00，解压出来一堆 1980 年的文件。 */
	hdr := &zip.FileHeader{Name: dir + "/3z9wOpt.exe", Method: zip.Deflate, Modified: now, Flags: 0x800}
	w, err := zw.CreateHeader(hdr)
	if err == nil {
		_, err = io.Copy(w, src)
	}
	src.Close()
	if err != nil {
		zw.Close()
		f.Close()
		os.Remove(tmp)
		return 0, err
	}

	if w, err := zw.CreateHeader(&zip.FileHeader{
		Name: dir + "/使用说明.txt", Method: zip.Deflate, Modified: now, Flags: 0x800,
	}); err == nil {
		_, _ = w.Write(rb)
	}
	if err := zw.Close(); err != nil {
		f.Close()
		os.Remove(tmp)
		return 0, err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return 0, err
	}
	if err := os.Rename(tmp, outPath); err != nil {
		os.Remove(tmp)
		return 0, err
	}
	return fileSize(outPath), nil
}

// readNotes 一行一条，去掉空行与行首尾空白；文件不存在返回空（调用方回落到上一次的说明）
func readNotes(path string) []string {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var out []string
	for _, line := range strings.Split(strings.ReplaceAll(string(b), "\r\n", "\n"), "\n") {
		line = strings.TrimSpace(strings.TrimLeft(line, "-*·"))
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		out = append(out, line)
	}
	return out
}

func lastNotes(lg ledger) []string {
	for i := len(lg.Builds) - 1; i >= 0; i-- {
		if len(lg.Builds[i].Notes) > 0 {
			return lg.Builds[i].Notes
		}
	}
	return []string{}
}
