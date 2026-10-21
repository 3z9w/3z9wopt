package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"
)

/* ============ 操作记录（2026-09-22 新增） ============
   这个软件会改 154 项注册表、动开机启动项、跑系统修复命令 ——
   用户改完最想知道的是「我到底改过什么、什么时候改的、要不要重启」。
   没有记录，还原点就只是个孤零零的快照，凑不成证据链。
   规矩：
     · 只记真实发生的动作（成功 / 失败都记，失败写明原因）
     · 落在本机 %LOCALAPPDATA%\3z9wOpt\oplog.json，不上传
     · 最多留 300 条，超出丢最旧的，别把文件撑大 */

// OpLogEntry 一条操作记录
type OpLogEntry struct {
	Ts     string   `json:"ts"`     // 2026-09-22 23:31
	Kind   string   `json:"kind"`   // optimize 应用优化 / revert 撤销优化 / startup 启动项 / tool 工具
	Title  string   `json:"title"`  // 一句话：应用 12 项系统优化
	Items  []string `json:"items"`  // 具体项名（优化项 / 启动项），最多记 40 条
	Reboot bool     `json:"reboot"` // 是否需要重启才完全生效
	OK     int      `json:"ok"`     // 成功条数
	Fail   int      `json:"fail"`   // 失败条数
	Err    string   `json:"err"`    // 失败原因（多条取第一条）
}

const opLogMax = 300

var opLogMu sync.Mutex

func opLogPath() string {
	d := filepath.Join(os.Getenv("LOCALAPPDATA"), "3z9wOpt")
	_ = os.MkdirAll(d, 0o755)
	return filepath.Join(d, "oplog.json")
}

func readOpLog() []OpLogEntry {
	b, err := os.ReadFile(opLogPath())
	if err != nil {
		return nil
	}
	var out []OpLogEntry
	_ = json.Unmarshal(b, &out)
	return out
}

// appendOpLog 追加一条记录（最新的排在最前面，界面直接从上往下读）
func appendOpLog(e OpLogEntry) {
	opLogMu.Lock()
	defer opLogMu.Unlock()
	if e.Ts == "" {
		e.Ts = time.Now().Format("2006-01-02 15:04")
	}
	if len(e.Items) > 40 {
		e.Items = e.Items[:40]
	}
	list := append([]OpLogEntry{e}, readOpLog()...)
	if len(list) > opLogMax {
		list = list[:opLogMax]
	}
	b, err := json.Marshal(list)
	if err != nil {
		return
	}
	_ = os.WriteFile(opLogPath(), b, 0o644)
}

// GetOpLog 操作记录（新→旧）
func (a *App) GetOpLog() []OpLogEntry {
	opLogMu.Lock()
	defer opLogMu.Unlock()
	l := readOpLog()
	if l == nil {
		return []OpLogEntry{}
	}
	return l
}

// ClearOpLog 清空操作记录（用户手动点「清空」）
func (a *App) ClearOpLog() bool {
	opLogMu.Lock()
	defer opLogMu.Unlock()
	return os.Remove(opLogPath()) == nil || os.WriteFile(opLogPath(), []byte("[]"), 0o644) == nil
}

// ---------- 各处动作埋点 ----------

// logOptimize 记录一次「应用优化」/「撤销优化」
func logOptimize(kind string, res []OptResult) {
	if len(res) == 0 {
		return
	}
	known := map[string]*OptItem{}
	for i := range optCatalog {
		known[optCatalog[i].ID] = &optCatalog[i]
	}
	var items []string
	ok, fail := 0, 0
	errTxt := ""
	rb := false
	for _, r := range res {
		if r.OK {
			ok++
			items = append(items, r.Name)
			if it, has := known[r.ID]; has && itemNeedsReboot(it) {
				rb = true
			}
		} else {
			fail++
			if errTxt == "" && r.Err != "" {
				errTxt = r.Err
			}
		}
	}
	title := "应用 " + itoa(ok) + " 项系统优化"
	if kind == "revert" {
		title = "撤销 " + itoa(ok) + " 项优化，恢复系统默认"
	}
	appendOpLog(OpLogEntry{
		Kind: kind, Title: title, Items: items,
		Reboot: rb, OK: ok, Fail: fail, Err: errTxt,
	})
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [8]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}
