package supervisor

import (
	"os"
	"strings"
	"testing"
	"time"
)

// PRD §4.2 / R-05: status.json must not carry a shard credential by default.
// The 0600 mode stays (defence in depth), but the secret itself is absent.
// These tests read the RAW file, not just the decoded struct, so a stray
// "token": "" or a leftover value from a previous write cannot slip through.

func TestStatusFileOmitsTokenByDefault(t *testing.T) {
	cfg := gate0Config(t)
	sup := New(cfg, gate0Logger())
	sup.ManualSetShard("shard_a", ShardProcess{
		Config:    cfg.Shards[0],
		Status:    "healthy",
		StartTime: time.Now(),
		lastStart: time.Now(),
	})

	sup.WriteStatusFile()

	raw, err := os.ReadFile(cfg.Global.StatusFile)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), `"token"`) {
		t.Fatalf("status.json still contains a token field by default: %s", raw)
	}
	if strings.Contains(string(raw), "test-token") {
		t.Fatalf("status.json leaked the shard credential by default: %s", raw)
	}
	info, err := os.Stat(cfg.Global.StatusFile)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0600 {
		t.Errorf("status file mode = %04o, want 0600 (unchanged)", perm)
	}
	sf, err := ReadStatusFile(cfg.Global.StatusFile)
	if err != nil {
		t.Fatal(err)
	}
	if len(sf.Shards) != 1 || sf.Shards[0].Token != "" {
		t.Fatalf("decoded status must have no token: %+v", sf.Shards)
	}
	if !strings.Contains(sf.Shards[0].AttachSQL, "127.0.0.1") {
		t.Errorf("removing the token must not break the dialable ATTACH SQL: %q", sf.Shards[0].AttachSQL)
	}
}

func TestStatusFilePersistsTokenOnlyWithOptIn(t *testing.T) {
	cfg := gate0Config(t)
	cfg.Global.PersistTokens = true
	sup := New(cfg, gate0Logger())
	sup.ManualSetShard("shard_a", ShardProcess{
		Config:    cfg.Shards[0],
		Status:    "healthy",
		StartTime: time.Now(),
		lastStart: time.Now(),
	})

	sup.WriteStatusFile()

	raw, err := os.ReadFile(cfg.Global.StatusFile)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "test-token") {
		t.Fatalf("explicit opt-in must restore the 0600-persisted token: %s", raw)
	}
	info, err := os.Stat(cfg.Global.StatusFile)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0600 {
		t.Errorf("opt-in status file mode = %04o, want 0600", perm)
	}
}

func TestTurningTheSwitchOffRemovesPreviouslyPersistedToken(t *testing.T) {
	cfg := gate0Config(t)
	cfg.Global.PersistTokens = true
	sup := New(cfg, gate0Logger())
	sup.ManualSetShard("shard_a", ShardProcess{
		Config:    cfg.Shards[0],
		Status:    "healthy",
		StartTime: time.Now(),
		lastStart: time.Now(),
	})
	sup.WriteStatusFile()

	// The operator removes the opt-in; the very next status write must not
	// leave the old credential behind on disk.
	cfg.Global.PersistTokens = false
	sup.WriteStatusFile()

	raw, err := os.ReadFile(cfg.Global.StatusFile)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "test-token") || strings.Contains(string(raw), `"token"`) {
		t.Fatalf("old credential survived after the opt-in was removed: %s", raw)
	}
}
