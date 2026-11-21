package app

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestConcurrentStatusToggleAndConfigPublication(t *testing.T) {
	m, a, _ := managerFixture(t)
	var wg sync.WaitGroup
	errors := make(chan string, 8)
	for worker := 0; worker < 4; worker++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 30; i++ {
				status := adminRequest(a, "GET", "/__linkgate/v1/status", "")
				if status.Code != 200 || !strings.Contains(status.Body.String(), `"name":"primary"`) {
					errors <- status.Body.String()
					return
				}
				w := adminRequest(a, "POST", "/__linkgate/v1/toggle?group=test&upstream=primary&enabled=true", "")
				if w.Code != 200 {
					errors <- w.Body.String()
					return
				}
			}
		}()
	}
	for i := 0; i < 30; i++ {
		w := adminRequest(a, "PUT", "/__linkgate/v1/groups", fmt.Sprintf(`{"name":"test","comment":"revision-%d"}`, i))
		if w.Code != 200 {
			t.Fatal(w.Body.String())
		}
	}
	wg.Wait()
	close(errors)
	for err := range errors {
		t.Error(err)
	}
	if m.admin.cfg != m.cfg {
		t.Fatal("admin not synchronized")
	}
}

func TestAdminListenMoveAndImmutableStartupFields(t *testing.T) {
	m, _, cfg := managerFixture(t)
	old := cfg.Admin.Listen
	next := freeAddress(t)
	if err := m.Mutate(func(c *Config) error { c.Admin.Listen = next; return nil }); err != nil {
		t.Fatal(err)
	}
	assertPort(t, old, false)
	assertPort(t, next, true)
	resp, err := http.Get("http://" + next + "/__linkgate/v1/health")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	before, _ := os.ReadFile(m.path)
	if err := m.Mutate(func(c *Config) error { c.Log.MaxBackups = 7; return nil }); err == nil || apiErrorStatus(err) != 409 {
		t.Fatalf("startup-only edit %v", err)
	}
	after, _ := os.ReadFile(m.path)
	if string(before) != string(after) {
		t.Fatal("rejected startup field changed disk")
	}
}

func TestFinalRenameFailureRestoresExistingBackup(t *testing.T) {
	dir := t.TempDir()
	// Destination directory forces failure at the final config rename, after .bak staging.
	path := filepath.Join(dir, "config.yaml")
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path+".bak", []byte("original backup"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := saveConfigFile(path, []byte("new"), []byte("old")); err == nil {
		t.Fatal("rename should fail")
	}
	b, err := os.ReadFile(path + ".bak")
	if err != nil || string(b) != "original backup" {
		t.Fatalf("backup rollback %s %v", b, err)
	}
	entries, _ := os.ReadDir(dir)
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".linkgate-config-") {
			t.Fatal("temporary file leaked")
		}
	}
}

func TestListenAliasesAndWildcardOverlap(t *testing.T) {
	for _, alias := range []string{"localhost:18765", ":18765", "0.0.0.0:18765", "[::]:18765"} {
		cfg := testConfig(UpstreamConfig{Name: "primary", URL: "http://example.invalid"})
		cfg.Admin.Disabled = true
		cfg.Groups = append(cfg.Groups, GroupConfig{Name: "other", Listen: alias})
		if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "冲突") {
			t.Fatalf("alias %s: %v", alias, err)
		}
	}
}
