//go:build darwin || windows

package main

import (
	"fmt"
	"strings"
	"testing"
)

func TestServiceSummaryIncludesActiveRoute(t *testing.T) {
	g := group{Name: "API", Active: "private", Upstreams: []upstream{
		{Name: "private", Enabled: true, Status: "healthy", LastProbeLatency: 12},
		{Name: "backup", Enabled: true, Status: "unknown"},
	}}
	if got := groupSummary(g); got != "API · private 正常 12ms" {
		t.Fatal(got)
	}
	g.Active = ""
	if !strings.Contains(groupSummary(g), "无可用线路") {
		t.Fatal(groupSummary(g))
	}
}

func TestServicesSummaryHasNoEightServiceLimit(t *testing.T) {
	var groups []group
	for i := 0; i < 20; i++ {
		groups = append(groups, group{Name: fmt.Sprintf("service-%d", i), Active: "route", Upstreams: []upstream{{Name: "route", Enabled: true, Status: "healthy"}}})
	}
	groups[19].Upstreams[0].Status = "unknown"
	groups[18].Active = ""
	groups[17].Upstreams[0].Enabled = false
	if got := servicesSummary(groups); got != "20 个服务 · 17 正常" {
		t.Fatal(got)
	}
	if got := servicesSummary(nil); got != "0 个服务 · 0 正常" {
		t.Fatal(got)
	}
}
