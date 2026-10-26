//go:build windows

package main

import "os"

// Windows 没有 SIGHUP；热重载走管理接口或重启服务。
func watchHangup(ch chan<- os.Signal) {}
