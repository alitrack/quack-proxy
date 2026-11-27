//go:build !windows

package supervisor

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/alitrack/quack-proxy/internal/config"
	"github.com/alitrack/quack-proxy/internal/health"
)

// This is the end-to-end suite the original project never had (R-15). It
// forks a REAL child process group that serves real HTTP, and drives the
// supervisor through start -> healthy -> external kill -> self-heal ->
// graceful stop, checking that no orphan survives.
//
// It is deliberately NOT a DuckDB/Quack protocol test: the stub answers the
// health probe but speaks no Quack. Loading the real extension and verifying
// token auth still requires a pinned DuckDB build (tracked in REMEDIATION.md
// as the remaining Quack E2E gap). What this test does prove is the process
// lifecycle, the restart state machine, the health path and status output
// against an actual OS process rather than a `duckdb_bin: true` stub.

const stubGreeting = "quack-greeting-stub"

// TestHelperProcess is re-executed by the test binary itself (see
// quackStubScript). It only activates when the marker env var is set, so it
// is a no-op during the normal test run.
func TestHelperProcess(t *testing.T) {
	if os.Getenv("GO_WANT_HELPER_PROCESS") != "1" {
		return
	}
	port := os.Getenv("QUACK_STUB_PORT")
	if os.Getenv("QUACK_STUB_IGNORE_TERM") == "1" {
		// Simulate an engine that ignores SIGTERM while shutting down. The
		// bash leader exits immediately, so cmd.Wait() returns while this
		// listener still holds the port (R-21).
		signal.Ignore(syscall.SIGTERM)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = io.WriteString(w, stubGreeting)
	})
	srv := &http.Server{Addr: net.JoinHostPort("127.0.0.1", port), Handler: mux}
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		fmt.Fprintf(os.Stderr, "stub server: %v\n", err)
		os.Exit(1)
	}
	os.Exit(0)
}

func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("pick a free port: %v", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	if err := ln.Close(); err != nil {
		t.Fatalf("close probe listener: %v", err)
	}
	return port
}

func waitEndpoint(t *testing.T, port int, want bool, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		got := health.CheckWithExpect("127.0.0.1", port, "/", 300*time.Millisecond, stubGreeting)
		if got == want {
			return
		}
		time.Sleep(80 * time.Millisecond)
	}
	t.Fatalf("endpoint 127.0.0.1:%d did not reach reachable=%v within %v", port, want, timeout)
}

func TestIntegrationRealProcessLifecycle(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test skipped in -short mode")
	}
	bin, err := os.Executable()
	if err != nil {
		t.Fatalf("locate test binary: %v", err)
	}
	t.Setenv("QUACK_STUB_BIN", bin)

	tmp := t.TempDir()
	port := freePort(t)
	// The port is encoded in the database filename so the stub (which is
	// invoked as `<duckdb_bin> <database>`) knows where to listen.
	dbPath := filepath.Join(tmp, fmt.Sprintf("shard-%d.db", port))
	if err := os.WriteFile(dbPath, []byte{}, 0600); err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(tmp, "quack-stub.sh")
	stubScript := `#!/bin/sh
export GO_WANT_HELPER_PROCESS=1
export QUACK_STUB_PORT="$(basename "$1" | tr -cd '0-9')"
exec "$QUACK_STUB_BIN" -test.run='^TestHelperProcess$' -test.timeout=5m
`
	if err := os.WriteFile(script, []byte(stubScript), 0700); err != nil {
		t.Fatal(err)
	}

	cfg := &config.Config{
		Global: config.GlobalConfig{
			DuckDBBin:  script,
			StatusFile: filepath.Join(tmp, "status.json"),
		},
		Listener: config.ListenerConfig{
			BindHost:       "127.0.0.1",
			HealthPath:     "/",
			HealthInterval: time.Second,
			// Pin the response identity: an unrelated 2xx server on this port
			// must NOT be accepted as a healthy shard (R-09).
			HealthExpect: stubGreeting,
		},
		Shards: []config.ShardConfig{{Name: "it", Database: dbPath, Port: port}},
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	sup := New(cfg, logger)
	sup.startupGrace = 0
	t.Cleanup(sup.StopAll)

	if err := sup.StartAll(context.Background()); err != nil {
		t.Fatalf("StartAll with a real child process: %v", err)
	}
	waitEndpoint(t, port, true, 15*time.Second)

	// The stub engine cannot answer a real quack_query round-trip, so the
	// background probe cannot pass here. Simulate a passed probe (what the
	// daemon sees with a real engine) before judging the health decision.
	sup.mu.Lock()
	sup.shards["it"].AuthProbe = "ok"
	sup.mu.Unlock()

	sup.checkAll(context.Background())
	if got := sup.Status()[0].Status; got != "healthy" {
		t.Fatalf("status after a real healthy probe = %q, want healthy", got)
	}
	sf, err := ReadStatusFile(cfg.Global.StatusFile)
	if err != nil || len(sf.Shards) != 1 || sf.Shards[0].Status != "healthy" {
		t.Fatalf("status file does not reflect the live shard: %+v, err=%v", sf, err)
	}

	oldPID := sup.Status()[0].PID
	if oldPID <= 0 {
		t.Fatal("no real child PID recorded")
	}

	// Simulate an operator `kill -9` of the whole shard process group.
	if err := syscall.Kill(-oldPID, syscall.SIGKILL); err != nil {
		t.Fatalf("kill shard process group: %v", err)
	}
	waitEndpoint(t, port, false, 10*time.Second)

	// Clear the backoff so the very next tick performs the restart.
	sup.mu.Lock()
	sp := sup.shards["it"]
	sp.lastUnhealthy = time.Time{}
	sp.backoff = 0
	sup.mu.Unlock()

	sup.checkAll(context.Background())
	waitEndpoint(t, port, true, 15*time.Second)

	sup.mu.RLock()
	healed := *sup.shards["it"]
	sup.mu.RUnlock()
	if healed.PID == oldPID {
		t.Error("self-heal did not fork a new process group")
	}
	if healed.Restarts != 1 {
		t.Errorf("restarts = %d, want 1 after one crash", healed.Restarts)
	}
	if healed.Status != "starting" && healed.Status != "healthy" {
		t.Errorf("unexpected status after self-heal: %q", healed.Status)
	}

	newPID := healed.PID
	sup.StopAll()
	waitEndpoint(t, port, false, 10*time.Second)

	// The whole group (leader + descendants) must be gone, not just the
	// wrapper. A leaked `tail -f` would keep the port and the group alive.
	deadline := time.Now().Add(3 * time.Second)
	for {
		if err := signalProcessGroup(newPID, 0); err != nil {
			break
		}
		if time.Now().After(deadline) {
			t.Error("process group still alive after StopAll (orphan leaked)")
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	for _, s := range sup.Status() {
		if s.Status != "stopped" {
			t.Errorf("shard %s status = %q after StopAll, want stopped", s.Config.Name, s.Status)
		}
	}
}

// R-21: cmd.Wait() only reaps the group leader. If a descendant ignores
// SIGTERM (or is simply slower to exit), the daemon used to log "shard
// stopped" while the port was still bound. stopShard must confirm the WHOLE
// group is gone and SIGKILL any survivor.
func TestStopAllReapsLingeringDescendants(t *testing.T) {
	t.Setenv("QUACK_STUB_IGNORE_TERM", "1")
	bin, err := os.Executable()
	if err != nil {
		t.Fatalf("locate test binary: %v", err)
	}
	t.Setenv("QUACK_STUB_BIN", bin)

	tmp := t.TempDir()
	port := freePort(t)
	dbPath := filepath.Join(tmp, fmt.Sprintf("shard-%d.db", port))
	if err := os.WriteFile(dbPath, []byte{}, 0600); err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(tmp, "quack-stub.sh")
	stubScript := `#!/bin/sh
export GO_WANT_HELPER_PROCESS=1
export QUACK_STUB_PORT="$(basename "$1" | tr -cd '0-9')"
exec "$QUACK_STUB_BIN" -test.run='^TestHelperProcess$' -test.timeout=5m
`
	if err := os.WriteFile(script, []byte(stubScript), 0700); err != nil {
		t.Fatal(err)
	}

	cfg := &config.Config{
		Global:   config.GlobalConfig{DuckDBBin: script},
		Listener: config.ListenerConfig{BindHost: "127.0.0.1", HealthPath: "/", HealthExpect: stubGreeting},
		Shards:   []config.ShardConfig{{Name: "linger", Database: dbPath, Port: port}},
	}
	sup := New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err := sup.StartAll(context.Background()); err != nil {
		t.Fatalf("StartAll: %v", err)
	}
	waitEndpoint(t, port, true, 15*time.Second)
	oldPID := sup.Status()[0].PID

	sup.StopAll()

	// No polling here on purpose: once StopAll returns, the port MUST be free.
	conn, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", fmt.Sprint(port)), 200*time.Millisecond)
	if err == nil {
		conn.Close()
		t.Error("port still bound after StopAll: a descendant survived (R-21)")
	}
	if groupAlive(oldPID) {
		t.Error("process group still alive after StopAll (R-21)")
	}
	for _, sp := range sup.Status() {
		if sp.Status != "stopped" {
			t.Errorf("shard status = %q after StopAll, want stopped", sp.Status)
		}
	}
}

// R-07: a reload that changes a shard's port must really tear down the old
// listener and bind the new one. The startFn-based unit tests can only prove
// the diff logic; this one drives actual OS process groups.
func TestIntegrationReloadSwitchesRealPort(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test skipped in -short mode")
	}
	bin, err := os.Executable()
	if err != nil {
		t.Fatalf("locate test binary: %v", err)
	}
	t.Setenv("QUACK_STUB_BIN", bin)

	tmp := t.TempDir()
	oldPort := freePort(t)
	newPort := freePort(t)
	for newPort == oldPort && newPort < 65535 {
		newPort = freePort(t)
	}
	if newPort == oldPort {
		t.Skip("could not obtain two distinct free ports")
	}
	oldDB := filepath.Join(tmp, fmt.Sprintf("shard-%d.db", oldPort))
	newDB := filepath.Join(tmp, fmt.Sprintf("shard-%d.db", newPort))
	for _, p := range []string{oldDB, newDB} {
		if err := os.WriteFile(p, []byte{}, 0600); err != nil {
			t.Fatal(err)
		}
	}
	script := filepath.Join(tmp, "quack-stub.sh")
	stubScript := `#!/bin/sh
export GO_WANT_HELPER_PROCESS=1
export QUACK_STUB_PORT="$(basename "$1" | tr -cd '0-9')"
exec "$QUACK_STUB_BIN" -test.run='^TestHelperProcess$' -test.timeout=5m
`
	if err := os.WriteFile(script, []byte(stubScript), 0700); err != nil {
		t.Fatal(err)
	}

	newConfig := func(port int, db string) *config.Config {
		return &config.Config{
			Global: config.GlobalConfig{
				DuckDBBin:  script,
				StatusFile: filepath.Join(tmp, "status.json"),
			},
			Listener: config.ListenerConfig{
				BindHost:       "127.0.0.1",
				HealthPath:     "/",
				HealthInterval: time.Second,
				HealthExpect:   stubGreeting,
			},
			Shards: []config.ShardConfig{{Name: "it", Database: db, Port: port}},
		}
	}

	sup := New(newConfig(oldPort, oldDB), slog.New(slog.NewTextHandler(io.Discard, nil)))
	sup.startupGrace = 0
	t.Cleanup(sup.StopAll)
	if err := sup.StartAll(context.Background()); err != nil {
		t.Fatalf("StartAll: %v", err)
	}
	waitEndpoint(t, oldPort, true, 15*time.Second)
	oldPID := sup.Status()[0].PID

	added, removed, restarted := sup.ApplyConfig(context.Background(), newConfig(newPort, newDB))
	if len(added) != 0 || len(removed) != 0 || len(restarted) != 1 {
		t.Fatalf("added=%v removed=%v restarted=%v, want only one restart", added, removed, restarted)
	}
	waitEndpoint(t, oldPort, false, 10*time.Second)
	waitEndpoint(t, newPort, true, 15*time.Second)
	if got := sup.Status()[0].PID; got == oldPID {
		t.Error("reload did not fork a new process group for the changed port")
	}
	if got := sup.Status()[0].Config.Port; got != newPort {
		t.Errorf("live shard port = %d, want %d", got, newPort)
	}

	sup.StopAll()
	waitEndpoint(t, newPort, false, 10*time.Second)
}

// R-22: a second daemon must NOT start on a port still held by the first
// daemon's shard. This is the "crashed daemon left an orphan" scenario from
// scripts/daemon-crash-drill.sh, reproduced with real process groups.
func TestIntegrationSecondDaemonRefusesOrphanedPort(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test skipped in -short mode")
	}
	bin, err := os.Executable()
	if err != nil {
		t.Fatalf("locate test binary: %v", err)
	}
	t.Setenv("QUACK_STUB_BIN", bin)

	tmp := t.TempDir()
	port := freePort(t)
	dbPath := filepath.Join(tmp, fmt.Sprintf("shard-%d.db", port))
	if err := os.WriteFile(dbPath, []byte{}, 0600); err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(tmp, "quack-stub.sh")
	stubScript := `#!/bin/sh
export GO_WANT_HELPER_PROCESS=1
export QUACK_STUB_PORT="$(basename "$1" | tr -cd '0-9')"
exec "$QUACK_STUB_BIN" -test.run='^TestHelperProcess$' -test.timeout=5m
`
	if err := os.WriteFile(script, []byte(stubScript), 0700); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{
		Global: config.GlobalConfig{
			DuckDBBin:  script,
			StatusFile: filepath.Join(tmp, "status.json"),
		},
		Listener: config.ListenerConfig{
			BindHost:       "127.0.0.1",
			HealthPath:     "/",
			HealthInterval: time.Second,
			HealthExpect:   stubGreeting,
		},
		Shards: []config.ShardConfig{{Name: "orphan", Database: dbPath, Port: port}},
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	first := New(cfg, logger)
	first.startupGrace = 0
	t.Cleanup(first.StopAll)
	if err := first.StartAll(context.Background()); err != nil {
		t.Fatalf("first daemon StartAll: %v", err)
	}
	waitEndpoint(t, port, true, 15*time.Second)

	// Simulate a NEW daemon booting while the old (orphaned) shard still
	// owns the port. It must fail loudly instead of reporting healthy.
	second := New(cfg, logger)
	second.startupGrace = 0
	t.Cleanup(second.StopAll)
	err = second.StartAll(context.Background())
	if err == nil {
		t.Fatal("second daemon started on an orphaned port (false healthy status)")
	}
	if !strings.Contains(err.Error(), "already occupied") {
		t.Errorf("error does not explain the port conflict: %v", err)
	}
	// The original shard must be untouched.
	waitEndpoint(t, port, true, 3*time.Second)
}
