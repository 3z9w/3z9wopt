package main

// 工具库 · 磁盘工具类（disk）
// 新增/修改工具：在 init() 里加一条 regTool(...)，详见 toolkit.go 顶部的扩展手册。

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

func init() {
	regTool(ToolItem{
		ID: "disk-smart", Cat: "disk", Name: "磁盘健康检测",
		Desc: "读取硬盘型号、容量、接口与 S.M.A.R.T. 健康状态",
		Cmd:  "WMI 查询 Win32_DiskDrive / MSStorageDriver_FailurePredictStatus", Risk: "readonly", OutText: true, Timeout: 120,
	}, toolImpl{Run: toolDiskSmart})

	regTool(ToolItem{
		ID: "disk-speed", Cat: "disk", Name: "磁盘读写测速",
		Desc: "实测顺序写入与读取速度（MB/s）",
		Cmd:  "本地写入 32MB 测试文件并读回", Risk: "safe", Timeout: 300,
		Note: "会在临时目录写入约 32MB 测试文件，测完自动删除。读取速度含系统缓存影响，仅供参考。",
	}, toolImpl{Run: toolDiskSpeed})

	regTool(ToolItem{
		ID: "disk-defrag-analyze", Cat: "disk", Name: "磁盘碎片分析",
		Desc: "分析碎片情况，判断是否需要整理",
		Cmd:  "defrag <盘符> /A", Risk: "readonly", Long: true, Timeout: 1800,
		ArgKind: "drive", ArgLabel: "盘符",
	}, toolImpl{Plan: func(arg string) []toolStep {
		return []toolStep{{exe: "defrag.exe", args: []string{driveOrSys(arg), "/A"}}}
	}})

	regTool(ToolItem{
		ID: "disk-defrag-optimize", Cat: "disk", Name: "磁盘优化整理",
		Desc: "SSD 发 TRIM 指令、机械盘整理碎片",
		Cmd:  "defrag <盘符> /O", Risk: "safe", Long: true, Timeout: 3600,
		ArgKind: "drive", ArgLabel: "盘符",
		Note: "优化期间可以正常用电脑，但会占用一定磁盘性能。",
	}, toolImpl{Plan: func(arg string) []toolStep {
		return []toolStep{{exe: "defrag.exe", args: []string{driveOrSys(arg), "/O"}}}
	}})
}

// ---- 磁盘健康（WMI） ----
func toolDiskSmart(tc *toolCtx) (int, string) {
	tc.log("head", "▶ 查询物理磁盘")
	rows, err := wmiQuery("root\\cimv2",
		"SELECT Model,SerialNumber,Size,InterfaceType,MediaType,Status FROM Win32_DiskDrive",
		[]string{"Model", "SerialNumber", "Size", "InterfaceType", "MediaType", "Status"})
	if err != nil {
		tc.log("err", shortErr(err))
		return 1, "查询失败"
	}
	if len(rows) == 0 {
		tc.log("line", "未查询到物理磁盘信息")
		return 1, "无数据"
	}
	for i, r := range rows {
		sz, _ := strconv.ParseInt(r["Size"], 10, 64)
		tc.log("line", fmt.Sprintf("磁盘 %d：%s", i, emptyDash(r["Model"])))
		tc.log("line", fmt.Sprintf("    容量 %s    接口 %s    %s", fmtBytes(sz), emptyDash(r["InterfaceType"]), emptyDash(r["MediaType"])))
		if r["SerialNumber"] != "" {
			tc.log("line", "    序列号 "+r["SerialNumber"])
		}
		tc.log("line", "    状态 "+emptyDash(r["Status"]))
		tc.log("line", "")
	}
	// S.M.A.R.T. 预测失败（很多盘/驱动不支持，失败静默）
	srows, serr := wmiQuery("root\\wmi",
		"SELECT InstanceName,PredictFailure FROM MSStorageDriver_FailurePredictStatus",
		[]string{"InstanceName", "PredictFailure"})
	tc.log("head", "▶ S.M.A.R.T. 健康预测")
	if serr != nil || len(srows) == 0 {
		tc.log("dim", "该硬盘或驱动未提供 S.M.A.R.T. 预测接口（常见于 NVMe/USB 硬盘盒），可参考上方「状态」字段")
	} else {
		bad := 0
		for _, r := range srows {
			if strings.EqualFold(r["PredictFailure"], "true") {
				bad++
				tc.log("err", "预测即将故障："+r["InstanceName"])
			}
		}
		if bad == 0 {
			tc.log("ok", fmt.Sprintf("已检测 %d 个硬盘，均未预测到故障", len(srows)))
		}
	}
	return 0, ""
}

// ---- 磁盘读写测速 ----
func toolDiskSpeed(tc *toolCtx) (int, string) {
	const total = 32 << 20
	dir := os.TempDir()
	fp := filepath.Join(dir, "3z9wopt_speed_test.tmp")
	defer os.Remove(fp)

	buf := make([]byte, 1<<20)
	for i := range buf {
		buf[i] = byte(i*31 + 7)
	}

	tc.log("head", "▶ 写入测试（32MB）")
	fh, err := os.Create(fp)
	if err != nil {
		tc.log("err", "无法创建测试文件："+shortErr(err))
		return 1, "测试失败"
	}
	start := time.Now()
	written := 0
	for written < total {
		if tc.ctx.Err() != nil {
			fh.Close()
			return -1, "已停止"
		}
		n, werr := fh.Write(buf)
		if werr != nil {
			fh.Close()
			tc.log("err", "写入失败（磁盘可能已满）："+shortErr(werr))
			return 1, "测试失败"
		}
		written += n
	}
	_ = fh.Sync()
	fh.Close()
	wsec := time.Since(start).Seconds()
	wspeed := float64(total) / wsec / (1 << 20)
	tc.log("line", fmt.Sprintf("写入 %.1f MB/s（%.2f 秒）", wspeed, wsec))

	tc.log("head", "▶ 读取测试")
	rf, err := os.Open(fp)
	if err != nil {
		tc.log("err", "打开测试文件失败："+shortErr(err))
		return 1, "测试失败"
	}
	defer rf.Close()
	rbuf := make([]byte, 1<<20)
	read := 0
	start = time.Now()
	for {
		if tc.ctx.Err() != nil {
			return -1, "已停止"
		}
		n, rerr := rf.Read(rbuf)
		read += n
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			tc.log("err", "读取失败："+shortErr(rerr))
			return 1, "测试失败"
		}
	}
	rsec := time.Since(start).Seconds()
	rspeed := float64(read) / rsec / (1 << 20)
	tc.log("line", fmt.Sprintf("读取 %.1f MB/s（%.2f 秒）", rspeed, rsec))
	tc.log("dim", "读取速度包含系统内存缓存的影响，仅作参考；连续测两次时第二次会明显偏快。")

	tc.log("ok", fmt.Sprintf("结论：写入约 %.0f MB/s，读取约 %.0f MB/s", wspeed, rspeed))
	return 0, ""
}
