package main

import (
	"errors"
	"fmt"
	"strings"
)

type windowsRegistrationMethod string

const (
	windowsRegisterXML         windowsRegistrationMethod = "schtasks XML / S4U"
	windowsRegisterPowerShell  windowsRegistrationMethod = "PowerShell / S4U"
	windowsRegisterInteractive windowsRegistrationMethod = "PowerShell / Interactive"
)

type windowsRegistrationResult struct {
	Method      windowsRegistrationMethod
	LogonType   string
	Diagnostics string
}

// 注入系统调用以在非 Windows 上验证：首选 XML，失败后试 cmdlet，最后兼容交互任务。
func registerWindowsTaskWithFallback(register func(windowsRegistrationMethod) error) (windowsRegistrationResult, error) {
	var failures []error
	for _, method := range []windowsRegistrationMethod{windowsRegisterXML, windowsRegisterPowerShell, windowsRegisterInteractive} {
		if err := register(method); err != nil {
			failures = append(failures, fmt.Errorf("%s 注册失败: %w", method, err))
			continue
		}
		result := windowsRegistrationResult{Method: method, LogonType: "S4U"}
		if method == windowsRegisterInteractive {
			result.LogonType = "Interactive"
		}
		if len(failures) > 0 {
			result.Diagnostics = errors.Join(failures...).Error()
		}
		return result, nil
	}
	return windowsRegistrationResult{}, fmt.Errorf("三种任务注册方式均失败，未请求管理员权限；请查看完整诊断:\n%w", errors.Join(failures...))
}

func windowsInteractiveTask(logonType string) bool {
	return strings.EqualFold(logonType, "Interactive") || strings.EqualFold(logonType, "InteractiveToken")
}

func supportedWindowsTask(logonType string) bool {
	return logonType == "S4U" || windowsInteractiveTask(logonType)
}

const windowsInteractiveWarning = "兼容模式：S4U 不可用，内核会以可见命令行窗口运行；关闭该窗口会停止内核。托盘仍独立运行。"
