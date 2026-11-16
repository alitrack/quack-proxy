package supervisor

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/alitrack/quack-proxy/internal/config"
)

// Gate 0 regression suite. These tests drive the state machine through the
// injectable probe/launch seams, so they need no real DuckDB process and stay
// hermetic and fast. See docs/REMEDIATION.md R-02 / R-05 / R-10 / R-11.

func gate0Config(t *testing.T) *config.Config {
	t.Helper()
	return &config.Config{
		Global: config.GlobalConfig{
			DuckDBBin:  "true",
			StatusFile: filepath.Join(t.TempDir(), "status.json"),
		},
		Listener: config.ListenerConfig{
			BindHost:       "0.0.0.0",
			HealthPath:     "/",
			HealthInterval: time.Second,
		},
		Shards: []config.ShardConfig{
			{Name: "shard_a", Database: ".", Port: 19701, Token: "test-token"},
		},
	}
}

func gate0Logger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// R-02: a healthy response must clear the accumulated restart budget.
// Before the fix, checkAll only reset backoff/lastUnhealthy, so intermittent
// failures accumulated until the shard was parked permanently.
func TestGate0HealthyResetsRestartBudget(t *testing.T) {
	cfg := gate0Config(t)
	sup := New(cfg, gate0Logger())
	sup.startupGrace = 0
	sup.checkFn = func(string, int, string, time.Duration, string) bool { return true }

	sup.ManualSetShard("shard_a", ShardProcess{
		Config:    cfg.Shards[0],
		Status:    "unhealthy",
		Restarts:  maxRestarts - 1,
		backoff:   16 * time.Second,
		lastStart: time.Now().Add(-time.Hour),
	})

	sup.checkAll(context.Background())

	sp := sup.shards["shard_a"]
	if sp == nil {
		t.Fatal("shard missing after checkAll")
	}
	if sp.Status != "healthy" {
		t.Errorf("status = %q, want healthy", sp.Status)
	}
	if sp.Restarts != 0 {
		t.Errorf("restarts = %d, want 0 after recovery", sp.Restarts)
	}
	if sp.backoff != initialBackoff {
		t.Errorf("backoff = %v, want %v", sp.backoff, initialBackoff)
	}
}

// R-02: a shard parked in "stopped" must be revived by reload with a fresh
// retry budget. Before the fix, ApplyConfig skipped every existing name and
// README's troubleshooting advice ("reload to recover") could never work.
func TestGate0StoppedShardRecoversOnReload(t *testing.T) {
	cfg := gate0Config(t)
	sup := New(cfg, gate0Logger())

	launched := 0
	sup.startFn = func(_ context.Context, sh config.ShardConfig) error {
		launched++
		// Mirror what startShard does: install a fresh ShardProcess.
		// ApplyConfig deliberately calls launch WITHOUT s.mu held (R-03),
		// so the seam may use the locking helper.
		sup.ManualSetShard(sh.Name, ShardProcess{
			Config:    sh,
			Status:    "starting",
			StartTime: time.Now(),
			lastStart: time.Now(),
		})
		return nil
	}

	sup.ManualSetShard("shard_a", ShardProcess{
		Config:   cfg.Shards[0],
		Status:   "stopped",
		Restarts: maxRestarts,
	})

	sup.ApplyConfig(context.Background(), cfg)

	if launched != 1 {
		t.Fatalf("reload launched %d shards, want 1 (stopped shard must be revived)", launched)
	}
	sp := sup.shards["shard_a"]
	if sp.Status == "stopped" {
		t.Error("shard is still stopped after reload")
	}
	if sp.Restarts != 0 {
		t.Errorf("restarts = %d, want a fresh budget", sp.Restarts)
	}
}

// R-05 / R-11: the status file carries shard tokens, so it must be private,
// complete (atomic rename, never a half-written JSON) and leave no temp files.
func TestGate0StatusFileIsPrivateAndComplete(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX permission bits are not enforced on Windows")
	}
	cfg := gate0Config(t)
	sup := New(cfg, gate0Logger())
	sup.ManualSetShard("shard_a", ShardProcess{
		Config:    cfg.Shards[0],
		Status:    "healthy",
		StartTime: time.Now(),
		lastStart: time.Now(),
	})

	sup.WriteStatusFile()

	info, err := os.Stat(cfg.Global.StatusFile)
	if err != nil {
		t.Fatalf("status file not written: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0600 {
		t.Errorf("status file mode = %04o, want 0600", perm)
	}
	sf, err := ReadStatusFile(cfg.Global.StatusFile)
	if err != nil {
		t.Fatalf("status file is not valid JSON: %v", err)
	}
	if len(sf.Shards) != 1 || sf.Shards[0].Token != "test-token" {
		t.Errorf("status file content incomplete: %+v", sf.Shards)
	}
	if !strings.Contains(sf.Shards[0].AttachSQL, "127.0.0.1") {
		t.Errorf("status AttachSQL is not dialable: %q", sf.Shards[0].AttachSQL)
	}
	leftovers, _ := filepath.Glob(filepath.Join(filepath.Dir(cfg.Global.StatusFile), ".quack-proxy-status-*.tmp"))
	if len(leftovers) != 0 {
		t.Errorf("atomic write left temp files behind: %v", leftovers)
	}
}

// R-10: with the default bind_host 0.0.0.0, generated ATTACH statements must
// use a dialable address instead of the bind-only wildcard.
func TestGate0AttachSQLUsesDialableHost(t *testing.T) {
	cfg := gate0Config(t)
	sup := New(cfg, gate0Logger())
	sup.ManualSetShard("shard_a", ShardProcess{Config: cfg.Shards[0], Status: "healthy"})

	sql := sup.AttachSQL()
	if strings.Contains(sql, "0.0.0.0") {
		t.Errorf("AttachSQL contains a bind-only address: %q", sql)
	}
	if !strings.Contains(sql, "ATTACH 'quack:127.0.0.1:19701' AS shard_a;") {
		t.Errorf("AttachSQL missing dialable ATTACH line: %q", sql)
	}
}
