package main

// hwbrand.go —— 厂商名中译 + 型号串清洗（2026-09-24）
//
// 用户要求：「表述要用中文表述，让人一眼就看懂」。
//
// ⚠️ 这里的规矩要讲清楚，免得后人拎不清：
//   本文件**只做翻译和去噪，不做规格推断**。
//   「Kingston → 金士顿」是翻译；「看到 99P5732-001.A00G 就说是 DDR4 2400」是编规格，严禁。
//   型号本体（G5400 / H310M DS2 / WD5000AZLX）一律保留原文 —— 这些本来就是型号，
//   硬翻成中文反而认不出（游戏加加、鲁大师也都保留英文型号）。

import (
	"regexp"
	"strings"
)

/* brandCN 厂商名中译。
   匹配原则：从「最具体的写法」往「笼统写法」试，避免 "Intel Corporation" 被截成 "Intel" 后
   又匹配到别的。命不中就返回原文（宁可显示英文，也不猜一个中文名）。 */
var brandRules = []struct {
	key string // 小写关键词
	cn  string
}{
	{"advanced micro devices", "AMD"},
	{"intel", "英特尔"},
	{"amd", "AMD"},
	{"nvidia", "英伟达"},
	{"geforce", "英伟达"},
	{"radeon", "AMD"},
	{"kingston", "金士顿"},
	{"samsung", "三星"},
	{"corsair", "海盗船"},
	{"g.skill", "芝奇"},
	{"crucial", "英睿达"},
	{"micron", "美光"},
	{"sk hynix", "海力士"},
	{"hynix", "海力士"},
	{"adata", "威刚"},
	{"team group", "十铨"},
	{"transcend", "创见"},
	{"patriot", "博帝"},
	{"gigabyte", "技嘉"},
	{"asustek", "华硕"},
	{"asus", "华硕"},
	{"micro-star", "微星"},
	{"msi", "微星"},
	{"asrock", "华擎"},
	{"colorful", "七彩虹"},
	{"galax", "影驰"},
	{"zotac", "索泰"},
	{"sapphire", "蓝宝石"},
	{"western digital", "西部数据"},
	{"wdc", "西部数据"},
	{"seagate", "希捷"},
	{"toshiba", "东芝"},
	{"sandisk", "闪迪"},
	{"kioxia", "铠侠"},
	{"crucial", "英睿达"},
	{"phison", "群联"},
	{"realtek", "瑞昱"},
	{"american megatrends", "AMI"},
	{"phoenix", "凤凰"},
	{"insyde", "系微"},
	{"byosoft", "百敖"},
	{"dell", "戴尔"},
	{"lenovo", "联想"},
	{"hewlett-packard", "惠普"},
	{"hewlett packard", "惠普"},
	{"hp ", "惠普"},
	{"huawei", "华为"},
	{"apple", "苹果"},
	{"acer", "宏碁"},
	{"sony", "索尼"},
	{"shenzhen", "深圳厂商"},
}

// brandCN 把厂商原文翻成中文；认不出来就原样返回
func brandCN(raw string) string {
	s := strings.TrimSpace(raw)
	if s == "" {
		return ""
	}
	low := strings.ToLower(s)
	for _, r := range brandRules {
		if strings.Contains(low, r.key) {
			return r.cn
		}
	}
	return s
}

var (
	reMark  = regexp.MustCompile(`\s*\((?:R|TM|C|r|tm|c)\)`)
	reSpace = regexp.MustCompile(`\s{2,}`)
	reTail  = regexp.MustCompile(`\s+(CPU|Processor)\s*@\s*[\d.]+\s*GHz\s*$`)
	reTail2 = regexp.MustCompile(`\s+@\s*[\d.]+\s*GHz\s*$`)
)

/* cleanModel 型号串清洗：只去掉冗余，不改型号本体。
     "Intel(R) Pentium(R) Gold G5400 CPU @ 3.70GHz" → "Pentium Gold G5400"
     "Intel(R) Core(TM) i5-9400F CPU @ 2.90GHz"    → "Core i5-9400F"
     "AMD Ryzen 5 3600 6-Core Processor"           → "Ryzen 5 3600 6-Core"
   注意：这里**不**把 "Pentium" 翻成「奔腾」—— 型号是给人认的，翻了反而认不出。
   品牌单独一列显示中文（brandCN），跟型号分开放。 */
func cleanModel(raw string) string {
	s := strings.TrimSpace(raw)
	if s == "" {
		return ""
	}
	s = reMark.ReplaceAllString(s, "")   // (R) (TM) (C)
	s = reTail.ReplaceAllString(s, "")   // 尾部 "CPU @ 3.70GHz"
	s = reTail2.ReplaceAllString(s, "")  // 尾部 "@ 3.70GHz"
	s = strings.TrimSuffix(s, " Processor")
	s = strings.TrimSuffix(s, " CPU")
	s = reSpace.ReplaceAllString(s, " ")
	return strings.TrimSpace(s)
}

/* cpuBrand 从 CPU 型号串里摘品牌（中文）。
   CPUID 商标串常是 "Intel(R) Pentium(R) Gold G5400 CPU @ 3.70GHz" 或 "AMD Ryzen 5 3600..."，
   这里只取最前面那个厂商词，不碰后面的型号。 */
func cpuBrand(model string) string {
	return brandCN(model)
}

/* cpuSeries CPU 产品系列的固定中文名。
   这些都是厂商自己定的产品名，属于**翻译**范畴（不是规格），可以翻：
   Pentium=奔腾、Core=酷睿、Ryzen=锐龙… 型号数字（G5400 / i5-9400F）一律不动。
   只用在 CPU 型号上 —— 显卡/硬盘里碰巧出现 "Gold" 这种词不能被翻。 */
var cpuSeries = []struct {
	en string
	cn string
}{
	{"Threadripper", "线程撕裂者"},
	{"Pentium", "奔腾"},
	{"Core", "酷睿"},
	{"Celeron", "赛扬"},
	{"Xeon", "至强"},
	{"Itanium", "安腾"},
	{"Atom", "凌动"},
	{"Ryzen", "锐龙"},
	{"Athlon", "速龙"},
	{"EPYC", "霄龙"},
	{"Platinum", "铂金"},
	{"Bronze", "铜牌"},
	{"Silver", "银牌"},
	{"Gold", "金牌"},
	{"Intel", "英特尔"},
	{"AMD", "AMD"},
}

// cpuModelCN CPU 型号中译：先去噪，再把产品系列翻成中文
func cpuModelCN(raw string) string {
	s := cleanModel(raw)
	if s == "" {
		return ""
	}
	for _, r := range cpuSeries {
		s = wordReplace(s, r.en, r.cn)
	}
	return reSpace.ReplaceAllString(strings.TrimSpace(s), " ")
}

// wordReplace 按整词替换（避免 "Golden" 被当成 "Gold" 翻掉）
func wordReplace(s, en, cn string) string {
	if !strings.Contains(s, en) {
		return s
	}
	re, err := regexp.Compile(`\b` + regexp.QuoteMeta(en) + `\b`)
	if err != nil {
		return s
	}
	return re.ReplaceAllString(s, cn)
}

/* gpuBrand 显卡品牌：先按显卡名里的厂商关键词判，判不出再看 VendorID。
   DXGI 给了 VendorID：0x8086=英特尔、0x10DE=英伟达、0x1002=AMD。 */
func gpuBrand(name string, vendor uint32) string {
	if b := brandCN(name); b != name {
		return b // brandCN 命中了
	}
	switch vendor {
	case 0x8086:
		return "英特尔"
	case 0x10DE:
		return "英伟达"
	case 0x1002:
		return "AMD"
	}
	return ""
}

// brandOf 先查中译表，查不到再退回型号里的原样厂商词（见 brandLoose）
func brandOf(raw string) string {
	s := strings.TrimSpace(raw)
	if s == "" {
		return ""
	}
	if b := brandCN(s); b != s {
		return b
	}
	return brandLoose(s)
}

/* diskBrand 硬盘品牌：型号串一般是 "WDC WD5000AZLX-00ZR6A0" / "LuminouTek 256GB"，
   取第一个空格前的词去查表，查不到就用整个串再试一次。 */
func diskBrand(model string) string {
	s := strings.TrimSpace(model)
	if s == "" {
		return ""
	}
	if i := strings.IndexByte(s, ' '); i > 0 {
		if b := brandCN(s[:i]); b != s[:i] {
			return b
		}
	}
	if b := brandCN(s); b != s {
		return b
	}
	return brandLoose(s)
}

/* brandLoose 查表查不到时的兜底：取第一个「像厂商名」的词原样返回。
   "LuminouTek 256GB" → "LuminouTek"；"256GB SSD" 首词带数字，不算厂商名，返回空。
   为什么不一味留空：部件卡上「品牌」那一格空着，用户会以为没读到 ——
   给原样首词是可信的（那就是型号串里厂商写自己的那个词），比编一个中文名稳妥。 */
func brandLoose(s string) string {
	t := strings.TrimSpace(s)
	if i := strings.IndexByte(t, ' '); i > 0 {
		t = t[:i]
	}
	t = strings.TrimSpace(t)
	if len(t) < 2 {
		return ""
	}
	for _, r := range t {
		if r >= '0' && r <= '9' {
			return "" // 带数字的首词是容量/型号，不是厂商名
		}
	}
	return t
}
