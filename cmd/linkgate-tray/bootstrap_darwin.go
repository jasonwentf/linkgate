//go:build darwin

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"text/scanner"
	"time"

	"gopkg.in/yaml.v3"
)

var darwinLaunchdLabel = "dev.linkgate.kernel"
var darwinLaunchagentsDir string
var darwinBootstrapStates sync.Map

const darwinRecoveryLimit = 3

type darwinBootstrapState struct {
	initialAttempt bool
	observed       bool
	failures       int
	retryAfter     time.Time
	lastErr        error
	terminal       error
}

type darwinTrayAgent struct {
	Label            string
	ProgramArguments []string
	PID              int
}

type darwinBootstrapOps struct {
	read    func(string) ([]byte, error)
	command func(string, ...string) ([]byte, error)
	query   func(string) (darwinTrayAgent, bool, error)
	health  func(*client) bool
	wait    func(*tray, string) error
	now     func() time.Time
}

func init() {
	bootstrapKernel = bootstrapDarwinKernel
}

func bundledKernel(executable string) string {
	dir := filepath.Dir(executable)
	contents := filepath.Dir(dir)
	if filepath.Base(dir) != "MacOS" || filepath.Base(contents) != "Contents" ||
		!strings.HasSuffix(filepath.Dir(contents), ".app") {
		return ""
	}
	return filepath.Join(dir, "linkgate")
}

func trayRealPath(path string) (string, error) {
	path, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	real, err := filepath.EvalSymlinks(path)
	if err == nil || !errors.Is(err, os.ErrNotExist) {
		return real, err
	}
	if target, linkErr := os.Readlink(path); linkErr == nil {
		if !filepath.IsAbs(target) {
			target = filepath.Join(filepath.Dir(path), target)
		}
		return trayRealPath(target)
	}
	parent := filepath.Dir(path)
	if parent == path {
		return "", err
	}
	real, err = trayRealPath(parent)
	return filepath.Join(real, filepath.Base(path)), err
}

func stableDarwinBundle(kernel, home string) bool {
	if strings.HasPrefix(kernel, "/Volumes/") || strings.Contains(kernel, "/AppTranslocation/") {
		return false
	}
	for _, root := range []string{"/Applications", filepath.Join(home, "Applications")} {
		real, err := trayRealPath(root)
		if err == nil && strings.HasPrefix(kernel, real+string(os.PathSeparator)) {
			return true
		}
	}
	return false
}

func bootstrapDarwinKernel(t *tray) (bool, error) {
	executable, err := os.Executable()
	if err != nil {
		return false, err
	}
	kernel := bundledKernel(executable)
	if kernel == "" {
		return false, nil
	}
	kernel, err = trayRealPath(kernel)
	if err != nil {
		return false, err
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return false, err
	}
	if !stableDarwinBundle(kernel, home) {
		t.settings.tokenErr = nil
		return false, fmt.Errorf("启动失败：请先把 Linkgate.app 拖进 Applications，再从那里启动")
	}
	value, _ := darwinBootstrapStates.LoadOrStore(t, &darwinBootstrapState{})
	state := value.(*darwinBootstrapState)
	agentDir := darwinLaunchagentsDir
	if agentDir == "" {
		agentDir = filepath.Join(home, "Library", "LaunchAgents")
	}
	ops := darwinBootstrapOps{read: os.ReadFile, command: runDarwinTrayCommand, health: kernelReachable, wait: waitDarwinKernel, now: time.Now}
	ops.query = func(label string) (darwinTrayAgent, bool, error) { return queryDarwinTrayAgent(label, ops.command) }
	started, err := ensureDarwinKernel(t, state, kernel, darwinLaunchdLabel, agentDir, ops)
	if err != nil {
		// The shared refresh path otherwise hides startup errors behind a missing-token error.
		t.settings.tokenErr = nil
	}
	return started, err
}

func runDarwinTrayCommand(name string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	output, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
	if err != nil {
		return output, fmt.Errorf("%s: %w", filepath.Base(name), err)
	}
	return output, nil
}

func decodeDarwinTrayAgent(data []byte) (darwinTrayAgent, error) {
	cmd := exec.Command("/usr/bin/plutil", "-convert", "json", "-o", "-", "--", "-")
	cmd.Stdin = bytes.NewReader(data)
	output, err := cmd.Output()
	var identity darwinTrayAgent
	if err == nil {
		err = json.Unmarshal(output, &identity)
	}
	return identity, err
}

// launchctl list uses semicolon-separated arrays; it cannot be decoded by plutil.
func decodeDarwinTrayList(data []byte) (darwinTrayAgent, error) {
	var identity darwinTrayAgent
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

func queryDarwinTrayAgent(label string, command func(string, ...string) ([]byte, error)) (darwinTrayAgent, bool, error) {
	output, err := command("/bin/launchctl", "list", label)
	var exit *exec.ExitError
	if errors.As(err, &exit) && (exit.ExitCode() == 3 || exit.ExitCode() == 113) {
		return darwinTrayAgent{}, false, nil
	}
	if err != nil {
		return darwinTrayAgent{}, false, err
	}
	identity, err := decodeDarwinTrayList(output)
	return identity, err == nil, err
}

func verifyDarwinTrayOwner(identity darwinTrayAgent, kernel, config, label string) error {
	conflict := fmt.Errorf("启动失败：LaunchAgent 属于其他安装或配置，请先从原安装位置卸载")
	if identity.Label != label || len(identity.ProgramArguments) < 3 {
		return conflict
	}
	binary, err := trayRealPath(identity.ProgramArguments[0])
	if err != nil || binary != kernel {
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
	oldConfig, err = trayRealPath(expand(oldConfig))
	if err != nil || oldConfig != config {
		return conflict
	}
	return nil
}

func ensureDarwinKernel(t *tray, state *darwinBootstrapState, kernel, label, agentDir string, ops darwinBootstrapOps) (bool, error) {
	terminal := func(err error) (bool, error) { state.terminal = err; return false, err }
	fail := func(err error) (bool, error) {
		state.failures++
		state.retryAfter = ops.now().Add(30 * time.Second)
		state.lastErr = err
		if state.failures >= darwinRecoveryLimit {
			return terminal(fmt.Errorf("启动失败：自动恢复已停止（%d 次），请手动检查配置或重新安装: %w", state.failures, err))
		}
		return false, err
	}
	config, err := trayRealPath(t.settings.configPath)
	if err != nil {
		return terminal(fmt.Errorf("启动失败：无法定位配置: %w", err))
	}
	var partial struct {
		Admin struct {
			Disabled bool `yaml:"disabled"`
		} `yaml:"admin"`
	}
	if data, err := ops.read(config); err == nil {
		if err := yaml.Unmarshal(data, &partial); err != nil {
			return terminal(fmt.Errorf("启动失败：无法读取配置，未修改服务: %w", err))
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return terminal(fmt.Errorf("启动失败：无法读取配置: %w", err))
	}
	if partial.Admin.Disabled {
		return false, fmt.Errorf("管理接口已禁用，内核不会被重启")
	}
	if ops.health(t.cli) {
		state.failures, state.lastErr, state.terminal = 0, nil, nil
		return false, nil
	}
	if state.terminal != nil {
		return false, state.terminal
	}
	if ops.now().Before(state.retryAfter) {
		return false, state.lastErr
	}
	statePath := filepath.Join(filepath.Dir(config), "."+label+".install-state")
	plistPath := filepath.Join(agentDir, label+".plist")
	if _, err := os.Stat(plistPath); err == nil {
		// Serialize recovery with install/uninstall, then re-read their durable intent under the lock.
		lock, err := os.OpenFile(filepath.Join(agentDir, "."+label+".install-lock"), os.O_CREATE|os.O_RDWR, 0o600)
		if err != nil {
			return fail(fmt.Errorf("启动失败：无法锁定安装状态: %w", err))
		}
		defer lock.Close()
		if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
			return false, fmt.Errorf("安装或卸载操作正在进行，暂不恢复内核")
		}
		defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
	}
	marker, err := ops.read(statePath)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return terminal(fmt.Errorf("启动失败：无法核验安装状态: %w", err))
	}
	if strings.TrimSpace(string(marker)) == "uninstalled" {
		return terminal(fmt.Errorf("已卸载内核；如需恢复，请明确运行 -install，不会自动重新安装"))
	}
	plist, err := ops.read(plistPath)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return terminal(fmt.Errorf("启动失败：无法读取 LaunchAgent: %w", err))
	}
	plistExists := err == nil
	if plistExists {
		identity, err := decodeDarwinTrayAgent(plist)
		if err != nil {
			return terminal(fmt.Errorf("启动失败：无法核验 LaunchAgent，未修改服务: %w", err))
		}
		if err := verifyDarwinTrayOwner(identity, kernel, config, label); err != nil {
			return terminal(err)
		}
	}
	live, loaded, err := ops.query(label)
	if err != nil {
		return fail(fmt.Errorf("启动失败：无法核验 launchd，未修改服务: %w", err))
	}
	if loaded {
		if err := verifyDarwinTrayOwner(live, kernel, config, label); err != nil {
			return terminal(err)
		}
	}
	if !plistExists && (state.observed || loaded || len(marker) != 0 || state.initialAttempt) {
		return terminal(fmt.Errorf("启动失败：安装项已移除或首次安装失败，请明确运行 -install；不会自动重装"))
	}
	if !plistExists {
		// Initialization is attempted once. Runtime recovery never invokes -install.
		state.initialAttempt = true
		if _, err := ops.command(kernel, "-config", config, "-install", "-install-if-missing"); err != nil {
			return terminal(fmt.Errorf("启动失败：首次安装失败，请手动检查后运行 -install: %w", err))
		}
		state.observed = true
	} else {
		state.observed = true
		if loaded && live.PID > 0 {
			return fail(fmt.Errorf("启动失败：内核运行中但管理接口不可用，不会停止或重装服务"))
		}
		if loaded {
			// Without -k, kickstart does not kill an already-running process.
			_, err = ops.command("/bin/launchctl", "kickstart", "gui/"+strconv.Itoa(os.Getuid())+"/"+label)
		} else {
			_, err = ops.command("/bin/launchctl", "bootstrap", "gui/"+strconv.Itoa(os.Getuid()), plistPath)
		}
		if err != nil {
			return fail(fmt.Errorf("启动失败：恢复已有 LaunchAgent 失败: %w", err))
		}
	}
	if err := ops.wait(t, statePath); err != nil {
		return fail(fmt.Errorf("启动失败：内核未就绪: %w", err))
	}
	state.failures, state.lastErr = 0, nil
	return true, nil
}

func waitDarwinKernel(t *tray, statePath string) error {
	deadline := time.Now().Add(10 * time.Second)
	for {
		if marker, err := os.ReadFile(statePath); err == nil && strings.TrimSpace(string(marker)) == "uninstalled" {
			return fmt.Errorf("内核已被显式卸载，停止恢复")
		}
		fresh := loadSettings(t.settings.configPath, t.settings.adminOverride)
		t.settings = fresh
		t.cli.base = "http://" + fresh.adminAddr
		t.cli.token = fresh.token
		if kernelReachable(t.cli) && fresh.tokenErr == nil {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("请查看配置目录中的 kernel.log")
		}
		time.Sleep(200 * time.Millisecond)
	}
}

func kernelReachable(cli *client) bool {
	resp, err := cli.http.Get(cli.base + "/__linkgate/v1/health")
	if err != nil {
		return false
	}
	_ = resp.Body.Close()
	return resp.StatusCode >= http.StatusOK
}
