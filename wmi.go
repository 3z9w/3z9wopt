package main

import (
	"fmt"
	"runtime"
	"strings"

	"github.com/go-ole/go-ole"
	"github.com/go-ole/go-ole/oleutil"
)

// wmiQuery 执行一条 WQL 查询，返回若干行「属性名 → 值」。
//
// 为什么不用 wmic：
//   1. Win11 24H2 起微软已移除 wmic.exe，精简版系统也常把它一并砍掉；
//   2. wmic 的输出编码/格式随版本漂移，解析不稳。
// WMI COM 接口（WbemScripting.SWbemLocator）从 XP 起就是系统内置组件，
// 不经过 PowerShell、不经过 wmic，全版本可用。
//
// 线程注意：Go 的 goroutine 会在 OS 线程间迁移，而 COM 组件按线程初始化，
// 所以这里必须 LockOSThread，否则查询会概率性失败（0x800401F0 未初始化）。
func wmiQuery(namespace, wql string, fields []string) ([]map[string]string, error) {
	if namespace == "" {
		namespace = `root\cimv2`
	}
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	// S_FALSE（已初始化）也会返回 error，直接忽略即可；配对 CoUninitialize 是规范要求
	_ = ole.CoInitializeEx(0, ole.COINIT_MULTITHREADED)
	defer ole.CoUninitialize()

	loc, err := oleutil.CreateObject("WbemScripting.SWbemLocator")
	if err != nil {
		return nil, fmt.Errorf("WMI 定位器创建失败: %v", err)
	}
	defer loc.Release()

	wmi, err := loc.QueryInterface(ole.IID_IDispatch)
	if err != nil {
		return nil, fmt.Errorf("WMI 接口获取失败: %v", err)
	}
	defer wmi.Release()

	svcRaw, err := oleutil.CallMethod(wmi, "ConnectServer", nil, namespace)
	if err != nil {
		return nil, fmt.Errorf("连接 WMI 命名空间 %s 失败: %v", namespace, err)
	}
	svc := svcRaw.ToIDispatch()
	defer svc.Release()

	resRaw, err := oleutil.CallMethod(svc, "ExecQuery", wql)
	if err != nil {
		return nil, fmt.Errorf("WMI 查询失败: %v", err)
	}
	res := resRaw.ToIDispatch()
	defer res.Release()

	cntRaw, err := oleutil.GetProperty(res, "Count")
	if err != nil {
		return nil, err
	}
	n := int(cntRaw.Val)
	out := make([]map[string]string, 0, n)
	for i := 0; i < n; i++ {
		itemRaw, err := oleutil.CallMethod(res, "ItemIndex", i)
		if err != nil {
			continue
		}
		item := itemRaw.ToIDispatch()
		if item == nil {
			continue
		}
		row := make(map[string]string, len(fields))
		for _, f := range fields {
			v, err := oleutil.GetProperty(item, f)
			if err != nil || v == nil {
				row[f] = ""
				continue
			}
			row[f] = strings.TrimSpace(v.ToString())
		}
		item.Release()
		out = append(out, row)
	}
	return out, nil
}

// wmiFirst 只取第一行（单值查询用，如主板、BIOS）
func wmiFirst(namespace, wql string, fields []string) map[string]string {
	rows, err := wmiQuery(namespace, wql, fields)
	if err != nil || len(rows) == 0 {
		return map[string]string{}
	}
	return rows[0]
}

// fmtBytes 人类可读容量（工具库内部用，十进制与界面口径一致）
func fmtBytes(n int64) string {
	if n <= 0 {
		return "—"
	}
	units := []string{"B", "KB", "MB", "GB", "TB", "PB"}
	f := float64(n)
	i := 0
	for f >= 1000 && i < len(units)-1 {
		f /= 1000
		i++
	}
	if i == 0 {
		return fmt.Sprintf("%d B", n)
	}
	return fmt.Sprintf("%.1f %s", f, units[i])
}
