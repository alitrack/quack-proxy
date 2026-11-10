package supervisor

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/alitrack/quack-proxy/internal/config"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, nil))
}

// testConfig builds a supervisor config the way config.Load would for tests.
// The critical field is a non-empty DuckDBBin: a zero value would put an empty
// command in the shell pipeline and the child would die with "command not
// found" (this is the failure the old tests silently tripped over). The child
// work is stubbed by "true" (exit 0, ignores its arguments).
func testConfig(shards ...config.ShardConfig) *config.Config {
	return &config.Config{
		Global:   config.GlobalConfig{DuckDBBin: "true"},
		Listener: config.ListenerConfig{BindHost: "127.0.0.1", HealthPath: "/", HealthInterval: time.Second},
		Shards:   shards,
	}
}

func TestNewSupervisor(t *testing.T) {
	cfg := testConfig(config.ShardConfig{Name: "test", Database: "/tmp/test.db", Port: 9491})
	sup := New(cfg, testLogger())

	if len(sup.shards) != 0 {
		t.Error("new supervisor should have empty shards map")
	}
}

func TestAttachSQL(t *testing.T) {
	cfg := testConfig(
		config.ShardConfig{Name: "analytics", Database: "/tmp/a.db", Port: 9491},
		config.ShardConfig{Name: "logs", Database: "/tmp/l.db", Port: 9492},
	)
	sup := New(cfg, testLogger())

	// No shards started, so AttachSQL should be empty
	if sql := sup.AttachSQL(); sql != "" {
		t.Errorf("AttachSQL with no shards = %q, want empty", sql)
	}

	// A healthy injected shard produces an ATTACH line.
	sup.ManualSetShard("analytics", ShardProcess{Config: cfg.Shards[0], Status: "healthy"})
	if sql := sup.AttachSQL(); !strings.Contains(sql, "ATTACH 'quack:127.0.0.1:9491' AS analytics;") {
		t.Errorf("AttachSQL missing expected line, got: %q", sql)
	}
}

func TestStartAndStopShard(t *testing.T) {
	tmp := t.TempDir()
	dbPath := filepath.Join(tmp, "test.db")
	if err := os.WriteFile(dbPath, []byte{}, 0644); err != nil {
		t.Fatal(err)
	}

	cfg := testConfig(config.ShardConfig{Name: "testshard", Database: dbPath, Port: 9499})
	sup := New(cfg, testLogger())

	if err := sup.StartAll(context.Background()); err != nil {
		t.Fatalf("StartAll: %v", err)
	}
	t.Cleanup(sup.StopAll)

	sp := sup.shards["testshard"]
	if sp == nil {
		t.Fatal("shard not found in supervisor")
	}
	if sp.Status != "starting" {
		t.Errorf("status = %q, want starting", sp.Status)
	}
	if sp.PID <= 0 {
		t.Errorf("PID = %d, want > 0", sp.PID)
	}
	if sp.Config.Token == "" || len(sp.Config.Token) != 32 {
		t.Errorf("token = %q, want a 32-char token", sp.Config.Token)
	}

	// The boot SQL temp file must have been created and removed by stop.
	sqlFile := filepath.Join(os.TempDir(), "quack-proxy-testshard-9499.sql")
	if _, err := os.Stat(sqlFile); err != nil {
		t.Errorf("expected boot sql temp file before stop: %v", err)
	}
}

// TestStopKillsWholeProcessGroup verifies that stopping a shard kills not just
// the bash wrapper but every descendant, including the `tail -f /dev/null`
// keep-alive that used to outlive the wrapper and keep the port bound.
//
// The stub DuckDBBin is "true" (exits immediately); the member that keeps the
// group alive after that is exactly the pipeline's `tail -f /dev/null`, the
// real cause of the old leak. Stopping must take the whole group with it.
func TestStopKillsWholeProcessGroup(t *testing.T) {
	tmp := t.TempDir()
	dbPath := filepath.Join(tmp, "group.db")
	if err := os.WriteFile(dbPath, []byte{}, 0644); err != nil {
		t.Fatal(err)
	}

	cfg := &config.Config{
		Global:   config.GlobalConfig{DuckDBBin: "true"},
		Listener: config.ListenerConfig{BindHost: "127.0.0.1", HealthPath: "/", HealthInterval: time.Second},
		Shards:   []config.ShardConfig{{Name: "grp", Database: dbPath, Port: 9498}},
	}
	sup := New(cfg, testLogger())
	if err := sup.startShardLocked(context.Background(), cfg.Shards[0]); err != nil {
		t.Fatalf("startShardLocked: %v", err)
	}
	t.Cleanup(sup.StopAll)

	time.Sleep(300 * time.Millisecond) // let bash fork the pipeline

	sp := sup.shards["grp"]
	groupLeader := sp.PID // capture before stop zeros it
	if groupLeader <= 0 {
		t.Fatal("no leader pid after start")
	}
	sup.stopShardLocked("grp", sp)

	if sp.cmd != nil || sp.PID != 0 || sp.Status != "stopped" {
		t.Errorf("after stop: cmd=%v pid=%d status=%q, want nil/0/stopped",
			sp.cmd, sp.PID, sp.Status)
	}

	// The group must disappear (poll to allow the OS to reap members).
	deadline := time.Now().Add(3 * time.Second)
	for {
		if err := signalProcessGroup(groupLeader, 0); err != nil {
			break // group is gone — success
		}
		if time.Now().After(deadline) {
			t.Error("process group still present after stop (descendants leaked)")
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// TestBuildBootSQLInitSQL verifies that init_sql is prepended to the boot SQL
// (extensions/hooks load BEFORE quack_serve) and absent when not configured.
func TestBuildBootSQLInitSQL(t *testing.T) {
	cfg := config.ShardConfig{Name: "s", Database: "/x.db", Port: 9599,
		InitSQL: "INSTALL luajit FROM community; LOAD luajit;"}
	got, err := buildBootSQL("0.0.0.0", cfg, "tk")
	if err != nil {
		t.Fatalf("buildBootSQL: %v", err)
	}
	initIdx := strings.Index(got, "INSTALL luajit")
	serveIdx := strings.Index(got, "quack_serve")
	if initIdx == -1 || serveIdx == -1 || initIdx > serveIdx {
		t.Errorf("init_sql must precede quack_serve (init=%d serve=%d):\n%s", initIdx, serveIdx, got)
	}

	plain := config.ShardConfig{Name: "s", Database: "/x.db", Port: 9600}
	got2, err := buildBootSQL("127.0.0.1", plain, "tk2")
	if err != nil {
		t.Fatalf("buildBootSQL: %v", err)
	}
	if strings.Contains(got2, "luajit") {
		t.Errorf("no init_sql configured but found injected SQL:\n%s", got2)
	}
	if !strings.Contains(got2, "quack_serve('quack:127.0.0.1:9600'") {
		t.Errorf("boot stanza malformed:\n%s", got2)
	}
}

// TestBuildBootSQLMissingInitFile verifies a missing init_sql_file is a hard
// error (fail loud) rather than a silently dropped hook, and that a readable
// file is appended after any inline init_sql.
func TestBuildBootSQLMissingInitFile(t *testing.T) {
	missing := config.ShardConfig{Name: "s", Database: "/x.db", Port: 9601,
		InitSQLFile: filepath.Join(t.TempDir(), "does-not-exist.sql")}
	if _, err := buildBootSQL("127.0.0.1", missing, "tk"); err == nil {
		t.Error("expected error for missing init_sql_file, got nil")
	}

	dir := t.TempDir()
	p := filepath.Join(dir, "extra.sql")
	if err := os.WriteFile(p, []byte("SELECT 1;"), 0600); err != nil {
		t.Fatal(err)
	}
	cfg := config.ShardConfig{Name: "s", Database: "/x.db", Port: 9602,
		InitSQL: "SELECT 0;", InitSQLFile: p}
	got, err := buildBootSQL("127.0.0.1", cfg, "tk")
	if err != nil {
		t.Fatalf("buildBootSQL: %v", err)
	}
	if !strings.Contains(got, "SELECT 0;\nSELECT 1;") {
		t.Errorf("init_sql and init_sql_file should both be present in order:\n%s", got)
	}
}

func TestStatus(t *testing.T) {
	cfg := testConfig(
		config.ShardConfig{Name: "s1", Database: "/tmp/s1.db", Port: 9491},
		config.ShardConfig{Name: "s2", Database: "/tmp/s2.db", Port: 9492},
	)
	sup := New(cfg, testLogger())

	if n := len(sup.Status()); n != 0 {
		t.Errorf("Status with no started shards = %d items, want 0", n)
	}
}

// TestCheckAllRestartsThenBacksOff drives the health loop's restart path
// against a port that never answers: the shard must be restarted (not
// stormed), backoff must grow, and the streak must reset after recovery.
func TestCheckAllRestartsThenBacksOff(t *testing.T) {
	tmp := t.TempDir()
	dbPath := filepath.Join(tmp, "hc.db")
	if err := os.WriteFile(dbPath, []byte{}, 0644); err != nil {
		t.Fatal(err)
	}

	const port = 19503 // nothing listens here
	cfg := testConfig(config.ShardConfig{Name: "hc", Database: dbPath, Port: port})
	cfg.Listener.HealthInterval = 100 * time.Millisecond
	sup := New(cfg, testLogger())
	t.Cleanup(sup.StopAll)

	// Disable the per-shard boot grace for this test: it exercises the
	// restart/backoff math in isolation, not the slow-boot tolerance (which
	// has its own test below).
	sup.startupGrace = 0

	if err := sup.startShardLocked(context.Background(), cfg.Shards[0]); err != nil {
		t.Fatalf("startShardLocked: %v", err)
	}

	sup.checkAll(context.Background())
	sp := sup.shards["hc"]
	if sp.Status != "starting" || sp.Restarts != 1 {
		t.Fatalf("after first failure: status=%q restarts=%d, want starting/1", sp.Status, sp.Restarts)
	}
	b1 := sp.backoff

	// Immediately retrying must be suppressed by the backoff window.
	sup.checkAll(context.Background())
	sp = sup.shards["hc"]
	if sp.Restarts != 1 || sp.backoff != b1 || sp.Status != "unhealthy" {
		t.Errorf("restart should be suppressed within the window: restarts=%d backoff=%v status=%q, want 1/%v/unhealthy",
			sp.Restarts, sp.backoff, sp.Status, b1)
	}

	// Once the window elapses, the next failure must restart again and double
	// the backoff.
	sup.mu.Lock()
	sp.lastUnhealthy = time.Time{}
	sup.mu.Unlock()
	sup.checkAll(context.Background())
	sp = sup.shards["hc"]
	if sp.Restarts != 2 {
		t.Fatalf("second failure should restart, got %d restarts", sp.Restarts)
	}
	if sp.backoff != b1*2 || sp.backoff > maxBackoff {
		t.Errorf("backoff = %v, want 2x previous (%v) capped at %v", sp.backoff, b1*2, maxBackoff)
	}

	// A healthy response must clear the streak.
	sup.mu.Lock()
	sp.Status = "healthy"
	sp.lastUnhealthy = time.Time{}
	sp.backoff = initialBackoff
	sp.Restarts = 0
	sup.mu.Unlock()
}

// TestCheckAllParksAfterMaxRestarts verifies a persistently-failing shard is
// stopped after maxRestarts attempts instead of restarting forever.
func TestCheckAllParksAfterMaxRestarts(t *testing.T) {
	tmp := t.TempDir()
	dbPath := filepath.Join(tmp, "park.db")
	if err := os.WriteFile(dbPath, []byte{}, 0644); err != nil {
		t.Fatal(err)
	}

	const port = 19504 // nothing listens here
	cfg := testConfig(config.ShardConfig{Name: "park", Database: dbPath, Port: port})
	sup := New(cfg, testLogger())
	t.Cleanup(sup.StopAll)

	// Inject a shard that has already exhausted its retry budget. The fake
	// duckdb ("true") exits immediately, so no real children accumulate.
	sup.ManualSetShard("park", ShardProcess{
		Config:   cfg.Shards[0],
		Status:   "unhealthy",
		Restarts: maxRestarts - 1,
	})

	sup.checkAll(context.Background())
	sp := sup.shards["park"]
	if sp.Status != "stopped" || sp.Restarts != maxRestarts {
		t.Fatalf("after exhausting retries: status=%q restarts=%d, want stopped/%d",
			sp.Status, sp.Restarts, maxRestarts)
	}
}

// TestCheckAllRespectsStartupGrace verifies a shard that is still inside its
// startup grace (slow INSTALL/LOAD before quack_serve binds) is NOT restarted
// while unhealthy, but is restarted once the grace has elapsed.
func TestCheckAllRespectsStartupGrace(t *testing.T) {
	tmp := t.TempDir()
	dbPath := filepath.Join(tmp, "grace.db")
	if err := os.WriteFile(dbPath, []byte{}, 0644); err != nil {
		t.Fatal(err)
	}

	const port = 19505 // nothing listens here
	cfg := testConfig(config.ShardConfig{Name: "grace", Database: dbPath, Port: port})
	cfg.Listener.HealthInterval = 100 * time.Millisecond
	sup := New(cfg, testLogger())
	t.Cleanup(sup.StopAll)
	sup.startupGrace = 5 * time.Second

	if err := sup.startShardLocked(context.Background(), cfg.Shards[0]); err != nil {
		t.Fatalf("startShardLocked: %v", err)
	}

	// First failure while inside the grace window: no restart, status stays
	// "starting" so the dashboard shows a boot in progress.
	sup.checkAll(context.Background())
	sp := sup.shards["grace"]
	if sp.Restarts != 0 || sp.Status != "starting" {
		t.Fatalf("inside startup grace: restarts=%d status=%q, want 0/starting", sp.Restarts, sp.Status)
	}

	// The grace window has now elapsed: the next failure must restart.
	sup.mu.Lock()
	sp.lastStart = time.Now().Add(-10 * time.Second)
	sp.lastUnhealthy = time.Time{}
	sup.mu.Unlock()

	sup.checkAll(context.Background())
	sp = sup.shards["grace"]
	if sp.Restarts != 1 {
		t.Fatalf("after the grace elapses: restarts=%d, want 1", sp.Restarts)
	}
}

func TestLookupBin(t *testing.T) {
	if _, ok := lookupBin("sh"); !ok {
		t.Error("lookupBin(\"sh\") should find the shell on PATH")
	}
	if _, ok := lookupBin("quack-proxy-definitely-not-a-real-binary-xyz"); ok {
		t.Error("lookupBin of a nonexistent binary must report not found")
	}
}

func TestStartShardLockedRejectsMissingBin(t *testing.T) {
	tmp := t.TempDir()
	dbPath := filepath.Join(tmp, "bin.db")
	if err := os.WriteFile(dbPath, []byte{}, 0644); err != nil {
		t.Fatal(err)
	}

	cfg := testConfig(config.ShardConfig{Name: "bin", Database: dbPath, Port: 19506})
	cfg.Global.DuckDBBin = "quack-proxy-definitely-not-a-real-binary-xyz"
	sup := New(cfg, testLogger())

	if err := sup.startShardLocked(context.Background(), cfg.Shards[0]); err == nil {
		t.Fatal("expected an error for a missing duckdb_bin, got nil")
	}
	if len(sup.shards) != 0 {
		t.Error("no shard may be registered when the engine is missing")
	}
}

func TestStopAllCleansUp(t *testing.T) {
	cfg := testConfig()
	sup := New(cfg, testLogger())

	// StopAll on empty supervisor should not panic
	sup.StopAll()

	if sup.cancel != nil {
		t.Error("expected cancel to be nil after start without StartAll")
	}
}

func TestRandomTokenUniqueness(t *testing.T) {
	seen := make(map[string]bool, 1000)
	for i := 0; i < 1000; i++ {
		tok := randomToken(32)
		if len(tok) != 32 {
			t.Fatalf("token length = %d, want 32", len(tok))
		}
		if seen[tok] {
			t.Fatalf("token collision among 1000 generations: %s", tok)
		}
		seen[tok] = true
	}
}
