package main

// 全量工具输出编码审计（2026-09-21 用户要求：乱码问题其他工具也要预防核实）
//
// 背景：sfc 的输出是 UTF-16LE，曾经整段变乱码。工具库 32 个工具调用的是 20 多种
// 不同年代的原生命令，编码各不相同（ANSI/GBK、UTF-8、UTF-16、混合），必须逐个核实。
//
// 做法：用 _proto/cap_enc.py 把各命令的真实输出字节抓到 _proto/enc/*.bin，
// 这里用与运行时**完全相同**的解码链（consoleReader → pumpLines → decodeConsole）
// 解码，断言：
//   1. 不出现替换字符 U+FFFD（解码失败的特征）；
//   2. 不残留 0x00（UTF-16 没转干净的特征）；
//   3. 含中文的输出必须解出符合语境的汉字（不是"楠岃瘉"这种 GBK/UTF-8 互串的乱码）。
// 抓包文件不存在时自动跳过（换机器/清理后仍能跑测试）。
//
// 以后新增工具时：把它的真实输出也抓一份进 _proto/enc/，在下面登记期望关键词即可。

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 抓包目录：默认取环境变量 OPT_ENC_CAP，未设置时用系统临时目录下的 opt-enc-cap
// （2026-10-09 开源整理：原来写死开发机的 D:\... 路径）
var encCapDir = func() string {
	if d := os.Getenv("OPT_ENC_CAP"); d != "" {
		return d
	}
	return filepath.Join(os.TempDir(), "opt-enc-cap")
}()

// 每个抓包文件的期望：want 里任一关键词出现即算通过（用不着全中，命令措辞随系统版本变）
type encExpect struct {
	tag  string   // 抓包文件名（不含 .bin）
	want []string // 期望在解码结果里出现的中文关键词
}

var encExpects = []encExpect{
	{"fix-dism-scan", []string{"版本", "映像", "存储", "组件"}},
	{"fix-bcd", []string{"启动", "标识符", "管理器", "device"}},
	{"net-show", []string{"适配器", "IPv4", "网关", "子网"}},
	{"net-flushdns", []string{"刷新", "DNS", "缓存"}},
	{"net-ping", []string{"字节", "时间", "TTL", "数据包"}},
	{"net-tracert", []string{"路由", "跟踪", "跃点", "跳"}},
	{"net-port", []string{"TCP", "UDP", "LISTENING"}},
	{"net-port-task", []string{"映像名称", "PID", "内存"}},
	{"info-drivers", []string{"模块名", "显示名称", "驱动", "Module"}},
	{"info-battery", []string{"电池", "报告", "battery", "无法执行操作", "错误"}},
	{"util-activation", []string{"激活", "许可证", "产品", "Windows"}},
	// w32tm 在"时间服务未启动"时会直接报错，那也是正确解码的中文
	{"util-timesync", []string{"时间", "同步", "源", "time", "服务尚未启动", "发生下列错误"}},
	{"util-shutdown-cancel", []string{"关机", "计划", "shutdown"}},
	{"net-reset-kin", []string{"配置", "接口", "IP", "地址"}},
	{"net-share-kin", []string{"共享", "名称", "share"}},
	{"help-dism", []string{"映像", "部署", "DISM"}},
	{"help-chkdsk", []string{"磁盘", "检查", "CHKDSK"}},
	{"help-netsh", []string{"命令", "用法", "netsh"}},
	{"help-net", []string{"命令", "服务", "net"}},
	{"help-defrag", []string{"碎片", "盘", "defrag"}},
	{"disk-defrag-analyze", []string{"碎片", "分析", "卷", "SSD"}},
	{"fix-chkdsk", []string{"CHKDSK", "卷", "文件系统", "访问"}},
	{"sfc-verifyonly", []string{"验证", "完整性", "扫描"}},
}

// 解码一个抓包文件，返回日志行（用运行时同一套解码链）
func decodeCapFile(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读不到 %s：%v", path, err)
	}
	var lines []string
	pumpLines(consoleReader(bytes.NewReader(raw)), func(kind, text string) {
		if kind == "line" || kind == "cr" {
			lines = append(lines, text)
		}
	})
	return strings.Join(lines, "\n")
}

func TestToolOutputEncodingAudit(t *testing.T) {
	if _, err := os.Stat(encCapDir); err != nil {
		t.Skipf("没有抓包目录 %s（用 _proto/cap_enc.py 生成后再跑），跳过", encCapDir)
	}
	checked := 0
	for _, e := range encExpects {
		p := filepath.Join(encCapDir, e.tag+".bin")
		if _, err := os.Stat(p); err != nil {
			continue // 未抓到的跳过（不同机器命令可用性不同）
		}
		out := decodeCapFile(t, p)
		checked++
		if out == "" {
			// 命令无输出（例如拒绝访问、无电池）——不算乱码，但要看一眼
			t.Logf("[空输出] %s", e.tag)
			continue
		}
		if strings.ContainsRune(out, '\uFFFD') {
			t.Errorf("%s 解码出现替换字符 U+FFFD，说明编码没识别对：\n%s", e.tag, firstLines(out, 6))
		}
		if strings.ContainsRune(out, '\x00') {
			t.Errorf("%s 解码后仍残留 0x00（UTF-16 没转干净）", e.tag)
		}
		hit := false
		for _, w := range e.want {
			if strings.Contains(out, w) {
				hit = true
				break
			}
		}
		if !hit {
			t.Errorf("%s 解码结果里找不到任何期望关键词 %v，疑似乱码：\n%s", e.tag, e.want, firstLines(out, 8))
		} else {
			t.Logf("[通过] %-20s %s", e.tag, firstLines(out, 1))
		}
	}
	t.Logf("共核实 %d 个命令输出", checked)
	if checked == 0 {
		t.Skip("抓包目录里没有可核实的文件")
	}
}

func firstLines(s string, n int) string {
	ls := strings.Split(s, "\n")
	if len(ls) > n {
		ls = ls[:n]
	}
	for i := range ls {
		if len(ls[i]) > 90 {
			ls[i] = ls[i][:90] + "…"
		}
	}
	return strings.Join(ls, "\n")
}
