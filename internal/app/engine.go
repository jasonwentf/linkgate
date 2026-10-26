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
type groupRuntime struct {
	name        string
	listen      string
	stripPrefix string
	addHeaders  map[string]string
	httpCfg     HTTPConfig
	upstreams   []*upstreamRuntimeCfg // 已按优先级排序
	sem         chan struct{}
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
	mu     sync.RWMutex
	cfg    *Config
	store  *Store
	events *EventLog
	log    *slog.Logger
	groups map[string]*groupRuntime
	closed bool
}

func NewEngine(cfg *Config, store *Store, events *EventLog, logger *slog.Logger) (*Engine, error) {
	e := &Engine{store: store, events: events, log: logger}
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
	e.mu.Lock()
	old := e.groups
	e.cfg = cfg
	e.groups = groups
	e.mu.Unlock()
	if old != nil {
		for _, g := range old {
			for _, u := range g.upstreams {
				u.transport.CloseIdleConnections()
			}
		}
	}
	return nil
}

func buildGroups(cfg *Config) (map[string]*groupRuntime, error) {
	out := make(map[string]*groupRuntime, len(cfg.Groups))
	for gi := range cfg.Groups {
		g := &cfg.Groups[gi]
		gr := &groupRuntime{
			name:        g.Name,
			listen:      g.Listen,
			stripPrefix: strings.TrimSuffix(g.StripPrefix, "/"),
			addHeaders:  g.AddHeaders,
			httpCfg:     cfg.EffectiveHTTP(g),
			sem:         make(chan struct{}, cfg.Buffer.MaxConcurrent),
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
				DisableCompression:    true,
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
		for j := i; j > 0 && us[j].Priority < us[j-1].Priority; j-- {
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

// Handler 返回某个组的 HTTP 处理器。
func (e *Engine) Handler(group string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/__linkgate") {
			http.Error(w, "reserved path", http.StatusNotFound)
			return
		}
		e.serveGroup(w, r, group)
	})
}

// Close 关闭所有空闲连接。
func (e *Engine) Close() {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.closed = true
	for _, g := range e.groups {
		for _, u := range g.upstreams {
			u.transport.CloseIdleConnections()
		}
	}
}

func (e *Engine) group(name string) *groupRuntime {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.groups[name]
}

type attemptResult struct {
	err       error
	resp      *http.Response
	upstream  string
	connectMS int64
	headerMS  int64
	attempted int
}

func (e *Engine) serveGroup(w http.ResponseWriter, r *http.Request, groupName string) {
	g := e.group(groupName)
	if g == nil {
		http.Error(w, "unknown group", http.StatusServiceUnavailable)
		return
	}
	cfg := e.Config()
	start := time.Now()

	// 并发缓冲限制
	select {
	case g.sem <- struct{}{}:
		defer func() { <-g.sem }()
	case <-r.Context().Done():
		return
	}

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

	order := e.candidates(g, body.size)
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
	var (
		lastErr  error
		lastName string
	)
	for i := 0; i < maxAttempts; i++ {
		up := order[i]
		name := up.cfg.Name
		attemptStart := time.Now()
		resp, err, at := e.doAttempt(r, g, up, body)
		if err == nil {
			// 注意：不能在这里关闭缓冲副本。收到响应头不代表上传已经结束
			// （HTTP/2 双向流、服务端提前响应都会继续读请求体），提前关文件会
			// 把还在上传的请求截断。统一由 defer body.Close() 在流结束后释放。
			e.store.RecordSelected(groupName, name, i > 0 || name != primaryName)
			e.stream(w, r, resp, g, up, name, time.Since(attemptStart), start)
			return
		}

		// 只有“请求头一个字节都没写出去”才允许换线路：此时上游不可能收到请求，
		// 重发不会重复计费。判断依据来自 httptrace，不看错误类型或错误字符串。
		if !at.failoverSafe() {
			e.store.RecordFailure(groupName, name, at.reason(err), time.Since(start))
			e.events.Add(Event{
				Level: "error", Kind: "request", Group: groupName, Upstream: name,
				DurationMS: time.Since(start).Milliseconds(),
				Message:    at.reason(err),
			})
			e.log.Error("upstream request failed", "group", groupName, "upstream", name, "reason", at.reason(err), "err", err)
			writeProxyError(w, groupName, name, err)
			return
		}

		lastErr, lastName = err, name
		e.store.RecordFailure(groupName, name, at.reason(err), time.Since(start))
		if i+1 >= maxAttempts {
			break
		}
		e.store.RecordFailover(groupName, name)
		next := order[i+1].cfg.Name
		e.events.Add(Event{
			Level: "warn", Kind: "failover", Group: groupName, Upstream: name, From: name, To: next,
			DurationMS: time.Since(attemptStart).Milliseconds(),
			Message:    "连接未建立，切换线路: " + shorten(errString(err), 200),
		})
		e.log.Warn("failover", "group", groupName, "from", name, "to", next, "err", err)
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
	return err.Error()
}

// candidates 返回按优先级排序的线路：先可用线路，再接上不可用线路作为兜底。
// 运行时被停用的线路、以及请求体超过其 max_body_bytes 的线路直接排除。
func (e *Engine) candidates(g *groupRuntime, bodySize int64) []*upstreamRuntimeCfg {
	healthy := make([]*upstreamRuntimeCfg, 0, len(g.upstreams))
	unhealthy := make([]*upstreamRuntimeCfg, 0, len(g.upstreams))
	for _, u := range g.upstreams {
		if !u.cfg.IsEnabled() || !e.store.Enabled(g.name, u.cfg.Name) {
			continue
		}
		if u.cfg.MaxBodyBytes > 0 && bodySize > u.cfg.MaxBodyBytes {
			continue
		}
		if e.store.IsUsable(g.name, u.cfg.Name) {
			healthy = append(healthy, u)
		} else {
			unhealthy = append(unhealthy, u)
		}
	}
	return append(healthy, unhealthy...)
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
	resp, err := up.transport.RoundTrip(req)
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
func (e *Engine) stream(w http.ResponseWriter, orig *http.Request, resp *http.Response, g *groupRuntime, up *upstreamRuntimeCfg, upstream string, headerLatency time.Duration, start time.Time) {
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
		e.log.Debug("client flush failed", "group", g.name, "upstream", upstream, "err", err)
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
		e.log.Debug("client went away", "group", g.name, "upstream", upstream, "err", readErr, "bytes", total)
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
			"err", readErr, "bytes", total)
		return
	}

	// 一次尝试只提交一个终态，避免同一请求既算成功又算失败。
	switch {
	case idleFired.Load():
		e.store.RecordFailure(g.name, upstream, "stream idle timeout", time.Since(start))
		panic(http.ErrAbortHandler)
	case truncated:
		e.store.RecordFailure(g.name, upstream, "stream truncated: "+readErr.Error(), time.Since(start))
		e.events.Add(Event{Level: "error", Kind: "request", Group: g.name, Upstream: upstream,
			Status: resp.StatusCode, DurationMS: time.Since(start).Milliseconds(),
			Message: "响应流异常结束（可能被截断）: " + shorten(readErr.Error(), 200)})
		panic(http.ErrAbortHandler)
	case statusFailed:
		e.store.RecordFailure(g.name, upstream, fmt.Sprintf("HTTP %d 被该线路配置计为故障", resp.StatusCode), time.Since(start))
		e.events.Add(Event{Level: "warn", Kind: "health", Group: g.name, Upstream: upstream,
			Status: resp.StatusCode, DurationMS: time.Since(start).Milliseconds(),
			Message: fmt.Sprintf("按配置把 HTTP %d 计为线路故障", resp.StatusCode)})
	case resp.StatusCode >= 500:
		// 上游自身错误：计入失败数，但不改变线路健康状态。
		e.store.RecordHTTPError(g.name, upstream, time.Since(start))
		e.events.Add(Event{Level: "warn", Kind: "request", Group: g.name, Upstream: upstream,
			Status: resp.StatusCode, DurationMS: time.Since(start).Milliseconds(),
			Message: fmt.Sprintf("上游返回 %d", resp.StatusCode)})
	default:
		e.store.RecordSuccess(g.name, upstream, time.Since(start))
		e.events.Add(Event{Level: "info", Kind: "request", Group: g.name, Upstream: upstream,
			Status: resp.StatusCode, DurationMS: time.Since(start).Milliseconds(),
			Message: fmt.Sprintf("%s %s → %d", orig.Method, orig.URL.Path, resp.StatusCode)})
	}

	if headerLatency > 3*time.Second {
		e.log.Info("slow first byte", "group", g.name, "upstream", upstream, "header_ms", headerLatency.Milliseconds(), "bytes", total)
	}
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
