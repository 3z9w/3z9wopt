package main

// 用户点名补充的常用优化项（2026-09-21）
//
// 为什么单独放一个文件：
//   optimize_data.go 是 _proto/gen_optimize_data.py 从 ZyperWin++ v4.1 ZyperData.xml 生成的产物
//   （文件头写明 DO NOT EDIT），151 项源名单 + 基本 87 / 深度 127 预设属于用户红线（不许擅自增删）。
//   这里以「增量追加」的方式补 3 项常用设置：不污染生成物，也不进任何预设名单（Basic/Deep 均为 false）。
//
// 真机核对结论（Win10 Pro 22H2 / build 19045，2026-09-21）：
//   - 电源「高性能」计划：目录里本来就有（xingneng-29，powercfg -setactive SCHEME_MIN），无需重复添加
//   - 任务栏搜索：原有 explorer-1 只能设 0（隐藏）；这里补 1（仅搜索图标）/ 2（搜索框），凑成完整三档
//   - 资讯和兴趣：原目录完全没有（只有 Edge 新标签页资讯那条，不是任务栏的）
func init() {
	optCatalog = append(optCatalog,
		OptItem{
			ID: "explorer-30", Cat: "explorer", CatName: "外观/资源管理器",
			Name:     "30、关闭「资讯和兴趣」（任务栏新闻与天气）",
			CheckKey: `HKEY_CURRENT_USER\SOFTWARE\Microsoft\Windows\CurrentVersion\Feeds`, CheckValue: "ShellFeedsTaskbarViewMode", Optimized: "2",
			Basic: false, Deep: false,
			Optimize: []OptAction{
				// Win10：任务栏资讯和兴趣项 —— 2 = 隐藏
				{Kind: "regwrite", Key: `HKEY_CURRENT_USER\SOFTWARE\Microsoft\Windows\CurrentVersion\Feeds`, Value: "ShellFeedsTaskbarViewMode", Vtype: "REG_DWORD", Data: "2"},
				// Win10/11：组策略关闭资讯源（Win11 上把小组件资讯也一并关掉）
				{Kind: "regwrite", Key: `HKEY_LOCAL_MACHINE\SOFTWARE\Policies\Microsoft\Windows\Windows Feeds`, Value: "EnableFeeds", Vtype: "REG_DWORD", Data: "0"},
				{Kind: "regwrite", Key: `HKEY_LOCAL_MACHINE\SOFTWARE\Policies\Microsoft\Dsh`, Value: "AllowNewsAndInterests", Vtype: "REG_DWORD", Data: "0"},
				// Win11：隐藏任务栏「小组件」按钮（Win10 无此键，写入无副作用）
				{Kind: "regwrite", Key: `HKEY_CURRENT_USER\SOFTWARE\Microsoft\Windows\CurrentVersion\Explorer\Advanced`, Value: "TaskbarDa", Vtype: "REG_DWORD", Data: "0"},
			},
			Restore: []OptAction{
				{Kind: "regdelete", Key: `HKEY_CURRENT_USER\SOFTWARE\Microsoft\Windows\CurrentVersion\Feeds`, Value: "ShellFeedsTaskbarViewMode"},
				{Kind: "regdelete", Key: `HKEY_LOCAL_MACHINE\SOFTWARE\Policies\Microsoft\Windows\Windows Feeds`, Value: "EnableFeeds"},
				{Kind: "regdelete", Key: `HKEY_LOCAL_MACHINE\SOFTWARE\Policies\Microsoft\Dsh`, Value: "AllowNewsAndInterests"},
				{Kind: "regdelete", Key: `HKEY_CURRENT_USER\SOFTWARE\Microsoft\Windows\CurrentVersion\Explorer\Advanced`, Value: "TaskbarDa"},
			},
		},
		OptItem{
			ID: "explorer-31", Cat: "explorer", CatName: "外观/资源管理器",
			Name:     "31、任务栏搜索：仅显示搜索图标",
			CheckKey: `HKEY_CURRENT_USER\SOFTWARE\Microsoft\Windows\CurrentVersion\Search`, CheckValue: "SearchboxTaskbarMode", Optimized: "1",
			Basic: false, Deep: false,
			Optimize: []OptAction{
				{Kind: "regwrite", Key: `HKEY_CURRENT_USER\SOFTWARE\Microsoft\Windows\CurrentVersion\Search`, Value: "SearchboxTaskbarMode", Vtype: "REG_DWORD", Data: "1"}},
			Restore: []OptAction{
				{Kind: "regdelete", Key: `HKEY_CURRENT_USER\SOFTWARE\Microsoft\Windows\CurrentVersion\Search`, Value: "SearchboxTaskbarMode"}},
		},
		OptItem{
			ID: "explorer-32", Cat: "explorer", CatName: "外观/资源管理器",
			Name:     "32、任务栏搜索：显示搜索框",
			CheckKey: `HKEY_CURRENT_USER\SOFTWARE\Microsoft\Windows\CurrentVersion\Search`, CheckValue: "SearchboxTaskbarMode", Optimized: "2",
			Basic: false, Deep: false,
			Optimize: []OptAction{
				{Kind: "regwrite", Key: `HKEY_CURRENT_USER\SOFTWARE\Microsoft\Windows\CurrentVersion\Search`, Value: "SearchboxTaskbarMode", Vtype: "REG_DWORD", Data: "2"}},
			Restore: []OptAction{
				{Kind: "regdelete", Key: `HKEY_CURRENT_USER\SOFTWARE\Microsoft\Windows\CurrentVersion\Search`, Value: "SearchboxTaskbarMode"}},
		},
		OptItem{
			// 2026-11-12 采纳（豆包 27 条对照新增）：「最佳性能」基底 + 保留缩略图/字体平滑/拖动内容显示。
			// VisualFXSetting=3（自定义）+ UserPreferencesMask 压掉动画类效果（90,12,03,80,10,00,00,00
			// 为社区多年实证配方）；注销或重启资源管理器后完全生效。还原回「让 Windows 选择」。
			ID: "explorer-33", Cat: "explorer", CatName: "外观/资源管理器",
			Name:     "33、视觉效果调整为最佳性能",
			CheckKey: `HKEY_CURRENT_USER\Software\Microsoft\Windows\CurrentVersion\Explorer\VisualEffects`, CheckValue: "VisualFXSetting", Optimized: "3",
			Basic: false, Deep: true,
		New: true,
			Optimize: []OptAction{
				{Kind: "regwrite", Key: `HKEY_CURRENT_USER\Software\Microsoft\Windows\CurrentVersion\Explorer\VisualEffects`, Value: "VisualFXSetting", Vtype: "REG_DWORD", Data: "3"},
				{Kind: "regwrite", Key: `HKEY_CURRENT_USER\Control Panel\Desktop`, Value: "UserPreferencesMask", Vtype: "REG_BINARY", Data: "90,12,03,80,10,00,00,00"},
				{Kind: "regwrite", Key: `HKEY_CURRENT_USER\Control Panel\Desktop\WindowMetrics`, Value: "MinAnimate", Vtype: "REG_SZ", Data: "0"},
				{Kind: "regwrite", Key: `HKEY_CURRENT_USER\Software\Microsoft\Windows\CurrentVersion\Explorer\Advanced`, Value: "TaskbarAnimations", Vtype: "REG_DWORD", Data: "0"},
				{Kind: "regwrite", Key: `HKEY_CURRENT_USER\Software\Microsoft\Windows\CurrentVersion\Explorer\Advanced`, Value: "IconsOnly", Vtype: "REG_DWORD", Data: "0"},
				{Kind: "regwrite", Key: `HKEY_CURRENT_USER\Software\Microsoft\Windows\CurrentVersion\Explorer\Advanced`, Value: "ListviewAlphaSelect", Vtype: "REG_DWORD", Data: "0"},
				{Kind: "regwrite", Key: `HKEY_CURRENT_USER\Software\Microsoft\Windows\CurrentVersion\Explorer\Advanced`, Value: "ListviewShadow", Vtype: "REG_DWORD", Data: "0"},
				{Kind: "regwrite", Key: `HKEY_CURRENT_USER\Control Panel\Desktop`, Value: "FontSmoothing", Vtype: "REG_SZ", Data: "2"},
				{Kind: "regwrite", Key: `HKEY_CURRENT_USER\Control Panel\Desktop`, Value: "FontSmoothingType", Vtype: "REG_DWORD", Data: "2"},
				{Kind: "regwrite", Key: `HKEY_CURRENT_USER\Control Panel\Desktop`, Value: "DragFullWindows", Vtype: "REG_SZ", Data: "1"}},
			Restore: []OptAction{
				{Kind: "regwrite", Key: `HKEY_CURRENT_USER\Software\Microsoft\Windows\CurrentVersion\Explorer\VisualEffects`, Value: "VisualFXSetting", Vtype: "REG_DWORD", Data: "0"},
				{Kind: "regwrite", Key: `HKEY_CURRENT_USER\Control Panel\Desktop`, Value: "UserPreferencesMask", Vtype: "REG_BINARY", Data: "9E,3E,07,80,12,00,00,00"},
				{Kind: "regwrite", Key: `HKEY_CURRENT_USER\Control Panel\Desktop\WindowMetrics`, Value: "MinAnimate", Vtype: "REG_SZ", Data: "1"},
				{Kind: "regwrite", Key: `HKEY_CURRENT_USER\Software\Microsoft\Windows\CurrentVersion\Explorer\Advanced`, Value: "TaskbarAnimations", Vtype: "REG_DWORD", Data: "1"},
				{Kind: "regwrite", Key: `HKEY_CURRENT_USER\Software\Microsoft\Windows\CurrentVersion\Explorer\Advanced`, Value: "IconsOnly", Vtype: "REG_DWORD", Data: "0"},
				{Kind: "regwrite", Key: `HKEY_CURRENT_USER\Software\Microsoft\Windows\CurrentVersion\Explorer\Advanced`, Value: "ListviewAlphaSelect", Vtype: "REG_DWORD", Data: "1"},
				{Kind: "regwrite", Key: `HKEY_CURRENT_USER\Software\Microsoft\Windows\CurrentVersion\Explorer\Advanced`, Value: "ListviewShadow", Vtype: "REG_DWORD", Data: "1"},
				{Kind: "regwrite", Key: `HKEY_CURRENT_USER\Control Panel\Desktop`, Value: "FontSmoothing", Vtype: "REG_SZ", Data: "2"},
				{Kind: "regwrite", Key: `HKEY_CURRENT_USER\Control Panel\Desktop`, Value: "FontSmoothingType", Vtype: "REG_DWORD", Data: "2"},
				{Kind: "regwrite", Key: `HKEY_CURRENT_USER\Control Panel\Desktop`, Value: "DragFullWindows", Vtype: "REG_SZ", Data: "1"}},
		},
	)
}
