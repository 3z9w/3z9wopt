package main

// ============================================================================
// 重启守门（2026-10-15 立，2026-10-16 随「只剩立即重启」重写）
//
// 为什么值得单独写：这个按钮干的事是**把用户的电脑关掉再打开**，
// 是所有清理动作里唯一有可能让人丢东西的一步。
// 这里守的不是"功能对不对"，而是两条安全底线：
//   1. 「/t 0」有且只有一个出处（rebootImmediateArgs）—— 谁再往里加一条构造重启参数的路，
//      这条测试就该红。加一条"带缓冲"的路等于把用户手滑的代价放大一次。
//   2. 不许带 /f 强制关程序 —— 得给用户程序挽留文档的机会。
// 这些断言全落在纯函数和源码文本上，**不碰任何系统状态**（不会真的安排重启）。
// ============================================================================

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
)

// TestRebootImmediateArgsShape 立刻重启的参数长什么样
func TestRebootImmediateArgsShape(t *testing.T) {
	a := rebootImmediateArgs()
	if len(a) < 4 {
		t.Fatalf("参数太少，连 /r /t 0 都凑不齐：%v", a)
	}
	if a[0] != "/r" {
		t.Fatalf("第一个参数必须是 /r（重启），实际是 %q", a[0])
	}
	if a[1] != "/t" || a[2] != "0" {
		t.Fatalf("立即重启必须是 /t 0（用户点的就是「立刻」），实际是 %v", a[:3])
	}
	for _, s := range a {
		if s == "/f" {
			t.Fatal("不许带 /f：强制关掉程序会让用户来不及保存文件，这条底线不能动")
		}
	}
}

// TestRebootArgsHasReason 重启要带上"为什么"，否则系统会弹出一条没头没尾的通知
func TestRebootArgsHasReason(t *testing.T) {
	a := rebootImmediateArgs()
	reason := ""
	for i, s := range a {
		if s == "/c" && i+1 < len(a) {
			reason = a[i+1]
		}
	}
	if reason == "" {
		t.Fatal("重启没带说明文字 —— 用户看到系统通知会不知道是谁要重启、为什么重启")
	}
}

// TestRebootHasOnlyOneWayToBuildArgs 「构造 shutdown 参数」这件事在本包里有且只有一处。
//
// 为什么不用"跑一遍看输出"的方式守（那是上一版的写法）：上一版有"带缓冲"和"立即"两条路，
// 于是测试得穷举入参、逐一断言 0 不许漏出来 —— 可只要有人再复制一个构造函数出来，
// 这些断言就全成了摆设，因为它们只覆盖它认识的那两个函数。
//
// 2026-10-16 用户把带缓冲那条路整个砍了（「10 秒后重启简直就是画蛇添足，直接删掉」），
// 于是这里的守门方式也跟着换：**数源码里出现过几次字符串 "/t"**。
// 只有一个，就说明只有一个出口；多出来一个，就是有人又把缓冲路请回来了。
// 文本层面守"结构"确实土，但它比穷举入参结实 —— 穷举只覆盖它认识的那几个函数。
//
// ⚠️ 用 AST 数而不是 strings.Count：注释里一定会提到 "/t"（本文档就在提），
//    字符串计数会把注释也算进去，第一天就自己把自己判红（2026-10-16 实测踩过）。
//    只认 token.STRING 字面量，注释天然被排除。
func TestRebootHasOnlyOneWayToBuildArgs(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "system_reboot.go", nil, 0)
	if err != nil {
		t.Fatalf("解析 system_reboot.go 失败：%v", err)
	}
	n := 0
	found := false
	ast.Inspect(f, func(nd ast.Node) bool {
		if lit, ok := nd.(*ast.BasicLit); ok && lit.Kind == token.STRING && lit.Value == `"/t"` {
			n++
		}
		if fd, ok := nd.(*ast.FuncDecl); ok && fd.Name.Name == "rebootImmediateArgs" {
			found = true
		}
		return true
	})
	if !found {
		t.Fatal("没找到 rebootImmediateArgs —— 守门测试认的那个唯一出口不见了")
	}
	if n != 1 {
		t.Fatalf(`system_reboot.go 里字符串 "/t" 出现了 %d 次，应当只有 1 次（rebootImmediateArgs）。
多出来的那条一定是"带缓冲的重启"又回来了 —— 用户 2026-10-16 明确要求删掉它，
要加回来请先看文件头那段说明和当天的原话。`, n)
	}
}
