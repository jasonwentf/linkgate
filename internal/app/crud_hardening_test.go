package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func cloneConfig(t *testing.T, cfg *Config) *Config {
	t.Helper()
	raw, err := cfg.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	next, err := Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return next
}

func getBody(t *testing.T, addr, path string) string {
	t.Helper()
	resp, err := http.Get("http://" + addr + path)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s%s = %d %s", addr, path, resp.StatusCode, b)
	}
	return string(b)
}

// Bug 1：encoding/json 的大小写/Unicode 折叠匹配不能成为绕过入口。
func TestCRUDRejectsAliasedAndDuplicateKeys(t *testing.T) {
	m, a, _ := managerFixture(t)
	groupPath := "/__linkgate/v1/groups"
	upPath := groupPath + "/test/upstreams"
	for _, body := range []string{
		`{"name":"test","Name":"renamed"}`, // 大小写变体：typed decoder 会折叠到 name
		`{"name":"test","upstreamſ":[]}`,   // U+017F 长 s：Go 折叠为 s
		`{"name":"test","name":"other"}`,   // 重复键
		`{"name":"test","upstreamS":[]}`,   // 大小写变体
		`{"name":"test","Upstreams":[]}`,   // 首字母大写
	} {
		expectCode(t, adminRequest(a, http.MethodPut, groupPath, body), http.StatusBadRequest)
	}
	g := m.cfg.Group("test")
	if g == nil || g.Name != "test" || len(g.Upstreams) != 1 || g.Upstreams[0].Name != "primary" {
		t.Fatalf("rejected request still mutated group: %+v", g)
	}
	expectCode(t, adminRequest(a, http.MethodPut, upPath, `{"name":"primary","URL":"http://evil.invalid"}`), http.StatusBadRequest)
	expectCode(t, adminRequest(a, http.MethodPut, upPath, `{"name":"primary","url":"http://example.invalid/new"}`), http.StatusOK)
	if got := findUpstream(m.cfg.Group("test"), "primary").URL; got != "http://example.invalid/new" {
		t.Fatalf("legit update lost: url = %q", got)
	}
	expectCode(t, adminRequest(a, http.MethodDelete, groupPath, `{"Name":"test"}`), http.StatusBadRequest)
	expectCode(t, adminRequest(a, http.MethodDelete, upPath, `{"name":"primary","name":"primary"}`), http.StatusBadRequest)
}

// Bug 6：会破坏路径段的组名必须在写入前拒绝，否则 CRUD 接口永远访问不到。
func TestGroupNameRejectsPathBreaking(t *testing.T) {
	m, a, _ := managerFixture(t)
	for _, name := range []string{".", "..", "a/b", `a\b`, "a#b", "a?b", " a", "a ", "a\x01b"} {
		body, err := json.Marshal(map[string]string{"name": name, "listen": freeAddress(t)})
		if err != nil {
			t.Fatal(err)
		}
		expectCode(t, adminRequest(a, http.MethodPost, "/__linkgate/v1/groups", string(body)), http.StatusBadRequest)
		if m.cfg.Group(name) != nil {
			t.Fatalf("group %q created despite invalid name", name)
		}
	}
	validBody, _ := json.Marshal(map[string]string{"name": "a.b", "listen": freeAddress(t)})
	expectCode(t, adminRequest(a, http.MethodPost, "/__linkgate/v1/groups", string(validBody)), http.StatusCreated)
}

// Bug 5：错误文案里的 URL 可能带 userinfo 或 query secret，必须脱敏。
func TestURLErrorTextIsRedacted(t *testing.T) {
	cfg := testConfig(UpstreamConfig{Name: "primary", URL: "http://user:s3cret@[::1/path?token=leak"})
	err := cfg.Validate()
	if err == nil {
		t.Fatal("malformed URL accepted")
	}
	msg := err.Error()
	for _, secret := range []string{"s3cret", "leak", "user:"} {
		if strings.Contains(msg, secret) {
			t.Fatalf("validation error leaked %q: %s", secret, msg)
		}
	}
	uerr := &url.Error{Op: "Get", URL: "https://user:pw@example.invalid/api?token=abc", Err: errors.New("boom")}
	got := errString(uerr)
	for _, secret := range []string{"pw@", "token=abc", "user:"} {
		if strings.Contains(got, secret) {
			t.Fatalf("errString leaked %q: %s", secret, got)
		}
	}
	if !strings.Contains(got, "https://example.invalid") {
		t.Fatalf("host dropped from redacted error: %s", got)
	}
}

// Bug 7：监听地址冲突按实际地址族归一化比较，且与系统占用同为 409。
func TestListenConflictNormalization(t *testing.T) {
	cfg := testConfig(UpstreamConfig{Name: "primary", URL: "http://upstream.invalid"})
	cfg.Admin.Listen = "127.0.0.1:18777"
	cfg.Groups[0].Listen = "localhost:18777"
	err := cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), "冲突") {
		t.Fatalf("localhost/127.0.0.1 collision not detected: %v", err)
	}
	if status := validationStatus(err); status != http.StatusConflict {
		t.Fatalf("conflict status = %d, want 409", status)
	}
	if !strings.Contains(err.Error(), "回环") {
		t.Fatalf("loopback alias conflict message is not explicit: %v", err)
	}
	v6Alias := testConfig(UpstreamConfig{Name: "primary", URL: "http://upstream.invalid"})
	v6Alias.Admin.Listen = "localhost:18780"
	v6Alias.Groups[0].Listen = "[::1]:18780"
	if err := v6Alias.Validate(); err == nil || !strings.Contains(err.Error(), "回环") {
		t.Fatalf("localhost/::1 alias collision not explained: %v", err)
	}

	v4Wild := testConfig(UpstreamConfig{Name: "primary", URL: "http://upstream.invalid"})
	v4Wild.Admin.Listen = "0.0.0.0:18778"
	v4Wild.Groups[0].Listen = "[::1]:18778"
	if err := v4Wild.Validate(); err != nil {
		t.Fatalf("IPv4 wildcard and IPv6 loopback wrongly conflict: %v", err)
	}

	covered := testConfig(UpstreamConfig{Name: "primary", URL: "http://upstream.invalid"})
	covered.Admin.Listen = "0.0.0.0:18779"
	covered.Groups[0].Listen = "127.0.0.1:18779"
	if err := covered.Validate(); err == nil {
		t.Fatal("IPv4 wildcard must cover IPv4 loopback")
	}
}

// Bug 2：旧请求在新配置提交后才失败，失败归因不能落到改名/改址后的同名新线路上。
func TestStaleRequestFailureNotAttributedToNewLine(t *testing.T) {
	m, a, _ := managerFixture(t)
	entered := make(chan struct{})
	release := make(chan struct{})
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		<-release
		w.WriteHeader(http.StatusServiceUnavailable)
		io.WriteString(w, "old upstream down")
	}))
	defer slow.Close()
	var healthyHits atomic.Int32
	healthy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		healthyHits.Add(1)
		io.WriteString(w, "new")
	}))
	defer healthy.Close()

	if err := m.Mutate(func(c *Config) error {
		u := findUpstream(c.Group("test"), "primary")
		u.URL = slow.URL
		u.FailureStatuses = []int{http.StatusServiceUnavailable}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	h := m.engine.Handler("test") // 绑定提交前的那一代
	rec := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "http://group.test/", nil))
		close(done)
	}()
	waitSignal(t, entered)
	expectCode(t, adminRequest(a, http.MethodPut, "/__linkgate/v1/groups/test/upstreams",
		fmt.Sprintf(`{"name":"primary","url":%q}`, healthy.URL)), http.StatusOK)
	close(release)
	waitSignal(t, done)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("old request status = %d, want 503 from its own generation", rec.Code)
	}
	if healthyHits.Load() != 0 {
		t.Fatalf("old request reached new upstream %d times", healthyHits.Load())
	}
	state := testState(t, m.store, m.cfg, "primary")
	if state.RequestsFailed != 0 || state.ConsecutiveFailures != 0 || state.Status == StatusUnhealthy {
		t.Fatalf("stale failure polluted the new line: %+v", state)
	}
}

// Bug 3：请求体缓冲期间线路集被替换，旧请求仍应使用自己那代的线路，而不是空候选 503。
func TestConfigChangeDuringBufferingKeepsRequestOnItsLine(t *testing.T) {
	m, _, _ := managerFixture(t)
	serverA := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		io.WriteString(w, "A")
	}))
	defer serverA.Close()
	serverB := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		io.WriteString(w, "B")
	}))
	defer serverB.Close()

	if err := m.Mutate(func(c *Config) error {
		findUpstream(c.Group("test"), "primary").URL = serverA.URL
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	h := m.engine.Handler("test") // 绑定旧一代线路集
	pr, pw := io.Pipe()
	rec := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "http://group.test/v1/chat", pr))
		close(done)
	}()
	if _, err := pw.Write([]byte(`{"slow":true}`)); err != nil {
		t.Fatal(err)
	}

	// 缓冲尚未结束就整体替换线路（旧名字消失）。
	if err := m.Mutate(func(c *Config) error {
		c.Groups[0].Upstreams = []UpstreamConfig{{Name: "replacement", URL: serverB.URL}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	pw.Close()
	waitSignal(t, done)

	if rec.Code != http.StatusOK || rec.Body.String() != "A" {
		t.Fatalf("request bound to old generation got %d %q, want 200 A", rec.Code, rec.Body.String())
	}
}

// Bug 4：两个线路组互换监听地址后，每个地址必须服务它当前绑定的那一组。
func TestGroupAddressSwapKeepsDispatchBound(t *testing.T) {
	m, _, cfg := managerFixture(t)
	addr1 := cfg.Groups[0].Listen
	addr2 := freeAddress(t)
	serverA := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "A") }))
	defer serverA.Close()
	serverB := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "B") }))
	defer serverB.Close()

	if err := m.Mutate(func(c *Config) error {
		c.Groups[0].Upstreams[0].URL = serverA.URL
		c.Groups = append(c.Groups, GroupConfig{Name: "second", Listen: addr2, Upstreams: []UpstreamConfig{{Name: "up", URL: serverB.URL}}})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := m.Mutate(func(c *Config) error {
		c.Groups[0].Listen, c.Groups[1].Listen = addr2, addr1
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if got := getBody(t, addr1, "/"); got != "B" {
		t.Fatalf("addr1 serves %q, want B (second group)", got)
	}
	if got := getBody(t, addr2, "/"); got != "A" {
		t.Fatalf("addr2 serves %q, want A (first group)", got)
	}
}

// Bug 4（窄窗口）：dispatch 已捕获但配置随后提交时，旧 handler 必须服务自己
// 绑定的那一代，而不是按组名读到新映射（组被改名后旧名字不复存在）。
func TestPreSwapHandlerKeepsItsOwnGenerationAcrossRename(t *testing.T) {
	m, _, _ := managerFixture(t)
	serverA := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "A")
	}))
	defer serverA.Close()
	if err := m.Mutate(func(c *Config) error {
		findUpstream(c.Group("test"), "primary").URL = serverA.URL
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	h := m.engine.Handler("test") // 旧 dispatch，先于提交被捕获
	next := cloneConfig(t, m.cfg)
	next.Groups[0].Name = "renamed"
	raw, err := next.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Save(raw); err != nil {
		t.Fatal(err)
	}
	if m.cfg.Group("test") != nil || m.cfg.Group("renamed") == nil {
		t.Fatalf("rename did not apply: %v", m.cfg.SortedGroups())
	}

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "http://group.test/", nil))
	if rec.Code != http.StatusOK || rec.Body.String() != "A" {
		t.Fatalf("stale dispatch got %d %q, want 200 A", rec.Code, rec.Body.String())
	}
}

// Bug 8：Close 与写事务共用 m.mu 并置 closed，关闭后排队的事务必须被拒绝。
func TestManagerCloseRejectsWrites(t *testing.T) {
	m, _, _ := managerFixture(t)
	m.Close()
	err := m.Mutate(func(c *Config) error { return nil })
	var api *APIError
	if !errors.As(err, &api) || api.Status != http.StatusServiceUnavailable {
		t.Fatalf("mutate after close = %v, want 503", err)
	}
	if err := m.Save([]byte("version: 1")); err == nil {
		t.Fatal("save after close accepted")
	}
	if err := m.Reload(); err == nil {
		t.Fatal("reload after close accepted")
	}
	m.Close() // 幂等
}

type stubListenerHandler struct{}

func (*stubListenerHandler) Serve(ln net.Listener) error {
	for {
		c, err := ln.Accept()
		if err != nil {
			return err
		}
		c.Close()
	}
}
func (*stubListenerHandler) Update(ListenerHandler)         {}
func (*stubListenerHandler) Shutdown(context.Context) error { return nil }

// Bug 8：Prepare 之后 Close，再 Commit 不能对 nil 监听起服务循环。
func TestListenerCommitAfterCloseDoesNotPanic(t *testing.T) {
	lm := NewListenerManager(slog.New(slog.NewTextHandler(io.Discard, nil)))
	spec := []ListenerSpec{{Name: "svc", Address: freeAddress(t), Handler: &stubListenerHandler{}}}
	first, err := lm.Prepare(spec)
	if err != nil {
		t.Fatal(err)
	}
	first.Commit()
	next, err := lm.Prepare(spec)
	if err != nil {
		t.Fatal(err)
	}
	lm.Close()
	next.Commit()
	time.Sleep(50 * time.Millisecond)
	lm.mu.Lock()
	remaining := len(lm.active)
	lm.mu.Unlock()
	if remaining != 0 {
		t.Fatalf("commit after close resurrected %d listeners", remaining)
	}
}

// “同一条线路”的判定：只有 URL/Host/TLS/headers 变了才算新身份。
func TestUpstreamIdentityGovernsStateReuse(t *testing.T) {
	th := Thresholds{FailThreshold: 1, RecoverThreshold: 1}
	cfg := testConfig(UpstreamConfig{Name: "primary", URL: "http://a.invalid"})
	store := NewStore(cfg, th)
	store.RecordFailure(store.Config(), "test", "primary", "dial failed", 0)
	if got := testState(t, store, cfg, "primary"); got.RequestsFailed != 1 || got.Status != StatusUnhealthy {
		t.Fatalf("setup state = %+v", got)
	}

	nonSubstantive := cloneConfig(t, cfg)
	comment := findUpstream(nonSubstantive.Group("test"), "primary")
	comment.Comment = "只改备注"
	comment.Priority = intPtr(99)
	store.Load(nonSubstantive, th, store)
	if got := testState(t, store, nonSubstantive, "primary"); got.RequestsFailed != 1 || got.Status != StatusUnhealthy {
		t.Fatalf("non-substantive change dropped state: %+v", got)
	}

	for _, mutate := range []func(*UpstreamConfig){
		func(u *UpstreamConfig) { u.URL = "http://b.invalid" },
		func(u *UpstreamConfig) { u.Host = "other.invalid" },
		func(u *UpstreamConfig) { u.Headers = map[string]string{"X-Route": "new"} },
		func(u *UpstreamConfig) { u.TLSInsecureSkipVerify = true },
	} {
		store.Load(nonSubstantive, th, store)
		store.RecordFailure(store.Config(), "test", "primary", "dial failed", 0)
		next := cloneConfig(t, nonSubstantive)
		mutate(findUpstream(next.Group("test"), "primary"))
		store.Load(next, th, store)
		if got := testState(t, store, next, "primary"); got.RequestsFailed != 0 || got.ConsecutiveFailures != 0 || got.Status != StatusUnknown {
			t.Fatalf("endpoint change inherited old health: %+v", got)
		}
	}
}

// Bug 7：配置内的监听冲突与系统端口占用对外统一为 409。
func TestCRUDDuplicateListenReturnsConflict(t *testing.T) {
	m, a, _ := managerFixture(t)
	body := fmt.Sprintf(`{"name":"dup","listen":%q}`, m.cfg.Groups[0].Listen)
	expectCode(t, adminRequest(a, http.MethodPost, "/__linkgate/v1/groups", body), http.StatusConflict)
}

// 修2：脱敏必须覆盖非 http scheme、协议相对形式，以及秘密里含引号的情况。
func TestURLRedactionVariants(t *testing.T) {
	for _, tc := range []struct{ in, secret string }{
		{"ftp://user:pass@host:bad", "pass"},
		{"//user:pass@host/path", "pass"},
		{`http://user:p'ass@host/x`, "p'ass"},
		{"http://user:p`ass@host", "p`ass"},
		{"socks5://user:pass@host:1080", "pass"},
		{`Get "https://user:pw@host/api?token=abc": boom`, "token=abc"},
	} {
		got := redactURLText("upstream failed: " + tc.in)
		if strings.Contains(got, tc.secret) {
			t.Fatalf("redactURLText(%q) = %q leaks %q", tc.in, got, tc.secret)
		}
		if strings.Contains(got, "user:") {
			t.Fatalf("redactURLText(%q) = %q keeps userinfo", tc.in, got)
		}
	}
	if got := redactURLText("dial tcp 127.0.0.1:80: connect: connection refused"); !strings.Contains(got, "127.0.0.1:80") {
		t.Fatalf("non-URL text over-redacted: %q", got)
	}
}

// 修3：嵌套已知对象（timeout_overrides / probe / upstreams 数组）同样严格；
// headers 是用户自定义 map，键不受限制。
func TestNestedObjectKeysAreStrict(t *testing.T) {
	m, a, _ := managerFixture(t)
	groupPath := "/__linkgate/v1/groups"
	upPath := groupPath + "/test/upstreams"
	for _, body := range []string{
		`{"name":"test","timeout_overrides":{"Response_Header_Timeout":"1s"}}`,
		`{"name":"test","timeout_overrides":{"response_header_timeout":"1s","response_header_timeout":"2s"}}`,
		`{"name":"test","timeout_overrides":{"typo":"1s"}}`,
	} {
		expectCode(t, adminRequest(a, http.MethodPut, groupPath, body), http.StatusBadRequest)
	}
	for _, body := range []string{
		`{"name":"primary","probe":{"Path":"/health"}}`,
		`{"name":"primary","probe":{"type":"http","type":"http"}}`,
	} {
		expectCode(t, adminRequest(a, http.MethodPut, upPath, body), http.StatusBadRequest)
	}
	// 合法嵌套与自定义 headers 仍然可用。
	expectCode(t, adminRequest(a, http.MethodPut, upPath, `{"name":"primary","probe":{"type":"http","path":"/health"},"headers":{"X-Custom":"ok"}}`), http.StatusOK)
	if got := findUpstream(m.cfg.Group("test"), "primary"); got.Probe.Path != "/health" || got.Headers["X-Custom"] != "ok" {
		t.Fatalf("legit nested update lost: %+v", got)
	}
	// POST groups 的 upstreams 数组也递归校验。
	body := fmt.Sprintf(`{"name":"g2","listen":%q,"upstreams":[{"name":"u","url":"http://x.invalid","Priority":1}]}`, freeAddress(t))
	expectCode(t, adminRequest(a, http.MethodPost, groupPath, body), http.StatusBadRequest)
	okBody := fmt.Sprintf(`{"name":"full","listen":%q,"upstreams":[{"name":"u","url":"http://x.invalid","probe":{"type":"http","path":"/health"}}]}`, freeAddress(t))
	expectCode(t, adminRequest(a, http.MethodPost, groupPath, okBody), http.StatusCreated)
}

// 修1：配置换代后的回退只能使用入站时冻结的可用线路，不能复活已停用的线路。
func TestGenerationFallbackDoesNotReviveDisabledLine(t *testing.T) {
	m, a, _ := managerFixture(t)
	var hits atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		io.WriteString(w, "old")
	}))
	defer up.Close()
	if err := m.Mutate(func(c *Config) error {
		findUpstream(c.Group("test"), "primary").URL = up.URL
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	expectCode(t, adminRequest(a, http.MethodPost, "/__linkgate/v1/toggle?group=test&upstream=primary&enabled=false", ""), http.StatusOK)
	if m.store.Enabled("test", "primary") {
		t.Fatal("toggle did not disable the line")
	}

	h := m.engine.Handler("test")
	pr, pw := io.Pipe()
	rec := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "http://group.test/v1/chat", pr))
		close(done)
	}()
	// Write 返回说明 serveGroup 已经读到请求体，入站快照（含 disabled）已完成。
	if _, err := pw.Write([]byte(`{"hello":1}`)); err != nil {
		t.Fatal(err)
	}
	// 换代：配置里 primary 仍是 enabled，但旧请求必须遵守入站时的运行时停用。
	if err := m.Mutate(func(c *Config) error {
		c.Groups[0].Upstreams = []UpstreamConfig{{Name: "replacement", URL: "http://replacement.invalid"}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	pw.Close()
	waitSignal(t, done)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d %q, want 503 (no usable line at entry)", rec.Code, rec.Body.String())
	}
	if hits.Load() != 0 {
		t.Fatalf("disabled line was revived after generation change: hits=%d", hits.Load())
	}
}

// 组级 add_headers 会合并进真实请求，必须参与线路身份：
// 改组级 Authorization 视为新线路；只改 comment 不重置；线路级同名头覆盖组级，
// 改组级被遮挡的值不改变最终生效请求头，身份不变。
func TestGroupHeadersParticipateInUpstreamIdentity(t *testing.T) {
	th := Thresholds{FailThreshold: 1, RecoverThreshold: 1}
	markDown := func(store *Store) {
		store.RecordFailure(store.Config(), "test", "primary", "dial failed", 0)
	}
	base := testConfig(UpstreamConfig{Name: "primary", URL: "http://a.invalid"})
	store := NewStore(base, th)
	markDown(store)

	// A：组级请求头变化 → 新身份，不继承旧状态。
	groupAuth := cloneConfig(t, base)
	groupAuth.Groups[0].AddHeaders = map[string]string{"Authorization": "Bearer a"}
	store.Load(groupAuth, th, store)
	if got := testState(t, store, groupAuth, "primary"); got.RequestsFailed != 0 || got.Status != StatusUnknown {
		t.Fatalf("group add_headers change kept old state: %+v", got)
	}

	// B：只改 comment → 同一身份，状态保留。
	markDown(store)
	commentOnly := cloneConfig(t, groupAuth)
	commentOnly.Groups[0].Comment = "note"
	store.Load(commentOnly, th, store)
	if got := testState(t, store, commentOnly, "primary"); got.RequestsFailed != 1 || got.Status != StatusUnhealthy {
		t.Fatalf("comment change reset identity: %+v", got)
	}

	// C：线路级同名头覆盖组级（大小写不敏感）→ 改组级被遮挡的值不影响身份。
	shadow := cloneConfig(t, base)
	shadow.Groups[0].AddHeaders = map[string]string{"authorization": "group-old"}
	findUpstream(shadow.Group("test"), "primary").Headers = map[string]string{"Authorization": "line-wins"}
	store.Load(shadow, th, store)
	markDown(store)
	shadow2 := cloneConfig(t, shadow)
	shadow2.Groups[0].AddHeaders = map[string]string{"authorization": "group-new"}
	store.Load(shadow2, th, store)
	if got := testState(t, store, shadow2, "primary"); got.RequestsFailed != 1 || got.Status != StatusUnhealthy {
		t.Fatalf("shadowed group header change reset identity: %+v", got)
	}
}

// 【A】dispatch 与 freeze 之间发生提交：freeze 读的是 groupRuntime 自带的
// 状态视图，不受提交次数影响。这里用连续两次 Mutate 把“存几代快照”的边界
// 打穿：任何按代缓存（只留上一代）的实现都会在这里复活已停用线路。
func TestFreezeUsesPreCommitRuntimeState(t *testing.T) {
	m, a, _ := managerFixture(t)
	var hits atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		io.WriteString(w, "old")
	}))
	defer up.Close()
	if err := m.Mutate(func(c *Config) error {
		findUpstream(c.Group("test"), "primary").URL = up.URL
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	// 模拟 dispatch 已选中这一代、但 handler 还没执行（窗口期）。
	h := m.engine.Handler("test")
	expectCode(t, adminRequest(a, http.MethodPost, "/__linkgate/v1/toggle?group=test&upstream=primary&enabled=false", ""), http.StatusOK)
	// 连续两次提交换代；运行时 toggle 不写盘，换代后配置里 primary 仍是 enabled。
	for _, url := range []string{"http://replacement-1.invalid", "http://replacement-2.invalid"} {
		target := url
		if err := m.Mutate(func(c *Config) error {
			findUpstream(c.Group("test"), "primary").URL = target
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	if !m.store.Enabled("test", "primary") {
		t.Fatal("setup: new generation should be enabled from config")
	}

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "http://group.test/", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d %q, want 503 (line was disabled in request generation)", rec.Code, rec.Body.String())
	}
	if hits.Load() != 0 {
		t.Fatalf("pre-freeze generation change revived a disabled line: hits=%d", hits.Load())
	}
}

// 【B】typed decode 的报错会回显字段原值，必须同样脱敏。
func TestDecodeErrorTextIsRedacted(t *testing.T) {
	_, a, _ := managerFixture(t)
	for _, body := range []string{
		`{"name":"test","timeout_overrides":{"connect_timeout":"http://user:supersecret@host:bad"}}`,
		`{"name":"test","http://user:pw2@host":"x"}`,
		`{"name":"test","timeout_overrides":{"http://user:pw3@host":"1s"}}`,
	} {
		w := adminRequest(a, http.MethodPut, "/__linkgate/v1/groups", body)
		expectCode(t, w, http.StatusBadRequest)
		out := w.Body.String()
		for _, secret := range []string{"supersecret", "pw2", "pw3", "user:"} {
			if strings.Contains(out, secret) {
				t.Fatalf("decode error leaked %q: %s", secret, out)
			}
		}
	}
}

// Oracle 终验：入站冻结为可用 → 运行时停用 → 单次提交换代 →
// candidates 必须继续按入站快照保留候选，不能被旧状态表的 enabled 二次过滤成 0。
func TestFrozenEligibilitySurvivesCommit(t *testing.T) {
	m, _, _ := managerFixture(t)
	g := m.engine.group("test")
	frozen := m.engine.freezeLines(g)
	if !frozen["primary"].usable || !frozen["primary"].enabled {
		t.Fatalf("setup expected usable entry snapshot: %+v", frozen["primary"])
	}
	m.store.SetEnabled("test", "primary", false)
	if err := m.Mutate(func(c *Config) error {
		c.Groups[0].Comment = "next"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	order := m.engine.candidates(g, frozen, 0, "")
	if len(order) != 1 || order[0].cfg.Name != "primary" {
		t.Fatalf("after commit candidates ignored frozen entry eligibility: %+v", order)
	}
}
