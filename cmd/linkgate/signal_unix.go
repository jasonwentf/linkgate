//go:build !windows

package main

import (
	"os"
	"os/signal"
	"syscall"
)

func watchHangup(ch chan<- os.Signal) {
	signal.Notify(ch, syscall.SIGHUP)
}
