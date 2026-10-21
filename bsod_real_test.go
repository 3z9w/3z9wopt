package main

import (
	"os"
	"testing"
)

// 真实转储回归：拿一份真机的蓝屏文件跑一遍（路径由 BSOD_REAL_DMP 指定，没设就跳过）。
func TestZZBsodReal(t *testing.T) {
	p := os.Getenv("BSOD_REAL_DMP")
	if p == "" {
		t.Skip("未设置 BSOD_REAL_DMP")
	}
	a := &App{}
	d := a.BsodDetail(p)
	t.Logf("valid=%v err=%q", d.Valid, d.Err)
	t.Logf("code=%s title=%s", d.CodeHex, d.Title)
	t.Logf("os=%s build=%s arch=%s time=%s uptime=%q", d.OS, d.Build, d.Arch, d.Time, d.Uptime)
	t.Logf("image=%s symbol=%s bucket=%s", d.Image, d.Symbol, d.Bucket)
	t.Logf("crashAt=%s crashIn=%s", d.CrashAt, d.CrashIn)
	t.Logf("note=%s", d.Note)
	for _, x := range d.Params {
		t.Logf("  param: %s", x)
	}
	t.Logf("modules=%d drivers=%d stack=%d", len(d.Modules), len(d.Drivers), len(d.Stack))
	for _, m := range d.Modules[:minInt(6, len(d.Modules))] {
		t.Logf("  mod %-24s base=%s size=%s stamp=%q inStack=%v verLocal=%v", m.Name, m.Base, m.Size, m.Stamp, m.InStack, m.VerLocal)
	}
	for _, x := range d.Drivers {
		t.Logf("  DRV %-24s off=%s comp=%q ver=%q local=%v", x.Name, x.Off, x.Comp, x.Ver, x.VerLocal)
	}
	for _, f := range d.Stack {
		t.Logf("  #%d %s %-20s %s (%s)", f.No, f.Addr, f.Module, f.Off, f.Call)
	}
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
