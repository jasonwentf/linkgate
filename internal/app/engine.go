package app

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptrace"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// ErrBodyTooLarge 表示请求体超过配置上限。
var ErrBodyTooLarge = errors.New("请求体超过上限")

// bufferedBody 把请求体完整读进内存或临时文件，以便失败后可以重放。
// 这是“连接失败后再换线路”的前提：没有完整副本就没法重发。
type bufferedBody struct {
	size int64
	mem  []byte
	file *os.File
	path string
}

func bufferRequestBody(ctx context.Context, r *http.Request, cfg BufferConfig) (*bufferedBody, error) {
	if r.Body == nil || r.Body == http.NoBody {
		return &bufferedBody{}, nil
	}
	limit := cfg.MaxRequestBytes
	spill := cfg.SpillThreshold

	var mem bytes.Buffer
	mem.Grow(int(min64(r.ContentLength, 1<<20)))
	var f *os.File
	var path string
	var written int64
	buf := make([]byte, 64<<10)

	spillToFile := func() error {
		dir := cfg.TempDir
		if dir == "" {
			dir = os.TempDir()
		}
		tmp, err := os.CreateTemp(dir, "linkgate-body-*")
		if err != nil {
			return fmt.Errorf("创建临时文件失败: %w", err)
		}
		path = tmp.Name()
		if _, err := tmp.Write(mem.Bytes()); err != nil {
			tmp.Close()
			os.Remove(path)
			return fmt.Errorf("写入临时文件失败: %w", err)
		}
		mem = bytes.Buffer{} // 释放已落盘的内存副本，不要留 capacity 引用
		f = tmp
		return nil
	}

	for {
		n, err := r.Body.Read(buf)
		if n > 0 {
			written += int64(n)
			if written > limit {
				if f != nil {
					f.Close()
					os.Remove(path)
				}
				return nil, ErrBodyTooLarge
			}
			if f != nil {
				if _, werr := f.Write(buf[:n]); werr != nil {
					f.Close()
					os.Remove(path)
					return nil, fmt.Errorf("写入临时文件失败: %w", werr)
				}
			} else {
				mem.Write(buf[:n])
				if spill > 0 && int64(mem.Len()) >= spill {
					if serr := spillToFile(); serr != nil {
						return nil, serr
					}
				}
			}
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			if f != nil {
				f.Close()
				os.Remove(path)
			}
			return nil, fmt.Errorf("读取请求体失败: %w", err)
		}
		select {
		case <-ctx.Done():
			if f != nil {
				f.Close()
				os.Remove(path)
			}
			return nil, ctx.Err()
		default:
		}
	}
	if f != nil {
		if _, err := f.Seek(0, io.SeekStart); err != nil {
			f.Close()
			os.Remove(path)
			return nil, err
		}
	}
	return &bufferedBody{size: written, mem: mem.Bytes(), file: f, path: path}, nil
}

func min64(a, b int64) int64 {
	if a < 0 {
		return 0
	}
	if a < b {
		return a
	}
	return b
}

// Reader 返回一个从头开始的独立读取器，用于每次尝试。
func (b *bufferedBody) Reader() (io.ReadCloser, int64) {
	if b == nil {
		return http.NoBody, 0
	}
	if b.file != nil {
		return io.NopCloser(io.NewSectionReader(b.file, 0, b.size)), b.size
	}
	if len(b.mem) == 0 {
		return http.NoBody, 0
	}
	return io.NopCloser(bytes.NewReader(b.mem)), b.size
}

func (b *bufferedBody) Close() {
	if b == nil {
		return
	}
	if b.file != nil {
		b.file.Close()
		if b.path != "" {
			os.Remove(b.path)
		}
	}
	b.mem = nil
}

// groupRuntime 是一个监听端口对应的线路集合。
// 每个实例只属于一 submit 的配置代际：g.cfg 指向这一代的 *Config，
// 请求一旦绑定到 g，就全程使用这一代的配置与线路，不再中途回查 engine。
type groupRuntime struct {
	cfg         *Config     // 本运行时所属的配置代际
	state       *stateTable // 本代运行时状态视图；换代后成为不可变快照
	name        string
	listen      string
	stripPrefix string
	addHeaders  map[string]string
	httpCfg     HTTPConfig
	upstreams   []*upstreamRuntimeCfg // 已按优先级排序
	uaRules     []uaRuleRuntime       // 本代编译后的规则，按配置顺序
	sem         *concurrencyLimit
}

type upstreamRuntimeCfg struct {
	cfg       UpstreamConfig
	base      *url.URL
	transport *http.Transport
	// headerPair 便于快速应用
	headers  []string
	lastUsed atomic.Int64 // 上次成功拿到响应头的时间（UnixNano）
}

// Engine 是代理内核。无 cgo、无平台依赖。
type Engine struct {
	mu          sync.RWMutex
	cfg         *Config
	store       *Store
	events      *EventLog
	log         *slog.Logger
	groups      map[string]*groupRuntime
	gate        *sync.RWMutex
	global      *concurrencyLimit
	groupLimits map[string]*concurrencyLimit
}

func NewEngine(cfg *Config, store *Store, events *EventLog, logger *slog.Logger) (*Engine, error) {
	e := &Engine{store: store, events: events, log: logger, gate: &sync.RWMutex{}, global: newConcurrencyLimit(cfg.Buffer.GlobalMaxConcurrent), groupLimits: make(map[string]*concurrencyLimit)}
	if err := e.rebuild(cfg); err != nil {
		return nil, err
	}
	return e, nil
}

// rebuild 依据配置重建运行时（热重载也走这里）。
func (e *Engine) rebuild(cfg *Config) error {
	groups, err := buildGroups(cfg)
	if err != nil {
		return err
	}
	e.commitGroups(cfg, groups)
	return nil
}

func (e *Engine) commitGroups(cfg *Config, groups map[string]*groupRuntime) {
	e.mu.Lock()
	old := e.groups
	for name, g := range groups {
		// 把本代状态表引用挂到运行时上。之后请求只读这份引用：
		// 提交不会改旧表，连续多少次提交都不影响已 dispatch 的请求。
		g.state = e.store.View(cfg)
		limit := e.groupLimits[name]
		if limit == nil {
			limit = newConcurrencyLimit(cfg.Buffer.MaxConcurrent)
			e.groupLimits[name] = limit
		}
		limit.resize(cfg.Buffer.MaxConcurrent)
		g.sem = limit
	}
	for name := range e.groupLimits {
		if groups[name] == nil {
			delete(e.groupLimits, name)
		}
	}
	e.global.resize(cfg.Buffer.GlobalMaxConcurrent)
	e.cfg = cfg
	e.groups = groups
	e.mu.Unlock()
	closeGroupTransports(old)
}

func closeGroupTransports(groups map[string]*groupRuntime) {
	for _, g := range groups {
		for _, u := range g.upstreams {
			u.transport.CloseIdleConnections()
		}
	}
}

func buildGroups(cfg *Config) (map[string]*groupRuntime, error) {
	out := make(map[string]*groupRuntime, len(cfg.Groups))
	for gi := range cfg.Groups {
		g := &cfg.Groups[gi]
		gr := &groupRuntime{
			cfg:         cfg,
			name:        g.Name,
			listen:      g.Listen,
			stripPrefix: strings.TrimSuffix(g.StripPrefix, "/"),
			addHeaders:  g.AddHeaders,
			httpCfg:     cfg.EffectiveHTTP(g),
			sem:         newConcurrencyLimit(cfg.Buffer.MaxConcurrent),
		}
		for i, rule := range g.UARules {
			matcher, err := compileUAMatch(rule.Match)
			if err != nil {
				return nil, fmt.Errorf("group %q.ua_rules[%d]: %w", g.Name, i, err)
			}
			gr.uaRules = append(gr.uaRules, uaRuleRuntime{matcher: matcher, line: rule.Line})
		}
		// 按优先级排序（同优先级保持书写顺序）
		ordered := make([]UpstreamConfig, len(g.Upstreams))
		copy(ordered, g.Upstreams)
		stableSortByPriority(ordered)
		for i := range ordered {
			u := ordered[i]
			base, err := u.Base()
			if err != nil {
				return nil, err
			}
			connectTO := u.ConnectTimeout.Or(gr.httpCfg.ConnectTimeout.Or(2 * time.Second))
			headerTO := u.ResponseHeaderTimeout.Or(gr.httpCfg.ResponseHeaderTimeout.Or(600 * time.Second))
			dialer := &net.Dialer{
				Timeout:   connectTO,
				KeepAlive: gr.httpCfg.KeepAlive.Or(30 * time.Second),
			}
			tlsCfg := &tls.Config{
				InsecureSkipVerify: u.TLSInsecureSkipVerify, //nolint:gosec // 仅用于内网自签证书，配置显式开启
				ServerName:         u.TLSServerName,
			}
			tr := &http.Transport{
				// 关键：不读系统代理。代理必须直连线路，否则会绕进 Clash/VPN 造成二次故障。
				Proxy:                 nil,
				DialContext:           dialer.DialContext,
				TLSClientConfig:       tlsCfg,
				TLSHandshakeTimeout:   gr.httpCfg.TLSHandshakeTimeout.Or(5 * time.Second),
				ResponseHeaderTimeout: headerTO,
				IdleConnTimeout:       gr.httpCfg.IdleConnTimeout.Or(90 * time.Second),
				MaxIdleConns:          128,
				MaxIdleConnsPerHost:   gr.httpCfg.MaxIdleConnsPerHost,
				DisableCompression:    compressionDisabled(gr.httpCfg),
				ForceAttemptHTTP2:     true,
			}
			gr.upstreams = append(gr.upstreams, &upstreamRuntimeCfg{
				cfg:       u,
				base:      base,
				transport: tr,
				headers:   flattenHeaders(u.Headers),
			})
		}
		out[g.Name] = gr
	}
	return out, nil
}

func stableSortByPriority(us []UpstreamConfig) {
	for i := 1; i < len(us); i++ {
		for j := i; j > 0 && us[j].PriorityValue() < us[j-1].PriorityValue(); j-- {
			us[j], us[j-1] = us[j-1], us[j]
		}
	}
}

func flattenHeaders(h map[string]string) []string {
	if len(h) == 0 {
		return nil
	}
	out := make([]string, 0, len(h)*2)
	for k, v := range h {
		out = append(out, k, v)
	}
	return out
}

// Reload 应用新配置。
func (e *Engine) Reload(cfg *Config) error {
	if err := cfg.Validate(); err != nil {
		return err
	}
	if err := e.rebuild(cfg); err != nil {
		return err
	}
	e.events.Add(Event{Level: "info", Kind: "config", Message: "配置已重新加载"})
	return nil
}

// Config 返回当前配置（只读用途）。
func (e *Engine) Config() *Config {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.cfg
}

// Groups 返回组名到监听地址的映射。
func (e *Engine) Groups() map[string]string {
	e.mu.RLock()
	defer e.mu.RUnlock()
	out := make(map[string]string, len(e.groups))
	for k, v := range e.groups {
		out[k] = v.listen
	}
	return out
}

// Handler 按组名解析当前这一代的 groupRuntime 并返回处理器。
// 只在启动装配或测试入口使用；监听器必须使用 handlerFor 绑定具体代际，
// 否则“先取 dispatch、后按组名回查”会在换组名/互换地址时串组。
func (e *Engine) Handler(group string) http.Handler {
	return e.handlerFor(e.group(group))
}

func (e *Engine) group(name string) *groupRuntime {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.groups[name]
}

// snapshotGroups 复制当前组映射（只读用途，启动时装配监听器）。
func (e *Engine) snapshotGroups() map[string]*groupRuntime {
	e.mu.RLock()
	defer e.mu.RUnlock()
	out := make(map[string]*groupRuntime, len(e.groups))
	for name, g := range e.groups {
		out[name] = g
	}
	return out
}

// handlerFor 把处理器直接绑定到某一代 groupRuntime。
// 请求进入后不再按组名回查 engine：组被改名、删除或两个组互换监听地址时，
// 旧 dispatch 仍只会服务它自己绑定过的那一代对象。
func (e *Engine) handlerFor(g *groupRuntime) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/__linkgate") {
			http.Error(w, "reserved path", http.StatusNotFound)
			return
		}
		if g == nil {
			http.Error(w, "unknown group", http.StatusServiceUnavailable)
			return
		}
		e.serveGroup(w, r, g)
	})
}

// Close 关闭所有空闲连接。
func (e *Engine) Close() {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, g := range e.groups {
		for _, u := range g.upstreams {
			u.transport.CloseIdleConnections()
		}
	}
}

// serveGroup 只处理已经绑定到某一代 g 的请求。
//
// 请求入口处一次性取定：g（含组配置与线路列表）、g.cfg（这一代的全局配置）。
// 之后读请求体、选路、发请求、写健康状态都用这份快照，不再按组名回查 engine，
// 也不因为缓冲期间配置换代而改用新线路集——否则旧请求会拿到空候选或把失败
// 记到新线路上。
func (e *Engine) serveGroup(w http.ResponseWriter, r *http.Request, g *groupRuntime) {
	if g == nil {
		http.Error(w, "unknown group", http.StatusServiceUnavailable)
		return
	}
	cfg := g.cfg
	groupName := g.name
	// 入站时冻结每条线路的运行时可用性。配置在缓冲期间换代后，选路只能回退到
	// “入站当时就是可用”的线路——不能复活已停用或当时已判死的线路。
	frozen := e.freezeLines(g)
	// UA 规则与线路同属 g 的代际；入站即冻结命中结果，不在缓冲后读新规则。
	pinned := g.pinnedLine(r.UserAgent())
	start := time.Now()
	e.store.RecordRequestStart(cfg, groupName, start)
	var completedOK, usedBackup bool
	var completedLine string
	defer func() {
		if r.Context().Err() != nil {
			e.store.RecordRequestCanceled(cfg, groupName)
			return
		}
		e.store.RecordRequestEnd(cfg, groupName, r.UserAgent(), completedLine, completedOK, usedBackup)
	}()

	// Limits cover buffering and forwarding; reload does not reset active slots.
	if !g.sem.acquire(r.Context()) {
		return
	}
	defer g.sem.release()
	if !e.global.acquire(r.Context()) {
		return
	}
	defer e.global.release()

	// 客户端发完请求头就停住不传时，Read 会一直阻塞；给整段读取一个期限。
	rcIn := http.NewResponseController(w)
	_ = rcIn.SetReadDeadline(time.Now().Add(cfg.Buffer.ReadIdleTimeout.Or(120 * time.Second)))
	body, err := bufferRequestBody(r.Context(), r, cfg.Buffer)
	_ = rcIn.SetReadDeadline(time.Time{})
	if err != nil {
		if errors.Is(err, ErrBodyTooLarge) {
			http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
			e.events.Add(Event{Level: "warn", Kind: "request", Group: groupName, Message: fmt.Sprintf("请求体超过上限（%d 字节）", cfg.Buffer.MaxRequestBytes)})
			return
		}
		http.Error(w, "failed to read request body", http.StatusBadRequest)
		return
	}
	defer body.Close()

	order := e.candidates(g, frozen, body.size, pinned)
	if len(order) == 0 {
		http.Error(w, "no upstream configured", http.StatusServiceUnavailable)
		return
	}
	maxAttempts := cfg.Retry.MaxAttempts
	if maxAttempts > len(order) {
		maxAttempts = len(order)
	}
	if cfg.Retry.Mode == "none" {
		maxAttempts = 1
	}

	primaryName := ""
	for _, up := range g.upstreams {
		if up.cfg.IsEnabled() {
			primaryName = up.cfg.Name
			break
		}
	}
	if pinned != "" {
		primaryName = pinned
	}
	var (
		lastErr  error
		lastName string
	)
	for i := 0; i < maxAttempts; i++ {
		up := order[i]
		name := up.cfg.Name
		usedBackup = usedBackup || i > 0 || name != primaryName
		attemptStart := time.Now()
		resp, err, at := e.doAttempt(r, g, up, body)
		if err == nil {
			completedLine = name
			// 注意：不能在这里关闭缓冲副本。收到响应头不代表上传已经结束
			// （HTTP/2 双向流、服务端提前响应都会继续读请求体），提前关文件会
			// 把还在上传的请求截断。统一由 defer body.Close() 在流结束后释放。
			e.store.RecordSelected(cfg, groupName, name, i > 0 || name != primaryName)
			completedOK = e.stream(w, r, resp, g, up, name, time.Since(attemptStart), start)
			return
		}

		if r.Context().Err() != nil {
			e.log.Debug("client canceled before response headers", "group", groupName, "upstream", name, "err", errString(err))
			e.events.Add(Event{Level: "debug", Kind: "request", Group: groupName, Upstream: name, Message: "客户端主动取消请求"})
			return
		}

		// 只有“请求头一个字节都没写出去”才允许换线路：此时上游不可能收到请求，
		// 重发不会重复计费。判断依据来自 httptrace，不看错误类型或错误字符串。
		if !at.failoverSafe() {
			e.store.RecordFailure(cfg, groupName, name, at.reason(err), time.Since(start))
			e.events.Add(Event{
				Level: "error", Kind: "request", Group: groupName, Upstream: name,
				DurationMS: time.Since(start).Milliseconds(),
				Message:    at.reason(err),
			})
			e.log.Error("upstream request failed", "group", groupName, "upstream", name, "reason", at.reason(err), "err", errString(err))
			writeProxyError(w, groupName, name, err)
			return
		}

		lastErr, lastName = err, name
		e.store.RecordFailure(cfg, groupName, name, at.reason(err), time.Since(start))
		if i+1 >= maxAttempts {
			break
		}
		e.store.RecordFailover(cfg, groupName, name)
		next := order[i+1].cfg.Name
		e.events.Add(Event{
			Level: "warn", Kind: "failover", Group: groupName, Upstream: name, From: name, To: next,
			DurationMS: time.Since(attemptStart).Milliseconds(),
			Message:    "连接未建立，切换线路: " + shorten(errString(err), 200),
		})
		e.log.Warn("failover", "group", groupName, "from", name, "to", next, "err", errString(err))
		if cfg.Retry.Backoff.Duration > 0 && i+1 < maxAttempts {
			select {
			case <-time.After(cfg.Retry.Backoff.Duration):
			case <-r.Context().Done():
				return
			}
		}
	}

	e.events.Add(Event{
		Level: "error", Kind: "request", Group: groupName, Upstream: lastName,
		DurationMS: time.Since(start).Milliseconds(),
		Message:    "所有线路均连接失败: " + shorten(errString(lastErr), 300),
	})
	writeProxyError(w, groupName, lastName, lastErr)
}

func errString(err error) string {
	if err == nil {
		return "unknown"
	}
	// 错误文案可能带完整上游 URL（含 userinfo/query），对外与落日志前统一脱敏。
	return redactURLText(err.Error())
}

// lineSnapshot 是请求入站时一条线路的运行时可用性快照。
type lineSnapshot struct {
	enabled bool
	usable  bool // 运行时启用且健康状态不是 unhealthy
}

// freezeLines 在请求入口冻结每条线路的可用性。数据源是 g.state：
// 请求自己持有那一代的状态表，不按代际回查 Store、也不依赖任何“保留几代”
// 的缓存，因此连续多次提交不会让在飞请求丢失自己那代的停用/健康信息。
func (e *Engine) freezeLines(g *groupRuntime) map[string]lineSnapshot {
	out := make(map[string]lineSnapshot, len(g.upstreams))
	for _, u := range g.upstreams {
		enabled, usable := e.lineAvailability(g, u)
		out[u.cfg.Name] = lineSnapshot{enabled: enabled, usable: usable}
	}
	return out
}

// lineAvailability 从 groupRuntime 自带的状态表引用读取线路可用性。
// 引用生命周期随 groupRuntime（及其 handler / 在飞请求），与 Store 已经提交
// 了几代无关：换代表在提交后停止写入，读到的就是请求那一代的最终状态；
// 仍是当前代时读到的是实时状态（健康变化、运行时开关）。
func (e *Engine) lineAvailability(g *groupRuntime, u *upstreamRuntimeCfg) (enabled, usable bool) {
	enabled = u.cfg.IsEnabled()
	usable = enabled
	if rtEnabled, status, ok := e.store.LineState(g.state, g.name, u.cfg.Name); ok {
		enabled = rtEnabled
		usable = rtEnabled && status != StatusUnhealthy
	}
	return enabled, usable
}

// candidates 返回按优先级排序的线路：先可用线路，再接上不可用线路作为兜底。
// 请求体超过线路 max_body_bytes 的候选始终排除。
//
// 选路只看这一代 g 的线路列表，按是否仍是当前代分成两种语义：
//   - live=true：读实时状态（配置 enabled + 运行时开关 + 健康），未判死的
//     线路优先；已判死的线路保留在兜底尾巴里。
//   - live=false（请求缓冲/执行期间配置已换代）：**只用入站冻结快照**——
//     入站时 enabled 且 usable 的线路才入选，完全不经过旧状态表的 enabled
//     过滤。这样“先可用、后被停用/判死”的线路不会被旧表二次判死，也不会
//     复活入站时就不可用的线路。
//
// UA pinning 只排序已有候选：目标排最前，其余保持原健康/优先级顺序；
// 不绕过停用、体积或代际资格过滤，也不改变 failoverSafe 的换线许可。
func (e *Engine) candidates(g *groupRuntime, frozen map[string]lineSnapshot, bodySize int64, pinned string) []*upstreamRuntimeCfg {
	healthy := make([]*upstreamRuntimeCfg, 0, len(g.upstreams))
	unhealthy := make([]*upstreamRuntimeCfg, 0, len(g.upstreams))
	// 本代仍是当前代时，健康/运行时开关可以继续变化，按实时值选路；
	// 一旦换代，本代状态表已冻结，只认请求入站时冻结的可用线路。
	live := e.store.Config() == g.cfg
	for _, u := range g.upstreams {
		if !live {
			// 换代后纯用入站快照，不再经过旧状态表/旧配置的 enabled 过滤。
			snap, ok := frozen[u.cfg.Name]
			if !ok || !snap.enabled || !snap.usable {
				continue
			}
			if u.cfg.MaxBodyBytes > 0 && bodySize > u.cfg.MaxBodyBytes {
				continue
			}
			healthy = append(healthy, u)
			continue
		}
		if !u.cfg.IsEnabled() {
			continue
		}
		enabled, usable := e.lineAvailability(g, u)
		if !enabled {
			continue
		}
		if u.cfg.MaxBodyBytes > 0 && bodySize > u.cfg.MaxBodyBytes {
			continue
		}
		if usable {
			healthy = append(healthy, u)
		} else {
			unhealthy = append(unhealthy, u)
		}
	}
	order := append(healthy, unhealthy...)
	for i, u := range order {
		if u.cfg.Name == pinned {
			copy(order[1:i+1], order[:i])
			order[0] = u
			break
		}
	}
	return order
}

// attemptTrace 精确回答“这次尝试到底有没有把请求发出去”。
//
// 判断换路绝不能靠错误字符串：TLS 层可能在服务端已经读完 POST 之后
// 才返回畸形记录，按错误类型猜会误判成“连接没建立”从而重发。
// 唯一可靠的信号是 httptrace 的两个回调：
//   - ConnectDone(err) 非空 = 这条线路的新连接没建成（TCP 或 TLS 都没完成）；
//   - WroteHeaders 被调用 = 请求头已经写到线路上，对端有可能已经收到。
//
// 只有「连接失败 且 没写过任何请求头」才允许换线路重发。
type attemptTrace struct {
	mu           sync.Mutex
	gotConn      bool
	connectErr   error
	wroteHeaders bool
	onWrote      func()
}

func (t *attemptTrace) clientTrace() *httptrace.ClientTrace {
	return &httptrace.ClientTrace{
		GotConn: func(httptrace.GotConnInfo) {
			t.mu.Lock()
			t.gotConn = true
			t.mu.Unlock()
		},
		ConnectDone: func(_, _ string, err error) {
			if err != nil {
				t.mu.Lock()
				t.connectErr = err
				t.mu.Unlock()
			}
		},
		WroteHeaders: func() {
			t.mu.Lock()
			t.wroteHeaders = true
			t.mu.Unlock()
		},
		WroteRequest: func(httptrace.WroteRequestInfo) {
			t.mu.Lock()
			hook := t.onWrote
			t.mu.Unlock()
			if hook != nil {
				hook()
			}
		},
	}
}

// failoverSafe 回答“这次尝试能不能安全地换到下一条线路重发”。
//
// 只有一个许可：从未拿到过连接（GotConn 没触发）。这意味着 DNS、TCP、TLS
// 全部失败，一个字节都没发出去。
//
// 曾经试过更宽松的规则（“拨号失败且没写完请求头也算安全”），评审用
// Happy Eyeballs 实测推翻：一个地址拨号失败、另一个成功，连接建立后请求头
// 已经写出若干字节才报错，WroteHeaders 却还没触发，结果备用线路收到了请求。
// 部分写出的字节无法通过 httptrace 观察，所以只能一刀切：拿到连接就不再换路。
//
// 代价是连接池里的旧连接失效时会直接 502 而不是换路；缓解办法是
// ReconnectAfterIdle：闲置一段时间后强制重新拨号，让线路故障表现为
// 干净的拨号失败（此时 GotConn 不会触发）。
func (t *attemptTrace) failoverSafe() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return !t.gotConn
}

func (t *attemptTrace) reason(err error) string {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.wroteHeaders {
		return "请求已发出（已写请求头），不重发: " + errString(err)
	}
	if t.connectErr != nil {
		return "连接未建立: " + errString(t.connectErr)
	}
	return "未发出请求: " + errString(err)
}

// doAttempt 发起一次上游请求。err 非空表示这次尝试失败。
func (e *Engine) doAttempt(orig *http.Request, g *groupRuntime, up *upstreamRuntimeCfg, body *bufferedBody) (*http.Response, error, *attemptTrace) {
	u := up.base
	path := orig.URL.Path
	if g.stripPrefix != "" && strings.HasPrefix(path, g.stripPrefix) {
		path = strings.TrimPrefix(path, g.stripPrefix)
		if path == "" {
			path = "/"
		}
	}
	target := &url.URL{
		Scheme:   u.Scheme,
		Host:     u.Host,
		Path:     strings.TrimSuffix(u.Path, "/") + path,
		RawQuery: orig.URL.RawQuery,
	}

	reader, size := body.Reader()
	baseCtx, cancel := context.WithCancel(orig.Context())
	uploadIdle := g.httpCfg.UploadIdleTimeout.Or(60 * time.Second)
	guard := newUploadGuard(reader, cancel, uploadIdle)
	at := &attemptTrace{onWrote: guard.Stop}
	reqCtx := httptrace.WithClientTrace(baseCtx, at.clientTrace())
	req, err := http.NewRequestWithContext(reqCtx, orig.Method, target.String(), guard)
	if err != nil {
		guard.Close()
		cancel()
		return nil, err, at
	}
	req.ContentLength = size
	if size == 0 {
		req.ContentLength = 0
	}
	req.Header = cloneHeader(orig.Header)
	stripHopByHop(req.Header)
	req.Header.Del("Expect")

	// Host 处理：默认用上游地址，keep_client_host 时保留客户端原值。
	switch {
	case up.cfg.KeepClientHost:
		req.Host = orig.Host
	case up.cfg.Host != "":
		req.Host = up.cfg.Host
	default:
		req.Host = u.Host
	}

	for k, v := range g.addHeaders {
		req.Header.Set(k, v)
	}
	for i := 0; i+1 < len(up.headers); i += 2 {
		req.Header.Set(up.headers[i], up.headers[i+1])
	}
	if size > 0 && req.Header.Get("Content-Type") == "" {
		// 不臆造类型，保持透明；这里只是避免 Go 自动补 content-type 为空的情况
	}

	if idle := g.httpCfg.ReconnectAfterIdle.Duration; idle > 0 {
		if last := up.lastUsed.Load(); last > 0 && time.Since(time.Unix(0, last)) > idle {
			up.transport.CloseIdleConnections()
		}
	}
	var warnTimer *time.Timer
	if warn := g.httpCfg.ResponseHeaderWarnAfter.Duration; warn > 0 {
		warnTimer = time.AfterFunc(warn, func() {
			if orig.Context().Err() != nil {
				return
			}
			e.log.Warn("waiting for response headers", "group", g.name, "upstream", up.cfg.Name, "after", warn)
			e.events.Add(Event{Level: "warn", Kind: "request", Group: g.name, Upstream: up.cfg.Name,
				Message: fmt.Sprintf("超过 %s 仍未收到响应头（仅预警，不重发）", warn)})
		})
	}
	resp, err := up.transport.RoundTrip(req)
	if warnTimer != nil {
		warnTimer.Stop()
	}
	if err != nil {
		guard.Stop()
		guard.Close()
		cancel()
		if guard.fired.Load() {
			return nil, fmt.Errorf("上传请求体超过 %s 无进展（对端未读取）: %w", uploadIdle, err), at
		}
		return nil, err, at
	}
	up.lastUsed.Store(time.Now().UnixNano())
	resp.Body = &cancelOnClose{ReadCloser: resp.Body, cleanup: func() {
		guard.Stop()
		cancel()
	}}
	return resp, nil, at
}

type cancelOnClose struct {
	io.ReadCloser
	cleanup func()
}

func (c *cancelOnClose) Close() error {
	err := c.ReadCloser.Close()
	if c.cleanup != nil {
		c.cleanup()
	}
	return err
}

// uploadGuard 在“上传请求体长时间没有任何进展”时取消请求。
//
// 为什么需要它：ResponseHeaderTimeout 只覆盖“请求写完到收到响应头”这一段。
// 如果对端接受了 TCP 连接却一直不读，写 12MiB 会无限期阻塞，健康检查也没法救。
// 这里看的是“有没有进展”，不是总时长——慢线路传大文件可以慢，但不能停。
type uploadGuard struct {
	r      io.ReadCloser
	timer  *time.Timer
	cancel context.CancelFunc
	idle   time.Duration
	fired  atomic.Bool
}

func newUploadGuard(r io.ReadCloser, cancel context.CancelFunc, idle time.Duration) *uploadGuard {
	g := &uploadGuard{r: r, cancel: cancel, idle: idle}
	if idle > 0 {
		g.timer = time.AfterFunc(idle, func() {
			g.fired.Store(true)
			cancel()
		})
	}
	return g
}

// Read 在读到数据时重置计时。
//
// 计时只在 httptrace 的 WroteRequest 回调里停止——那才是“请求真的写完了”的时刻。
// 不能在读到 EOF 或拿到响应头时停：HTTP/1 在最后 flush 前会先关闭请求体，
// HTTP/2 可以在上传仍被 flow-control 卡住时先返回响应头。这两段时间都必须由
// 这个计时兜住，否则对端不读、不窗口时请求会无限期挂着。
func (g *uploadGuard) Read(p []byte) (int, error) {
	n, err := g.r.Read(p)
	if n > 0 && g.timer != nil {
		g.timer.Reset(g.idle)
	} else if err != nil && err != io.EOF && g.timer != nil {
		g.timer.Stop()
	}
	return n, err
}

// Stop 只在确认“请求真的写完了”或明确的终止路径上调用。
func (g *uploadGuard) Stop() {
	if g.timer != nil {
		g.timer.Stop()
	}
}

// Close 只关闭 reader，不停表。
//
// net/http 在 HTTP/1 上会先关闭请求 Body、之后才做最后一次 bufio.Flush；
// 在 HTTP/2 上可能先返回响应头、请求体仍被 flow-control 卡住。这两段时间都要
// 由 upload watchdog 兜住，所以停表只能发生在 WroteRequest 回调或错误清理路径。
func (g *uploadGuard) Close() error {
	return g.r.Close()
}

// stream 把上游响应原样写回客户端，边读边 flush，适配 SSE。
//
// 三种结束方式要区分清楚：
//   - io.EOF：正常读完；
//   - 空闲超时：上游卡死，主动打断；
//   - 其他读错误（含 chunked 缺少终止块）：响应被截断，必须让客户端也能看出来，
//     不能悄悄补一个正常的结束标记，否则 harness 会以为流是完整的。
func (e *Engine) stream(w http.ResponseWriter, orig *http.Request, resp *http.Response, g *groupRuntime, up *upstreamRuntimeCfg, upstream string, headerLatency time.Duration, start time.Time) (success bool) {
	defer resp.Body.Close()

	dst := w.Header()
	copyHeader(dst, resp.Header)
	stripHopByHop(dst)
	if resp.ContentLength >= 0 {
		dst.Set("Content-Length", strconv.FormatInt(resp.ContentLength, 10))
	}

	idle := g.httpCfg.StreamIdleTimeout.Or(600 * time.Second)
	rc := http.NewResponseController(w)
	setDeadline := func() { _ = rc.SetWriteDeadline(time.Now().Add(idle)) }

	setDeadline()
	w.WriteHeader(resp.StatusCode)
	if err := rc.Flush(); err != nil {
		e.log.Debug("client flush failed", "group", g.name, "upstream", upstream, "err", errString(err))
		return
	}

	var idleFired atomic.Bool
	timer := time.AfterFunc(idle, func() {
		idleFired.Store(true)
		e.events.Add(Event{Level: "warn", Kind: "request", Group: g.name, Upstream: upstream,
			Message: fmt.Sprintf("流式响应超过 %s 无数据，判定为卡死并断开", idle)})
		// 关闭上游响应体，读循环会立刻返回
		resp.Body.Close()
	})

	buf := make([]byte, 32<<10)
	var total int64
	var readErr error
	clientGone := false
	for {
		n, err := resp.Body.Read(buf)
		if n > 0 {
			timer.Reset(idle)
			// 每次写之前续期：客户端如果只保持连接却不读，Write/Flush 不能永久阻塞。
			setDeadline()
			if _, werr := w.Write(buf[:n]); werr != nil {
				clientGone, readErr = true, werr
				break
			}
			total += int64(n)
			if ferr := rc.Flush(); ferr != nil {
				clientGone, readErr = true, ferr
				break
			}
		}
		if err != nil {
			readErr = err
			break
		}
	}
	timer.Stop()
	_ = rc.SetWriteDeadline(time.Time{})

	if clientGone {
		// 客户端自己断开，不归因于线路；上游请求会随 context 取消。
		e.log.Debug("client went away", "group", g.name, "upstream", upstream, "err", redactURLText(errString(readErr)), "bytes", total)
		return
	}

	statusFailed := false
	for _, code := range up.cfg.FailureStatuses {
		if resp.StatusCode == code {
			statusFailed = true
			break
		}
	}
	truncated := readErr != nil && !errors.Is(readErr, io.EOF)

	// 客户端自己断开导致的上游读取中止，不算线路故障。
	// 典型场景：harness 收完最后一个块就关连接，r.Context() 随即取消，
	// resp.Body.Read 返回 context canceled——这不是截断，不能记成线路失败。
	if orig.Context().Err() != nil {
		e.log.Debug("client canceled during stream", "group", g.name, "upstream", upstream,
			"err", redactURLText(errString(readErr)), "bytes", total)
		return
	}

	// 一次尝试只提交一个终态，避免同一请求既算成功又算失败。
	switch {
	case idleFired.Load():
		e.store.RecordFailure(g.cfg, g.name, upstream, "stream idle timeout", time.Since(start))
		panic(http.ErrAbortHandler)
	case truncated:
		e.store.RecordFailure(g.cfg, g.name, upstream, "stream truncated: "+redactURLText(readErr.Error()), time.Since(start))
		e.events.Add(Event{Level: "error", Kind: "request", Group: g.name, Upstream: upstream,
			Status: resp.StatusCode, DurationMS: time.Since(start).Milliseconds(),
			Message: "响应流异常结束（可能被截断）: " + shorten(redactURLText(readErr.Error()), 200)})
		panic(http.ErrAbortHandler)
	case statusFailed:
		e.store.RecordFailure(g.cfg, g.name, upstream, fmt.Sprintf("HTTP %d 被该线路配置计为故障", resp.StatusCode), time.Since(start))
		e.events.Add(Event{Level: "warn", Kind: "health", Group: g.name, Upstream: upstream,
			Status: resp.StatusCode, DurationMS: time.Since(start).Milliseconds(),
			Message: fmt.Sprintf("按配置把 HTTP %d 计为线路故障", resp.StatusCode)})
	case resp.StatusCode >= 500:
		// 上游自身错误：计入失败数，但不改变线路健康状态。
		e.store.RecordHTTPError(g.cfg, g.name, upstream, time.Since(start))
		e.events.Add(Event{Level: "warn", Kind: "request", Group: g.name, Upstream: upstream,
			Status: resp.StatusCode, DurationMS: time.Since(start).Milliseconds(),
			Message: fmt.Sprintf("上游返回 %d", resp.StatusCode)})
	default:
		e.store.RecordSuccess(g.cfg, g.name, upstream, time.Since(start))
		e.events.Add(Event{Level: "info", Kind: "request", Group: g.name, Upstream: upstream,
			Status: resp.StatusCode, DurationMS: time.Since(start).Milliseconds(),
			Message: fmt.Sprintf("%s %s → %d", orig.Method, orig.URL.Path, resp.StatusCode)})
	}

	if headerLatency > 3*time.Second {
		e.log.Info("slow first byte", "group", g.name, "upstream", upstream, "header_ms", headerLatency.Milliseconds(), "bytes", total)
	}
	return !statusFailed && resp.StatusCode < 500
}

func writeProxyError(w http.ResponseWriter, group, upstream string, err error) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusBadGateway)
	msg := errString(err)
	fmt.Fprintf(w, `{"error":{"type":"linkgate_upstream_unreachable","group":%q,"upstream":%q,"message":%q}}`, group, upstream, msg)
}

func shorten(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

var hopByHop = map[string]bool{
	"Connection":          true,
	"Proxy-Connection":    true,
	"Keep-Alive":          true,
	"Proxy-Authenticate":  true,
	"Proxy-Authorization": true,
	"Te":                  true,
	"Trailer":             true,
	"Transfer-Encoding":   true,
	"Upgrade":             true,
}

func stripHopByHop(h http.Header) {
	for _, v := range h.Values("Connection") {
		for _, f := range strings.Split(v, ",") {
			if k := strings.TrimSpace(f); k != "" {
				h.Del(k)
			}
		}
	}
	for k := range hopByHop {
		h.Del(k)
	}
}

func cloneHeader(src http.Header) http.Header {
	dst := make(http.Header, len(src))
	for k, v := range src {
		dst[k] = append([]string(nil), v...)
	}
	return dst
}

func copyHeader(dst, src http.Header) {
	for k, v := range src {
		for _, vv := range v {
			dst.Add(k, vv)
		}
	}
}
