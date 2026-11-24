package main

import (
	"errors"
	"reflect"
	"strings"
	"testing"
)

func TestWindowsRegistrationFallback(t *testing.T) {
	methods := []windowsRegistrationMethod{windowsRegisterXML, windowsRegisterPowerShell, windowsRegisterInteractive}
	for success := range methods {
		t.Run(string(methods[success]), func(t *testing.T) {
			var calls []windowsRegistrationMethod
			result, err := registerWindowsTaskWithFallback(func(method windowsRegistrationMethod) error {
				calls = append(calls, method)
				if method == methods[success] {
					return nil
				}
				return errors.New("完整 stderr：拒绝访问\n详细信息第二行\n末尾标记")
			})
			if err != nil || !reflect.DeepEqual(calls, methods[:success+1]) {
				t.Fatalf("calls=%v err=%v", calls, err)
			}
			wantMode := "S4U"
			if success == 2 {
				wantMode = "Interactive"
			}
			if result.LogonType != wantMode || result.Method != methods[success] {
				t.Fatalf("result=%+v", result)
			}
			if success > 0 && (!strings.Contains(result.Diagnostics, "第二行") || !strings.Contains(result.Diagnostics, "末尾标记")) {
				t.Fatal("lost prior native stderr")
			}
		})
	}
}

func TestWindowsRegistrationAllFailuresPreserved(t *testing.T) {
	_, err := registerWindowsTaskWithFallback(func(method windowsRegistrationMethod) error {
		return errors.New(string(method) + " 拒绝访问\n" + strings.Repeat("完整中文原文", 2000) + "\n最后一行")
	})
	if err == nil {
		t.Fatal("expected registration failure")
	}
	for _, method := range []windowsRegistrationMethod{windowsRegisterXML, windowsRegisterPowerShell, windowsRegisterInteractive} {
		if !strings.Contains(err.Error(), string(method)+" 拒绝访问") {
			t.Fatalf("missing %s failure", method)
		}
	}
	if strings.Count(err.Error(), "最后一行") != 3 || strings.Count(err.Error(), "完整中文原文") != 6000 {
		t.Fatal("stderr truncated")
	}
}

func TestWindowsInteractiveCompatibilityMode(t *testing.T) {
	for _, mode := range []string{"S4U", "Interactive", "InteractiveToken"} {
		if !supportedWindowsTask(mode) {
			t.Fatalf("healthy task cannot reenter: %s", mode)
		}
	}
	if supportedWindowsTask("Password") || windowsInteractiveTask("S4U") {
		t.Fatal("unexpected logon mode")
	}
	if !strings.Contains(windowsInteractiveWarning, "可见命令行窗口") || !strings.Contains(windowsInteractiveWarning, "关闭该窗口会停止内核") {
		t.Fatal("degraded mode must warn about its visible window")
	}
}
