package main

// ============================================================================
// 3z9wOpt v1.1 · Windows 命令与系统目录解析
//
// 这两个函数在 v1.0 里属于 toolkit.go，但**驱动管理 / 工具箱 / 蓝屏分析都要用**，
// 所以按 1.1 的拆法单独放这里（谁要谁用，不为一个函数把整个工具箱拖进来）。
// 逻辑照 v1.0 原样，未改行为。
// ============================================================================

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"unicode/utf8"

	"golang.org/x/text/encoding/simplifiedchinese"
)

// resolveSystemExe 解析 Windows 自带命令行工具（pnputil / powershell / dism 等）的完整路径，
// 优先用原生 System32，确保 32 位编译的程序也能调到正确的原生（64 位）版本。
// 带路径分隔符或 System32 里确实没有的，原样返回交给系统兜底。
func resolveSystemExe(name string) string {
	if name == "" || strings.ContainsAny(name, `\/`) {
		return name
	}
	cands := []string{filepath.Join(nativeSystem32(), name)}
	root := os.Getenv("SystemRoot")
	if root == "" {
		root = `C:\Windows`
	}
	cands = append(cands, filepath.Join(root, "System32", name), name)
	for _, c := range cands {
		if c == name {
			return name
		}
		if _, err := os.Stat(c); err == nil {
			return c
		}
	}
	return name
}

// decodeConsole 控制台输出解码：优先按 UTF-8，失败再按 GBK。
// 中文系统的 sfc / dism / netstat 等原生输出是 GBK，直接当 UTF-8 读会全是乱码。
func decodeConsole(b []byte) string {
	if len(b) == 0 {
		return ""
	}
	if utf8.Valid(b) {
		return string(b)
	}
	if s, err := simplifiedchinese.GBK.NewDecoder().Bytes(b); err == nil {
		return strings.TrimRight(string(s), "\x00")
	}
	return string(b)
}

// resolveExe 把裸命令名解析成 System32 下的绝对路径。
// 为什么必须这么做：
//  1. 精简版系统 / PATH 残缺的进程里，exec.Command 走 PATH 查找会直接失败
//     （实测报 executable file not found in %PATH%，工具点了没反应）；
//  2. 本程序以管理员身份运行，裸命令名存在 PATH 劫持风险 —— 一旦被劫持，
//     等于把管理员权限交给别人。
//
// 带路径分隔符的、或 System32 里确实没有的，原样返回交给系统兜底。
func resolveExe(name string) string {
	if name == "" || strings.ContainsAny(name, `\/`) {
		return name
	}
	root := os.Getenv("SystemRoot")
	if root == "" {
		root = `C:\Windows`
	}
	p := filepath.Join(root, "System32", name)
	if _, err := os.Stat(p); err == nil {
		return p
	}
	return name
}

// nativeSystem32 拿到**真正的（原生位数的）System32** 路径，绕开 32 位进程的 WOW64 文件重定向。
// 32 位程序访问 %SystemRoot%\System32 会被系统重定向到 SysWOW64，那里缺少部分系统工具
// （或行为不一致），而 DISM++ 之类工具用的是原生 64 位版本。用 Sysnative 这个只有 32 位进程
// 可见的别名，可以直达真实 System32。64 位进程不需要重定向，Sysnative 也不存在，直接用 System32。
func nativeSystem32() string {
	root := os.Getenv("SystemRoot")
	if root == "" {
		root = `C:\Windows`
	}
	if runtime.GOARCH == "386" {
		if p := filepath.Join(root, "Sysnative"); dirExists(p) {
			return p
		}
	}
	return filepath.Join(root, "System32")
}
