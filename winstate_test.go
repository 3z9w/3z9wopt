package main

/* 窗口几何记忆的单测护栏（2026-09-30）
   这里盯的是「记忆」这个功能最容易坑人的两条：
     1. 换显示器 / 拔外接屏后，旧记录不能把窗口送到屏幕外或者撑得比屏幕还大；
     2. 读到的旧记录再脏（负数、0、超大）也不能让软件起不来。
   真·窗口操作那部分（runtime.WindowMaximise 之类）跑不了无头测试，交给桌面端实测。 */

import (
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"testing"
)

func TestWinStateSaveLoadRoundTrip(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("LOCALAPPDATA", dir)

	if _, err := loadWinState(); err == nil {
		t.Fatal("首次运行不该有旧记录，读不到应该报错")
	}

	want := winState{W: 1440, H: 900, X: 210, Y: 130, Max: false}
	if err := saveWinState(want); err != nil {
		t.Fatalf("存失败: %v", err)
	}

	p := filepath.Join(dir, "3z9wOpt", "winstate.json")
	if _, err := os.Stat(p); err != nil {
		t.Fatalf("记录应该落在 %%LOCALAPPDATA%%\\3z9wOpt\\winstate.json，实际没有: %v", err)
	}

	got, err := loadWinState()
	if err != nil {
		t.Fatalf("读回来失败: %v", err)
	}
	if got != want {
		t.Fatalf("往返不一致: 存 %+v 读 %+v", want, got)
	}
}

func TestLoadWinStateRejectsGarbage(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("LOCALAPPDATA", dir)
	_ = os.MkdirAll(filepath.Join(dir, "3z9wOpt"), 0o755)

	bad := []string{
		`{,`,                                        // 坏 JSON
		`{"w":0,"h":0,"x":0,"y":0,"max":false}`,     // 空尺寸（最小化时误存出来的脏数据）
		`{"w":-5,"h":780,"x":0,"y":0,"max":false}`,  // 负数
	}
	for i, b := range bad {
		if err := os.WriteFile(filepath.Join(dir, "3z9wOpt", "winstate.json"), []byte(b), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := loadWinState(); err == nil {
			t.Fatalf("第 %d 份脏记录应该被挡回去，实际被当成有效记录: %s", i+1, b)
		}
	}
}

func TestFitWinState(t *testing.T) {
	full := winRect32{0, 0, 1920, 1040} // 主显示器可用区（已扣掉 40px 任务栏）

	cases := []struct {
		name string
		wa   winRect32
		in   winState
		want winState
	}{
		{"正常记录原样保留", full,
			winState{W: 1220, H: 780, X: 350, Y: 130},
			winState{W: 1220, H: 780, X: 350, Y: 130}},

		{"最大化记录不做修正，交给 WindowMaximise", full,
			winState{W: 1000, H: 700, X: -50, Y: -50, Max: true},
			winState{W: 1000, H: 700, X: -50, Y: -50, Max: true}},

		{"换到小显示器：尺寸夹到可用区以内", winRect32{0, 0, 1366, 768},
			winState{W: 1600, H: 900, X: 100, Y: 100},
			winState{W: 1366, H: 768, X: 0, Y: 0}},

		{"窗口跑到左上角屏幕外：拉回来", full,
			winState{W: 1220, H: 780, X: -400, Y: -200},
			winState{W: 1220, H: 780, X: 0, Y: 0}},

		{"窗口探出右下角：回收到刚好贴边", full,
			winState{W: 1220, H: 780, X: 1500, Y: 900},
			winState{W: 1220, H: 780, X: 700, Y: 260}},

		{"脏得离谱的旧记录：兜到设计最小值", full,
			winState{W: 800, H: 500, X: 0, Y: 0},
			winState{W: winMinW, H: winMinH, X: 0, Y: 0}},

		{"副屏在左边时以该负坐标为准", winRect32{-1920, 0, 0, 1040},
			winState{W: 1220, H: 780, X: -3000, Y: 100},
			winState{W: 1220, H: 780, X: -1920, Y: 100}},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := fitWinState(c.in, c.wa)
			if got != c.want {
				t.Fatalf("期望 %+v，实际 %+v", c.want, got)
			}
		})
	}
}

// 窗口内部拉伸以后 restore 一定不能把窗口送出去：展开一条宽1730的曾经最大化的窗口再存一遍看看。
func TestFitWinStateAlwaysVisible(t *testing.T) {
	wa := winRect32{0, 0, 1920, 1040}
	for _, s := range []winState{
		{W: 1440, H: 900, X: 99999, Y: 99999},
		{W: 1920, H: 1040, X: 1, Y: 1},
		{W: 1, H: 1, X: -1, Y: -1},
	} {
		got := fitWinState(s, wa)
		if got.X < int(wa.Left) || got.Y < int(wa.Top) {
			t.Fatalf("左上角跑到工作区外了: %+v", got)
		}
		if got.X+got.W > int(wa.Right) || got.Y+got.H > int(wa.Bottom) {
			t.Fatalf("窗口底边/右边超出工作区了: %+v", got)
		}
		if got.W < winMinW || got.H < winMinH {
			t.Fatalf("比设计最小值还小了: %+v", got)
		}
	}
}

// 跟 main.go 的 Width/Height/MinWidth/MinHeight 对齐做断言：
// 将来有人只改一边（比如把默认尺寸调大却忘了 winstate.go 的下限），会在这儿炸。
func TestWinSizeConstantsMatchMain(t *testing.T) {
	b, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatal(err)
	}
	src := string(b)
	pick := func(key string) int {
		m := regexp.MustCompile(key + `:\s*(\d+)`).FindStringSubmatch(src)
		if m == nil {
			t.Fatalf("main.go 里找不到 %s 后面的数字", key)
		}
		n, _ := strconv.Atoi(m[1])
		return n
	}
	defW, defH := pick("Width"), pick("Height")
	minW, minH := pick("MinWidth"), pick("MinHeight")

	if minW != winMinW || minH != winMinH {
		t.Fatalf("main.go 的 MinWidth/MinHeight 是 %d/%d，winstate.go 里写的是 %d/%d，两边必须一致",
			minW, minH, winMinW, winMinH)
	}
	if defW < winMinW || defH < winMinH {
		t.Fatalf("main.go 默认尺寸 %dx%d 比最小尺寸 %dx%d 还小，用户一拉就弹回去",
			defW, defH, winMinW, winMinH)
	}
}
