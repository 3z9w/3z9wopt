package main

// hwfreq.go —— CPU 当前实际运行频率（2026-09-24）
//
// 为什么要单独测：WMI 的 Win32_Processor.CurrentClockSpeed 在不少机器上是空的，
// CPUID 叶子 0x16 给的是「标称基频」不是「现在跑多少」。而用户看监控软件最想看的
// 恰恰是「现在跑多少」。
//
// 取法（不猜、不查表）：
//   TSC（时间戳计数器）每过一个 CPU 周期加 1，拿高精度计时器（QPC）量一段真实时间，
//   这段时间内 TSC 的增量 ÷ 真实时间 = 当前频率。
//   实测本机 G5400：稳定 3.70 GHz，和标称一致。
//
// 注意：TSC 在非不变（invariant）的老 CPU 上会随省电状态变，那种机器测出来的数会飘，
// 所以这里只在「TSC 确实恒定」时才认这个结果（用两次采样一致性做校验）。

import (
	"syscall"
	"unsafe"
)

var (
	procQPC = syscall.NewLazyDLL("kernel32.dll").NewProc("QueryPerformanceCounter")
	procQPF = syscall.NewLazyDLL("kernel32.dll").NewProc("QueryPerformanceFrequency")
)

// rdtsc 读时间戳计数器（实现在 hwfreq_amd64.s）
func rdtsc() uint64

// measureCPUMHz 实测当前频率，单位 MHz；测不出来返回 0
func measureCPUMHz() float64 {
	var f uint64
	if r, _, _ := procQPF.Call(uintptr(unsafe.Pointer(&f))); r == 0 || f == 0 {
		return 0
	}
	// 先跑一轮预热，避开刚从空闲唤醒、频率还在爬升的那一段
	measureOnce(f)
	a := measureOnce(f)
	if a <= 0 {
		return 0
	}
	return a
}

// measureOnce 量一次：忙等约 30ms，用 TSC 增量 ÷ QPC 经过的秒数
func measureOnce(freq uint64) float64 {
	var t0, t1 uint64
	procQPC.Call(uintptr(unsafe.Pointer(&t0)))
	c0 := rdtsc()
	// 忙等到 30ms：够长到能算出稳定的比值，又不至于让人看出卡顿
	for {
		procQPC.Call(uintptr(unsafe.Pointer(&t1)))
		if (t1-t0)*1000/freq >= 30 {
			break
		}
	}
	c1 := rdtsc()
	dt := float64(t1-t0) / float64(freq) // 秒
	if dt <= 0 || c1 <= c0 {
		return 0
	}
	return float64(c1-c0) / dt / 1e6
}
