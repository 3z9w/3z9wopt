package main

/* 首页「能力带」的真实数字来源（2026-09-23）。
   首页要在第一屏告诉小白「这软件能干嘛」，靠的是可感知的量级——
   但这些数字必须是真的：写了假的 154/34，用户进到优化页数出来是 151 就穿帮了。
   所以统一由后端按注册表现数，前端一处都不许写死。 */

/* 侧栏徽标要显示「154项/7组」这种口径，所以项数和组数都给 ——
   组数由各自的分类字段现数（优化按 Cat 去重、垃圾按 Group 去重），
   与页面里真正列出来的分组数同源，不会出现「侧栏说 7 组、进去只有 5 组」。 */
type CapabilityStats struct {
	OptCount   int `json:"optCount"`
	OptGroups  int `json:"optGroups"`
	JunkCount  int `json:"junkCount"`
	JunkGroups int `json:"junkGroups"`
	ToolCount  int `json:"toolCount"`
}

// GetCapabilityStats 返回本机这份程序实际注册的能力数量。
func (a *App) GetCapabilityStats() CapabilityStats {
	// 工具目录是懒组装的（sync.Once，首次访问 GetToolCatalog 时才拼）。
	// 首页 mount 往往比工具库页更早，不补这一次就会读到 0、能力带显示成「--」。
	buildCatalog()
	optTotal, optCats, _, _ := OptCatalogCheck()
	junkTotal := 0
	junkGroups := a.GetJunkGroups()
	for _, g := range junkGroups {
		junkTotal += g.Items
	}
	return CapabilityStats{
		OptCount:   optTotal,
		OptGroups:  optCats,
		JunkCount:  junkTotal,
		JunkGroups: len(junkGroups),
		ToolCount:  len(toolCatalog),
	}
}
