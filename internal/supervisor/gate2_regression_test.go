package supervisor

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alitrack/quack-proxy/internal/config"
)

// Gate 2 regression suite: concurrency and restart-budget semantics that the
// original 35 tests never exercised. See docs/REMEDIATION.md R-03 / R-09 / R-17.

// R-03: a health probe (which can block for seconds) must not hold the
// supervisor mutex. Before the refactor, checkAll held s.mu across 2s*N
// serial HTTP calls plus a 10s process wait, so status/reload/shutdown were
// stalled for the whole tick.
func TestGate2ProbeDoesNotBlockStatusReads(t *testing.T) {
	cfg := gate0Config(t)
	cfg.Listener.HealthExpect = "quack-greeting"
	sup := New(cfg, gate0Logger())
	sup.startupGrace = 0
	sup.startFn = func(_ context.Context, sh config.ShardConfig) error {
		sup.ManualSetShard(sh.Name, ShardProcess{
			Config: sh, Status: "starting", StartTime: time.Now(), lastStart: time.Now(),
		})
		return nil
	}

	entered := make(chan struct{})
	release := make(chan struct{})
	var gotExpect atomic.Value
	sup.checkFn = func(_ string, _ int, _ string, _ time.Duration, expect string) bool {
		gotExpect.Store(expect)
		close(entered)
		<-release // hold the probe open while we poke the supervisor
		return false
	}
	sup.ManualSetShard("shard_a", ShardProcess{
		Config: cfg.Shards[0], Status: "starting",
		lastStart: time.Now().Add(-time.Hour),
	})

	done := make(chan struct{})
	go func() {
		sup.checkAll(context.Background())
		close(done)
	}()
	<-entered

	readable := make(chan struct{})
	go func() {
		_ = sup.AttachSQL() // takes s.mu.RLock
		close(readable)
	}()
	select {
	case <-readable:
		// good: the lock is free while the probe is in flight
	case <-time.After(2 * time.Second):
		t.Error("status read blocked while a health probe was in flight (R-03)")
	}

	close(release)
	<-done
	if v, _ := gotExpect.Load().(string); v != "quack-greeting" {
		t.Errorf("health_expect was not forwarded to the probe: %q", v)
	}
}

// R-17: a restart must NOT refresh lastStart. The old code gave every
// attempt a fresh 30s startup grace, which swallowed the 1s..30s exponential
// backoff entirely — a broken shard was killed every 30s regardless of the
// configured backoff.
func TestGate2RestartDoesNotRefreshStartupGrace(t *testing.T) {
	cfg := gate0Config(t)
	sup := New(cfg, gate0Logger())
	sup.startupGrace = 30 * time.Second
	sup.checkFn = func(string, int, string, time.Duration, string) bool { return false }
	sup.startFn = func(_ context.Context, sh config.ShardConfig) error {
		// A real fork would stamp lastStart = now. That is exactly the
		// behaviour performRestart must undo.
		sup.ManualSetShard(sh.Name, ShardProcess{
			Config: sh, Status: "starting", StartTime: time.Now(), lastStart: time.Now(),
		})
		return nil
	}

	original := time.Now().Add(-2 * time.Hour)
	sup.ManualSetShard("shard_a", ShardProcess{
		Config: cfg.Shards[0], Status: "unhealthy",
		Restarts: 0, backoff: initialBackoff, lastStart: original,
	})

	sup.checkAll(context.Background())
	sp := sup.shards["shard_a"]
	if sp == nil || sp.Restarts != 1 {
		t.Fatalf("expected exactly one restart, got %+v", sp)
	}
	if !sp.lastStart.Equal(original) {
		t.Errorf("restart refreshed lastStart (got %v, want %v): the 30s grace would swallow the backoff again",
			sp.lastStart, original)
	}

	// With the original (stale) timestamp, an immediate second check is
	// suppressed by the BACKOFF window, not by a fresh startup grace.
	sup.checkAll(context.Background())
	sp = sup.shards["shard_a"]
	if sp.Status != "unhealthy" {
		t.Errorf("status = %q, want unhealthy (backoff window, not startup grace)", sp.Status)
	}
	if sp.Restarts != 1 {
		t.Errorf("second check restarted the shard again: restarts=%d", sp.Restarts)
	}
}

// R-12: StopAll must stop every shard, including shards injected without a
// real child process, and must leave no shard in a non-stopped state.
func TestGate2StopAllStopsEveryShard(t *testing.T) {
	cfg := gate0Config(t)
	sup := New(cfg, gate0Logger())
	for _, name := range []string{"a", "b", "c", "d"} {
		sup.ManualSetShard(name, ShardProcess{
			Config: config.ShardConfig{Name: name, Port: 19000, Database: "."},
			Status: "healthy",
		})
	}

	sup.StopAll()
	for _, sp := range sup.Status() {
		if sp.Status != "stopped" {
			t.Errorf("shard %s status = %q after StopAll, want stopped", sp.Config.Name, sp.Status)
		}
	}
}
