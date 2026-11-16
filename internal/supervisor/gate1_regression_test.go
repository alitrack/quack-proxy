package supervisor

import (
	"strings"
	"testing"

	"github.com/alitrack/quack-proxy/internal/config"
)

// Gate 1 regression suite: security-relevant defaults must be observable in
// the generated boot SQL and shell command, without forking a real process.
// See docs/REMEDIATION.md R-05 / R-14.

func TestGate1BootSQLDefaultsToStrictHostname(t *testing.T) {
	sh := config.ShardConfig{Name: "s1", Database: "/x.db", Port: 9491}
	got, err := buildBootSQL("127.0.0.1", sh, "tk", false)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "allow_other_hostname = false") {
		t.Errorf("Host-header check must be strict by default, got:/n%s", got)
	}
	optIn, err := buildBootSQL("127.0.0.1", sh, "tk", true)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(optIn, "allow_other_hostname = true") {
		t.Errorf("explicit opt-in must be honoured, got:/n%s", optIn)
	}
}

func TestGate1UnsignedFlagIsOptIn(t *testing.T) {
	cfg := &config.Config{Global: config.GlobalConfig{DuckDBBin: "duckdb"}}
	sh := config.ShardConfig{Name: "s1", Database: "/tmp/x.db", Port: 9491}

	cmd := buildShardCommand(cfg, sh, "/tmp/boot.sql")
	if strings.Contains(cmd, "-unsigned") {
		t.Errorf("-unsigned must not be enabled by default: %s", cmd)
	}
	if !strings.Contains(cmd, "'duckdb' '/tmp/x.db'") {
		t.Errorf("signed launch line malformed: %s", cmd)
	}

	cfg.Global.AllowUnsignedExtensions = true
	cmd = buildShardCommand(cfg, sh, "/tmp/boot.sql")
	if !strings.Contains(cmd, "-unsigned") {
		t.Errorf("explicit opt-in must add -unsigned: %s", cmd)
	}
}
