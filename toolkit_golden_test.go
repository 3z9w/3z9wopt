package main

// 工具库规格快照：把 32 个工具的完整定义 + 各参数下的执行步骤dump成 JSON 基线。
// 作用：
//   1. 重构/改代码时，跑一次就知道"工具规格有没有被意外改动"；
//   2. 以后有意增删改工具时，用 GOLDEN_UPDATE=1 刷新基线，diff 一眼看清改了哪些字段。
// 刷新：GOLDEN_UPDATE=1 go test -run TestToolGolden .
import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

const goldenPath = "testdata/golden_tools.json"

// 采样参数：覆盖空参数、端口、盘符、域名等各分支
var goldenArgs = []string{"", "8080", "D:", "C:", "www.baidu.com", "s:60", "taskmgr"}

type goldenStep struct {
	Title string   `json:"title,omitempty"`
	Exe   string   `json:"exe"`
	Args  []string `json:"args,omitempty"`
	Stdin string   `json:"stdin,omitempty"`
	Silent string  `json:"silent,omitempty"`
}

type goldenTool struct {
	ToolItem
	Impl  string                  `json:"impl"`  // custom = 定制实现；steps = 按步骤表
	Steps map[string][]goldenStep `json:"steps"` // key = 采样参数
}

func dumpGolden() []goldenTool {
	buildCatalog()
	var out []goldenTool
	for _, it := range toolCatalog {
		g := goldenTool{ToolItem: it, Steps: map[string][]goldenStep{}}
		impl := toolImpls[it.ID]
		if impl.Run != nil {
			g.Impl = "custom"
		} else {
			g.Impl = "steps"
		}
		for _, a := range goldenArgs {
			var plan []toolStep
			switch {
			case impl.Run != nil:
				plan = nil
			case impl.Plan != nil:
				plan = impl.Plan(a)
			default:
				plan = impl.Steps
			}
			steps := make([]goldenStep, 0, len(plan))
			for _, s := range plan {
				steps = append(steps, goldenStep{Title: s.title, Exe: s.exe, Args: s.args, Stdin: s.stdin, Silent: s.silent})
			}
			g.Steps[a] = steps
		}
		out = append(out, g)
	}
	return out
}

func TestToolGolden(t *testing.T) {
	got := dumpGolden()
	blob, err := json.MarshalIndent(got, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if os.Getenv("GOLDEN_UPDATE") != "" {
		if err := os.MkdirAll(filepath.Dir(goldenPath), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(goldenPath, blob, 0o644); err != nil {
			t.Fatal(err)
		}
		t.Logf("已刷新基线：%s（%d 个工具）", goldenPath, len(got))
		return
	}
	want, err := os.ReadFile(goldenPath)
	if err != nil {
		t.Fatalf("读不到基线 %s（首次可跑 GOLDEN_UPDATE=1 go test -run TestToolGolden . 生成）：%v", goldenPath, err)
	}
	var wantTools []goldenTool
	if err := json.Unmarshal(want, &wantTools); err != nil {
		t.Fatal(err)
	}
	if len(wantTools) != len(got) {
		t.Fatalf("工具数量变了：基线 %d，现在 %d", len(wantTools), len(got))
	}
	for i := range got {
		if !reflect.DeepEqual(got[i], wantTools[i]) {
			g1, _ := json.MarshalIndent(got[i], "", "  ")
			g2, _ := json.MarshalIndent(wantTools[i], "", "  ")
			t.Fatalf("工具 %s 的规格与基线不一致\n—— 现在 ——\n%s\n—— 基线 ——\n%s", got[i].ID, g1, g2)
		}
	}
}
