package app

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// UARuleConfig prefers a line for matching inbound User-Agents; it never disables fallback.
type UARuleConfig struct {
	Match   string `yaml:"match" json:"match"`
	Line    string `yaml:"line" json:"line"`
	Comment string `yaml:"comment,omitempty" json:"comment,omitempty"`
}

type uaMatcher struct {
	mode    string
	pattern string
	regex   *regexp.Regexp
}

type uaRuleRuntime struct {
	matcher uaMatcher
	line    string
}

// Literal matches are case-insensitive. Regex uses Go syntax and explicit (?i)
// for case folding. Errors must not echo match strings, which can contain secrets.
func compileUAMatch(raw string) (uaMatcher, error) {
	m := uaMatcher{mode: "substring", pattern: raw}
	lower := strings.ToLower(raw)
	for _, mode := range []string{"exact", "prefix", "regex"} {
		if strings.HasPrefix(lower, mode+":") {
			m.mode, m.pattern = mode, raw[len(mode)+1:]
			break
		}
	}
	if strings.TrimSpace(m.pattern) == "" {
		return m, errors.New("match 及匹配前缀后的内容不能为空")
	}
	for _, r := range m.pattern {
		if r < 0x20 || r == 0x7f {
			return m, errors.New("match 不能含控制字符")
		}
	}
	if m.mode == "regex" {
		re, err := regexp.Compile(m.pattern)
		if err != nil {
			return m, errors.New("match 的 regex 正则无效（使用 Go 正则语法）")
		}
		m.regex = re
	} else {
		m.pattern = strings.ToLower(m.pattern)
	}
	return m, nil
}

func (m uaMatcher) matches(ua string) bool {
	if m.regex != nil {
		return m.regex.MatchString(ua)
	}
	ua = strings.ToLower(ua)
	switch m.mode {
	case "exact":
		return ua == m.pattern
	case "prefix":
		return strings.HasPrefix(ua, m.pattern)
	default:
		return strings.Contains(ua, m.pattern)
	}
}

func validateUARules(rules []UARuleConfig, lines map[string]bool) error {
	seen := make(map[string]bool, len(rules))
	for i, rule := range rules {
		m, err := compileUAMatch(rule.Match)
		if err != nil {
			return fmt.Errorf("ua_rules[%d]: %w", i, err)
		}
		key := m.mode + "\x00" + m.pattern
		if seen[key] {
			return fmt.Errorf("ua_rules[%d]: match 重复", i)
		}
		seen[key] = true
		if !lines[rule.Line] {
			return fmt.Errorf("ua_rules[%d]: line 必须为本组 upstreams 中的线路名", i)
		}
	}
	return nil
}

func (g *groupRuntime) pinnedLine(ua string) string {
	for _, rule := range g.uaRules {
		if rule.matcher.matches(ua) {
			return rule.line
		}
	}
	return ""
}
