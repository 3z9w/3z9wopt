package main

// 2026-10-14：L3（清理前停服务）没生效的现场诊断。
// 真机复测里 searchidx 只清掉 18%，而对照实验手动停 WSearch 是 100% —— 差了 82 个点，
// 必须查清楚是「服务没停成」还是「停了也删不掉」，不许猜。

import (
	"testing"
	"time"
)

func TestDiagSvcPause(t *testing.T) {
	const svc = "WSearch"
	t.Logf("svcRunning(%s) = %v", svc, svcRunning(svc))

	restore := pauseServicesFor("searchidx", nil)
	t.Logf("pauseServicesFor(searchidx) 后 svcRunning = %v", svcRunning(svc))
	time.Sleep(200 * time.Millisecond)
	restore()
	time.Sleep(500 * time.Millisecond)
	t.Logf("restore() 后 svcRunning = %v", svcRunning(svc))

	// 直接调底层，分清是「打不开服务句柄」还是「停不下来」
	if err := svcStop(svc); err != nil {
		t.Logf("svcStop 直接调用报错: %v", err)
	} else {
		t.Logf("svcStop 直接调用成功，等 3s 后状态 = %v", func() bool {
			time.Sleep(3 * time.Second)
			return svcRunning(svc)
		}())
		_ = svcStart(svc)
	}
}
