package app

import (
	"bytes"
	"encoding/json"
	"net/http"
	"reflect"
	"sort"
)

// This schema is deliberately a writable subset, not Config. Startup resources
// cannot sneak into PUT; nested keys reuse the CRUD byte-exact schema decoder.
type Settings struct {
	Log     SettingsLog    `json:"log"`
	DataDir string         `json:"data_dir"`
	Admin   SettingsAdmin  `json:"admin"`
	Health  HealthConfig   `json:"health"`
	Retry   RetryConfig    `json:"retry"`
	Buffer  SettingsBuffer `json:"buffer"`
	HTTP    SettingsHTTP   `json:"http"`
}
type SettingsAdmin struct {
	Listen    string `json:"listen"`
	TokenFile string `json:"token_file"`
}
type SettingsLog struct {
	File         string `json:"file"`
	Format       string `json:"format"`
	MaxSizeBytes int64  `json:"max_size_bytes"`
	MaxBackups   int    `json:"max_backups"`
	RingSize     int    `json:"ring_size"`
	Level        string `json:"level"`
}
type SettingsBuffer struct {
	MaxConcurrent int `json:"max_concurrent"`
}
type SettingsHTTP struct {
	ConnectTimeout          Duration `json:"connect_timeout"`
	TLSHandshakeTimeout     Duration `json:"tls_handshake_timeout"`
	ResponseHeaderTimeout   Duration `json:"response_header_timeout"`
	StreamIdleTimeout       Duration `json:"stream_idle_timeout"`
	UploadIdleTimeout       Duration `json:"upload_idle_timeout"`
	IdleConnTimeout         Duration `json:"idle_conn_timeout"`
	ReconnectAfterIdle      Duration `json:"reconnect_after_idle"`
	KeepAlive               Duration `json:"keep_alive"`
	ResponseHeaderWarnAfter Duration `json:"response_header_warn_after"`
}

var settingsSchema = buildSchema(reflect.TypeOf(Settings{}))
var uiSchema = buildSchema(reflect.TypeOf(UIConfig{}))

func settingsOf(cfg *Config) Settings {
	h := cfg.HTTP
	return Settings{Log: SettingsLog{RingSize: cfg.Log.RingSize, Level: cfg.Log.Level, File: cfg.Log.File, Format: cfg.Log.Format, MaxSizeBytes: cfg.Log.MaxSizeBytes, MaxBackups: cfg.Log.MaxBackups}, DataDir: cfg.DataDir, Admin: SettingsAdmin{cfg.Admin.Listen, cfg.Admin.TokenFile}, Health: cfg.Health, Retry: cfg.Retry, Buffer: SettingsBuffer{cfg.Buffer.MaxConcurrent}, HTTP: SettingsHTTP{h.ConnectTimeout, h.TLSHandshakeTimeout, h.ResponseHeaderTimeout, h.StreamIdleTimeout, h.UploadIdleTimeout, h.IdleConnTimeout, h.ReconnectAfterIdle, h.KeepAlive, h.ResponseHeaderWarnAfter}}
}

// Settings objects patch recursively; arrays/scalars replace. Null is rejected
// rather than silently clearing a required scalar or bypassing numeric validation.
func rejectNull(fields map[string]json.RawMessage) error {
	for _, raw := range fields {
		if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
			return &APIError{400, "设置不接受 null；请填写明确的值"}
		}
		if len(bytes.TrimSpace(raw)) > 0 && bytes.TrimSpace(raw)[0] == '{' {
			obj, err := decodeObjectStrict(raw)
			if err != nil {
				return err
			}
			if err := rejectNull(obj); err != nil {
				return err
			}
		}
	}
	return nil
}
func mergePatch(old any, fields map[string]json.RawMessage, dst any) error {
	b, err := json.Marshal(old)
	if err != nil {
		return err
	}
	var all map[string]json.RawMessage
	if err := json.Unmarshal(b, &all); err != nil {
		return err
	}
	for k, v := range fields {
		if trimmed := bytes.TrimSpace(v); len(trimmed) > 0 && trimmed[0] == '{' {
			var prior map[string]json.RawMessage
			if err := json.Unmarshal(all[k], &prior); err != nil {
				return err
			}
			patch, err := decodeObjectStrict(v)
			if err != nil {
				return err
			}
			var merged map[string]json.RawMessage
			if err := mergePatch(prior, patch, &merged); err != nil {
				return err
			}
			v, err = json.Marshal(merged)
			if err != nil {
				return err
			}
		}
		all[k] = v
	}
	b, err = json.Marshal(all)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, dst)
}

type SettingField struct {
	Path   string `json:"path"`
	Effect string `json:"effect"`
}

func settingFields(node *schemaNode, prefix string) []SettingField {
	var out []SettingField
	for k, child := range node.fields {
		path := joinFieldPath(prefix, k)
		if child != nil && len(child.fields) > 0 {
			out = append(out, settingFields(child, path)...)
		} else {
			effect := "hot"
			if path == "health.interval" {
				effect = "next_cycle"
			}
			switch path {
			case "log.file", "log.format", "log.max_size_bytes", "log.max_backups", "data_dir", "admin.listen", "admin.token_file":
				effect = "restart"
			}
			out = append(out, SettingField{path, effect})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}

func (a *AdminServer) handleSettings(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		a.gate.RLock()
		defer a.gate.RUnlock()
		a.mu.RLock()
		cfg := a.cfg
		saved := a.saved
		pending := append([]string{}, a.pendingRestart...)
		a.mu.RUnlock()
		restart := map[string]any{"log.file": cfg.Log.File, "log.format": cfg.Log.Format, "log.max_size_bytes": cfg.Log.MaxSizeBytes, "log.max_backups": cfg.Log.MaxBackups, "data_dir": cfg.DataDir, "admin.listen": cfg.Admin.Listen, "admin.token_file": cfg.Admin.TokenFile}
		fields := settingFields(settingsSchema, "")
		writeJSON(w, 200, map[string]any{"settings": settingsOf(saved), "active_settings": settingsOf(cfg), "fields": fields, "restart_required": restart, "pending_restart": pending})
	case http.MethodPut:
		var input Settings
		fields, err := decodeFields(r, settingsSchema, &input)
		if err == nil {
			err = rejectNull(fields)
		}
		if err == nil {
			err = a.manager.MutateSettings(func(cfg *Config) error {
				var next Settings
				if err := mergePatch(settingsOf(cfg), fields, &next); err != nil {
					return err
				}
				if next.Log.RingSize < 1 || next.Log.RingSize > 100000 || next.Buffer.MaxConcurrent < 1 || next.Retry.MaxAttempts < 1 || next.Health.FailThreshold < 1 || next.Health.RecoverThreshold < 1 {
					return &APIError{400, "容量、并发数、尝试次数与健康阈值必须大于 0；ring_size 上限 100000"}
				}
				cfg.Log.File, cfg.Log.Format, cfg.Log.MaxSizeBytes, cfg.Log.MaxBackups = next.Log.File, next.Log.Format, next.Log.MaxSizeBytes, next.Log.MaxBackups
				cfg.DataDir = next.DataDir
				cfg.Admin.Listen, cfg.Admin.TokenFile = next.Admin.Listen, next.Admin.TokenFile
				cfg.Log.RingSize, cfg.Log.Level, cfg.Buffer.MaxConcurrent = next.Log.RingSize, next.Log.Level, next.Buffer.MaxConcurrent
				cfg.Health, cfg.Retry = next.Health, next.Retry
				h := next.HTTP
				cfg.HTTP.ConnectTimeout, cfg.HTTP.TLSHandshakeTimeout, cfg.HTTP.ResponseHeaderTimeout = h.ConnectTimeout, h.TLSHandshakeTimeout, h.ResponseHeaderTimeout
				cfg.HTTP.StreamIdleTimeout, cfg.HTTP.UploadIdleTimeout, cfg.HTTP.IdleConnTimeout = h.StreamIdleTimeout, h.UploadIdleTimeout, h.IdleConnTimeout
				cfg.HTTP.ReconnectAfterIdle, cfg.HTTP.KeepAlive, cfg.HTTP.ResponseHeaderWarnAfter = h.ReconnectAfterIdle, h.KeepAlive, h.ResponseHeaderWarnAfter
				return nil
			})
		}
		if err != nil {
			a.mutationResult(w, r.Method, err)
			return
		}
		a.mu.RLock()
		pending := append([]string{}, a.pendingRestart...)
		a.mu.RUnlock()
		message := "已保存并热生效"
		if len(pending) > 0 {
			message = "已保存；启动资源将在重启后生效"
		}
		writeJSON(w, 200, map[string]any{"ok": true, "pending_restart": pending, "message": message})
	default:
		writeJSON(w, 405, map[string]any{"error": "只支持 GET/PUT"})
	}
}

func (a *AdminServer) handleStats(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, 405, map[string]any{"error": "只支持 GET"})
		return
	}
	a.gate.RLock()
	defer a.gate.RUnlock()
	a.mu.RLock()
	cfg := a.cfg
	a.mu.RUnlock()
	groups := a.store.Snapshot(cfg)
	sortServiceStates(groups, cfg.UI)
	lines := 0
	for _, g := range groups {
		lines += len(g.Upstreams)
	}
	retained, total := a.events.Counts()
	writeJSON(w, 200, map[string]any{"service_count": len(groups), "line_count": lines, "event_count": retained, "event_total": total, "groups": a.store.ServiceStats(groups), "ui": cfg.UI, "counter_scope": "process_lifetime", "event_rings": a.events.RingStats(), "storage": map[string]any{"spill_threshold": cfg.Buffer.SpillThreshold, "max_request_bytes": cfg.Buffer.MaxRequestBytes, "log_file_configured": cfg.Log.File != "", "global_max_concurrent": cfg.Buffer.GlobalMaxConcurrent}})
}

func (a *AdminServer) handleUIOrder(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPut {
		writeJSON(w, 405, map[string]any{"error": "只支持 PUT"})
		return
	}
	var input UIConfig
	fields, err := decodeFields(r, uiSchema, &input)
	if err == nil {
		err = rejectNull(fields)
	}
	if err == nil {
		err = a.manager.Mutate(func(cfg *Config) error {
			if _, ok := fields["service_order"]; ok {
				if len(input.ServiceOrder) != len(cfg.Groups) {
					return &APIError{400, "service_order 必须包含全部现有服务，且每个名称只出现一次"}
				}
			}
			if err := mergeFields(cfg.UI, fields, &cfg.UI); err != nil {
				return err
			}
			if _, ok := fields["service_order"]; ok {
				seen := make(map[string]bool)
				ordered := make([]GroupConfig, 0, len(cfg.Groups))
				for _, name := range cfg.UI.ServiceOrder {
					g := cfg.Group(name)
					if g == nil || seen[name] {
						return &APIError{400, "service_order 含未知或重复服务"}
					}
					seen[name] = true
					ordered = append(ordered, *g)
				}
				cfg.Groups = ordered
			}
			return nil
		})
	}
	a.mutationResult(w, r.Method, err)
}

type upstreamOrder struct {
	Order []string `json:"order"`
}

var orderSchema = buildSchema(reflect.TypeOf(upstreamOrder{}))

func (a *AdminServer) handleUpstreamOrder(w http.ResponseWriter, r *http.Request, group string) {
	if r.Method != http.MethodPut {
		writeJSON(w, 405, map[string]any{"error": "只支持 PUT"})
		return
	}
	var input upstreamOrder
	fields, err := decodeFields(r, orderSchema, &input)
	if err == nil {
		err = rejectNull(fields)
	}
	if err == nil {
		err = a.manager.Mutate(func(cfg *Config) error {
			g := cfg.Group(group)
			if g == nil {
				return &APIError{404, "未知服务"}
			}
			if _, ok := fields["order"]; !ok || len(input.Order) != len(g.Upstreams) {
				return &APIError{400, "order 必须包含全部线路"}
			}
			seen := make(map[string]bool)
			ordered := make([]UpstreamConfig, 0, len(input.Order))
			for i, name := range input.Order {
				u := findUpstream(g, name)
				if u == nil || seen[name] {
					return &APIError{400, "order 含未知或重复线路"}
				}
				seen[name] = true
				copy := *u
				priority := (i + 1) * 10
				copy.Priority = &priority
				ordered = append(ordered, copy)
			}
			g.Upstreams = ordered
			return nil
		})
	}
	a.mutationResult(w, r.Method, err)
}
