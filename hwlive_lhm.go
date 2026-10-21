package main

import (
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/sys/windows/registry"
)

/* ============================================================================
   LibreHardwareMonitor 探测（2026-09-24）
   ----------------------------------------------------------------------------
   为什么要有这一块：Windows 用户态**拿不到**风扇转速，也拿不到真正的 CPU 封装温度 ——
   这两个都要主板 SuperIO 芯片的驱动，微软没给通用接口。LibreHardwareMonitor（开源免费）
   装了并开启 WMI 接口后就能补上，所以页面要给一条「要不要装」的引导，而不是干写「读不到」。

   规矩：
     - 只**探测**和**引导**，绝不自动下载、绝不自动安装、绝不后台拉起。
     - 能不能用看两个条件：① 进程在跑 ② WMI 接口开着（要在 LHM 里勾 Remote Web Server / WMI）。
   ============================================================================ */

const lhmSite = "https://github.com/LibreHardwareMonitor/LibreHardwareMonitor/releases"

// LHMStatus LibreHardwareMonitor 的可用性（给前端决定要不要显示引导入口）
type LHMStatus struct {
	Running  bool   `json:"running"`  // 进程在跑（WMI Sensor 表能查到就算）
	Install  bool   `json:"install"`  // 机器上装了（进程没跑也算子）
	Path     string `json:"path"`     // 装在哪（能找到的话），页面可以提示「已安装，启动它即可」
	Site     string `json:"site"`     // 官方下载页
	HasReal  bool   `json:"hasReal"`  // 这次采样里有没有拿到「真传感器」数据（LHM 才给得出风扇）
	Hint     string `json:"hint"`     // 一句人话：现在该干什么
}

var lhmCache struct {
	at    time.Time
	stat  LHMStatus
}

// GetLHMStatus 探测 LibreHardwareMonitor（结果缓存 10 秒，别每次刷页面都去查进程）
func (a *App) GetLHMStatus() LHMStatus {
	if time.Since(lhmCache.at) < 10*time.Second {
		return lhmCache.stat
	}
	s := LHMStatus{Site: lhmSite}
	s.Running = lhmWMIAlive()
	s.Install, s.Path = lhmInstalled()
	s.HasReal = len(readFans()) > 0

	switch {
	case s.HasReal:
		s.Hint = "已接上，温度和风扇转速都是真实读数"
	case s.Running:
		s.Hint = "LibreHardwareMonitor 在跑，但还没读到风扇 —— 在它里面勾选「WMI 接口」"
	case s.Install:
		s.Hint = "本机已安装，启动它并开启 WMI 接口，就能读到温度和风扇转速"
	default:
		s.Hint = "装一个 LibreHardwareMonitor（开源免费），就能看到真实的 CPU 温度和风扇转速"
	}
	lhmCache.stat, lhmCache.at = s, time.Now()
	return s
}

// lhmWMIAlive 能不能查到 LHM 的 WMI Sensor 表 —— 能查到说明它在跑且开了 WMI 接口
func lhmWMIAlive() bool {
	rows, err := wmiQuery(`root\LibreHardwareMonitor`,
		"SELECT Name FROM Sensor", []string{"Name"})
	return err == nil && len(rows) > 0
}

// lhmInstalled 找没装。先看常见安装目录，再看卸载表 —— 两条路都找不着才算没装。
func lhmInstalled() (bool, string) {
	dirs := []string{}
	if pf := os.Getenv("ProgramFiles"); pf != "" {
		dirs = append(dirs, filepath.Join(pf, "LibreHardwareMonitor"))
	}
	if pf86 := os.Getenv("ProgramFiles(x86)"); pf86 != "" {
		dirs = append(dirs, filepath.Join(pf86, "LibreHardwareMonitor"))
	}
	if la := os.Getenv("LOCALAPPDATA"); la != "" {
		dirs = append(dirs, filepath.Join(la, "Programs", "LibreHardwareMonitor"))
	}
	for _, d := range dirs {
		for _, exe := range []string{"LibreHardwareMonitor.exe", "LibreHardwareMonitorLib.exe"} {
			p := filepath.Join(d, exe)
			if st, err := os.Stat(p); err == nil && !st.IsDir() {
				return true, p
			}
		}
	}
	// 卸载表里搜一把（有人装在了别处）
	/* 注意：**绝不能查 Win32_Product**。那个类不是普通查表 —— 它每次调用都会触发
	   Windows Installer 的一致性问题检查（msiexec 重配置），实测能把整页渲染卡住十几秒，
	   事件日志还会刷 MsiInstaller 警告。凡是「查装没装某个软件」都走注册表卸载表。 */
	for _, root := range []registry.Key{registry.LOCAL_MACHINE, registry.CURRENT_USER} {
		for _, sub := range []string{
			`SOFTWARE\Microsoft\Windows\CurrentVersion\Uninstall`,
			`SOFTWARE\WOW6432Node\Microsoft\Windows\CurrentVersion\Uninstall`,
		} {
			if ok, p := lhmScanUninstall(root, sub); ok {
				return true, p
			}
		}
	}
	return false, ""
}

// lhmScanUninstall 在卸载表的子键里找 DisplayName 含 LibreHardwareMonitor 的
func lhmScanUninstall(root registry.Key, path string) (bool, string) {
	k, err := registry.OpenKey(root, path, registry.ENUMERATE_SUB_KEYS|registry.WOW64_64KEY)
	if err != nil {
		return false, ""
	}
	defer k.Close()
	subs, err := k.ReadSubKeyNames(-1)
	if err != nil {
		return false, ""
	}
	for _, s := range subs {
		sk, err := registry.OpenKey(root, path+`\`+s, registry.QUERY_VALUE|registry.WOW64_64KEY)
		if err != nil {
			continue
		}
		name, _, _ := sk.GetStringValue("DisplayName")
		loc, _, _ := sk.GetStringValue("InstallLocation")
		sk.Close()
		if strings.Contains(strings.ToLower(name), "librehardwaremonitor") {
			return true, strings.TrimSpace(loc)
		}
	}
	return false, ""
}
