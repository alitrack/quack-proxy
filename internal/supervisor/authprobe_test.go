package supervisor

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/alitrack/quack-proxy/internal/config"
)

// stubEngine writes a fake duckdb binary in one of three shapes:
//
//	"echo"   — prints its arguments (so the random probe marker comes back)
//	"refuse" — prints an auth refusal and exits 1, the broken-engine shape
//	"silent" — exits 0 with no output, a serve that answers nothing
func stubEngine(t *testing.T, mode string) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "duckdb")
	var script string
	switch mode {
	case "echo":
		script = "#!/bin/sh\necho \"$@\"\nexit 0\n"
	case "refuse":
		script = "#!/bin/sh\necho 'Invalid Input Error: Authentication failed'\nexit 1\n"
	case "silent":
		script = "#!/bin/sh\nexit 0\n"
	default:
		t.Fatalf("unknown stub mode %q", mode)
	}
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatalf("write stub: %v", err)
	}
	return bin
}

func probeSupervisor(t *testing.T, mode string) (*Supervisor, *ShardProcess) {
	t.Helper()
	cfg := &config.Config{}
	cfg.Global.DuckDBBin = stubEngine(t, mode)
	cfg.Listener.BindHost = "127.0.0.1"
	sup := New(cfg, testLogger())
	return sup, &ShardProcess{Config: config.ShardConfig{Name: "s", Port: 9491, Token: "tok"}}
}

// TestProbeAuthAcceptsValue covers the usable engine: the client round-trip
// echoes the probe marker, so the shard counts as serving.
func TestProbeAuthAcceptsValue(t *testing.T) {
	sup, sp := probeSupervisor(t, "echo")
	if err := sup.probeAuth(sp); err != nil {
		t.Fatalf("probeAuth() = %v, want nil when the marker comes back", err)
	}
}

// TestProbeAuthRejectsRefusal covers the trap this probe exists for: the
// engine listens, and the client is refused. A port-level health check would
// call that shard healthy.
func TestProbeAuthRejectsRefusal(t *testing.T) {
	sup, sp := probeSupervisor(t, "refuse")
	err := sup.probeAuth(sp)
	if err == nil {
		t.Fatal("probeAuth() = nil, want an error when the client is refused")
	}
	if !strings.Contains(err.Error(), "Authentication failed") {
		t.Errorf("error should carry the client's refusal, got %v", err)
	}
}

// TestProbeAuthRejectsSilence covers a client that exits 0 but returns no
// value: exit status alone is not proof the round-trip worked.
func TestProbeAuthRejectsSilence(t *testing.T) {
	sup, sp := probeSupervisor(t, "silent")
	if err := sup.probeAuth(sp); err == nil {
		t.Fatal("probeAuth() = nil, want an error when no value comes back")
	}
}

// TestHealthStatusRequiresProbe pins the rule that a listening port alone is
// not "healthy": no attach string may be published before the probe passes.
func TestHealthStatusRequiresProbe(t *testing.T) {
	cases := []struct{ probe, want string }{
		{"", "starting"},
		{"ok", "healthy"},
		{"failed: Authentication failed", statusAuthFailed},
	}
	for _, c := range cases {
		sp := &ShardProcess{AuthProbe: c.probe}
		if got := healthStatus(sp); got != c.want {
			t.Errorf("healthStatus(auth_probe=%q) = %q, want %q", c.probe, got, c.want)
		}
	}
}

// TestAuthProbeFailureMarksShard covers the status transition: a failed
// probe must leave the shard in the loud auth-failed state, strip the attach
// string, and never be reported healthy afterwards.
func TestAuthProbeFailureMarksShard(t *testing.T) {
	cfg := testConfig()
	cfg.Global.StatusFile = filepath.Join(t.TempDir(), "status.json")
	sup := New(cfg, testLogger())
	sp := &ShardProcess{Config: config.ShardConfig{Name: "s", Port: 9491, Token: "tok"}, Status: "starting"}
	sup.shards["s"] = sp

	sup.setAuthProbe("s", sp, "failed: Authentication failed")

	if sp.Status != statusAuthFailed {
		t.Errorf("status = %q, want %q", sp.Status, statusAuthFailed)
	}
	sf, err := ReadStatusFile(cfg.Global.StatusFile)
	if err != nil {
		t.Fatalf("read status file: %v", err)
	}
	if len(sf.Shards) != 1 {
		t.Fatalf("status shards = %d, want 1", len(sf.Shards))
	}
	if !strings.HasPrefix(sf.Shards[0].AuthProbe, "failed") {
		t.Errorf("auth_probe = %q, want a failed prefix", sf.Shards[0].AuthProbe)
	}
	if sf.Shards[0].AttachSQL != "" {
		t.Errorf("attach sql = %q, want empty for an unusable shard", sf.Shards[0].AttachSQL)
	}
}
