package main

import (
	"fmt"
	"os"
	"testing"
	"time"
)

// 回收站扫描耗时守门（2026-10-14）
//
// 背景：原来用 shell32!SHQueryRecycleBinW 逐盘问，本机 D 盘实测 **1.843 秒**
// （1965 个文件），文件越多越慢（曾实测 12363 个文件 2.3 秒）。
// 而且它报 332 MB、清理却按 walkSize 的 910 MB 算 —— 扫描与清理两套口径，
// 用户看到的就是"说一套做一套"。现改为自己并发枚举 $Recycle.Bin，与清理口径一致。
//
// 这条测试守两件事：① 别又变回秒级 ② 别退化成"扫不到就报 0"（那更糟）。
//
// 跑法：RECYCLESPEED=1 go test -run TestRecycleScanSpeed -v -timeout 2m .
func TestRecycleScanSpeed(t *testing.T) {
	if os.Getenv("RECYCLESPEED") != "1" {
		t.Skip("设 RECYCLESPEED=1 才跑")
	}
	t0 := time.Now()
	b, n := recycleBinTotal()
	one := time.Since(t0)

	const times = 20
	t1 := time.Now()
	for i := 0; i < times; i++ {
		recycleBinTotal()
	}
	all := time.Since(t1)
	avg := all / times

	fmt.Printf("单次：%v（%d 个文件 / %.2f MB）\n", one.Round(time.Millisecond), n, float64(b)/1024/1024)
	fmt.Printf("连问 %d 次：%v → 平均 %v\n", times, all.Round(time.Millisecond), avg.Round(time.Millisecond))

	if avg > 400*time.Millisecond {
		t.Errorf("回收站扫描又变慢了：平均 %v（改回 SHQueryRecycleBinW 就会出现这个数）", avg)
	}
}
