package main

import (
	"strings"
	"testing"
)

// 验证：Windows 点名的"拖慢项"能不能对上启动项列表里的项。
// 这决定了能不能做"测出拖慢项 → 一键操作"，必须先拿真实数据说话。
func TestBootMatchRate(t *testing.T) {
	a := &App{}

	bp := a.GetBootPerf()
	if !bp.Ok {
		t.Fatalf("GetBootPerf 失败: %s", bp.Err)
	}
	items, err := a.ListStartup()
	if err != nil {
		t.Fatalf("ListStartup 失败: %v", err)
	}
	t.Logf("启动项总数: %d", len(items))
	t.Logf("开机记录数: %d", len(bp.Records))

	// 建索引：exe 全路径小写 → 项
	byExe := map[string][]StartupItem{}
	for _, it := range items {
		ex := strings.ToLower(cmdToExe(it.Cmd))
		if ex == "" {
			ex = strings.ToLower(strings.Trim(it.Cmd, `"`))
		}
		if ex != "" {
			byExe[ex] = append(byExe[ex], it)
		}
	}
	t.Logf("可解析出 exe 路径的启动项: %d / %d", len(byExe), len(items))

	// 最近 3 次开机逐个看匹配情况
	for ri, r := range bp.Records {
		if ri >= 3 {
			break
		}
		t.Logf("\n===== 第 %d 次开机 mainPath=%dms post=%dms 点名 %d 项 =====",
			ri+1, r.MainPathMs, r.PostBootMs, len(r.Slowest))
		hit := 0
		for _, c := range r.Slowest {
			p := strings.ToLower(strings.Trim(c.Path, `"`))
			base := p
			if i := strings.LastIndexAny(p, `\/`); i >= 0 {
				base = p[i+1:]
			}
			m := byExe[p]
			if len(m) == 0 {
				// 退一步：按文件名匹配（路径可能带引号 / 大小写差异）
				for k, v := range byExe {
					if strings.HasSuffix(k, base) {
						m = append(m, v...)
					}
				}
			}
			if len(m) > 0 {
				hit++
				for _, it := range m {
					t.Logf("  ✅ %-28s %6.1fs | kind=%s 列表项=[%s]%s id=%s enabled=%v advice=%q",
						c.Name, float64(c.TotalMs)/1000, c.Kind, it.Kind, it.Name, it.ID, it.Enabled, it.Advice)
				}
			} else {
				t.Logf("  ❌ %-28s %6.1fs | kind=%s path=%s —— 列表里没有",
					c.Name, float64(c.TotalMs)/1000, c.Kind, c.Path)
			}
		}
		t.Logf("  匹配 %d / %d", hit, len(r.Slowest))
	}

	// 反向看：列表里有多少项被 Windows 点名过（累计最近一次）
	if len(bp.Records) > 0 {
		t.Logf("\n===== 本次开机被点名且能一键操作的项 =====")
		r := bp.Records[0]
		n := 0
		for _, c := range r.Slowest {
			p := strings.ToLower(strings.Trim(c.Path, `"`))
			for _, it := range byExe[p] {
				if it.Enabled && it.Kind != "svc" {
					n++
					t.Logf("  可一键禁用: [%s] %s (%s)", it.Kind, it.Name, it.ID)
				}
			}
		}
		t.Logf("合计可操作: %d", n)
	}
}
