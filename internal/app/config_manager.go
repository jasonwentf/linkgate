package app

import (
	"bytes"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"sync"
)

// ConfigManager is the single serialized writer for YAML, CRUD and reloads.
// Only preparation and disk I/O can fail. Publication under gate is infallible.
type ConfigManager struct {
	mu        sync.Mutex
	gate      *sync.RWMutex
	closed    bool
	path      string
	raw       []byte
	cfg       *Config // live generation
	saved     *Config // last accepted disk configuration, including pending restart
	engine    *Engine
	store     *Store
	monitor   *HealthMonitor
	admin     *AdminServer
	listeners *ListenerManager
	events    *EventLog
}

// errManagerClosed 表示配置管理器已经进入关闭流程，拒绝新的写事务。
func errManagerClosed() error {
	return &APIError{503, "服务正在关闭，配置写操作被拒绝"}
}

func NewConfigManager(path string, cfg *Config, engine *Engine, store *Store, monitor *HealthMonitor, events *EventLog, log *slog.Logger) (*ConfigManager, error) {
	path, err := filepath.Abs(ExpandPath(path))
	if err != nil {
		return nil, err
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	m := &ConfigManager{path: ExpandPath(path), raw: raw, cfg: cfg, saved: cfg, engine: engine, store: store, monitor: monitor,
		events: events, gate: engine.gate, listeners: NewListenerManager(log)}
	return m, nil
}

func (m *ConfigManager) SetAdmin(a *AdminServer) {
	m.admin = a
	a.manager = m
	a.gate = m.gate
	m.monitor.gate = m.gate
}
func (m *ConfigManager) Start() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return errManagerClosed()
	}
	plan, err := m.listeners.Prepare(m.specs(m.cfg, m.engine.snapshotGroups()))
	if err != nil {
		return err
	}
	plan.Commit()
	return nil
}

// Close 与所有写事务共用 m.mu：先置 closed 再清监听，保证不会出现
// “Prepare 已拿到复用监听、Close 却先清空 active、Commit 再拿到 nil 监听”的竞态；
// closed 之后到达的写事务直接拒绝，避免排队请求把监听重新打开。
func (m *ConfigManager) Close() {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return
	}
	m.closed = true
	m.mu.Unlock()
	m.listeners.Close()
}

// specs 为某一代配置装配监听器。组处理器直接绑定到该代已建好的 groupRuntime，
// 不在请求时按组名回查 engine，避免换组名/互换监听地址时旧 dispatch 读到新映射。
func (m *ConfigManager) specs(cfg *Config, groups map[string]*groupRuntime) []ListenerSpec {
	out := make([]ListenerSpec, 0, len(cfg.Groups)+1)
	for _, g := range cfg.Groups {
		out = append(out, ListenerSpec{g.Name, g.Listen, NewHTTPListener(m.engine.handlerFor(groups[g.Name]), false, m.gate)})
	}
	if !cfg.Admin.Disabled {
		out = append(out, ListenerSpec{"admin", cfg.Admin.Listen, NewHTTPListener(m.admin.Handler(), true, m.gate)})
	}
	return out
}

func (m *ConfigManager) Save(raw []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return errManagerClosed()
	}
	next, err := Parse(raw)
	if err != nil {
		return &APIError{validationStatus(err), err.Error()}
	}
	return m.commit(raw, next, true)
}

// Mutate clones the last accepted config while holding the writer lock, preventing lost updates.
func (m *ConfigManager) Mutate(change func(*Config) error) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return errManagerClosed()
	}
	raw, err := m.saved.Marshal()
	if err != nil {
		return err
	}
	next, err := Parse(raw)
	if err != nil {
		return err
	}
	if err := change(next); err != nil {
		return err
	}
	// Clone saved values so unrelated CRUD preserves pending resources, while
	// explicit changes can update/cancel one field without applying the others.
	pending := restartFields(m.cfg, m.saved)
	next.applyDefaults()
	if err := next.Validate(); err != nil {
		return &APIError{validationStatus(err), err.Error()}
	}
	raw, err = next.Marshal()
	if err != nil {
		return err
	}
	if len(pending) > 0 {
		return m.commitPending(raw, next, true, restartFields(m.cfg, next))
	}
	return m.commit(raw, next, true)
}

// MutateSettings explicitly allows staging startup-only fields. Live resources
// remain unchanged until a process restart; all hot fields still commit atomically.
func (m *ConfigManager) MutateSettings(change func(*Config) error) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return errManagerClosed()
	}
	raw, err := m.saved.Marshal()
	if err != nil {
		return err
	}
	next, err := Parse(raw)
	if err != nil {
		return err
	}
	if err = change(next); err != nil {
		return err
	}
	next.applyDefaults()
	if err = next.Validate(); err != nil {
		return &APIError{validationStatus(err), err.Error()}
	}
	raw, err = next.Marshal()
	if err != nil {
		return err
	}
	return m.commitStaged(raw, next, true)
}

func carryRestart(dst, src *Config) {
	dst.DataDir = src.DataDir
	dst.Admin.Listen, dst.Admin.TokenFile = src.Admin.Listen, src.Admin.TokenFile
	dst.Log.File, dst.Log.Format, dst.Log.MaxSizeBytes, dst.Log.MaxBackups = src.Log.File, src.Log.Format, src.Log.MaxSizeBytes, src.Log.MaxBackups
}
func carryPending(dst, src *Config, fields []string) {
	for _, field := range fields {
		switch field {
		case "data_dir":
			dst.DataDir = src.DataDir
		case "admin.listen":
			dst.Admin.Listen = src.Admin.Listen
		case "admin.token_file":
			dst.Admin.TokenFile = src.Admin.TokenFile
		case "log.file":
			dst.Log.File = src.Log.File
		case "log.format":
			dst.Log.Format = src.Log.Format
		case "log.max_size_bytes":
			dst.Log.MaxSizeBytes = src.Log.MaxSizeBytes
		case "log.max_backups":
			dst.Log.MaxBackups = src.Log.MaxBackups
		}
	}
}
func (m *ConfigManager) commitPending(raw []byte, saved *Config, save bool, pending []string) error {
	liveRaw, err := saved.Marshal()
	if err != nil {
		return err
	}
	live, err := Parse(liveRaw)
	if err != nil {
		return err
	}
	carryPending(live, m.cfg, pending)
	return m.publish(raw, live, saved, save)
}
func restartFields(live, saved *Config) []string {
	fields := make([]string, 0)
	for _, v := range []struct {
		name string
		a, b any
	}{
		{"data_dir", live.DataDir, saved.DataDir}, {"admin.listen", live.Admin.Listen, saved.Admin.Listen}, {"admin.token_file", live.Admin.TokenFile, saved.Admin.TokenFile}, {"log.file", live.Log.File, saved.Log.File}, {"log.format", live.Log.Format, saved.Log.Format}, {"log.max_size_bytes", live.Log.MaxSizeBytes, saved.Log.MaxSizeBytes}, {"log.max_backups", live.Log.MaxBackups, saved.Log.MaxBackups},
	} {
		if !reflect.DeepEqual(v.a, v.b) {
			fields = append(fields, v.name)
		}
	}
	return fields
}
func (m *ConfigManager) PendingRestart() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return restartFields(m.cfg, m.saved)
}
func (m *ConfigManager) SavedConfig() *Config { m.mu.Lock(); defer m.mu.Unlock(); return m.saved }
func (m *ConfigManager) commitStaged(raw []byte, saved *Config, save bool) error {
	if err := saved.Validate(); err != nil {
		return &APIError{validationStatus(err), err.Error()}
	}
	liveRaw, err := saved.Marshal()
	if err != nil {
		return err
	}
	live, err := Parse(liveRaw)
	if err != nil {
		return err
	}
	carryRestart(live, m.cfg)
	return m.publish(raw, live, saved, save)
}

// Reload accepts an externally edited file, or restores the last accepted bytes on failure.
// No .bak is created on successful reload (the legacy contract).
func (m *ConfigManager) Reload() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return errManagerClosed()
	}
	raw, err := os.ReadFile(m.path)
	if err != nil {
		return err
	}
	next, err := Parse(raw)
	if err != nil {
		err = &APIError{validationStatus(err), err.Error()}
	}
	if err == nil {
		err = m.commit(raw, next, false)
	}
	if err == nil {
		return nil
	}
	if !bytes.Equal(raw, m.raw) {
		if restoreErr := replaceFile(m.path, m.raw); restoreErr != nil {
			return fmt.Errorf("重载失败: %v；恢复磁盘配置失败（运行态未变）: %w", err, restoreErr)
		}
	}
	return err
}

func (m *ConfigManager) commit(raw []byte, next *Config, save bool) error {
	if err := next.Validate(); err != nil {
		return &APIError{validationStatus(err), err.Error()}
	}
	// Once startup resources are staged, compare them field-by-field to live
	// values. YAML edits may update/cancel individual pending fields, but none
	// of these resources may take the legacy hot-apply path as a side effect.
	if len(restartFields(m.cfg, m.saved)) > 0 {
		return m.commitPending(raw, next, save, restartFields(m.cfg, next))
	}
	// These resources are initialized before serving; do not silently accept ineffective edits.
	oldLog, nextLog := m.cfg.Log, next.Log
	oldLog.RingSize, nextLog.RingSize = 0, 0
	oldLog.Level, nextLog.Level = "", ""
	if !reflect.DeepEqual(oldLog, nextLog) || next.DataDir != m.cfg.DataDir || next.Admin.TokenFile != m.cfg.Admin.TokenFile {
		return &APIError{409, "log 文件/格式/轮转参数、data_dir、admin.token_file 的变更需要重启；本次未应用"}
	}
	return m.publish(raw, next, next, save)
}

func (m *ConfigManager) publish(raw []byte, next, saved *Config, save bool) error {
	if err := next.Validate(); err != nil {
		return &APIError{validationStatus(err), err.Error()}
	}
	groups, err := buildGroups(next)
	if err != nil {
		return err
	}
	committed := false
	defer func() {
		if !committed {
			closeGroupTransports(groups)
		}
	}()
	plan, err := m.listeners.Prepare(m.specs(next, groups))
	if err != nil {
		m.events.Add(Event{Level: "warn", Kind: "config", Message: "监听预检失败，配置未变: " + err.Error()})
		return err
	}
	defer plan.Abort()

	m.gate.Lock()
	defer m.gate.Unlock()
	if save {
		if err := saveConfigFile(m.path, raw, m.raw); err != nil {
			return fmt.Errorf("保存配置失败（运行态未变）: %w", err)
		}
	}
	m.store.Load(next, configThresholds(next), m.store)
	m.engine.commitGroups(next, groups)
	m.monitor.Update(next)
	m.admin.Update(next)
	m.admin.updateSaved(saved, restartFields(next, saved))
	m.events.Resize(next.Log.RingSize)
	setLoggerLevel(m.engine.log, next.Log.Level)
	setLoggerLevel(m.monitor.log, next.Log.Level)
	setLoggerLevel(m.admin.log, next.Log.Level)
	plan.Commit()
	m.cfg, m.saved, m.raw = next, saved, append([]byte(nil), raw...)
	committed = true
	m.events.Add(Event{Level: "info", Kind: "config", Message: "配置与监听已原子更新"})
	return nil
}

func configThresholds(c *Config) Thresholds {
	return Thresholds{FailThreshold: c.Health.FailThreshold, RecoverThreshold: c.Health.RecoverThreshold}
}

type APIError struct {
	Status  int
	Message string
}

func (e *APIError) Error() string { return e.Message }
func apiErrorStatus(err error) int {
	var api *APIError
	if errors.As(err, &api) {
		return api.Status
	}
	var conflict *ListenConflictError
	if errors.As(err, &conflict) {
		return 409
	}
	return 500
}

// Stage both files on the destination filesystem. A failed final rename restores the previous .bak.
func saveConfigFile(path string, raw, old []byte) error {
	staged, err := stageFile(path, raw)
	if err != nil {
		return err
	}
	defer os.Remove(staged)
	bak := path + ".bak"
	prev, prevErr := os.ReadFile(bak)
	if prevErr != nil && !errors.Is(prevErr, os.ErrNotExist) {
		return prevErr
	}
	if err := replaceFile(bak, old); err != nil {
		return err
	}
	if err := os.Rename(staged, path); err != nil {
		var restore error
		if prevErr == nil {
			restore = replaceFile(bak, prev)
		} else {
			restore = os.Remove(bak)
		}
		if restore != nil {
			return fmt.Errorf("%v；恢复 .bak 失败: %w", err, restore)
		}
		return err
	}
	return nil
}

func stageFile(path string, b []byte) (string, error) {
	f, err := os.CreateTemp(filepath.Dir(path), ".linkgate-config-*")
	if err != nil {
		return "", err
	}
	name := f.Name()
	if err = f.Chmod(0o600); err == nil {
		_, err = f.Write(b)
	}
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		_ = os.Remove(name)
		return "", err
	}
	return name, nil
}
func replaceFile(path string, b []byte) error {
	name, err := stageFile(path, b)
	if err != nil {
		return err
	}
	defer os.Remove(name)
	return os.Rename(name, path)
}
