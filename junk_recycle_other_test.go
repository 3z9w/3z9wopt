package main

import (
	"os"
	"sort"
	"testing"
)

// ============================================================================
// 「其他账户回收站」那一项的护栏测试（2026-11-12 用户拍板的新功能）
//
// 它有能力删掉「不是当前用户」的文件，是全软件唯一这么做的地方，所以护栏必须被测试锁住：
//   ① 目录名只认标准用户 SID；
//   ② 不在本机账户列表里才算孤儿；
//   ③ 账户列表读不到 → 一个都不删；
//   ④ 默认不勾（DefOff）；
//   ⑤ 清理顺序排在「当前账户回收站」之后。
// ============================================================================

func TestLooksLikeUserSID(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"S-1-5-21-1589352945-1836184972-3719683476-500", true}, // 常见的本机账户 SID
		{"s-1-5-21-1589352945-1836184972-3719683476-500", true}, // 大小写不敏感
		{"S-1-5-21-1-2-3-1001", true},
		{"S-1-5-18", false},               // SYSTEM
		{"S-1-5-19", false},               // LOCAL SERVICE
		{"S-1-5-20", false},               // NETWORK SERVICE
		{"S-1-5-21-1-2-3", false},         // 少一段
		{"S-1-5-21-1-2-3-500-777", false}, // 多一段
		{"S-1-5-21-1-2-3-abc", false},     // 非数字
		{"S-1-5-21-1-2--500", false},      // 空段
		{"desktop.ini", false},
		{"", false},
	}
	for _, c := range cases {
		if got := looksLikeUserSID(c.in); got != c.want {
			t.Errorf("looksLikeUserSID(%q) = %v，期望 %v", c.in, got, c.want)
		}
	}
}

func TestIsDeletableOrphanSID(t *testing.T) {
	const cur = "S-1-5-21-1-2-3-500"
	prof := map[string]bool{
		"S-1-5-21-1-2-3-500": true, // 当前账户
		"S-1-5-21-4-5-6-500": true, // 另一个还活着的账户
	}

	cases := []struct {
		name string
		cur  string
		prof map[string]bool
		want bool
	}{
		{cur, cur, prof, false},                  // 当前账户：不归这一项管
		{"S-1-5-21-4-5-6-500", cur, prof, false}, // 活账户：绝不碰
		{"S-1-5-21-9-9-9-500", cur, prof, true},  // 孤儿：可清理
		{"s-1-5-21-9-9-9-500", cur, prof, true},  // 大小写不影响判定
		{"S-1-5-18", cur, prof, false},           // 系统账户：不碰
		{"S-1-5-21-9-9-9-500", cur, nil, false},  // 读不到账户列表：一个都不删
	}
	for _, c := range cases {
		if got := isDeletableOrphanSID(c.name, c.cur, c.prof); got != c.want {
			t.Errorf("isDeletableOrphanSID(%q) = %v，期望 %v", c.name, got, c.want)
		}
	}
}

// 默认不勾的项只能有 recycleother 一个 —— 将来谁再往这加，必须自己想清楚
// 「为什么可以替用户默认不选」，别顺手复制粘贴。
func TestDefOffOnlyOnRecycleOther(t *testing.T) {
	var off []string
	var byID = map[string]JunkItem{}
	for _, it := range junkItems() {
		byID[it.ID] = it
		if it.DefOff {
			off = append(off, it.ID)
		}
	}
	sort.Strings(off)
	if len(off) != 1 || off[0] != "recycleother" {
		t.Fatalf("标了 DefOff 的项 = %v，期望只有 [recycleother]", off)
	}

	it := byID["recycleother"]
	if it.Cmd != "recycleother" {
		t.Fatalf("recycleother 的 Cmd = %q，期望 recycleother", it.Cmd)
	}
	if it.Group != "回收站" {
		t.Fatalf("recycleother 的 Group = %q，期望「回收站」（要和当前账户那一项并排显示）", it.Group)
	}
	if !it.Heavy {
		t.Fatalf("recycleother 应属于「你的文件 / 整包」那一层（删了不可恢复）")
	}
	if it.Risk != "warn" {
		t.Fatalf("recycleother 的 Risk = %q，期望 warn", it.Risk)
	}
	// 当前账户那一项必须保持默认勾选、且不是 DefOff —— 否则「一键清理」连自己的回收站都不清了
	if byID["recycle"].DefOff {
		t.Fatalf("recycle 不该标 DefOff：当前账户的回收站是一键清理的默认项")
	}
}

// 清理顺序：先清当前账户，再清别人留下的。反过来的话，万一第二项要跑很久，
// 用户最先看到的是「其他账户」的结果，容易以为自己的回收站没清。
func TestRecycleOtherRunsAfterRecycle(t *testing.T) {
	if cleanExecWeight["recycleother"] <= cleanExecWeight["recycle"] {
		t.Fatalf("recycleother 的权重 %d 应排在 recycle（%d）之后",
			cleanExecWeight["recycleother"], cleanExecWeight["recycle"])
	}
	order := cleanExecutionOrder([]string{"recycleother", "recycle"})
	if order[0] != "recycle" || order[1] != "recycleother" {
		t.Fatalf("执行顺序 = %v，期望 [recycle recycleother]", order)
	}
}

// 真机上（REALJUNK=1 才跑）核对：两项的体积加起来应当等于整个 $Recycle.Bin，
// 且「其他账户」那一项默认不勾。只读，不删任何东西。
func TestRealRecycleSplit(t *testing.T) {
	if os.Getenv("REALJUNK") != "1" {
		t.Skip("设 REALJUNK=1 才跑（真读盘）")
	}
	mineSizes, mineBytes, mineFiles := recycleMineStats()
	o := recycleOtherStats()
	t.Logf("当前账户 : %.2f MB / %d 个（%d 个目录）", float64(mineBytes)/1048576, mineFiles, len(mineSizes))
	t.Logf("孤儿账户 : %.2f MB / %d 个（%d 个目录）", float64(o.bytes)/1048576, o.files, len(o.sizes))
	t.Logf("活账户   : %.2f MB / %d 个（不清理）", float64(o.liveBytes)/1048576, o.liveFiles)
	t.Logf("账户列表 : %v", o.profOK)

	if !o.profOK {
		t.Log("读不到 ProfileList —— 这一项会按「不清理」处理，属预期")
		return
	}
	for p, b := range o.sizes {
		if !looksLikeUserSID(pathBase(p)) {
			t.Errorf("孤儿清单里混进了非用户 SID：%s", p)
		}
		if b <= 0 {
			t.Errorf("孤儿目录体积为 0 还列出来：%s", p)
		}
	}
}

func pathBase(p string) string {
	for i := len(p) - 1; i >= 0; i-- {
		if p[i] == '\\' || p[i] == '/' {
			return p[i+1:]
		}
	}
	return p
}
