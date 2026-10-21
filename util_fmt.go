package main

// ============================================================================
// 3z9wOpt v1.1 · 小工具（格式化 / 解析）
//
// 这几个函数在 v1.0 里散落在 toolkit.go、space.go 里；1.1 按模块拆分后统一放这里，
// 谁需要谁用，避免为了一个小函数把整个工具箱模块拖进来。
// 逻辑照 v1.0 原样（只搬实现，不改行为）。
// ============================================================================

import (
	"fmt"
	"strings"
	"time"
)

// emptyDash 空串统一显示成「—」：界面上不留空白格，也不拿 0 冒充"有值"
func emptyDash(s string) string {
	if strings.TrimSpace(s) == "" {
		return "—"
	}
	return s
}

// errStr 把 error 折成字符串（nil → 空串），省掉到处 if err != nil 的分支
func errStr(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// shortErr 错误只取第一行、最多 160 字：写进界面/日志时不至于糊一屏
func shortErr(err error) string {
	s := err.Error()
	if i := strings.IndexByte(s, '\n'); i > 0 {
		s = s[:i]
	}
	if len(s) > 160 {
		s = s[:160]
	}
	return s
}

// fmtDuration 时长人话化：3 天 4 小时 12 分钟 / 4 小时 12 分钟 / 12 分钟
func fmtDuration(d time.Duration) string {
	day := d / (24 * time.Hour)
	d -= day * 24 * time.Hour
	h := d / time.Hour
	d -= h * time.Hour
	m := d / time.Minute
	if day > 0 {
		return fmt.Sprintf("%d 天 %d 小时 %d 分钟", day, h, m)
	}
	if h > 0 {
		return fmt.Sprintf("%d 小时 %d 分钟", h, m)
	}
	return fmt.Sprintf("%d 分钟", m)
}

// parseWmiDate 解析 WMI 日期。可能是 CIM_DATETIME（20240115000000.000000+480）、
// 纯日期（20240115），也可能是美式 M/D/YYYY（Win32_QuickFixEngineering.InstalledOn 常见）。
func parseWmiDate(s string) (time.Time, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}, false
	}
	if !strings.ContainsAny(s, "-/") {
		base := s
		if i := strings.IndexByte(s, '.'); i > 0 {
			base = s[:i]
		}
		if len(base) >= 14 {
			if t, err := time.ParseInLocation("20060102150405", base[:14], time.Local); err == nil {
				return t, true
			}
		}
		if len(base) == 8 {
			if t, err := time.ParseInLocation("20060102", base, time.Local); err == nil {
				return t, true
			}
		}
		return time.Time{}, false
	}
	for _, layout := range []string{"2006-01-02 15:04:05", "2006-01-02", "1/2/2006"} {
		if t, err := time.ParseInLocation(layout, s, time.Local); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}
