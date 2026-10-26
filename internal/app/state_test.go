package app

import (
	"testing"
)

func TestStoreRequestHealthThresholds(t *testing.T) {
	cfg := testConfig(UpstreamConfig{Name: "primary", URL: "http://upstream.invalid"})
	store := NewStore(cfg, Thresholds{FailThreshold: 3, RecoverThreshold: 2})
	check := func(status HealthStatus, failures, successes int) UpstreamState {
		t.Helper()
		state := testState(t, store, cfg, "primary")
		if state.Status != status || state.ConsecutiveFailures != failures || state.ConsecutiveSuccesses != successes {
			t.Fatalf("state = (%s, failures=%d, successes=%d), want (%s, %d, %d)", state.Status, state.ConsecutiveFailures, state.ConsecutiveSuccesses, status, failures, successes)
		}
		return state
	}

	check(StatusUnknown, 0, 0)
	store.RecordSuccess("test", "primary", 0)
	check(StatusHealthy, 0, 1) // 从未探测过：一次成功即可用
	store.RecordFailure("test", "primary", "dial failed", 0)
	check(StatusHealthy, 1, 0)
	store.RecordSuccess("test", "primary", 0)
	check(StatusHealthy, 0, 1) // 成功会中断失败计数
	for i := 1; i <= 3; i++ {
		store.RecordFailure("test", "primary", "dial failed", 0)
		want := StatusHealthy
		if i == 3 {
			want = StatusUnhealthy
		}
		check(want, i, 0)
	}
	if store.IsUsable("test", "primary") {
		t.Error("unhealthy line still usable")
	}
	// 恢复阈值只用于判死之后。
	store.RecordSuccess("test", "primary", 0)
	check(StatusUnhealthy, 0, 1)
	store.RecordSuccess("test", "primary", 0)
	check(StatusHealthy, 0, 2)
}

func TestStoreProbeAndRequestCountersAreIndependent(t *testing.T) {
	cfg := testConfig(UpstreamConfig{Name: "primary", URL: "http://upstream.invalid"})
	store := NewStore(cfg, Thresholds{FailThreshold: 2, RecoverThreshold: 2})
	store.RecordSuccess("test", "primary", 0)

	// 轻量探针成功不能冲掉真实请求的失败计数。
	store.RecordFailure("test", "primary", "write failed", 0)
	store.RecordProbe("test", "primary", true, 0, "")
	state := testState(t, store, cfg, "primary")
	if state.ConsecutiveFailures != 1 || state.ProbeSuccesses != 1 || state.Status != StatusHealthy {
		t.Fatalf("probe success clobbered request failures: %+v", state)
	}

	// 再来一次真实失败就应判死，即使探针仍然成功。
	store.RecordFailure("test", "primary", "write failed", 0)
	if state := testState(t, store, cfg, "primary"); state.Status != StatusUnhealthy {
		t.Fatalf("status = %s, want unhealthy", state.Status)
	}

	// 恢复要探针连续成功两次。
	store.RecordProbe("test", "primary", true, 0, "")
	if state := testState(t, store, cfg, "primary"); state.Status != StatusUnhealthy {
		t.Fatalf("recovered too early: %s", state.Status)
	}
	store.RecordProbe("test", "primary", true, 0, "")
	if state := testState(t, store, cfg, "primary"); state.Status != StatusHealthy {
		t.Fatalf("did not recover: %s", state.Status)
	}
}
