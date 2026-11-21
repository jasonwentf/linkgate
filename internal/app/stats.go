package app

import (
	"sort"
	"strings"
	"time"
)

// Service counters count client requests, not individual upstream attempts. They
// live in the generation table: clone on commit, never write an old generation.
type serviceRuntime struct {
	Total, Success, Failed, Failover, Canceled uint64
	LastRequestAt                              time.Time
	UALines                                    map[string]map[string]UALine
}

type UALine struct {
	UA     string    `json:"ua"`
	Line   string    `json:"line"`
	Count  uint64    `json:"count"`
	Failed uint64    `json:"failed"`
	LastAt time.Time `json:"last_at"`
}

type ServiceStats struct {
	Name          string         `json:"name"`
	Listen        string         `json:"listen"`
	Active        string         `json:"active"`
	Health        string         `json:"health"`
	Total         uint64         `json:"total"`
	Success       uint64         `json:"success"`
	Failed        uint64         `json:"failed"`
	Failover      uint64         `json:"failover"`
	Canceled      uint64         `json:"canceled"`
	LastRequestAt time.Time      `json:"last_request_at"`
	UALines       []UALine       `json:"ua_lines"`
	UARules       []UARuleConfig `json:"ua_rules"`
}

func cloneServiceRuntime(old *stateTable, g *GroupConfig) *serviceRuntime {
	r := &serviceRuntime{UALines: make(map[string]map[string]UALine)}
	if old == nil || old.services[g.Name] == nil {
		return r
	}
	prev := old.services[g.Name]
	r.Total, r.Success, r.Failed, r.Failover, r.LastRequestAt = prev.Total, prev.Success, prev.Failed, prev.Failover, prev.LastRequestAt
	r.Canceled = prev.Canceled
	for ua, lines := range prev.UALines {
		copied := make(map[string]UALine)
		for name, cell := range lines {
			// Removed lines must not grow the in-memory matrix indefinitely.
			if name == "" {
				copied[name] = cell
				continue
			}
			if next := findUpstream(g, name); next != nil {
				if previous := old.line(g.Name, name); previous != nil && previous.identity == upstreamIdentity(g, *next) {
					copied[name] = cell
				}
			}
		}
		r.UALines[ua] = copied
	}
	return r
}

const (
	// uaUnknown is the missing/blank UA bucket, kept distinct from "other" so a
	// client that sends no UA is not confused with an unrecognized one.
	uaUnknown = "unknown"
	// uaOther is the bounded overflow bucket for every UA not in the table.
	uaOther = "other"
)

// uaClassRule is one row of the caller-normalization table.
//
// A UA matches when its first token (text before the first "/" or space) equals
// one of Tokens, or when the lowercased UA contains one of Keywords. Rules are
// evaluated top to bottom, so specific fingerprints (Edge, Chrome) must come
// before generic ones (Safari) and rich UAs can fall back to Keywords.
//
// To support a new caller, add a row here. This table is the cardinality cap:
// anything not listed collapses into uaOther, so raw UA strings never create new
// buckets and are never stored (counters key by class only).
type uaClassRule struct {
	Class    string
	Tokens   []string
	Keywords []string
}

var uaClassRules = []uaClassRule{
	// AI harnesses / coding CLIs. Tokens cover the common first-token form;
	// keywords catch clients that prepend "Mozilla/5.0" or similar.
	{Class: "zcode", Tokens: []string{"zcode", "zcode-cli"}, Keywords: []string{"zcode"}},
	{Class: "dsh", Tokens: []string{"deepseek-harness", "dsh"}, Keywords: []string{"deepseek-harness"}},
	{Class: "claude-code", Tokens: []string{"claude-code", "claude-cli", "claude"}, Keywords: []string{"claude-code"}},
	{Class: "codex", Tokens: []string{"codex", "codex-cli", "openai-codex"}, Keywords: []string{"codex"}},
	{Class: "opencode", Tokens: []string{"opencode", "open-code"}, Keywords: []string{"opencode"}},
	{Class: "kiro", Tokens: []string{"kiro"}, Keywords: []string{"kiro"}},
	{Class: "devin", Tokens: []string{"devin"}, Keywords: []string{"devin"}},
	{Class: "kelivo", Tokens: []string{"kelivo"}, Keywords: []string{"kelivo"}},
	// SillyTavern speaks through node-fetch; keep both names in one bucket.
	{Class: "sillytavern", Tokens: []string{"node-fetch", "sillytavern"}, Keywords: []string{"sillytavern"}},
	// Browsers. Fingerprints are substring-based, most specific first: Edge and
	// Chrome UAs also contain "Safari/", so Safari must stay last.
	{Class: "edge", Keywords: []string{"edg/", "edge/", "edga/", "edgios/"}},
	{Class: "chrome", Keywords: []string{"chrome/", "crios/", "chromium/"}},
	{Class: "firefox", Keywords: []string{"firefox/", "fxios/"}},
	{Class: "safari", Keywords: []string{"safari/"}},
	// HTTP libraries and CLI fetchers.
	{Class: "curl", Tokens: []string{"curl"}},
	{Class: "wget", Tokens: []string{"wget"}},
	{Class: "python-requests", Tokens: []string{"python-requests"}},
	{Class: "python", Tokens: []string{"python", "python-httpx", "python-aiohttp", "python-urllib", "python-urllib3"}},
	{Class: "go-http-client", Tokens: []string{"go-http-client"}},
	{Class: "okhttp", Tokens: []string{"okhttp"}},
}

// uaClass maps a raw User-Agent to a stable short name. Blank UA is "unknown";
// anything outside uaClassRules is "other", which bounds cardinality and keeps
// raw UA strings out of memory.
func uaClass(raw string) string {
	ua := strings.ToLower(strings.TrimSpace(raw))
	if ua == "" {
		return uaUnknown
	}
	first := ua
	if i := strings.IndexAny(first, " \t"); i >= 0 {
		first = first[:i]
	}
	if i := strings.IndexByte(first, '/'); i >= 0 {
		first = first[:i]
	}
	for _, rule := range uaClassRules {
		for _, token := range rule.Tokens {
			if first == token {
				return rule.Class
			}
		}
		for _, keyword := range rule.Keywords {
			if strings.Contains(ua, keyword) {
				return rule.Class
			}
		}
	}
	return uaOther
}

func (s *Store) RecordRequestStart(gen *Config, group string, at time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cfg != gen {
		return
	}
	if r := s.table.services[group]; r != nil && at.After(r.LastRequestAt) {
		r.LastRequestAt = at
	}
}

func (s *Store) RecordRequestEnd(gen *Config, group, ua, line string, success, failover bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cfg != gen {
		return
	}
	r := s.table.services[group]
	if r == nil {
		return
	}
	r.Total++
	if success {
		r.Success++
	} else {
		r.Failed++
	}
	if failover {
		r.Failover++
	}
	// Empty line is a virtual undelivered node; no reserved real line name.
	class := uaClass(ua)
	if r.UALines[class] == nil {
		r.UALines[class] = make(map[string]UALine)
	}
	cell := r.UALines[class][line]
	cell.UA, cell.Line = class, line
	cell.Count++
	if !success {
		cell.Failed++
	}
	cell.LastAt = time.Now()
	r.UALines[class][line] = cell
}

func (s *Store) RecordRequestCanceled(gen *Config, group string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cfg != gen {
		return
	}
	if r := s.table.services[group]; r != nil {
		r.Total++
		r.Canceled++
	}
}

func (s *Store) ServiceStats(snapshot []GroupState) []ServiceStats {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]ServiceStats, 0, len(snapshot))
	for _, g := range snapshot {
		st := ServiceStats{Name: g.Name, Listen: g.Listen, Active: g.Active, Health: "unknown", UALines: make([]UALine, 0),
			UARules: append([]UARuleConfig{}, g.UARules...)}
		// Health summarizes the best enabled route, not transport selection.
		for _, u := range g.Upstreams {
			if !u.Enabled {
				continue
			}
			h := "unreachable"
			if u.Class == "" {
				h = "unknown"
			} else if u.Reach {
				h = "reachable"
				if u.Class == "healthy" {
					h = "healthy"
				}
			}
			if healthRank(h) > healthRank(st.Health) {
				st.Health = h
			}
		}
		if r := s.table.services[g.Name]; r != nil {
			st.Total, st.Success, st.Failed, st.Failover, st.LastRequestAt = r.Total, r.Success, r.Failed, r.Failover, r.LastRequestAt
			st.Canceled = r.Canceled
			for _, lines := range r.UALines {
				for _, cell := range lines {
					st.UALines = append(st.UALines, cell)
				}
			}
			sort.Slice(st.UALines, func(i, j int) bool {
				a, b := st.UALines[i], st.UALines[j]
				if a.UA != b.UA {
					return a.UA < b.UA
				}
				return a.Line < b.Line
			})
		}
		out = append(out, st)
	}
	return out
}

func healthRank(h string) int {
	switch h {
	case "healthy":
		return 4
	case "reachable":
		return 3
	case "unreachable":
		return 2
	default:
		return 1
	}
}

func sortServiceStates(groups []GroupState, ui UIConfig) {
	if ui.SortMode == "recent" {
		sort.SliceStable(groups, func(i, j int) bool { return groups[i].LastRequestAt.After(groups[j].LastRequestAt) })
	} else if ui.SortMode == "manual" {
		rank := make(map[string]int, len(ui.ServiceOrder))
		for i, name := range ui.ServiceOrder {
			rank[name] = i
		}
		sort.SliceStable(groups, func(i, j int) bool {
			a, aOK := rank[groups[i].Name]
			b, bOK := rank[groups[j].Name]
			if aOK != bOK {
				return aOK
			}
			return aOK && a < b
		})
	}
}
