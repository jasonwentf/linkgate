//go:build !windows && !darwin

package main

import "fmt"

func runInstall(configPath string, uninstall bool) error {
	return fmt.Errorf("该平台暂未实现自安装，请使用 scripts/install-*.sh 或对应平台的部署说明")
}
