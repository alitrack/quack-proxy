package supervisor

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/alitrack/quack-proxy/internal/config"
)

// R-07: reload used to only add/remove shards. A kept shard whose port,
// database, token, init_sql or init_sql_file changed silently kept running
// the OLD process. These tests drive ApplyConfig through the startFn seam so
// no real DuckDB process is forked.

func reloadTestConfig(t *testing.T, shards ...config.ShardConfig) *config.Config {
	t.Helper()
	return &config.Config{
		Global: config.GlobalConfig{
			LogLevel:   "info",
			DuckDBBin:  "duckdb",
			StatusFile: filepath.Join(t.TempDir(), "status.json"),
		},
		Listener: config.ListenerConfig{
			BindHost:       "127.0.0.1",
			HealthPath:     "/",
			HealthInterval: time.Second,
			PortStart:      19491,
		},
		Shards: shards,
	}
}

type reloadHarness struct {
	sup    *Supervisor
	mu     sync.Mutex
	starts []config.ShardConfig
}

func newReloadHarness(cfg *config.Config) *reloadHarness {
	h := &reloadHarness{}
	h.sup = New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	h.sup.startFn = func(_ context.Context, sh config.ShardConfig) error {
		h.mu.Lock()
		h.starts = append(h.starts, sh)
		h.mu.Unlock()
		// Mirror startShard: install a fresh process with the NEW config.
		h.sup.ManualSetShard(sh.Name, ShardProcess{
			Config:        sh,
			Status:        "starting",
			StartTime:     time.Now(),
			lastStart:     time.Now(),
			backoff:       initialBackoff,
			initSQLDigest: initSQLFileDigest(sh.InitSQLFile),
		})
		return nil
	}
	return h
}

func (h *reloadHarness) launched() []config.ShardConfig {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := append([]config.ShardConfig(nil), h.starts...)
	return out
}

func baseShard() config.ShardConfig {
	return config.ShardConfig{
		Name:     "shard_a",
		Database: "/tmp/shard_a.db",
		Port:     19491,
		Token:    "explicit-token",
	}
}

func TestReloadRestartsShardWhenAFieldChanges(t *testing.T) {
	old := baseShard()
	h := newReloadHarness(reloadTestConfig(t, old))
	h.sup.ManualSetShard(old.Name, ShardProcess{Config: old, Status: "healthy", PID: 4242})

	next := old
	next.Port = 19500
	added, removed, restarted := h.sup.ApplyConfig(context.Background(), reloadTestConfig(t, next))

	if len(added) != 0 || len(removed) != 0 {
		t.Fatalf("added=%v removed=%v, want both empty (this is a restart, not add/remove)", added, removed)
	}
	if len(restarted) != 1 || restarted[0] != old.Name {
		t.Fatalf("restarted=%v, want [%s]", restarted, old.Name)
	}
	launched := h.launched()
	if len(launched) != 1 || launched[0].Port != next.Port {
		t.Fatalf("relaunched with %+v, want port %d", launched, next.Port)
	}
	sp := h.sup.shards[old.Name]
	if sp == nil || sp.Config.Port != next.Port {
		t.Errorf("live shard still has old config: %+v", sp)
	}
	if sp.Status != "starting" || sp.Restarts != 0 {
		t.Errorf("restarted shard status=%q restarts=%d, want starting/0", sp.Status, sp.Restarts)
	}
	if sp.restarting {
		t.Error("restarting flag was not cleared after reload restart")
	}
}

// The subtle one: startShard resolves an omitted token into a random value
// and stores it in Config.Token. Comparing structs naively would report a
// change on EVERY reload for shards using generated credentials.
func TestReloadDoesNotRestartWhenOnlyGeneratedTokenDiffers(t *testing.T) {
	configured := baseShard()
	configured.Token = "" // operator relies on the generated token
	h := newReloadHarness(reloadTestConfig(t, configured))

	running := configured
	running.Token = "auto-generated-32-char-token"
	h.sup.ManualSetShard(configured.Name, ShardProcess{Config: running, Status: "healthy"})

	_, _, restarted := h.sup.ApplyConfig(context.Background(), reloadTestConfig(t, configured))
	if len(restarted) != 0 {
		t.Errorf("restarted=%v, want none: an omitted token must not look like a change", restarted)
	}
	if got := h.launched(); len(got) != 0 {
		t.Errorf("unexpected relaunch: %+v", got)
	}
}

func TestReloadRestartsShardOnExplicitTokenChange(t *testing.T) {
	old := baseShard()
	h := newReloadHarness(reloadTestConfig(t, old))
	h.sup.ManualSetShard(old.Name, ShardProcess{Config: old, Status: "healthy"})

	next := old
	next.Token = "rotated-token"
	_, _, restarted := h.sup.ApplyConfig(context.Background(), reloadTestConfig(t, next))
	if len(restarted) != 1 {
		t.Fatalf("restarted=%v, want 1 after an explicit token rotation", restarted)
	}
	if got := h.launched(); len(got) != 1 || got[0].Token != next.Token {
		t.Errorf("relaunched with %+v, want token %q", got, next.Token)
	}
}

func TestReloadIsIdempotentForAnUnchangedConfig(t *testing.T) {
	sh := baseShard()
	h := newReloadHarness(reloadTestConfig(t, sh))
	h.sup.ManualSetShard(sh.Name, ShardProcess{Config: sh, Status: "healthy"})

	for i := 0; i < 3; i++ {
		_, _, restarted := h.sup.ApplyConfig(context.Background(), reloadTestConfig(t, sh))
		if len(restarted) != 0 {
			t.Fatalf("reload #%d restarted %v, want none", i+1, restarted)
		}
	}
	if got := h.launched(); len(got) != 0 {
		t.Errorf("unchanged reloads relaunched shards: %+v", got)
	}
}

// Health-only settings are read from s.cfg on every tick; changing them must
// not tear down healthy shards.
func TestReloadDoesNotRestartOnHealthOnlyChange(t *testing.T) {
	sh := baseShard()
	h := newReloadHarness(reloadTestConfig(t, sh))
	h.sup.ManualSetShard(sh.Name, ShardProcess{Config: sh, Status: "healthy"})

	next := reloadTestConfig(t, sh)
	next.Listener.HealthInterval = 10 * time.Second
	next.Listener.HealthPath = "/healthz"
	next.Listener.HealthExpect = "quack-greeting"
	_, _, restarted := h.sup.ApplyConfig(context.Background(), next)
	if len(restarted) != 0 {
		t.Errorf("restarted=%v, want none for health-only changes", restarted)
	}
}

// Changing a global that is baked into the command/boot SQL affects EVERY
// shard, even when no shard-level field changed.
func TestReloadRestartsAllShardsOnProcessSpecChange(t *testing.T) {
	a := baseShard()
	b := a
	b.Name = "shard_b"
	b.Port = 19492
	h := newReloadHarness(reloadTestConfig(t, a, b))
	h.sup.ManualSetShard(a.Name, ShardProcess{Config: a, Status: "healthy"})
	h.sup.ManualSetShard(b.Name, ShardProcess{Config: b, Status: "healthy"})

	next := reloadTestConfig(t, a, b)
	next.Global.DuckDBBin = "duckdb-v2"
	_, _, restarted := h.sup.ApplyConfig(context.Background(), next)
	if len(restarted) != 2 {
		t.Fatalf("restarted=%v, want both shards after duckdb_bin change", restarted)
	}
	if got := h.launched(); len(got) != 2 {
		t.Errorf("launched %d shards, want 2: %+v", len(got), got)
	}
}

// Editing an init_sql_file policy without changing its path must restart the
// shard, otherwise the old policy keeps serving silently.
func TestReloadRestartsOnInitSQLFileContentChange(t *testing.T) {
	path := filepath.Join(t.TempDir(), "policy.sql")
	if err := os.WriteFile(path, []byte("SELECT 1;"), 0600); err != nil {
		t.Fatal(err)
	}
	sh := baseShard()
	sh.InitSQLFile = path
	h := newReloadHarness(reloadTestConfig(t, sh))
	h.sup.ManualSetShard(sh.Name, ShardProcess{
		Config:        sh,
		Status:        "healthy",
		initSQLDigest: initSQLFileDigest(path),
	})

	if _, _, restarted := h.sup.ApplyConfig(context.Background(), reloadTestConfig(t, sh)); len(restarted) != 0 {
		t.Fatalf("unchanged policy file triggered a restart: %v", restarted)
	}
	if err := os.WriteFile(path, []byte("SELECT 2;"), 0600); err != nil {
		t.Fatal(err)
	}
	_, _, restarted := h.sup.ApplyConfig(context.Background(), reloadTestConfig(t, sh))
	if len(restarted) != 1 {
		t.Fatalf("restarted=%v, want 1 after the policy file contents changed", restarted)
	}
}

// A removed shard's port must be freed before a changed/new shard binds it in
// the same reload. toStop is applied before toRestart/toStart by design.
func TestReloadStopsRemovedShardsBeforeRestarting(t *testing.T) {
	old := baseShard()
	h := newReloadHarness(reloadTestConfig(t, old))
	h.sup.ManualSetShard(old.Name, ShardProcess{Config: old, Status: "healthy"})

	replacement := old
	replacement.Name = "shard_b"
	added, removed, restarted := h.sup.ApplyConfig(context.Background(), reloadTestConfig(t, replacement))
	if len(removed) != 1 || removed[0] != old.Name {
		t.Errorf("removed=%v, want [%s]", removed, old.Name)
	}
	if len(added) != 1 || added[0] != replacement.Name {
		t.Errorf("added=%v, want [%s]", added, replacement.Name)
	}
	if len(restarted) != 0 {
		t.Errorf("restarted=%v, want none", restarted)
	}
}
