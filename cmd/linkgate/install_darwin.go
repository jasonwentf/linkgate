//go:build darwin

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/xml"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"text/scanner"
	"time"

	"linkgate/internal/app"
)

// Package-level dependencies allow isolated tests without environment-controlled production labels.
var darwinLaunchdLabel = "dev.linkgate.kernel"
var darwinLaunchagentsDir string
var darwinInstallIfMissing = flag.Bool("install-if-missing", false, "仅首次初始化安装项（macOS App 内部引导）")

type darwinCommand func(string, ...string) ([]byte, error)

type darwinAgentIdentity struct {
	Label            string
	ProgramArguments []string
	PID              int
}

type darwinInstallOps struct {
	initialOnly bool
	command     darwinCommand
	read        func(string) ([]byte, error)
	write       func(string, []byte, os.FileMode) error
	remove      func(string) error
	query       func(string) (darwinAgentIdentity, bool, error)
}

func runInstall(configPath string, uninstall bool) error {
	if err := darwinInstallFlagError(); err != nil {
		return err
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	binary, err := os.Executable()
	if err != nil {
		return err
	}
	binary, err = darwinRealPath(binary)
	if err != nil {
		return err
	}
	if err := darwinStableBinary(binary, home); err != nil {
		return err
	}
	configPath, err = darwinRealPath(app.ExpandPath(configPath))
	if err != nil {
		return err
	}
	agentDir := darwinLaunchagentsDir
	if agentDir == "" {
		agentDir = filepath.Join(home, "Library", "LaunchAgents")
	}
	ops := darwinInstallOps{initialOnly: *darwinInstallIfMissing, command: runDarwinCommand, read: os.ReadFile, write: writeDarwinFile, remove: os.Remove}
	ops.query = func(label string) (darwinAgentIdentity, bool, error) { return queryDarwinAgent(label, ops.command) }
	return installDarwinAgent(configPath, binary, home, darwinLaunchdLabel, agentDir, uninstall, ops)
}

func darwinInstallFlagError() error {
	var unsupported string
	flag.Visit(func(f *flag.Flag) {
		switch f.Name {
		case "dryrun", "install-start", "install-autostart", "install-tray", "install-open-panel":
			unsupported = f.Name
		}
	})
	if unsupported == "" && windowsInstall.dryrun {
		unsupported = "dryrun"
	}
	if unsupported == "" && !windowsInstall.start {
		unsupported = "install-start"
	}
	if unsupported != "" {
		return fmt.Errorf("macOS 不支持 -%s，未修改安装", unsupported)
	}
	return nil
}

func darwinRealPath(path string) (string, error) {
	path, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	real, err := filepath.EvalSymlinks(path)
	if err == nil {
		return real, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	if target, linkErr := os.Readlink(path); linkErr == nil {
		if !filepath.IsAbs(target) {
			target = filepath.Join(filepath.Dir(path), target)
		}
		return darwinRealPath(target)
	}
	parent := filepath.Dir(path)
	if parent == path {
		return "", err
	}
	real, err = darwinRealPath(parent)
	return filepath.Join(real, filepath.Base(path)), err
}

func darwinStableBinary(binary, home string) error {
	if !strings.HasPrefix(binary, "/Volumes/") && !strings.Contains(binary, "/AppTranslocation/") {
		for _, root := range []string{"/Applications", filepath.Join(home, "Applications")} {
			real, err := darwinRealPath(root)
			if err == nil && strings.HasPrefix(binary, real+string(os.PathSeparator)) {
				return nil
			}
		}
	}
	return fmt.Errorf("请先把 Linkgate.app 拖进 Applications，再从那里启动；拒绝注册临时或未安装位置")
}

func runDarwinCommand(name string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	output, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
	if err != nil {
		return output, fmt.Errorf("%s: %w", filepath.Base(name), err)
	}
	return output, nil
}

func decodeDarwinAgent(data []byte) (darwinAgentIdentity, error) {
	cmd := exec.Command("/usr/bin/plutil", "-convert", "json", "-o", "-", "--", "-")
	cmd.Stdin = bytes.NewReader(data)
	output, err := cmd.Output()
	var identity darwinAgentIdentity
	if err == nil {
		err = json.Unmarshal(output, &identity)
	}
	return identity, err
}

// launchctl list emits its own dictionary syntax (semicolon-separated arrays), not a valid plist.
func decodeDarwinList(data []byte) (darwinAgentIdentity, error) {
	var identity darwinAgentIdentity
	invalid := fmt.Errorf("无法解析 launchctl list 输出，未修改服务")
	var lexer scanner.Scanner
	lexer.Init(bytes.NewReader(data))
	lexer.Mode = scanner.ScanStrings | scanner.ScanInts | scanner.ScanIdents
	bad := false
	lexer.Error = func(*scanner.Scanner, string) { bad = true }
	if lexer.Scan() != '{' {
		return identity, invalid
	}
	values := make(map[string]any)
	for token := lexer.Scan(); token != '}'; token = lexer.Scan() {
		if token != scanner.String {
			return identity, invalid
		}
		key, err := strconv.Unquote(lexer.TokenText())
		if err != nil || lexer.Scan() != '=' {
			return identity, invalid
		}
		if _, exists := values[key]; exists {
			return identity, invalid
		}
		token = lexer.Scan()
		var value any
		switch token {
		case scanner.String:
			value, err = strconv.Unquote(lexer.TokenText())
		case scanner.Int, '-':
			text := lexer.TokenText()
			if token == '-' {
				if lexer.Scan() != scanner.Int {
					return identity, invalid
				}
				text += lexer.TokenText()
			}
			value, err = strconv.Atoi(text)
		case scanner.Ident:
			text := lexer.TokenText()
			if text != "true" && text != "false" {
				return identity, invalid
			}
			value = text == "true"
		case '(':
			var args []string
			for item := lexer.Scan(); item != ')'; item = lexer.Scan() {
				if item != scanner.String {
					return identity, invalid
				}
				argument, err := strconv.Unquote(lexer.TokenText())
				if err != nil {
					return identity, invalid
				}
				args = append(args, argument)
				separator := lexer.Scan()
				if separator == ')' {
					break
				}
				if separator != ';' && separator != ',' {
					return identity, invalid
				}
			}
			value = args
		default:
			return identity, invalid
		}
		if err != nil || lexer.Scan() != ';' {
			return identity, invalid
		}
		values[key] = value
	}
	token := lexer.Scan()
	if token == ';' {
		token = lexer.Scan()
	}
	if token != scanner.EOF || bad {
		return identity, invalid
	}
	var ok bool
	identity.Label, ok = values["Label"].(string)
	if !ok {
		return identity, invalid
	}
	identity.ProgramArguments, ok = values["ProgramArguments"].([]string)
	if !ok {
		return identity, invalid
	}
	if value, exists := values["PID"]; exists {
		identity.PID, ok = value.(int)
		if !ok || identity.PID < 0 {
			return identity, invalid
		}
	}
	return identity, nil
}

func queryDarwinAgent(label string, command darwinCommand) (darwinAgentIdentity, bool, error) {
	output, err := command("/bin/launchctl", "list", label)
	if darwinAgentAbsent(err) {
		return darwinAgentIdentity{}, false, nil
	}
	if err != nil {
		return darwinAgentIdentity{}, false, err
	}
	identity, err := decodeDarwinList(output)
	return identity, err == nil, err
}

func darwinAgentAbsent(err error) bool {
	var exit *exec.ExitError
	return errors.As(err, &exit) && (exit.ExitCode() == 3 || exit.ExitCode() == 113)
}

func verifyDarwinOwner(identity darwinAgentIdentity, binary, config, label string) error {
	conflict := fmt.Errorf("LaunchAgent 属于其他安装或配置，请先从原安装位置卸载；未修改现有服务")
	if identity.Label != label || len(identity.ProgramArguments) < 3 {
		return conflict
	}
	oldBinary, err := darwinRealPath(identity.ProgramArguments[0])
	if err != nil || oldBinary != binary {
		return conflict
	}
	oldConfig := ""
	for i := 1; i < len(identity.ProgramArguments); i++ {
		if identity.ProgramArguments[i] == "-config" && i+1 < len(identity.ProgramArguments) {
			if oldConfig != "" {
				return conflict
			}
			oldConfig = identity.ProgramArguments[i+1]
			i++
		}
	}
	if oldConfig == "" {
		return conflict
	}
	oldConfig, err = darwinRealPath(app.ExpandPath(oldConfig))
	if err != nil || oldConfig != config {
		return conflict
	}
	return nil
}

func writeDarwinFile(path string, data []byte, mode os.FileMode) error {
	file, err := os.CreateTemp(filepath.Dir(path), ".linkgate-install-*")
	if err != nil {
		return err
	}
	if err = file.Chmod(mode); err == nil {
		_, err = file.Write(data)
	}
	closeErr := file.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	return os.Rename(file.Name(), path)
}

func installDarwinAgent(configPath, binary, home, label, agentDir string, uninstall bool, ops darwinInstallOps) error {
	if err := os.MkdirAll(agentDir, 0o700); err != nil {
		return err
	}
	lock, err := os.OpenFile(filepath.Join(agentDir, "."+label+".install-lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		return err
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
	plistPath := filepath.Join(agentDir, label+".plist")
	statePath := filepath.Join(filepath.Dir(configPath), "."+label+".install-state")
	domain := "gui/" + strconv.Itoa(os.Getuid())
	oldPlist, err := ops.read(plistPath)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	plistExists := err == nil
	if plistExists {
		identity, err := decodeDarwinAgent(oldPlist)
		if err != nil {
			return fmt.Errorf("无法核验现有 LaunchAgent，未修改: %w", err)
		}
		if err := verifyDarwinOwner(identity, binary, configPath, label); err != nil {
			return err
		}
	}
	live, loaded, err := ops.query(label)
	if err != nil {
		return err
	}
	if loaded {
		if err := verifyDarwinOwner(live, binary, configPath, label); err != nil {
			return err
		}
		if !plistExists {
			return fmt.Errorf("现有运行服务缺少 LaunchAgent 文件，请先从原安装位置修复；未停止服务")
		}
	}
	oldState, err := ops.read(statePath)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	stateExists := err == nil
	if ops.initialOnly && !uninstall {
		if strings.TrimSpace(string(oldState)) == "uninstalled" || (stateExists && !plistExists) {
			return fmt.Errorf("安装已被移除，不会自动重新安装，请明确运行 -install")
		}
		if plistExists {
			return nil
		}
	}
	remove := func(path string) error {
		if err := ops.remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return nil
	}
	bootout := func() error {
		_, err := ops.command("/bin/launchctl", "bootout", domain+"/"+label)
		if darwinAgentAbsent(err) {
			return nil
		}
		return err
	}
	rollback := func(cause error, bootstrapAttempted bool) error {
		var recovery []error
		if bootstrapAttempted {
			recovery = append(recovery, bootout())
		}
		var plistRestoreErr error
		if plistExists {
			plistRestoreErr = ops.write(plistPath, oldPlist, 0o600)
			if plistRestoreErr != nil {
				// Atomic replacement can fail before changing the old file (for example, a read-only directory).
				if current, readErr := ops.read(plistPath); readErr == nil && bytes.Equal(current, oldPlist) {
					plistRestoreErr = nil
				}
			}
		} else {
			plistRestoreErr = remove(plistPath)
		}
		recovery = append(recovery, plistRestoreErr)
		if stateExists {
			recovery = append(recovery, ops.write(statePath, oldState, 0o600))
		} else {
			recovery = append(recovery, remove(statePath))
		}
		if loaded && plistRestoreErr == nil {
			_, err := ops.command("/bin/launchctl", "bootstrap", domain, plistPath)
			recovery = append(recovery, err)
		}
		if err := errors.Join(recovery...); err != nil {
			return errors.Join(cause, fmt.Errorf("回滚失败，需要手动恢复原安装: %w", err))
		}
		return fmt.Errorf("操作失败，已恢复原安装状态: %w", cause)
	}

	if uninstall {
		if err := os.MkdirAll(filepath.Dir(statePath), 0o700); err != nil {
			return err
		}
		// Record explicit intent before stopping, so a still-running tray cannot reinstall it.
		if err := ops.write(statePath, []byte("uninstalled\n"), 0o600); err != nil {
			return err
		}
		if loaded {
			if err := bootout(); err != nil {
				if stateExists {
					return errors.Join(err, ops.write(statePath, oldState, 0o600))
				}
				return errors.Join(err, remove(statePath))
			}
		}
		if err := remove(plistPath); err != nil {
			return rollback(err, false)
		}
		return nil
	}

	if _, err := os.Stat(configPath); errors.Is(err, os.ErrNotExist) {
		if _, err := ops.command(binary, "-config", configPath, "-init"); err != nil {
			return err
		}
	} else if err != nil {
		return err
	}
	if _, err := ops.command(binary, "-config", configPath, "-check"); err != nil {
		return err
	}
	logPath := filepath.Join(filepath.Dir(configPath), "kernel.log")
	for _, dir := range []string{agentDir, filepath.Dir(logPath)} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return err
		}
	}
	if loaded {
		if err := bootout(); err != nil {
			return err
		}
	}
	if err := ops.write(plistPath, darwinAgentPlist(binary, configPath, home, label, logPath), 0o600); err != nil {
		return rollback(err, false)
	}
	if _, err := ops.command("/bin/launchctl", "bootstrap", domain, plistPath); err != nil {
		return rollback(err, true)
	}
	if err := ops.write(statePath, []byte("installed\n"), 0o600); err != nil {
		return rollback(err, true)
	}
	fmt.Println("Linkgate 内核 LaunchAgent 已启动")
	return nil
}

func darwinAgentPlist(binary, configPath, home, label, logPath string) []byte {
	escape := func(value string) string {
		var b bytes.Buffer
		_ = xml.EscapeText(&b, []byte(value))
		return b.String()
	}
	return []byte(fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key><string>%s</string>
  <key>ProgramArguments</key>
  <array><string>%s</string><string>-config</string><string>%s</string></array>
  <key>EnvironmentVariables</key>
  <dict><key>HOME</key><string>%s</string><key>LINKGATE_CONFIG</key><string>%s</string></dict>
  <key>RunAtLoad</key><true/>
  <key>KeepAlive</key><true/>
  <key>ProcessType</key><string>Background</string>
  <key>StandardOutPath</key><string>%s</string>
  <key>StandardErrorPath</key><string>%s</string>
</dict>
</plist>
`, escape(label), escape(binary), escape(configPath), escape(home), escape(configPath), escape(logPath), escape(logPath)))
}
