package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"time"
)

// ListenerHandler owns a protocol's serving loop, not the listening socket.
// A future TCP engine can implement this interface without changing the manager.
type ListenerHandler interface {
	Serve(net.Listener) error
	Update(ListenerHandler)
	Shutdown(context.Context) error
}

type ListenerSpec struct {
	Name    string
	Address string
	Handler ListenerHandler
}

type managedListener struct {
	spec    ListenerSpec
	handler ListenerHandler
	ln      net.Listener
}

// ListenerManager reserves new sockets before changing any active listener.
// Configuration transactions serialize Prepare/Commit; Close is called at shutdown.
type ListenerManager struct {
	mu     sync.Mutex
	active map[string]*managedListener
	closed bool
	log    *slog.Logger
}

func NewListenerManager(log *slog.Logger) *ListenerManager {
	return &ListenerManager{active: make(map[string]*managedListener), log: log}
}

type ListenerPlan struct {
	manager   *ListenerManager
	specs     []ListenerSpec
	opened    map[string]*managedListener
	committed bool
}

type ListenConflictError struct {
	Name, Address string
	Err           error
}

func (e *ListenConflictError) Error() string {
	return fmt.Sprintf("监听 %s (%s) 不可用，保留原配置: %v", e.Name, e.Address, e.Err)
}
func (e *ListenConflictError) Unwrap() error { return e.Err }

func (m *ListenerManager) Prepare(specs []ListenerSpec) (*ListenerPlan, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return nil, errors.New("监听管理器已关闭")
	}
	p := &ListenerPlan{manager: m, specs: specs, opened: make(map[string]*managedListener)}
	seen := make(map[string]bool)
	for _, s := range specs {
		if seen[s.Address] {
			p.Abort()
			return nil, fmt.Errorf("重复监听地址: %s", s.Address)
		}
		seen[s.Address] = true
		if _, ok := m.active[s.Address]; ok {
			continue
		}
		ln, err := net.Listen("tcp", s.Address)
		if err != nil {
			p.Abort()
			m.log.Warn("listener preflight failed; configuration unchanged", "group", s.Name, "err", err)
			return nil, &ListenConflictError{s.Name, s.Address, err}
		}
		p.opened[s.Address] = &managedListener{spec: s, handler: s.Handler, ln: ln}
	}
	return p, nil
}

func (p *ListenerPlan) Abort() {
	if p == nil || p.committed {
		return
	}
	for _, l := range p.opened {
		_ = l.ln.Close()
	}
}

// Commit cannot fail: every required socket and handler has already been built.
// Removed sockets close immediately; established requests drain asynchronously.
func (p *ListenerPlan) Commit() {
	m := p.manager
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		// 已经进入关闭流程：不再复活任何监听，已预开的 socket 一并回收。
		p.committed = true
		for _, l := range p.opened {
			_ = l.ln.Close()
		}
		return
	}
	next := make(map[string]*managedListener, len(p.specs))
	for _, s := range p.specs {
		// 复用已有监听时，handler 只可能来自 active；新开监听时才在 opened 里。
		// 若 Prepare 之后 manager 已被关闭（active 清空且 opened 未持有），这里
		// 跳过而不是对 nil 启动服务循环。
		l := m.active[s.Address]
		if l == nil {
			l = p.opened[s.Address]
		}
		if l == nil {
			continue
		}
		if existing := m.active[s.Address]; existing != nil {
			existing.handler.Update(s.Handler)
			existing.spec.Name = s.Name
			next[s.Address] = existing
			continue
		}
		next[s.Address] = l
		go m.serve(l)
	}
	for addr, l := range m.active {
		if next[addr] == nil {
			_ = l.ln.Close()
			go drainListener(l)
		}
	}
	m.active = next
	p.committed = true
}

func (m *ListenerManager) serve(l *managedListener) {
	m.log.Info("listening", "addr", l.ln.Addr().String())
	if err := l.handler.Serve(l.ln); err != nil && !errors.Is(err, http.ErrServerClosed) && !errors.Is(err, net.ErrClosed) {
		m.log.Error("listener stopped", "err", err)
	}
}

func drainListener(l *managedListener) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = l.handler.Shutdown(ctx)
}

func (m *ListenerManager) Close() {
	m.mu.Lock()
	old := m.active
	m.active = make(map[string]*managedListener)
	m.closed = true
	m.mu.Unlock()
	for _, l := range old {
		_ = l.ln.Close()
		drainListener(l)
	}
}

// HTTPListener is the HTTP protocol adapter. Target changes also affect keep-alive requests.
type httpDispatch struct {
	handler http.Handler
	admin   bool
}

type HTTPListener struct {
	target atomic.Pointer[httpDispatch]
	server *http.Server
}

func NewHTTPListener(handler http.Handler, admin bool, gate *sync.RWMutex) *HTTPListener {
	h := &HTTPListener{}
	h.target.Store(&httpDispatch{handler: handler, admin: admin})
	h.server = &http.Server{ReadHeaderTimeout: 30 * time.Second, IdleTimeout: 120 * time.Second}
	h.server.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Capture the dispatch target outside the long-lived proxy request.
		gate.RLock()
		target := h.target.Load()
		gate.RUnlock()
		if target.admin {
			rc := http.NewResponseController(w)
			_ = rc.SetWriteDeadline(time.Now().Add(30 * time.Second))
			defer rc.SetWriteDeadline(time.Time{})
		}
		target.handler.ServeHTTP(w, r)
	})
	return h
}
func (h *HTTPListener) Serve(ln net.Listener) error { return h.server.Serve(ln) }
func (h *HTTPListener) Update(next ListenerHandler) {
	h.target.Store(next.(*HTTPListener).target.Load())
}
func (h *HTTPListener) Shutdown(ctx context.Context) error {
	if err := h.server.Shutdown(ctx); err != nil {
		_ = h.server.Close()
		return err
	}
	return nil
}
