package supervisor

import (
	"context"
	"io"
	"log/slog"
	"net"
	"os"
	"strings"
	"testing"

	"github.com/alitrack/quack-proxy/internal/config"
)

// R-22: scripts/daemon-crash-drill.sh proved that after a daemon SIGKILL the
// orphaned shard keeps the port, and a naive restart reports "healthy"
// because the ORPHAN answers the probe. startShard must refuse to start on
// an occupied port instead of manufacturing a false healthy status.

func TestPortInUseDetectsListener(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	if !portInUse("127.0.0.1", port) {
		t.Error("an active listener was not detected on its port")
	}
	if err := ln.Close(); err != nil {
		t.Fatal(err)
	}
	if portInUse("127.0.0.1", port) {
		t.Error("a closed listener is still reported as occupied (TIME_WAIT must not trigger a false positive)")
	}
}

func TestStartShardRejectsOccupiedPort(t *testing.T) {
	// r9: lookupBin now uses a platform-aware executableFile(), so this no
	// longer needs the Windows skip. startShard stops at the port check,
	// before any POSIX-only fork would happen.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	port := ln.Addr().(*net.TCPAddr).Port

	bin, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	sh := config.ShardConfig{Name: "orphan", Database: "/tmp/orphan.db", Port: port}
	cfg := reloadTestConfig(t, sh)
	cfg.Global.DuckDBBin = bin // a real executable, so lookupBin passes
	sup := New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))

	_, err = sup.startShard(context.Background(), sh)
	if err == nil {
		t.Fatal("startShard accepted an occupied port; a restarted daemon would report a false healthy status")
	}
	if !strings.Contains(err.Error(), "already occupied") || !strings.Contains(err.Error(), "crashed daemon") {
		t.Errorf("error does not explain the orphan/port condition: %v", err)
	}
}
