package main

// ============================================================================
// 3z9wOpt v1.1 · 注册表垃圾 / 失效快捷方式（2026-10-09 从 v1.0 junkreg.go 整文件复制）
// 与扫描侧共用：regItemMeasure / badlnkMeasure 给体积，regItemClean / badlnkClean 负责执行。
// ============================================================================

// 注册表垃圾 / 失效快捷方式 / 使用痕迹 —— 2026-09-20 补齐。
// 原则（宁漏勿错）：只报「确定失效」的项 —— 指向的文件已不存在；
// 可移动盘/网络盘上的目标一律不判死（设备没插 ≠ 垃圾）。

import (
	"context"
	"fmt"
	"os"
	"regexp"
	"strings"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows/registry"
)

// regRef 一条待清理的注册表引用：Value 为空表示删整键，否则只删该值（共享容器键专用）
type regRef struct {
	Key   string
	Value string
	Note  string
}

var k32 = syscall.NewLazyDLL("kernel32.dll")

// driveExcluded 目标在可移动盘/网络盘/光驱上 → 不判死（设备未接入不代表垃圾）
func driveExcluded(path string) bool {
	if len(path) < 3 || path[1] != ':' {
		return false
	}
	u16, err := syscall.UTF16PtrFromString(path[:3])
	if err != nil {
		return false
	}
	t, _, _ := k32.NewProc("GetDriveTypeW").Call(uintptr(unsafe.Pointer(u16)))
	// DRIVE_REMOVABLE=2 DRIVE_REMOTE=4 DRIVE_CDROM=5
	return t == 2 || t == 4 || t == 5
}

func fileExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && !st.IsDir()
}

func dirExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.IsDir()
}

// storePath 商店应用（WindowsApps）路径：ACL 特殊，判定不可靠 → 一律放行不报
func storePath(p string) bool {
	return strings.Contains(strings.ToLower(p), `\windowsapps\`)
}

var expandEnvW = k32.NewProc("ExpandEnvironmentStringsW")

// expandEnv 展开 %VAR% 环境变量（注册表里大量路径是 REG_EXPAND_SZ）
func expandEnv(s string) string {
	if !strings.Contains(s, "%") {
		return s
	}
	u16, err := syscall.UTF16PtrFromString(s)
	if err != nil {
		return s
	}
	n, _, _ := expandEnvW.Call(uintptr(unsafe.Pointer(u16)), 0, 0)
	if n == 0 {
		return s
	}
	buf := make([]uint16, n)
	expandEnvW.Call(uintptr(unsafe.Pointer(u16)), uintptr(unsafe.Pointer(&buf[0])), n)
	return syscall.UTF16ToString(buf)
}

// regGetStr 读默认值/指定值，SZ 与 EXPAND_SZ 都支持，自动展开环境变量
func regGetStr(root registry.Key, sub, name string) string {
	k, err := registry.OpenKey(root, sub, registry.READ)
	if err != nil {
		return ""
	}
	defer k.Close()
	v, vt, err := k.GetStringValue(name)
	if err != nil {
		return ""
	}
	if vt == registry.EXPAND_SZ {
		v = expandEnv(v)
	}
	return strings.TrimSpace(v)
}

// probeCmdTarget 从命令行串里抠出 exe 路径：支持 "带引号 path" args、裸 path args
// 返回值仅为候选路径（调用方再判断存在性）；非绝对路径返回空（无法判定 → 不报）
func probeCmdTarget(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	cand := ""
	if s[0] == '"' {
		if i := strings.Index(s[1:], `"`); i > 0 {
			cand = s[1 : i+1]
		}
	} else {
		cand = s
		if _, err := os.Stat(cand); err != nil {
			// 裸路径带参数：逐个空格截断尝试（带空格的无引号路径极少见，保守处理）
			for i := 0; i < len(s); i++ {
				if s[i] == ' ' {
					if _, err2 := os.Stat(s[:i]); err2 == nil {
						cand = s[:i]
						break
					}
				}
			}
		}
	}
	if cand == "" || len(cand) < 3 || cand[1] != ':' {
		return "" // 相对路径/URL/协议形式 → 不判
	}
	return cand
}

// forValues 遍历某键下全部值，fn 返回非 nil 就收录
func forValues(fullKey string, fn func(name string) *regRef) []regRef {
	root, sub, err := regSplit(fullKey)
	if err != nil {
		return nil
	}
	k, err := registry.OpenKey(root, sub, registry.READ)
	if err != nil {
		return nil
	}
	names, err := k.ReadValueNames(-1)
	k.Close()
	if err != nil {
		return nil
	}
	var out []regRef
	for _, n := range names {
		if r := fn(n); r != nil {
			r.Key = fullKey
			r.Value = n
			out = append(out, *r)
		}
	}
	return out
}

func forSubKeys(fullKey string, fn func(name string) *regRef) []regRef {
	root, sub, err := regSplit(fullKey)
	if err != nil {
		return nil
	}
	k, err := registry.OpenKey(root, sub, registry.READ)
	if err != nil {
		return nil
	}
	names, err := k.ReadSubKeyNames(-1)
	k.Close()
	if err != nil {
		return nil
	}
	var out []regRef
	for _, n := range names {
		if r := fn(n); r != nil {
			r.Key = fullKey + `\` + n
			out = append(out, *r)
		}
	}
	return out
}

// ---------- 各扫描器 ----------

// muiCacheRe MuiCache 值名格式：<exe完整路径>.<FriendlyAppName|ApplicationCompany>
var muiCacheRe = regexp.MustCompile(`^(.+)\.(FriendlyAppName|ApplicationCompany)$`)

func scanMuiCache() []regRef {
	var out []regRef
	for _, kp := range []string{
		`HKCU\Software\Classes\Local Settings\Software\Microsoft\Windows\Shell\MuiCache`,
		`HKLM\SOFTWARE\Classes\Local Settings\Software\Microsoft\Windows\Shell\MuiCache`,
	} {
		out = append(out, forValues(kp, func(name string) *regRef {
			m := muiCacheRe.FindStringSubmatch(name)
			if m == nil {
				return nil
			}
			p := m[1]
			if len(p) < 3 || p[1] != ':' || driveExcluded(p) || fileExists(p) {
				return nil
			}
			return &regRef{Note: p}
		})...)
	}
	return out
}

func scanAppPaths() []regRef {
	var out []regRef
	for _, base := range []string{
		`HKLM\SOFTWARE\Microsoft\Windows\CurrentVersion\App Paths`,
		`HKCU\Software\Microsoft\Windows\CurrentVersion\App Paths`,
	} {
		out = append(out, forSubKeys(base, func(name string) *regRef {
			target := probeCmdTarget(regGetStr(mustRoot(base), keyTail(base, name), ""))
			if target == "" || driveExcluded(target) || fileExists(target) {
				return nil
			}
			return &regRef{Note: target}
		})...)
	}
	return out
}

func scanUninstLeft() []regRef {
	var out []regRef
	for _, base := range []string{
		`HKLM\SOFTWARE\Microsoft\Windows\CurrentVersion\Uninstall`,
		`HKLM\SOFTWARE\WOW6432Node\Microsoft\Windows\CurrentVersion\Uninstall`,
		`HKCU\Software\Microsoft\Windows\CurrentVersion\Uninstall`,
	} {
		out = append(out, forSubKeys(base, func(name string) *regRef {
			if strings.HasPrefix(strings.ToUpper(name), "KB") {
				return nil // 系统补丁条目不碰
			}
			if regGetStr(mustRoot(base), keyTail(base, name), "SystemComponent") != "" {
				return nil // 系统组件标记 → 不碰（宁漏勿错）
			}
			un := regGetStr(mustRoot(base), keyTail(base, name), "UninstallString")
			if strings.Contains(strings.ToLower(un), "msiexec") {
				return nil // MSI 托管的交给 Windows Installer
			}
			target := probeCmdTarget(un)
			if target == "" || fileExists(target) {
				return nil
			}
			if loc := regGetStr(mustRoot(base), keyTail(base, name), "InstallLocation"); loc != "" {
				if dirExists(expandEnv(strings.Trim(loc, `"`))) {
					return nil // 安装目录还在 → 可能只是卸载器被删，保守不报
				}
			}
			dn := regGetStr(mustRoot(base), keyTail(base, name), "DisplayName")
			return &regRef{Note: dn}
		})...)
	}
	return out
}

func scanClsidDangling() []regRef {
	var out []regRef
	for _, base := range []string{
		`HKLM\SOFTWARE\Classes\CLSID`,
		`HKLM\SOFTWARE\WOW6432Node\Classes\CLSID`,
		`HKCU\Software\Classes\CLSID`,
	} {
		out = append(out, forSubKeys(base, func(name string) *regRef {
			seen, alive := false, false
			for _, sk := range []string{"InprocServer32", "LocalServer32"} {
				p := probeCmdTarget(regGetStr(mustRoot(base), keyTail(base, name)+`\`+sk, ""))
				if p == "" {
					continue
				}
				seen = true
				if driveExcluded(p) || fileExists(p) {
					alive = true
					break
				}
			}
			if !seen || alive {
				return nil
			}
			return &regRef{Note: name}
		})...)
	}
	return out
}

func scanSharedDLLs() []regRef {
	return forValues(`HKLM\SOFTWARE\Microsoft\Windows\CurrentVersion\SharedDLLs`, func(name string) *regRef {
		if len(name) < 3 || name[1] != ':' || driveExcluded(name) || fileExists(name) {
			return nil
		}
		return &regRef{Note: name}
	})
}

// ---------- 使用痕迹 ----------

func scanRunMRU() []regRef {
	return forValues(`HKCU\Software\Microsoft\Windows\CurrentVersion\Explorer\RunMRU`, func(string) *regRef {
		return &regRef{}
	})
}

func scanTypedPaths() []regRef {
	return forValues(`HKCU\Software\Microsoft\Windows\CurrentVersion\Explorer\TypedPaths`, func(string) *regRef {
		return &regRef{}
	})
}

func scanOpenSave() []regRef {
	return forSubKeys(`HKCU\Software\Microsoft\Windows\CurrentVersion\Explorer\ComDlg32\OpenSavePidlMRU`, func(string) *regRef {
		return &regRef{}
	})
}

func scanOfficeMRU() []regRef {
	var out []regRef
	officeRoot := `HKCU\Software\Microsoft\Office`
	for _, ver := range []string{"14.0", "15.0", "16.0", "17.0"} {
		for _, app := range []string{"Word", "Excel", "PowerPoint", "Access", "Publisher", "OneNote"} {
			out = append(out, forValues(officeRoot+`\`+ver+`\`+app+`\File MRU`, func(string) *regRef {
				return &regRef{}
			})...)
		}
	}
	return out
}

// ---------- 注册表根/相对子键辅助（forValues/forSubKeys 回调里重新打开键用） ----------

func mustRoot(full string) registry.Key {
	r, _, err := regSplit(full)
	if err != nil {
		return registry.CURRENT_USER
	}
	return r
}

// keyTail 返回 full 基础上的相对子键（base 根 + tail）：这里用于「base\<name>」拼装
func keyTail(base, name string) string {
	_, sub, err := regSplit(base)
	if err != nil {
		return name
	}
	return sub + `\` + name
}

// ---------- 扫描器注册表（ID 后缀 → 函数） ----------

var regScanners = map[string]func() []regRef{
	"mui":        scanMuiCache,
	"apppath":    scanAppPaths,
	"uninst":     scanUninstLeft,
	"clsid":      scanClsidDangling,
	"shared":     scanSharedDLLs,
	"runmru":     scanRunMRU,
	"typedpaths": scanTypedPaths,
	"opensave":   scanOpenSave,
	"officemru":  scanOfficeMRU,
}

// regItemMeasure 扫描项计量：注册表垃圾不按字节算（省不出磁盘空间），按「条数」呈现
func regItemMeasure(item *JunkItem) {
	fn := regScanners[strings.TrimPrefix(item.Cmd, "reg:")]
	if fn == nil {
		return
	}
	item.Files = len(fn())
	item.Bytes = 0
}

// regItemClean 重扫一遍拿新鲜清单再删（不能信扫描时的旧数据），失败的计数如实上报
func regItemClean(ctx context.Context, cmd string) (int64, string) {
	fn := regScanners[strings.TrimPrefix(cmd, "reg:")]
	if fn == nil {
		return 0, ""
	}
	refs := fn()
	failed := 0
	for _, r := range refs {
		if ctx.Err() != nil {
			return 0, "已停止"
		}
		var err error
		if r.Value == "" {
			err = delRegTree(r.Key)
		} else {
			err = delRegValue(r.Key, r.Value)
		}
		if err != nil {
			failed++
		}
	}
	if failed > 0 {
		return 0, fmt.Sprintf("%d 项无权限修改，已跳过", failed)
	}
	return 0, ""
}

// ---------- 失效快捷方式（PowerShell WScript.Shell 一次批量解析，Go 端判死） ----------
// 编码注意（2026-09-20 实测踩坑）：中文路径走 PowerShell 控制台输出是 GBK，Go 按 UTF-8 读
// 会把「抖音聊天」这类路径读成乱码 → os.Stat 失败 → 活软件被误判死链。
// 所以必须让 PS 把结果写进 UTF-8 临时文件，Go 直接读文件，不走控制台管道。

/* scanBadLnk 返回 (失效快捷方式, 读取是否成功)。第二个返回值是 2026-10-11 加的：
   原来 PowerShell 跑不起来（精简版/无 PowerShell 的 Server、90 秒超时、PS 语法错）时
   临时文件是空的，函数照样返回 nil —— 调用方把它当成"一个坏链都没有"，界面报"清理成功"。
   现在跑不起来就如实说，不许把"没跑成"报成"清干净了"。 */
func scanBadLnk() ([]string, error) {
	tmp, err := os.CreateTemp("", "badlnk-*.txt")
	if err != nil {
		return nil, err
	}
	tmp.Close()
	defer os.Remove(tmp.Name())
	// 临时文件路径要转义单引号再拼进 PS 字符串字面量（用户名里带 ' 会让 PS 语法错、静默失败）
	outPath := strings.ReplaceAll(tmp.Name(), "'", "''")
	ps := `$list = New-Object System.Collections.Generic.List[string]
$sh = New-Object -ComObject WScript.Shell
$dirs = @([Environment]::GetFolderPath('Desktop'),[Environment]::GetFolderPath('CommonDesktopDirectory'),[Environment]::GetFolderPath('StartMenu'),[Environment]::GetFolderPath('CommonStartMenu'),[Environment]::GetFolderPath('ApplicationData')+'\Microsoft\Internet Explorer\Quick Launch')
foreach($d in $dirs){
  if(-not (Test-Path $d)){continue}
  Get-ChildItem -LiteralPath $d -Recurse -Filter *.lnk | ForEach-Object {
    $t=''
    try{$t=$sh.CreateShortcut($_.FullName).TargetPath}catch{}
    if($t){ $list.Add($_.FullName + [char]9 + $t) }
  }
}
[IO.File]::WriteAllLines('` + outPath + `', $list, (New-Object System.Text.UTF8Encoding($false)))`
	if _, perr := runHiddenOut(90*time.Second, "powershell", "-NoProfile", "-ExecutionPolicy", "Bypass", "-Command", ps); perr != nil {
		return nil, fmt.Errorf("PowerShell 未执行成功（本机可能没有 PowerShell，或 90 秒超时）：%w", perr)
	}
	data, err := os.ReadFile(tmp.Name())
	if err != nil {
		return nil, err
	}
	var dead []string
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimRight(line, "\r")
		i := strings.Index(line, "\t")
		if i <= 0 {
			continue
		}
		lnk, target := strings.TrimSpace(line[:i]), expandEnv(strings.TrimSpace(line[i+1:]))
		if target == "" || storePath(target) || strings.Contains(target, "://") || len(target) < 3 || target[1] != ':' {
			continue // 指向网页/URI/商店应用的快捷方式永不判死
		}
		if driveExcluded(target) {
			continue
		}
		if fileExists(target) || dirExists(target) {
			continue
		}
		dead = append(dead, lnk)
	}
	return dead, nil
}

func badlnkMeasure(item *JunkItem) {
	list, err := scanBadLnk()
	if err != nil {
		// 读不到就是读不到：记 trace 备查，界面按 0 项显示 —— 不编数、也不谎报成功
		cleanTrace("badlnk 扫描失败：" + err.Error())
	}
	item.Files = len(list)
	item.Bytes = 0
}

func badlnkClean(ctx context.Context) (int64, string) {
	dead, err := scanBadLnk()
	if err != nil {
		return 0, "读取失败：" + err.Error()
	}
	if len(dead) == 0 {
		return 0, ""
	}
	failed := 0
	for _, p := range dead {
		if ctx.Err() != nil {
			return 0, "已停止"
		}
		if err := os.Remove(p); err != nil {
			failed++
		}
	}
	if failed > 0 {
		return 0, fmt.Sprintf("%d 个快捷方式删除失败", failed)
	}
	return 0, ""
}

// ============================================================================
// 注册表辅助（v1.0 appclean.go 切片：regSplit / delRegTree / delRegValue）
// ============================================================================

func regSplit(full string) (registry.Key, string, error) {
	i := strings.Index(full, `\`)
	if i <= 0 {
		return 0, "", fmt.Errorf("bad reg path: %s", full)
	}
	switch strings.ToUpper(full[:i]) {
	case "HKLM", "HKEY_LOCAL_MACHINE":
		return registry.LOCAL_MACHINE, full[i+1:], nil
	case "HKCU", "HKEY_CURRENT_USER":
		return registry.CURRENT_USER, full[i+1:], nil
	}
	return 0, "", fmt.Errorf("unsupported root: %s", full)
}

func delRegTree(full string) error {
	root, sub, err := regSplit(full)
	if err != nil {
		return err
	}
	_ = delRegTreeInner(root, sub)
	if k, err2 := registry.OpenKey(root, sub, registry.ALL_ACCESS); err2 == nil {
		k.Close()
		if err3 := registry.DeleteKey(root, sub); err3 == nil {
			return nil
		}
	}
	if _, err := registry.OpenKey(root, sub, registry.READ); err != nil {
		return nil // 已经不在了
	}
	return fmt.Errorf("删除失败（可能需要管理员权限）：%s", full)
}

func delRegTreeInner(root registry.Key, sub string) error {
	k, err := registry.OpenKey(root, sub, registry.READ)
	if err == nil {
		names, _ := k.ReadSubKeyNames(-1)
		k.Close()
		for _, n := range names {
			_ = delRegTreeInner(root, sub+`\`+n)
		}
	}
	return registry.DeleteKey(root, sub)
}

// delRegValue 只删某个键下的单个值（MuiCache / Run 启动项等共享容器键专用，
// 绝不能整树删——那会把所有应用的数据一起抹掉）
func delRegValue(full, val string) error {
	root, sub, err := regSplit(full)
	if err != nil {
		return err
	}
	k, err := registry.OpenKey(root, sub, registry.SET_VALUE|registry.QUERY_VALUE)
	if err != nil {
		if _, err2 := registry.OpenKey(root, sub, registry.READ); err2 != nil {
			return nil // 键已经不在了
		}
		return fmt.Errorf("无权限修改：%s", full)
	}
	defer k.Close()
	if err := k.DeleteValue(val); err != nil {
		if _, _, err2 := k.GetValue(val, make([]byte, 0)); err2 != nil {
			return nil // 值已经不在了
		}
		return err
	}
	return nil
}
