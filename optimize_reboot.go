package main

// 待重启生效清单（2026-09-21）
//
// 为什么需要：优化项里有 33 项是服务 / 内核 / 驱动 / UAC 级设置，写入成功但**必须重启电脑**才真正生效。
// 用户在结果弹窗点「稍后」之后容易忘掉，于是这里落一条持久记录，优化页顶部常驻提醒条据此显示。
//
// 什么时候自动消失：检测到系统已经重启过就自动清掉，不需要用户手动关——
//   - 开机时长比记录时更短 → 一定重启过（主信号）
//   - 预取器 BootId 有值且变了 → 重启过（辅助信号，兼容计时异常）
//
// 存放位置：%LOCALAPPDATA%\3z9wOpt\pending_reboot.json（与图标缓存同目录，跟着用户走）

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/windows/registry"
)

// RebootPending 待重启生效清单
type RebootPending struct {
	N      int      `json:"n"`      // 待生效项数
	Items  []string `json:"items"`  // 项名（展示用，冗余 N 说明总数）
	At     string   `json:"at"`     // 记录时间（给人看的）
	Uptime int64    `json:"uptime"` // 记录时的开机时长（毫秒）
	BootID string   `json:"bootId"` // 记录时的预取器引导会话号（读不到为空串）
}

var (
	tickDLL  = syscall.NewLazyDLL("kernel32.dll")
	procTick = tickDLL.NewProc("GetTickCount64")
)

// systemUptimeMs 系统已开机时长（毫秒）；调用失败返回 0（调用方按「不可判定」处理）
func systemUptimeMs() int64 {
	r, _, _ := procTick.Call()
	return int64(r)
}

// systemBootID 读预取器记录的引导会话编号（每次启动递增）；读不到返回空串
func systemBootID() string {
	k, err := registry.OpenKey(registry.LOCAL_MACHINE,
		`SYSTEM\CurrentControlSet\Control\Session Manager\Memory Management\PrefetchParameters`, registry.QUERY_VALUE)
	if err != nil {
		return ""
	}
	defer k.Close()
	v, _, err := k.GetIntegerValue("BootId")
	if err != nil {
		return ""
	}
	return strconv.FormatUint(v, 10)
}

func rebootStatePath() string {
	d := filepath.Join(os.Getenv("LOCALAPPDATA"), "3z9wOpt")
	_ = os.MkdirAll(d, 0o755)
	return filepath.Join(d, "pending_reboot.json")
}

// readPendingReboot 读清单；文件不存在或损坏一律按「无待重启项」处理，绝不因此报错打断界面
func readPendingReboot() RebootPending {
	var st RebootPending
	b, err := os.ReadFile(rebootStatePath())
	if err != nil {
		return st
	}
	_ = json.Unmarshal(b, &st)
	return st
}

func writePendingReboot(st RebootPending) {
	b, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return
	}
	_ = os.WriteFile(rebootStatePath(), b, 0o644)
}

// rebootedSince 判断记录之后系统是否重启过
func rebootedSince(st RebootPending) bool {
	if st.Uptime > 0 && systemUptimeMs() < st.Uptime {
		return true
	}
	if cur := systemBootID(); cur != "" && st.BootID != "" && cur != st.BootID {
		return true
	}
	return false
}

// addPendingReboot 把若干"需重启才生效"的项名写进清单。
// 优化模块（按优化项过滤后）与工具库（按工具的 Reboot 标记）共用这一条入口，
// 保证提醒条、自动消失逻辑只有一份实现。
func addPendingReboot(names []string) {
	var add []string
	for _, n := range names {
		if strings.TrimSpace(n) != "" {
			add = append(add, n)
		}
	}
	if len(add) == 0 {
		return
	}
	st := readPendingReboot()
	if st.At != "" && rebootedSince(st) {
		st = RebootPending{} // 上次那条是重启前记的，机器已经重启过，丢弃重记
	}
	seen := map[string]bool{}
	var all []string
	for _, n := range append(st.Items, add...) {
		if n == "" || seen[n] {
			continue
		}
		seen[n] = true
		all = append(all, n)
	}
	if len(all) > 40 { // 优化目录里需重启的项最多 33 项，40 是安全上限
		all = all[:40]
	}
	st.N = len(all)
	st.Items = all
	st.At = time.Now().Format("2006-01-02 15:04:05")
	st.Uptime = systemUptimeMs()
	st.BootID = systemBootID()
	writePendingReboot(st)
}

// recordPendingReboot 把本次成功执行、且需重启才生效的项累加进清单（应用方向）
func recordPendingReboot(res []OptResult) {
	known := map[string]*OptItem{}
	for i := range optCatalog {
		known[optCatalog[i].ID] = &optCatalog[i]
	}
	var add []string
	for _, r := range res {
		if !r.OK {
			continue
		}
		if it, ok := known[r.ID]; ok && itemNeedsReboot(it) {
			add = append(add, it.Name)
		}
	}
	addPendingReboot(add)
}

// clearPendingReboot 用户把需重启的项还原回默认 → 从清单里摘掉（还原方向）
func clearPendingReboot(res []OptResult) {
	st := readPendingReboot()
	if st.N == 0 {
		return
	}
	if rebootedSince(st) {
		_ = os.Remove(rebootStatePath())
		return
	}
	known := map[string]*OptItem{}
	for i := range optCatalog {
		known[optCatalog[i].ID] = &optCatalog[i]
	}
	drop := map[string]bool{}
	for _, r := range res {
		if !r.OK {
			continue
		}
		if it, ok := known[r.ID]; ok && itemNeedsReboot(it) {
			drop[it.Name] = true
		}
	}
	if len(drop) == 0 {
		return
	}
	var keep []string
	for _, n := range st.Items {
		if !drop[n] {
			keep = append(keep, n)
		}
	}
	st.Items = keep
	st.N = len(keep)
	if st.N == 0 {
		_ = os.Remove(rebootStatePath())
		return
	}
	writePendingReboot(st)
}

// GetPendingReboot 前端读待重启提醒（每次进优化页 / 应用完成后调用）。
// 检测到系统已重启则自动清除并返回空，做到「重启后自动消失」。
func (a *App) GetPendingReboot() RebootPending {
	st := readPendingReboot()
	if st.N == 0 {
		return RebootPending{}
	}
	if rebootedSince(st) {
		_ = os.Remove(rebootStatePath())
		return RebootPending{}
	}
	return st
}
