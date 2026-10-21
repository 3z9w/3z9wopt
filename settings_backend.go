package main

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"

	"golang.org/x/sys/windows/registry"
)

/* 设置页真正生效所需要的后端能力（2026-09-23）。
   背景：设置页原有的 5 个开关从未被任何代码消费（诊断见走查报告 #11），
   用户拨动它们只改 localStorage，软件行为一点不变。本次只把能真接的接上：
   restorePoint 由前端在应用优化前调 RunTool("fix-restore-point")，
   这里提供「开机自动启动」与「自身指纹校验」两件。 */

const (
	runKeyPath   = `Software\Microsoft\Windows\CurrentVersion\Run`
	runValueName = "3z9wOpt"
)

/*
SetAutoStart 设置或取消开机自启。

	返回空串表示成功，否则返回可以直接展示给用户的中文错误说明（前端直接 toast）。
	用 HKCU 而不是 HKLM：不需要额外提权，卸载后也不会留下需要管理员才能清的残留。
*/
func (a *App) SetAutoStart(on bool) string {
	k, err := registry.OpenKey(registry.CURRENT_USER, runKeyPath, registry.SET_VALUE)
	if err != nil {
		return "改不了开机启动：注册表打开失败，请检查是否禁用了注册表编辑"
	}
	defer k.Close()

	if !on {
		// 值本来就不存在时删除会报 ErrNotExist，这种情况视为成功（本来就没开）
		if err := k.DeleteValue(runValueName); err != nil && err != registry.ErrNotExist {
			return "取消开机启动失败：" + err.Error()
		}
		return ""
	}

	exe, err := os.Executable()
	if err != nil {
		return "取不到程序路径，无法设置开机启动"
	}
	// 路径加引号：Program Files 这类带空格的目录不加引号会导致启动失败
	if err := k.SetStringValue(runValueName, `"`+exe+`"`); err != nil {
		return "写开机启动项失败：" + err.Error()
	}
	return ""
}

/*
GetAutoStart 读取当前开机自启状态。

	注册表是真值源 —— 界面开关以这里为准，避免用户在系统设置里改了、软件还显示旧状态。
*/
func (a *App) GetAutoStart() bool {
	k, err := registry.OpenKey(registry.CURRENT_USER, runKeyPath, registry.QUERY_VALUE)
	if err != nil {
		return false
	}
	defer k.Close()
	v, _, err := k.GetStringValue(runValueName)
	return err == nil && v != ""
}

/*
GetSelfHash 返回当前运行 exe 的 SHA256，供设置页「官方原版校验」展示。

	用途：本软件未做代码签名，SmartScreen 会弹蓝色拦截；
	让用户能自己核对指纹，确认下载到的文件没被第三方改过。取不到就返回空串，前端自行降级。
*/
func (a *App) GetSelfHash() string {
	exe, err := os.Executable()
	if err != nil {
		return ""
	}
	f, err := os.Open(exe)
	if err != nil {
		return ""
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return ""
	}
	return hex.EncodeToString(h.Sum(nil))
}
