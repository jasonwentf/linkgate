package app

import (
	"sync"
	"time"
)

// HealthStatus 是面板上展示的线路状态。
type HealthStatus string

const (
	StatusUnknown   HealthStatus = "unknown"
	StatusHealthy   HealthStatus = "healthy"
	StatusUnhealthy HealthStatus = "unhealthy"
	StatusDisabled  HealthStatus = "disabled"
)

// Thresholds 控制健康状态机。主动探针和真实请求共用同一套阈值。
type Thresholds struct {
	FailThreshold    int
	RecoverThreshold int
	Cooldown         time.Duration
}

// UpstreamState 是单条线路的实时状态与计数。字段均为只读快照语义。
type UpstreamState struct {
	Group    string       `json:"group"`
	Name     string       `json:"name"`
	URL      string       `json:"url"`
	Priority int          `json:"priority"`
	Status   HealthStatus `json:"status"`
	Enabled  bool         `json:"enabled"`
	Comment  string       `json:"comment,omitempty"`

	// Consecutive* 只统计真实请求；Probe* 只统计主动探针。
	// 两边分开累计，避免轻量 /health 成功把真实请求的失败计数冲掉。
	ConsecutiveFailures  int `json:"consecutive_failures"`
	ConsecutiveSuccesses int `json:"consecutive_successes"`
	ProbeFailures        int `json:"probe_failures"`
	ProbeSuccesses       int `json:"probe_successes"`

	DownSince time.Time `json:"down_since,omitempty"`
	UpSince   time.Time `json:"up_since,omitempty"`

	LastProbeAt      time.Time `json:"last_probe_at,omitempty"`
	LastProbeLatency int64     `json:"last_probe_latency_ms"` // 毫秒
	LastProbeError   string    `json:"last_probe_error,omitempty"`

	LastRequestAt      time.Time `json:"last_request_at,omitempty"`
	LastRequestLatency int64     `json:"last_request_latency_ms"`
	LastRequestError   string    `json:"last_request_error,omitempty"`

	RequestsOK       uint64 `json:"requests_ok"`
	RequestsFailed   uint64 `json:"requests_failed"`
	FailoversOut     uint64 `json:"failovers_out"`      // 从这条线路切走的次数
	SelectedTotal    uint64 `json:"selected_total"`     // 被选为本次请求主线路的次数
	SelectedAsBackup uint64 `json:"selected_as_backup"` // 高优先级不可用而启用本条线路的次数
}

func (s UpstreamState) RequestTotal() uint64 { return s.RequestsOK + s.RequestsFailed }

// GroupState 是一个线路组的快照。
type GroupState struct {
	Name      string          `json:"name"`
	Listen    string          `json:"listen"`
	Upstreams []UpstreamState `json:"upstreams"`
	// Active 是当前会优先选择的线路名。
	Active string `json:"active"`
}

type upstreamRuntime struct {
	UpstreamState
}

// Store 保存所有线路的运行时状态，线程安全。
type Store struct {
	mu       sync.RWMutex
	th       Thresholds
	order    map[string][]string // group -> upstream 名称，按优先级排序
	runtimes map[string]map[string]*upstreamRuntime
}

// NewStore 依据配置创建状态表。
func NewStore(cfg *Config, th Thresholds) *Store {
	s := &Store{}
	s.Load(cfg, th, nil)
	return s
}

// Load 重建状态表。reload 非 nil 时保留同名线路的累计计数。
//
// 注意：热重载时调用方通常传的就是 s 自己，所以必须先读旧表、再整体替换；
// 原地清空会让“保留累计计数”读到自己刚清出来的空表。
func (s *Store) Load(cfg *Config, th Thresholds, reload *Store) {
	s.mu.Lock()
	defer s.mu.Unlock()

	newOrder := make(map[string][]string, len(cfg.Groups))
	newRuntimes := make(map[string]map[string]*upstreamRuntime, len(cfg.Groups))

	for gi := range cfg.Groups {
		g := &cfg.Groups[gi]
		names := sortedUpstreamNames(g)
		newOrder[g.Name] = names
		m := make(map[string]*upstreamRuntime, len(g.Upstreams))
		for _, u := range g.Upstreams {
			st := UpstreamState{
				Group:    g.Name,
				Name:     u.Name,
				URL:      u.URL,
				Priority: u.Priority,
				Enabled:  u.IsEnabled(),
				Comment:  u.Comment,
				Status:   StatusUnknown,
			}
			if !u.IsEnabled() {
				st.Status = StatusDisabled
			}
			if reload != nil {
				if old := reload.lookup(g.Name, u.Name); old != nil {
					st.RequestsOK = old.RequestsOK
					st.RequestsFailed = old.RequestsFailed
					st.FailoversOut = old.FailoversOut
					st.SelectedTotal = old.SelectedTotal
					st.SelectedAsBackup = old.SelectedAsBackup
					st.LastProbeLatency = old.LastProbeLatency
					st.LastProbeAt = old.LastProbeAt
					st.LastRequestLatency = old.LastRequestLatency
					st.LastRequestAt = old.LastRequestAt
					if st.Enabled && old.Status != StatusDisabled {
						st.Status = old.Status
						st.DownSince = old.DownSince
						st.UpSince = old.UpSince
						st.ConsecutiveFailures = old.ConsecutiveFailures
						st.ConsecutiveSuccesses = old.ConsecutiveSuccesses
						st.LastProbeError = old.LastProbeError
					}
				}
			}
			m[u.Name] = &upstreamRuntime{UpstreamState: st}
		}
		newRuntimes[g.Name] = m
	}

	s.th = th
	s.order = newOrder
	s.runtimes = newRuntimes
}

func (s *Store) lookup(group, name string) *UpstreamState {
	m, ok := s.runtimes[group]
	if !ok {
		return nil
	}
	rt, ok := m[name]
	if !ok {
		return nil
	}
	cp := rt.UpstreamState
	return &cp
}

func sortedUpstreamNames(g *GroupConfig) []string {
	names := make([]string, 0, len(g.Upstreams))
	for _, u := range g.Upstreams {
		names = append(names, u.Name)
	}
	// 按优先级稳定排序（同优先级保持书写顺序）
	for i := 1; i < len(names); i++ {
		for j := i; j > 0; j-- {
			a := findUpstream(g, names[j-1])
			b := findUpstream(g, names[j])
			if a == nil || b == nil || a.Priority <= b.Priority {
				break
			}
			names[j-1], names[j] = names[j], names[j-1]
		}
	}
	return names
}

func findUpstream(g *GroupConfig, name string) *UpstreamConfig {
	for i := range g.Upstreams {
		if g.Upstreams[i].Name == name {
			return &g.Upstreams[i]
		}
	}
	return nil
}

// OrderedNames 返回某组按优先级排序的线路名。
func (s *Store) OrderedNames(group string) []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]string, len(s.order[group]))
	copy(out, s.order[group])
	return out
}

// IsUsable 报告这条线路当前是否可以参与选择。
func (s *Store) IsUsable(group, name string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	rt := s.get(group, name)
	if rt == nil || !rt.Enabled {
		return false
	}
	return rt.Status != StatusUnhealthy
}

// Status 返回单条线路状态。
func (s *Store) Status(group, name string) HealthStatus {
	s.mu.RLock()
	defer s.mu.RUnlock()
	rt := s.get(group, name)
	if rt == nil {
		return StatusUnknown
	}
	return rt.Status
}

// Enabled 返回这条线路当前是否参与选择（配置文件与运行时开关都算）。
func (s *Store) Enabled(group, name string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	rt := s.get(group, name)
	return rt != nil && rt.Enabled
}

func (s *Store) get(group, name string) *upstreamRuntime {
	m, ok := s.runtimes[group]
	if !ok {
		return nil
	}
	return m[name]
}

func (s *Store) RecordSelected(group, name string, backup bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if rt := s.get(group, name); rt != nil {
		rt.SelectedTotal++
		if backup {
			rt.SelectedAsBackup++
		}
		rt.LastRequestAt = time.Now()
	}
}

func (s *Store) RecordSuccess(group, name string, latency time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rt := s.get(group, name)
	if rt == nil || !rt.Enabled {
		return
	}
	rt.RequestsOK++
	rt.LastRequestAt = time.Now()
	rt.LastRequestLatency = latency.Milliseconds()
	rt.LastRequestError = ""
	rt.ConsecutiveFailures = 0
	rt.ConsecutiveSuccesses++
	rt.ProbeFailures = 0
	s.maybeUpLocked(rt, rt.ConsecutiveSuccesses)
}

func (s *Store) RecordFailure(group, name string, errMsg string, latency time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rt := s.get(group, name)
	if rt == nil || !rt.Enabled {
		return
	}
	rt.RequestsFailed++
	rt.LastRequestAt = time.Now()
	rt.LastRequestLatency = latency.Milliseconds()
	rt.LastRequestError = errMsg
	rt.ConsecutiveSuccesses = 0
	rt.ProbeSuccesses = 0
	rt.ConsecutiveFailures++
	if rt.ConsecutiveFailures >= s.th.FailThreshold {
		s.markDownLocked(rt, errMsg)
	}
}

// RecordHTTPError 记录“请求拿到了响应，但结果是失败的”（典型是上游 5xx）。
// 只增加失败计数，不影响健康状态：这类错误多半来自 AxonHub 或更深层，不该算到线路上。
func (s *Store) RecordHTTPError(group, name string, latency time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rt := s.get(group, name)
	if rt == nil || !rt.Enabled {
		return
	}
	rt.RequestsFailed++
	rt.LastRequestAt = time.Now()
	rt.LastRequestLatency = latency.Milliseconds()
}

// RecordFailover 记录一次“从该线路切到别处”。
func (s *Store) RecordFailover(group, name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if rt := s.get(group, name); rt != nil {
		rt.FailoversOut++
	}
}

// RecordProbe 记录一次主动探针结果。
func (s *Store) RecordProbe(group, name string, ok bool, latency time.Duration, errMsg string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rt := s.get(group, name)
	if rt == nil || !rt.Enabled {
		return
	}
	rt.LastProbeAt = time.Now()
	rt.LastProbeLatency = latency.Milliseconds()
	if ok {
		rt.LastProbeError = ""
		rt.ProbeFailures = 0
		rt.ProbeSuccesses++
		s.maybeUpLocked(rt, rt.ProbeSuccesses)
		return
	}
	rt.LastProbeError = errMsg
	rt.ProbeSuccesses = 0
	rt.ProbeFailures++
	if rt.ProbeFailures >= s.th.FailThreshold {
		s.markDownLocked(rt, errMsg)
	}
}

// maybeUpLocked 用某一个来源的连续成功数判断能否恢复。
// 从未探测过的线路一次成功就够；判死之后要走满恢复阈值。
func (s *Store) maybeUpLocked(rt *upstreamRuntime, successes int) {
	if rt.Status == StatusHealthy {
		return
	}
	need := s.th.RecoverThreshold
	if rt.Status == StatusUnknown {
		need = 1
	}
	if need < 1 {
		need = 1
	}
	if successes >= need {
		rt.Status = StatusHealthy
		rt.UpSince = time.Now()
		rt.DownSince = time.Time{}
	}
}

func (s *Store) markDownLocked(rt *upstreamRuntime, errMsg string) {
	if rt.Status != StatusUnhealthy {
		rt.Status = StatusUnhealthy
		rt.DownSince = time.Now()
		rt.UpSince = time.Time{}
	}
	_ = errMsg
}

// SetEnabled 运行时启用/停用（不写回配置文件）。
func (s *Store) SetEnabled(group, name string, enabled bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rt := s.get(group, name)
	if rt == nil {
		return
	}
	rt.Enabled = enabled
	if enabled {
		rt.Status = StatusUnknown
		rt.ConsecutiveFailures = 0
		rt.ConsecutiveSuccesses = 0
	} else {
		rt.Status = StatusDisabled
	}
}

// Snapshot 返回可用于 JSON 序列化的完整快照。
func (s *Store) Snapshot(cfg *Config) []GroupState {
	s.mu.RLock()
	defer s.mu.RUnlock()

	out := make([]GroupState, 0, len(cfg.Groups))
	for gi := range cfg.Groups {
		g := &cfg.Groups[gi]
		gs := GroupState{Name: g.Name, Listen: g.Listen}
		for _, name := range s.order[g.Name] {
			rt := s.runtimes[g.Name][name]
			if rt == nil {
				continue
			}
			gs.Upstreams = append(gs.Upstreams, rt.UpstreamState)
			if gs.Active == "" && rt.Enabled && rt.Status != StatusUnhealthy {
				gs.Active = rt.Name
			}
		}
		out = append(out, gs)
	}
	return out
}

// Thresholds 返回当前阈值。
func (s *Store) Thresholds() Thresholds {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.th
}
