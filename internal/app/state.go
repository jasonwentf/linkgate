package app

import (
	"fmt"
	"net/http"
	"sort"
	"strings"
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
	Kind     string       `json:"kind,omitempty"`

	// Consecutive* 只统计真实请求；Probe* 只统计主动探针。
	// 两边分开累计，避免轻量 /health 成功把真实请求的失败计数冲掉。
	ConsecutiveFailures  int `json:"consecutive_failures"`
	ConsecutiveSuccesses int `json:"consecutive_successes"`
	ProbeFailures        int `json:"probe_failures"`
	ProbeSuccesses       int `json:"probe_successes"`

	DownSince time.Time `json:"down_since,omitempty"`
	UpSince   time.Time `json:"up_since,omitempty"`

	Reach            bool      `json:"reach"`
	Class            string    `json:"class"`
	HTTPStatus       int       `json:"http_status"`
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
	Comment   string          `json:"comment,omitempty"`
	Protocol  string          `json:"protocol,omitempty"`
	UARules   []UARuleConfig  `json:"ua_rules"`
	Upstreams []UpstreamState `json:"upstreams"`
	// Active 是当前会优先选择的线路名。
	Active        string    `json:"active"`
	LastRequestAt time.Time `json:"last_request_at"`
}

type upstreamRuntime struct {
	UpstreamState
	// identity 是“这条线路还是不是原来那条”的指纹，见 upstreamIdentity。
	identity string
}

// stateTable 是某一代配置的整张运行时状态表。
//
// Store 换代时整体替换这张表；旧表不再被任何写路径触碰，因而天然是不可变
// 快照。旧 groupRuntime 持有旧表引用，生命周期随在飞请求——请求只读自己那代
// 的表，不查缓存、也不受后续提交次数影响。
type stateTable struct {
	order    map[string][]string // group -> upstream 名称，按优先级排序
	runtimes map[string]map[string]*upstreamRuntime
	services map[string]*serviceRuntime
}

func (t *stateTable) line(group, name string) *upstreamRuntime {
	if t == nil {
		return nil
	}
	return t.runtimes[group][name]
}

// Store 保存当前代的线路运行时状态，线程安全。
type Store struct {
	mu    sync.RWMutex
	cfg   *Config
	th    Thresholds
	table *stateTable
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

	// reload 通常就是 s 自己，先取旧表引用再整体替换；旧表随后只读，
	// 仍被旧 groupRuntime / 在飞请求引用着。
	var old *stateTable
	if reload != nil {
		old = reload.table
	}

	newOrder := make(map[string][]string, len(cfg.Groups))
	newRuntimes := make(map[string]map[string]*upstreamRuntime, len(cfg.Groups))
	newServices := make(map[string]*serviceRuntime, len(cfg.Groups))

	for gi := range cfg.Groups {
		g := &cfg.Groups[gi]
		newServices[g.Name] = cloneServiceRuntime(old, g)
		names := sortedUpstreamNames(g)
		newOrder[g.Name] = names
		m := make(map[string]*upstreamRuntime, len(g.Upstreams))
		for _, u := range g.Upstreams {
			st := UpstreamState{
				Group:    g.Name,
				Name:     u.Name,
				URL:      u.URL,
				Priority: u.PriorityValue(),
				Enabled:  u.IsEnabled(),
				Comment:  u.Comment,
				Kind:     u.Kind,
				Status:   StatusUnknown,
			}
			if !u.IsEnabled() {
				st.Status = StatusDisabled
			}
			identity := upstreamIdentity(g, u)
			if old != nil {
				if old := old.line(g.Name, u.Name); old != nil && old.identity == identity {
					st.RequestsOK = old.RequestsOK
					st.RequestsFailed = old.RequestsFailed
					st.FailoversOut = old.FailoversOut
					st.SelectedTotal = old.SelectedTotal
					st.SelectedAsBackup = old.SelectedAsBackup
					st.Reach, st.Class, st.HTTPStatus = old.Reach, old.Class, old.HTTPStatus
					st.LastProbeLatency = old.LastProbeLatency
					st.LastProbeAt = old.LastProbeAt
					st.LastRequestLatency = old.LastRequestLatency
					st.LastRequestAt = old.LastRequestAt
					st.LastRequestError = old.LastRequestError
					st.ProbeFailures = old.ProbeFailures
					st.ProbeSuccesses = old.ProbeSuccesses
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
			m[u.Name] = &upstreamRuntime{UpstreamState: st, identity: identity}
		}
		newRuntimes[g.Name] = m
	}

	s.cfg = cfg
	s.th = th
	s.table = &stateTable{order: newOrder, runtimes: newRuntimes, services: newServices}
}

// Config 返回状态表对应的配置代际。配置提交时整体替换，指针即代际。
func (s *Store) Config() *Config {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.cfg
}

// LineState 读取某一代状态表里一条线路的当前运行时状态。
// 读旧表时同样持 Store 锁：换代表在提交那一刻停止写入，但换代表在被引用的
// 那段时间里仍可能被同代的 Record*/SetEnabled 并发更新，必须与写路径互斥。
func (s *Store) LineState(table *stateTable, group, name string) (enabled bool, status HealthStatus, ok bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	rt := table.line(group, name)
	if rt == nil {
		return false, StatusUnknown, false
	}
	return rt.Enabled, rt.Status, true
}

// View 返回某一代配置对应的状态表引用。
// engine 把它挂到该代的 groupRuntime 上；之后请求只读这份引用，不按代际
// 回查 Store，因此连续多次提交也不会让旧请求丢失自己那代的状态。
func (s *Store) View(cfg *Config) *stateTable {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.cfg != cfg {
		return nil
	}
	return s.table
}

// upstreamIdentity 定义“什么算同一条线路”。
//
// 只有决定请求实际去向与握手身份的字段才进入指纹：URL、Host / keep_client_host、
// TLS 相关设置，以及**最终生效的请求头**（组级 add_headers 与线路级 headers
// 合并，线路级同名键覆盖组级；请求头名按 HTTP 规则大小写不敏感）。其中任何
// 一个变了，就视为一条新线路——旧线路的健康状态与失败计数不能继承，否则改
// 组级 Authorization 之类后新请求会被旧故障连坐，或者旧 healthy 直接盖上来。
//
// 不进入指纹的字段（name、comment、priority、enabled、各类超时、max_body_bytes、
// failure_statuses、probe 配置）只影响调度与展示，不改变请求打到谁、带什么头。
func upstreamIdentity(g *GroupConfig, u UpstreamConfig) string {
	headers := make(map[string]string, len(g.AddHeaders)+len(u.Headers))
	for k, v := range g.AddHeaders {
		headers[http.CanonicalHeaderKey(k)] = v
	}
	for k, v := range u.Headers {
		headers[http.CanonicalHeaderKey(k)] = v // 线路级覆盖组级
	}
	keys := make([]string, 0, len(headers))
	for k := range headers {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	fmt.Fprintf(&b, "url\x00%s\x00host\x00%s\x00keephost\x00%t\x00insecure\x00%t\x00servername\x00%s",
		u.URL, u.Host, u.KeepClientHost, u.TLSInsecureSkipVerify, u.TLSServerName)
	for _, k := range keys {
		fmt.Fprintf(&b, "\x00h\x00%s\x00%s", k, headers[k])
	}
	return b.String()
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
			if a == nil || b == nil || a.PriorityValue() <= b.PriorityValue() {
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
	if s.table == nil {
		return nil
	}
	out := make([]string, len(s.table.order[group]))
	copy(out, s.table.order[group])
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
	return s.table.line(group, name)
}

// 以下 Record* 都带 gen（请求发起时那一代的 *Config）。状态表已经换代时
// 直接丢弃这次写入：旧请求的失败不能连坐到同名的新线路，反之亦然。
func (s *Store) RecordSelected(gen *Config, group, name string, backup bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cfg != gen {
		return
	}
	if rt := s.get(group, name); rt != nil {
		rt.SelectedTotal++
		if backup {
			rt.SelectedAsBackup++
		}
		rt.LastRequestAt = time.Now()
	}
}

func (s *Store) RecordSuccess(gen *Config, group, name string, latency time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cfg != gen {
		return
	}
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

func (s *Store) RecordFailure(gen *Config, group, name string, errMsg string, latency time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cfg != gen {
		return
	}
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
func (s *Store) RecordHTTPError(gen *Config, group, name string, latency time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cfg != gen {
		return
	}
	rt := s.get(group, name)
	if rt == nil || !rt.Enabled {
		return
	}
	rt.RequestsFailed++
	rt.LastRequestAt = time.Now()
	rt.LastRequestLatency = latency.Milliseconds()
}

// RecordFailover 记录一次“从该线路切到别处”。
func (s *Store) RecordFailover(gen *Config, group, name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cfg != gen {
		return
	}
	if rt := s.get(group, name); rt != nil {
		rt.FailoversOut++
	}
}

// RecordProbe 记录一次主动探针结果。
func (s *Store) RecordProbe(group, name string, ok bool, latency time.Duration, errMsg string) {
	class := "unreachable"
	if ok {
		class = "healthy"
	}
	s.RecordProbeResult(group, name, ProbeResult{OK: ok, Reach: ok, Class: class, LatencyMS: latency.Milliseconds(), Error: errMsg})
}

func (s *Store) RecordProbeResult(group, name string, result ProbeResult) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rt := s.get(group, name)
	if rt == nil || !rt.Enabled {
		return
	}
	rt.Reach, rt.Class, rt.HTTPStatus = result.Reach, result.Class, result.HTTPStatus
	rt.LastProbeAt = time.Now()
	rt.LastProbeLatency = result.LatencyMS
	if result.OK {
		rt.LastProbeError = ""
		rt.ProbeFailures = 0
		rt.ProbeSuccesses++
		s.maybeUpLocked(rt, rt.ProbeSuccesses)
		return
	}
	rt.LastProbeError = redactURLText(result.Error)
	rt.ProbeSuccesses = 0
	rt.ProbeFailures++
	if rt.ProbeFailures >= s.th.FailThreshold {
		s.markDownLocked(rt, result.Error)
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
		gs := GroupState{Name: g.Name, Listen: g.Listen, Comment: g.Comment, Protocol: g.Protocol,
			UARules: append([]UARuleConfig{}, g.UARules...)}
		if service := s.table.services[g.Name]; service != nil {
			gs.LastRequestAt = service.LastRequestAt
		}
		for _, name := range s.table.order[g.Name] {
			rt := s.table.runtimes[g.Name][name]
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
