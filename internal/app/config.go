// Package app 是 Linkgate 的全部运行时实现。
//
// 设计约束（改动前先读）：
//  1. 内核是纯 Go、无 cgo、无平台分支，必须在 darwin/windows/linux 上都能交叉编译。
//  2. 代理对请求是透明的：不改请求体、不改模型名、不改 Authorization；
//     唯一允许改的是 Host（按上游配置）和可选的附加请求头（如 CF Access）。
//  3. 只在“连接尚未建立”时换线路重试。一旦连接建立并写过请求体，
//     任何失败都返回错误，绝不重发，避免重复计费。
package app

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Duration 允许在 YAML 里写 "900s"、"2m" 这类字符串。
type Duration struct{ time.Duration }

func (d *Duration) UnmarshalYAML(node *yaml.Node) error {
	var s string
	if err := node.Decode(&s); err != nil {
		return err
	}
	s = strings.TrimSpace(s)
	if s == "" {
		d.Duration = 0
		return nil
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("无效时长 %q（示例：2s、90s、10m）: %w", s, err)
	}
	if v < 0 {
		return fmt.Errorf("时长不能为负: %q", s)
	}
	d.Duration = v
	return nil
}

func (d Duration) MarshalYAML() (any, error) { return d.Duration.String(), nil }

// Or 返回自身；为零值时返回 def。
func (d Duration) Or(def time.Duration) time.Duration {
	if d.Duration <= 0 {
		return def
	}
	return d.Duration
}

type Config struct {
	Version int           `yaml:"version" json:"version"`
	DataDir string        `yaml:"data_dir,omitempty" json:"data_dir,omitempty"`
	Admin   AdminConfig   `yaml:"admin" json:"admin"`
	Log     LogConfig     `yaml:"log" json:"log"`
	Buffer  BufferConfig  `yaml:"buffer" json:"buffer"`
	HTTP    HTTPConfig    `yaml:"http" json:"http"`
	Retry   RetryConfig   `yaml:"retry" json:"retry"`
	Health  HealthConfig  `yaml:"health" json:"health"`
	Groups  []GroupConfig `yaml:"groups" json:"groups"`
}

type AdminConfig struct {
	Listen    string `yaml:"listen" json:"listen"`
	TokenFile string `yaml:"token_file,omitempty" json:"token_file,omitempty"`
	// Disabled 关闭管理接口与面板（此时只能用配置文件）。
	Disabled bool `yaml:"disabled,omitempty" json:"disabled,omitempty"`
}

type LogConfig struct {
	Level    string `yaml:"level" json:"level"`
	Format   string `yaml:"format" json:"format"` // text | json
	File     string `yaml:"file,omitempty" json:"file,omitempty"`
	RingSize int    `yaml:"ring_size,omitempty" json:"ring_size,omitempty"`
}

type BufferConfig struct {
	// MaxRequestBytes 超过即直接 413，避免把内存或磁盘打爆。
	MaxRequestBytes int64 `yaml:"max_request_bytes" json:"max_request_bytes"`
	// SpillThreshold 小于此值放内存，大于则落临时文件（请求体常达数 MB）。
	SpillThreshold int64 `yaml:"spill_threshold" json:"spill_threshold"`
	// MaxConcurrent 同时缓冲的请求数上限，超出排队。
	MaxConcurrent int `yaml:"max_concurrent" json:"max_concurrent"`
	// TempDir 落盘位置，空则用系统临时目录。
	TempDir string `yaml:"temp_dir,omitempty" json:"temp_dir,omitempty"`
	// ReadIdleTimeout 是从客户端读取请求体的总期限（本机回环场景，默认 120s）。
	// 客户端发完请求头就停住不传时，不能让 handler 永久占用并发槽位。
	ReadIdleTimeout Duration `yaml:"read_idle_timeout" json:"read_idle_timeout"`
}

type HTTPConfig struct {
	ConnectTimeout        Duration `yaml:"connect_timeout" json:"connect_timeout"`
	TLSHandshakeTimeout   Duration `yaml:"tls_handshake_timeout" json:"tls_handshake_timeout"`
	ResponseHeaderTimeout Duration `yaml:"response_header_timeout" json:"response_header_timeout"`
	StreamIdleTimeout     Duration `yaml:"stream_idle_timeout" json:"stream_idle_timeout"`
	// UploadIdleTimeout 是“上传请求体时多久没有进展算卡死”。
	// 大请求体在慢线路上可能要几分钟，所以看的是“有没有进展”，不是总时长。
	UploadIdleTimeout Duration `yaml:"upload_idle_timeout" json:"upload_idle_timeout"`
	IdleConnTimeout   Duration `yaml:"idle_conn_timeout" json:"idle_conn_timeout"`
	// ReconnectAfterIdle 大于 0 时，距上次成功请求超过该时长就先关掉空闲连接，
	// 强制重新建连：让“线路已死但连接池里还有一根旧连接”变成干净的拨号失败，
	// 从而可以安全换线路。设 0 关闭。
	ReconnectAfterIdle      Duration `yaml:"reconnect_after_idle" json:"reconnect_after_idle"`
	KeepAlive               Duration `yaml:"keep_alive" json:"keep_alive"`
	MaxIdleConnsPerHost     int      `yaml:"max_idle_conns_per_host" json:"max_idle_conns_per_host"`
	DisableCompression      bool     `yaml:"disable_compression" json:"disable_compression"`
	ResponseHeaderWarnAfter Duration `yaml:"response_header_warn_after,omitempty" json:"response_header_warn_after,omitempty"`
}

type RetryConfig struct {
	// MaxAttempts 含首次尝试。1 表示不换线路。
	MaxAttempts int `yaml:"max_attempts" json:"max_attempts"`
	// Mode=connect 时只对“连接未建立”的失败换线路（安全，不会重复计费）。
	// Mode=none 关闭换线路。任何其他值都视为配置错误。
	Mode    string   `yaml:"mode" json:"mode"`
	Backoff Duration `yaml:"backoff,omitempty" json:"backoff,omitempty"`
}

type HealthConfig struct {
	Enabled          bool     `yaml:"enabled" json:"enabled"`
	Interval         Duration `yaml:"interval" json:"interval"`
	Timeout          Duration `yaml:"timeout" json:"timeout"`
	FailThreshold    int      `yaml:"fail_threshold" json:"fail_threshold"`
	RecoverThreshold int      `yaml:"recover_threshold" json:"recover_threshold"`
	Cooldown         Duration `yaml:"cooldown" json:"cooldown"`
	ProbePath        string   `yaml:"probe_path" json:"probe_path"`
	ProbeMethod      string   `yaml:"probe_method" json:"probe_method"`
	// ExpectStatus 探针认作健康的响应码；0 表示 200-399。
	ExpectStatus int `yaml:"expect_status,omitempty" json:"expect_status,omitempty"`
}

type GroupConfig struct {
	Name    string `yaml:"name" json:"name"`
	Listen  string `yaml:"listen" json:"listen"`
	Comment string `yaml:"comment,omitempty" json:"comment,omitempty"`
	// StripPrefix 转发前去掉的路径前缀（一般留空）。
	StripPrefix string `yaml:"strip_prefix,omitempty" json:"strip_prefix,omitempty"`
	// AddHeaders 该组所有上游共用的附加请求头（一般留空）。
	AddHeaders map[string]string `yaml:"add_headers,omitempty" json:"add_headers,omitempty"`

	TimeoutOverrides HTTPConfig `yaml:"timeout_overrides,omitempty" json:"timeout_overrides,omitempty"`

	Upstreams []UpstreamConfig `yaml:"upstreams" json:"upstreams"`
}

type UpstreamConfig struct {
	Name     string `yaml:"name" json:"name"`
	URL      string `yaml:"url" json:"url"`
	Priority int    `yaml:"priority" json:"priority"`
	Enabled  *bool  `yaml:"enabled,omitempty" json:"enabled,omitempty"`
	Comment  string `yaml:"comment,omitempty" json:"comment,omitempty"`

	// Host 覆盖发往上游的 Host 头；留空则用上游 URL 的 host:port。
	Host string `yaml:"host,omitempty" json:"host,omitempty"`
	// KeepClientHost 为 true 时原样转发客户端 Host（优先于 Host）。
	KeepClientHost bool `yaml:"keep_client_host,omitempty" json:"keep_client_host,omitempty"`
	// Headers 只加在这一条上游上的请求头，典型用途是 Cloudflare Access。
	Headers map[string]string `yaml:"headers,omitempty" json:"headers,omitempty"`

	TLSInsecureSkipVerify bool   `yaml:"tls_insecure_skip_verify,omitempty" json:"tls_insecure_skip_verify,omitempty"`
	TLSServerName         string `yaml:"tls_server_name,omitempty" json:"tls_server_name,omitempty"`

	HealthPath            string   `yaml:"health_path,omitempty" json:"health_path,omitempty"`
	ConnectTimeout        Duration `yaml:"connect_timeout,omitempty" json:"connect_timeout,omitempty"`
	ResponseHeaderTimeout Duration `yaml:"response_header_timeout,omitempty" json:"response_header_timeout,omitempty"`

	// MaxBodyBytes 大于 0 时，请求体超过该值的请求不会走这条线路。
	// 典型用法：Cloudflare 边缘约 100s 就返回 524，大请求不该排给它。
	MaxBodyBytes int64 `yaml:"max_body_bytes,omitempty" json:"max_body_bytes,omitempty"`
	// FailureStatuses 列出的状态码会被计为“线路故障”。
	// 只在能确定该状态码由这条线路产生时使用（例如 CF 的 524）。
	// 默认不列任何码：5xx 多半是 AxonHub 或更深层的问题，不该算到线路上。
	FailureStatuses []int `yaml:"failure_statuses,omitempty" json:"failure_statuses,omitempty"`
}

func (u UpstreamConfig) IsEnabled() bool { return u.Enabled == nil || *u.Enabled }

// Base 返回解析后的上游根地址（scheme://host:port，无尾斜杠、无 path）。
func (u UpstreamConfig) Base() (*url.URL, error) {
	raw := strings.TrimSpace(u.URL)
	if raw == "" {
		return nil, fmt.Errorf("upstream %q: url 为空", u.Name)
	}
	pu, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("upstream %q: url 解析失败: %w", u.Name, err)
	}
	if pu.Scheme != "http" && pu.Scheme != "https" {
		return nil, fmt.Errorf("upstream %q: url 只支持 http/https，当前 %q", u.Name, pu.Scheme)
	}
	if pu.Hostname() == "" {
		return nil, fmt.Errorf("upstream %q: url 缺少主机名", u.Name)
	}
	return pu, nil
}

func Default() *Config {
	return &Config{
		Version: 1,
		Admin: AdminConfig{
			Listen: "127.0.0.1:8766",
		},
		Log: LogConfig{
			Level:    "info",
			Format:   "text",
			RingSize: 500,
		},
		Buffer: BufferConfig{
			MaxRequestBytes: 128 << 20,
			SpillThreshold:  8 << 20,
			MaxConcurrent:   64,
			ReadIdleTimeout: Duration{120 * time.Second},
		},
		HTTP: HTTPConfig{
			ConnectTimeout:          Duration{2 * time.Second},
			TLSHandshakeTimeout:     Duration{5 * time.Second},
			ResponseHeaderTimeout:   Duration{600 * time.Second},
			StreamIdleTimeout:       Duration{600 * time.Second},
			UploadIdleTimeout:       Duration{60 * time.Second},
			IdleConnTimeout:         Duration{90 * time.Second},
			ReconnectAfterIdle:      Duration{60 * time.Second},
			KeepAlive:               Duration{30 * time.Second},
			MaxIdleConnsPerHost:     16,
			DisableCompression:      true,
			ResponseHeaderWarnAfter: Duration{120 * time.Second},
		},
		Retry: RetryConfig{
			MaxAttempts: 3,
			Mode:        "connect",
			Backoff:     Duration{150 * time.Millisecond},
		},
		Health: HealthConfig{
			Enabled:          true,
			Interval:         Duration{15 * time.Second},
			Timeout:          Duration{3 * time.Second},
			FailThreshold:    2,
			RecoverThreshold: 2,
			Cooldown:         Duration{20 * time.Second},
			ProbePath:        "/health",
			ProbeMethod:      "GET",
		},
	}
}

// DefaultPath 返回默认配置文件路径。
func DefaultPath() string {
	if p := strings.TrimSpace(os.Getenv("LINKGATE_CONFIG")); p != "" {
		return p
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "linkgate.yaml"
	}
	return filepath.Join(home, ".linkgate", "config.yaml")
}

// ExpandPath 展开 ~ 前缀。
func ExpandPath(p string) string {
	p = strings.TrimSpace(p)
	if p == "" {
		return p
	}
	if p == "~" {
		home, _ := os.UserHomeDir()
		return home
	}
	if strings.HasPrefix(p, "~/") {
		home, _ := os.UserHomeDir()
		return filepath.Join(home, p[2:])
	}
	return p
}

// Load 读取配置文件并应用默认值、校验。
func Load(path string) (*Config, error) {
	path = ExpandPath(path)
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("读取配置文件 %s 失败: %w", path, err)
	}
	cfg := Default()
	if err := yaml.Unmarshal(b, cfg); err != nil {
		return nil, fmt.Errorf("解析配置文件 %s 失败: %w", path, err)
	}
	cfg.applyDefaults()
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

// Parse 从字节解析配置（面板保存走这条路径）。
func Parse(b []byte) (*Config, error) {
	cfg := Default()
	if err := yaml.Unmarshal(b, cfg); err != nil {
		return nil, fmt.Errorf("YAML 解析失败: %w", err)
	}
	cfg.applyDefaults()
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

func (c *Config) Marshal() ([]byte, error) { return yaml.Marshal(c) }

func (c *Config) applyDefaults() {
	if c.Version == 0 {
		c.Version = 1
	}
	d := Default()
	if c.DataDir == "" {
		if home, err := os.UserHomeDir(); err == nil {
			c.DataDir = filepath.Join(home, ".linkgate")
		} else {
			c.DataDir = ".linkgate"
		}
	}
	c.DataDir = ExpandPath(c.DataDir)

	if c.Admin.Listen == "" {
		c.Admin.Listen = d.Admin.Listen
	}
	if c.Log.Level == "" {
		c.Log.Level = d.Log.Level
	}
	if c.Log.Format == "" {
		c.Log.Format = d.Log.Format
	}
	if c.Log.RingSize <= 0 {
		c.Log.RingSize = d.Log.RingSize
	}
	if c.Buffer.MaxRequestBytes <= 0 {
		c.Buffer.MaxRequestBytes = d.Buffer.MaxRequestBytes
	}
	if c.Buffer.SpillThreshold <= 0 {
		c.Buffer.SpillThreshold = d.Buffer.SpillThreshold
	}
	if c.Buffer.MaxConcurrent <= 0 {
		c.Buffer.MaxConcurrent = d.Buffer.MaxConcurrent
	}
	if c.Buffer.ReadIdleTimeout.Duration == 0 {
		c.Buffer.ReadIdleTimeout = d.Buffer.ReadIdleTimeout
	}
	if c.HTTP.ConnectTimeout.Duration == 0 {
		c.HTTP.ConnectTimeout = d.HTTP.ConnectTimeout
	}
	if c.HTTP.TLSHandshakeTimeout.Duration == 0 {
		c.HTTP.TLSHandshakeTimeout = d.HTTP.TLSHandshakeTimeout
	}
	if c.HTTP.ResponseHeaderTimeout.Duration == 0 {
		c.HTTP.ResponseHeaderTimeout = d.HTTP.ResponseHeaderTimeout
	}
	if c.HTTP.StreamIdleTimeout.Duration == 0 {
		c.HTTP.StreamIdleTimeout = d.HTTP.StreamIdleTimeout
	}
	if c.HTTP.UploadIdleTimeout.Duration == 0 {
		c.HTTP.UploadIdleTimeout = d.HTTP.UploadIdleTimeout
	}
	if c.HTTP.IdleConnTimeout.Duration == 0 {
		c.HTTP.IdleConnTimeout = d.HTTP.IdleConnTimeout
	}
	if c.HTTP.ReconnectAfterIdle.Duration == 0 {
		c.HTTP.ReconnectAfterIdle = d.HTTP.ReconnectAfterIdle
	}
	if c.HTTP.KeepAlive.Duration == 0 {
		c.HTTP.KeepAlive = d.HTTP.KeepAlive
	}
	if c.HTTP.MaxIdleConnsPerHost <= 0 {
		c.HTTP.MaxIdleConnsPerHost = d.HTTP.MaxIdleConnsPerHost
	}
	if c.HTTP.ResponseHeaderWarnAfter.Duration == 0 {
		c.HTTP.ResponseHeaderWarnAfter = d.HTTP.ResponseHeaderWarnAfter
	}
	if c.Retry.MaxAttempts <= 0 {
		c.Retry.MaxAttempts = d.Retry.MaxAttempts
	}
	if c.Retry.Mode == "" {
		c.Retry.Mode = d.Retry.Mode
	}
	if c.Health.Interval.Duration == 0 {
		c.Health.Interval = d.Health.Interval
	}
	if c.Health.Timeout.Duration == 0 {
		c.Health.Timeout = d.Health.Timeout
	}
	if c.Health.FailThreshold <= 0 {
		c.Health.FailThreshold = d.Health.FailThreshold
	}
	if c.Health.RecoverThreshold <= 0 {
		c.Health.RecoverThreshold = d.Health.RecoverThreshold
	}
	if c.Health.Cooldown.Duration == 0 {
		c.Health.Cooldown = d.Health.Cooldown
	}
	if c.Health.ProbePath == "" {
		c.Health.ProbePath = d.Health.ProbePath
	}
	if c.Health.ProbeMethod == "" {
		c.Health.ProbeMethod = "GET"
	}
	for gi := range c.Groups {
		g := &c.Groups[gi]
		if g.Listen == "" && gi == 0 {
			g.Listen = "127.0.0.1:8765"
		}
		for ui := range g.Upstreams {
			u := &g.Upstreams[ui]
			if u.Name == "" {
				u.Name = fmt.Sprintf("upstream-%d", ui+1)
			}
			// 未显式给优先级时按书写顺序，越靠前越优先。
			if u.Priority == 0 {
				u.Priority = (ui + 1) * 10
			}
		}
	}
}

// Validate 校验配置，错误信息面向人类可读。
func (c *Config) Validate() error {
	var errs []string
	if c.Version != 1 {
		errs = append(errs, fmt.Sprintf("version 只支持 1，当前 %d", c.Version))
	}
	if err := validListen(c.Admin.Listen); err != nil && !c.Admin.Disabled {
		errs = append(errs, "admin.listen: "+err.Error())
	}
	switch c.Log.Level {
	case "debug", "info", "warn", "error":
	default:
		errs = append(errs, `log.level 只支持 debug/info/warn/error`)
	}
	switch c.Log.Format {
	case "text", "json":
	default:
		errs = append(errs, `log.format 只支持 text/json`)
	}
	switch c.Retry.Mode {
	case "connect", "none":
	default:
		errs = append(errs, `retry.mode 只支持 connect/none（connect = 仅在连接未建立时换线路）`)
	}
	if c.Retry.MaxAttempts < 1 {
		errs = append(errs, "retry.max_attempts 至少为 1")
	}
	if c.Buffer.SpillThreshold > c.Buffer.MaxRequestBytes {
		errs = append(errs, "buffer.spill_threshold 不能大于 buffer.max_request_bytes")
	}
	if len(c.Groups) == 0 {
		errs = append(errs, "至少需要一个 groups 条目")
	}

	listens := map[string]string{}
	if !c.Admin.Disabled {
		listens[c.Admin.Listen] = "admin"
	}
	groupNames := map[string]bool{}
	for gi, g := range c.Groups {
		prefix := fmt.Sprintf("groups[%d]", gi)
		if strings.TrimSpace(g.Name) == "" {
			errs = append(errs, prefix+".name 不能为空")
		} else if groupNames[g.Name] {
			errs = append(errs, fmt.Sprintf("group 名称重复: %q", g.Name))
		} else {
			groupNames[g.Name] = true
			prefix = fmt.Sprintf("group %q", g.Name)
		}
		if err := validListen(g.Listen); err != nil {
			errs = append(errs, prefix+".listen: "+err.Error())
		} else if owner, dup := listens[g.Listen]; dup {
			errs = append(errs, fmt.Sprintf("%s.listen 与 %s 冲突: %s", prefix, owner, g.Listen))
		} else {
			listens[g.Listen] = prefix
		}
		if len(g.Upstreams) == 0 {
			errs = append(errs, prefix+" 至少需要一个 upstream")
		}
		upNames := map[string]bool{}
		for ui, u := range g.Upstreams {
			up := fmt.Sprintf("%s.upstreams[%d]", prefix, ui)
			if strings.TrimSpace(u.Name) == "" {
				errs = append(errs, up+".name 不能为空")
			} else if upNames[u.Name] {
				errs = append(errs, fmt.Sprintf("%s 名称重复: %q", prefix, u.Name))
			} else {
				upNames[u.Name] = true
				up = fmt.Sprintf("%s/%s", prefix, u.Name)
			}
			if _, err := u.Base(); err != nil {
				errs = append(errs, up+": "+err.Error())
			}
			for k := range u.Headers {
				if strings.ContainsAny(k, " \t\r\n:") {
					errs = append(errs, up+": 请求头名非法: "+k)
				}
			}
			if u.MaxBodyBytes < 0 {
				errs = append(errs, up+": max_body_bytes 不能为负")
			}
			for _, code := range u.FailureStatuses {
				if code < 100 || code > 599 {
					errs = append(errs, fmt.Sprintf("%s: failure_statuses 含非法状态码 %d", up, code))
				}
			}
		}
	}
	if len(errs) > 0 {
		return errors.New("配置校验失败:\n  - " + strings.Join(errs, "\n  - "))
	}
	return nil
}

func validListen(addr string) error {
	if strings.TrimSpace(addr) == "" {
		return errors.New("不能为空，例如 127.0.0.1:8765")
	}
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("格式应为 host:port: %w", err)
	}
	if port == "" {
		return errors.New("缺少端口")
	}
	if host == "" {
		// 允许 :8765，但会监听所有网卡，给出提示由调用方决定
		return nil
	}
	return nil
}

// SortedGroups 返回按名字排序的组名，供面板稳定显示。
func (c *Config) SortedGroups() []string {
	names := make([]string, 0, len(c.Groups))
	for _, g := range c.Groups {
		names = append(names, g.Name)
	}
	sort.Strings(names)
	return names
}

// Group 按名字查找组。
func (c *Config) Group(name string) *GroupConfig {
	for i := range c.Groups {
		if c.Groups[i].Name == name {
			return &c.Groups[i]
		}
	}
	return nil
}

// EffectiveHTTP 合并组级覆盖。
func (c *Config) EffectiveHTTP(g *GroupConfig) HTTPConfig {
	out := c.HTTP
	o := g.TimeoutOverrides
	if o.ConnectTimeout.Duration > 0 {
		out.ConnectTimeout = o.ConnectTimeout
	}
	if o.TLSHandshakeTimeout.Duration > 0 {
		out.TLSHandshakeTimeout = o.TLSHandshakeTimeout
	}
	if o.ResponseHeaderTimeout.Duration > 0 {
		out.ResponseHeaderTimeout = o.ResponseHeaderTimeout
	}
	if o.StreamIdleTimeout.Duration > 0 {
		out.StreamIdleTimeout = o.StreamIdleTimeout
	}
	if o.UploadIdleTimeout.Duration > 0 {
		out.UploadIdleTimeout = o.UploadIdleTimeout
	}
	if o.ReconnectAfterIdle.Duration > 0 {
		out.ReconnectAfterIdle = o.ReconnectAfterIdle
	}
	if o.ResponseHeaderWarnAfter.Duration > 0 {
		out.ResponseHeaderWarnAfter = o.ResponseHeaderWarnAfter
	}
	return out
}
