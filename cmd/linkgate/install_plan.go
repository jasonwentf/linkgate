package main

import (
	"encoding/xml"
	"fmt"
	"strings"
)

// 独立选项保持 runInstall 的跨平台双参数契约；其他平台忽略 Windows 专属选项。
var windowsInstall = struct {
	autostart bool
	tray      bool
	start     bool
	dryrun    bool
	openPanel bool
}{autostart: true, tray: true, start: true}

type windowsTask struct {
	UserID           string `json:"UserID"`
	Execute          string `json:"Execute"`
	Arguments        string `json:"Arguments"`
	WorkingDirectory string `json:"WorkingDirectory"`
	ActionCount      int    `json:"ActionCount"`
	LogonType        string `json:"LogonType"`
	Autostart        bool   `json:"Autostart"`
	Enabled          bool   `json:"Enabled"`
}

type windowsProcess struct {
	PID         int    `json:"PID"`
	Path        string `json:"Path"`
	CommandLine string `json:"CommandLine"`
	ConfigPath  string `json:"-"`
}

func sameWindowsPath(a, b string) bool {
	return a != "" && b != "" && strings.EqualFold(strings.ReplaceAll(a, "/", `\`), strings.ReplaceAll(b, "/", `\`))
}

func verifyWindowsTask(task *windowsTask, sid, executable string) error {
	if task != nil && (task.UserID != sid || task.ActionCount != 1 || !sameWindowsPath(task.Execute, executable)) {
		return fmt.Errorf("Linkgate 计划任务属于其他用户或安装目录，未做修改；请先从原安装目录卸载")
	}
	return nil
}

func classifyWindowsProcesses(processes []windowsProcess, self int, executable, config string) (bool, error) {
	running := false
	for _, p := range processes {
		if p.PID == self {
			continue
		}
		if !sameWindowsPath(p.Path, executable) {
			return false, fmt.Errorf("检测到其他目录或无法核验路径的 Linkgate 内核。请先退出旧便携实例，再重试；未修改启动项")
		}
		if !sameWindowsPath(p.ConfigPath, config) {
			return false, fmt.Errorf("同目录内核使用其他配置或配置路径无法核验，请先停止旧实例再重试")
		}
		if running {
			return false, fmt.Errorf("同目录存在多个 Linkgate 内核，请先停止重复实例")
		}
		running = true
	}
	return running, nil
}

func configFromWindowsArgs(args []string, defaultPath string) string {
	if len(args) == 0 {
		return ""
	}
	config := defaultPath
	for i := 1; i < len(args); i++ {
		arg := args[i]
		if arg == "--" || !strings.HasPrefix(arg, "-") {
			break
		}
		if arg == "-config" || arg == "--config" {
			i++
			if i >= len(args) {
				return ""
			}
			config = args[i]
		} else if strings.HasPrefix(arg, "-config=") || strings.HasPrefix(arg, "--config=") {
			config = strings.SplitN(arg, "=", 2)[1]
		}
	}
	return config
}

// S4U 不储存密码、不进入交互桌面。可访问本地文件与普通 TCP/HTTP，
// 但不能使用当前登录凭据访问 UNC/集成认证上游；这类上游需显式配置认证。
func windowsTaskXML(sid, executable, config, directory string, autostart bool) string {
	escape := func(s string) string {
		var b strings.Builder
		_ = xml.EscapeText(&b, []byte(s))
		return b.String()
	}
	trigger := ""
	if autostart {
		trigger = "<LogonTrigger><Enabled>true</Enabled><UserId>" + escape(sid) + "</UserId></LogonTrigger>"
	}
	return fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<Task version="1.2" xmlns="http://schemas.microsoft.com/windows/2004/02/mit/task">
  <RegistrationInfo><Description>Linkgate 当前用户后台内核</Description></RegistrationInfo>
  <Triggers>%s</Triggers>
  <Principals><Principal id="Author"><UserId>%s</UserId><LogonType>S4U</LogonType><RunLevel>LeastPrivilege</RunLevel></Principal></Principals>
  <Settings>
    <MultipleInstancesPolicy>IgnoreNew</MultipleInstancesPolicy>
    <DisallowStartIfOnBatteries>false</DisallowStartIfOnBatteries>
    <StopIfGoingOnBatteries>false</StopIfGoingOnBatteries>
    <AllowStartOnDemand>true</AllowStartOnDemand><Enabled>true</Enabled><Hidden>true</Hidden>
    <ExecutionTimeLimit>PT0S</ExecutionTimeLimit>
    <RestartOnFailure><Interval>PT1M</Interval><Count>255</Count></RestartOnFailure>
  </Settings>
  <Actions Context="Author"><Exec><Command>%s</Command><Arguments>%s</Arguments><WorkingDirectory>%s</WorkingDirectory></Exec></Actions>
</Task>`, trigger, escape(sid), escape(executable), escape(`-config "`+config+`"`), escape(directory))
}
