//go:build darwin || windows

// linkgate-tray 是托盘外壳：只负责状态展示与打开面板，代理本身在 linkgate 内核里。
//
// 它只通过管理 API（回环地址）与内核通信，不导入内核状态，也不统计 Token。
package main

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"linkgate/internal/app"

	"fyne.io/systray"
	"gopkg.in/yaml.v3"
)

// version 可用 -ldflags "-X main.version=x.y.z" 覆盖；连上内核后以内核版本为准。
var version string

func init() {
	if version == "" {
		version = app.Version
	}
}

//go:embed icons/tray-mac.png
var iconMac []byte

//go:embed icons/logo.png
var iconLogo []byte

//go:embed icons/tray-win.ico
var iconWin []byte

const (
	pollInterval        = 2 * time.Second
	detailedServiceRows = 6 // Larger configurations use a summary and a complete submenu.
	panelW              = 1000
	panelH              = 760
)

// ---------- 管理 API 客户端 ----------

type upstream struct {
	Name             string `json:"name"`
	Status           string `json:"status"`
	Enabled          bool   `json:"enabled"`
	LastProbeLatency int64  `json:"last_probe_latency_ms"`
	LastProbeError   string `json:"last_probe_error"`
}

type group struct {
	Name      string     `json:"name"`
	Active    string     `json:"active"`
	Upstreams []upstream `json:"upstreams"`
}

type status struct {
	Version    string  `json:"version"`
	UptimeSec  int64   `json:"uptime_seconds"`
	ConfigPath string  `json:"config_path"`
	Groups     []group `json:"groups"`
}

type client struct {
	base  string // http://127.0.0.1:8766
	token string
	http  *http.Client
}

func (c *client) do(method, path string, body []byte, out any) error {
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequest(method, c.base+path, rd)
	if err != nil {
		return err
	}
	req.Header.Set("X-Linkgate-Token", c.token)
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if resp.StatusCode != http.StatusOK {
		var e struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(b, &e)
		if e.Error == "" {
			e.Error = strings.TrimSpace(string(b))
		}
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, e.Error)
	}
	if out != nil {
		return json.Unmarshal(b, out)
	}
	return nil
}

func (c *client) status() (*status, error) {
	var s status
	if err := c.do(http.MethodGet, "/__linkgate/v1/status", nil, &s); err != nil {
		return nil, err
	}
	return &s, nil
}

// reload 让内核重新读取磁盘上的配置并热加载，不写盘、不产生 .bak。
func (c *client) reload() error {
	return c.do(http.MethodPost, "/__linkgate/v1/reload", nil, nil)
}

// ---------- 本机配置定位（只读，不校验整份配置） ----------

type localSettings struct {
	configPath string
	adminAddr  string
	token      string
	tokenErr   error
}

func expand(p string) string {
	p = strings.TrimSpace(p)
	if p == "~" || strings.HasPrefix(p, "~/") || strings.HasPrefix(p, `~\`) {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, strings.TrimLeft(p[1:], `/\`))
		}
	}
	return p
}

func defaultConfigPath() string {
	if p := strings.TrimSpace(os.Getenv("LINKGATE_CONFIG")); p != "" {
		return expand(p)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "linkgate.yaml"
	}
	return filepath.Join(home, ".linkgate", "config.yaml")
}

func loadSettings(configPath, adminOverride string) localSettings {
	s := localSettings{configPath: configPath, adminAddr: "127.0.0.1:8766"}

	// 只取 admin 与 data_dir 两项；配置有别的错误时托盘仍能工作。
	var partial struct {
		DataDir string `yaml:"data_dir"`
		Admin   struct {
			Listen    string `yaml:"listen"`
			TokenFile string `yaml:"token_file"`
		} `yaml:"admin"`
	}
	if b, err := os.ReadFile(configPath); err == nil {
		_ = yaml.Unmarshal(b, &partial)
	}
	if partial.Admin.Listen != "" {
		s.adminAddr = partial.Admin.Listen
	}
	if v := strings.TrimSpace(os.Getenv("LINKGATE_ADMIN")); v != "" {
		s.adminAddr = v
	}
	if adminOverride != "" {
		s.adminAddr = adminOverride
	}
	// 监听 :8766 或 0.0.0.0 时，托盘一律走回环地址
	if strings.HasPrefix(s.adminAddr, ":") {
		s.adminAddr = "127.0.0.1" + s.adminAddr
	}
	s.adminAddr = strings.Replace(s.adminAddr, "0.0.0.0:", "127.0.0.1:", 1)

	if t := strings.TrimSpace(os.Getenv("LINKGATE_TOKEN")); t != "" {
		s.token = t
		return s
	}
	tokenPath := expand(partial.Admin.TokenFile)
	if tokenPath == "" {
		dataDir := expand(partial.DataDir)
		if dataDir == "" {
			home, _ := os.UserHomeDir()
			dataDir = filepath.Join(home, ".linkgate")
		}
		tokenPath = filepath.Join(dataDir, "admin.token")
	}
	b, err := os.ReadFile(tokenPath)
	if err != nil {
		s.tokenErr = fmt.Errorf("读取令牌 %s 失败: %w", tokenPath, err)
		return s
	}
	s.token = strings.TrimSpace(string(b))
	return s
}

// ---------- 文案 ----------

var statusText = map[string]string{
	"healthy":   "正常",
	"unhealthy": "不可用",
	"unknown":   "未知",
	"disabled":  "已停用",
}

func groupSummary(g group) string {
	if g.Active == "" {
		return g.Name + " · 无可用线路"
	}
	for _, u := range g.Upstreams {
		if u.Name != g.Active {
			continue
		}
		st := statusText[u.Status]
		if st == "" {
			st = u.Status
		}
		line := fmt.Sprintf("%s · %s %s", g.Name, u.Name, st)
		if u.LastProbeLatency > 0 {
			line += fmt.Sprintf(" %dms", u.LastProbeLatency)
		}
		return line
	}
	return g.Name + " · " + g.Active
}

func servicesSummary(groups []group) string {
	normal := 0
	for _, g := range groups {
		for _, u := range g.Upstreams {
			if u.Name == g.Active && u.Enabled && u.Status == "healthy" {
				normal++
				break
			}
		}
	}
	return fmt.Sprintf("%d 个服务 · %d 正常", len(groups), normal)
}

func groupTooltip(g group) string {
	parts := make([]string, 0, len(g.Upstreams))
	for _, u := range g.Upstreams {
		st := statusText[u.Status]
		if st == "" {
			st = u.Status
		}
		parts = append(parts, u.Name+" "+st)
	}
	return strings.Join(parts, "，")
}

func formatUptime(sec int64) string {
	d := time.Duration(sec) * time.Second
	switch {
	case d >= 24*time.Hour:
		return fmt.Sprintf("%d天%d小时", int(d.Hours())/24, int(d.Hours())%24)
	case d >= time.Hour:
		return fmt.Sprintf("%d小时%d分", int(d.Hours()), int(d.Minutes())%60)
	default:
		return fmt.Sprintf("%d分", int(d.Minutes()))
	}
}

// ---------- 打开面板 / 文件 ----------

func browserCandidates() []string {
	switch runtime.GOOS {
	case "darwin":
		home, _ := os.UserHomeDir()
		var out []string
		for _, root := range []string{"/Applications", filepath.Join(home, "Applications")} {
			out = append(out,
				filepath.Join(root, "Google Chrome.app/Contents/MacOS/Google Chrome"),
				filepath.Join(root, "Microsoft Edge.app/Contents/MacOS/Microsoft Edge"),
			)
		}
		return out
	case "windows":
		var out []string
		for _, env := range []string{"ProgramFiles", "ProgramFiles(x86)", "LocalAppData"} {
			root := os.Getenv(env)
			if root == "" {
				continue
			}
			out = append(out,
				filepath.Join(root, `Google\Chrome\Application\chrome.exe`),
				filepath.Join(root, `Microsoft\Edge\Application\msedge.exe`),
			)
		}
		return out
	}
	return nil
}

func openPanel(panelURL string) error {
	for _, bin := range browserCandidates() {
		if _, err := os.Stat(bin); err != nil {
			continue
		}
		cmd := exec.Command(bin,
			"--app="+panelURL,
			fmt.Sprintf("--window-size=%d,%d", panelW, panelH),
		)
		if err := cmd.Start(); err == nil {
			go func() { _ = cmd.Wait() }()
			return nil
		}
	}
	return openDefault(panelURL)
}

func openDefault(target string) error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", target)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", target)
	default:
		return fmt.Errorf("不支持的平台 %s", runtime.GOOS)
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	go func() { _ = cmd.Wait() }()
	return nil
}

func openConfigFile(path string) error {
	if _, err := os.Stat(path); err != nil {
		return err
	}
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", "-t", path) // 用默认文本编辑器，避免 .yaml 无关联
	case "windows":
		cmd = exec.Command("notepad.exe", path)
	default:
		return fmt.Errorf("不支持的平台 %s", runtime.GOOS)
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	go func() { _ = cmd.Wait() }()
	return nil
}

// ---------- 托盘 ----------

type tray struct {
	settings localSettings
	cli      *client

	mu        sync.Mutex
	connected bool
	lastErr   string

	mTitle      *systray.MenuItem
	mInfo       *systray.MenuItem
	mGroups     []*systray.MenuItem
	mServices   *systray.MenuItem
	serviceRows map[string]*systray.MenuItem
	mRetry      *systray.MenuItem
	mPanel      *systray.MenuItem
	mReload     *systray.MenuItem
	mConfig     *systray.MenuItem
	mQuit       *systray.MenuItem

	refreshNow chan struct{}
}

func newTray(s localSettings) *tray {
	return &tray{
		settings: s,
		cli: &client{
			base:  "http://" + s.adminAddr,
			token: s.token,
			http:  &http.Client{Timeout: 1500 * time.Millisecond},
		},
		refreshNow:  make(chan struct{}, 1),
		serviceRows: make(map[string]*systray.MenuItem),
	}
}

func (t *tray) panelURL() string {
	return t.cli.base + "/__linkgate/ui#token=" + url.QueryEscape(t.cli.token)
}

func (t *tray) onReady() {
	if runtime.GOOS == "darwin" {
		systray.SetTemplateIcon(iconMac, iconMac)
	} else {
		icon, err := windowsIcon(iconLogo)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			icon = iconWin // Retain the generated ICO if the PNG is ever invalid.
		}
		systray.SetIcon(icon)
	}
	systray.SetTooltip("Linkgate")

	t.mTitle = systray.AddMenuItem("Linkgate · "+version, "")
	t.mTitle.Disable()
	t.mInfo = systray.AddMenuItem("未连接", "")
	t.mInfo.Disable()
	for i := 0; i < detailedServiceRows; i++ {
		m := systray.AddMenuItem("", "")
		m.Disable()
		m.Hide()
		t.mGroups = append(t.mGroups, m)
	}
	t.mServices = systray.AddMenuItem("服务", "展开查看所有服务的当前线路")
	t.mServices.Hide()
	t.mRetry = systray.AddMenuItem("重试连接", "立即重新拉取内核状态")
	systray.AddSeparator()
	t.mPanel = systray.AddMenuItem("打开面板", "")
	t.mReload = systray.AddMenuItem("重新加载配置", "让内核重新读取配置文件并热加载")
	t.mConfig = systray.AddMenuItem("打开配置文件", t.settings.configPath)
	systray.AddSeparator()
	t.mQuit = systray.AddMenuItem("退出", "只退出托盘，内核继续运行")

	go t.loop()
	go t.handleClicks()
}

func (t *tray) handleClicks() {
	for {
		select {
		case <-t.mRetry.ClickedCh:
			t.kick()
		case <-t.mPanel.ClickedCh:
			if err := openPanel(t.panelURL()); err != nil {
				t.setInfo("打开面板失败: " + err.Error())
			}
		case <-t.mReload.ClickedCh:
			if err := t.cli.reload(); err != nil {
				t.setInfo("重新加载失败: " + err.Error())
			} else {
				t.setInfo("配置已重新加载")
			}
			t.kick()
		case <-t.mConfig.ClickedCh:
			if err := openConfigFile(t.settings.configPath); err != nil {
				t.setInfo("打开配置文件失败: " + err.Error())
			}
		case <-t.mQuit.ClickedCh:
			systray.Quit()
			return
		}
	}
}

func (t *tray) kick() {
	select {
	case t.refreshNow <- struct{}{}:
	default:
	}
}

func (t *tray) setInfo(s string) {
	t.mInfo.SetTitle(truncate(s, 60))
	t.mInfo.SetTooltip(s)
}

func (t *tray) loop() {
	tick := time.NewTicker(pollInterval)
	defer tick.Stop()
	t.refresh()
	for {
		select {
		case <-tick.C:
		case <-t.refreshNow:
		}
		t.refresh()
	}
}

func (t *tray) refresh() {
	if t.settings.tokenErr != nil && t.cli.token == "" {
		// 令牌可能在内核首次启动后才生成，每轮重读一次
		fresh := loadSettings(t.settings.configPath, "")
		if fresh.token != "" {
			t.cli.token = fresh.token
			t.settings.tokenErr = nil
		}
	}

	st, err := t.cli.status()
	if err != nil {
		t.mu.Lock()
		t.connected = false
		t.mu.Unlock()
		msg := "未连接：" + shortErr(err)
		if t.settings.tokenErr != nil {
			msg = "未连接：" + t.settings.tokenErr.Error()
		}
		t.mTitle.SetTitle("Linkgate · " + version)
		t.setInfo(msg)
		for _, m := range t.mGroups {
			m.Hide()
		}
		t.mServices.Hide()
		t.mRetry.Show()
		t.mPanel.Disable()
		t.mReload.Disable()
		systray.SetTooltip("Linkgate · 未连接")
		return
	}

	t.mu.Lock()
	t.connected = true
	t.mu.Unlock()

	ver := st.Version
	if ver == "" {
		ver = version
	}
	t.mTitle.SetTitle("Linkgate · " + ver)
	t.mInfo.SetTitle("运行 " + formatUptime(st.UptimeSec))
	t.mInfo.SetTooltip(st.ConfigPath)
	t.mRetry.Hide()
	t.mPanel.Enable()
	t.mReload.Enable()

	tips := []string{"Linkgate " + ver, servicesSummary(st.Groups)}
	seen := make(map[string]bool, len(st.Groups))
	for _, g := range st.Groups {
		line := groupSummary(g)
		tips = append(tips, line)
		seen[g.Name] = true
		m := t.serviceRows[g.Name]
		if m == nil {
			m = t.mServices.AddSubMenuItem(line, groupTooltip(g))
			m.Disable()
			t.serviceRows[g.Name] = m
		}
		m.SetTitle(truncate(line, 72))
		m.SetTooltip(groupTooltip(g))
		m.Show()
	}
	for name, m := range t.serviceRows {
		if !seen[name] {
			m.Hide()
		}
	}
	if len(st.Groups) > detailedServiceRows {
		for _, m := range t.mGroups {
			m.Hide()
		}
		t.mServices.SetTitle(servicesSummary(st.Groups))
		t.mServices.Show()
	} else {
		t.mServices.Hide()
		for i, m := range t.mGroups {
			if i < len(st.Groups) {
				m.SetTitle(truncate(groupSummary(st.Groups[i]), 72))
				m.SetTooltip(groupTooltip(st.Groups[i]))
				m.Show()
			} else {
				m.Hide()
			}
		}
	}
	if len(st.Groups) == 0 {
		t.mGroups[0].SetTitle("暂无服务 · 在面板中添加")
		t.mGroups[0].Show()
	}
	systray.SetTooltip(strings.Join(tips, "\n"))
}

func shortErr(err error) string {
	s := err.Error()
	if strings.Contains(s, "connection refused") || strings.Contains(s, "actively refused") {
		return "内核未运行"
	}
	if strings.Contains(s, "Client.Timeout") || strings.Contains(s, "timeout") {
		return "请求超时"
	}
	if strings.HasPrefix(s, "HTTP 401") {
		return "令牌不匹配"
	}
	return truncate(s, 48)
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}

func main() {
	configPath := flag.String("config", defaultConfigPath(), "配置文件路径（默认 LINKGATE_CONFIG 或 ~/.linkgate/config.yaml）")
	adminAddr := flag.String("admin", "", "管理接口地址，覆盖配置里的 admin.listen（也可用 LINKGATE_ADMIN）")
	once := flag.Bool("once", false, "只拉取一次状态并打印菜单摘要，然后退出（用于排查）")
	showVersion := flag.Bool("version", false, "打印托盘版本后退出")
	flag.Parse()
	if *showVersion {
		fmt.Printf("linkgate-tray %s\n", version)
		return
	}

	s := loadSettings(expand(*configPath), *adminAddr)
	t := newTray(s)

	if *once {
		st, err := t.cli.status()
		if err != nil {
			if s.tokenErr != nil {
				fmt.Fprintln(os.Stderr, s.tokenErr)
			}
			fmt.Fprintln(os.Stderr, "未连接:", err)
			os.Exit(1)
		}
		fmt.Printf("Linkgate · %s  运行 %s\n", st.Version, formatUptime(st.UptimeSec))
		for _, g := range st.Groups {
			fmt.Println(groupSummary(g))
		}
		return
	}

	systray.Run(t.onReady, func() {})
}
