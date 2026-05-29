package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadValidConfig(t *testing.T) {
	tmp := t.TempDir()
	dbPath := filepath.Join(tmp, "test.db")
	if err := os.WriteFile(dbPath, []byte{}, 0644); err != nil {
		t.Fatal(err)
	}

	yaml := `
global:
  log_level: debug
listener:
  port_start: 9600
  health_interval: 10s
shards:
  - name: shard1
    database: ` + dbPath + `
    token: secret123
  - name: shard2
    database: ` + dbPath + `
`
	cfgPath := filepath.Join(tmp, "config.yaml")
	if err := os.WriteFile(cfgPath, []byte(yaml), 0644); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(cfgPath)
	if err != nil {
		t.Fatalf("Load(): %v", err)
	}

	if cfg.Global.LogLevel != "debug" {
		t.Errorf("log_level = %q, want %q", cfg.Global.LogLevel, "debug")
	}

	if cfg.Listener.PortStart != 9600 {
		t.Errorf("port_start = %d, want 9600", cfg.Listener.PortStart)
	}

	if cfg.Listener.HealthInterval.String() != "10s" {
		t.Errorf("health_interval = %v, want 10s", cfg.Listener.HealthInterval)
	}

	if len(cfg.Shards) != 2 {
		t.Fatalf("shards count = %d, want 2", len(cfg.Shards))
	}

	if cfg.Shards[0].Token != "secret123" {
		t.Errorf("shard[0].token = %q, want %q", cfg.Shards[0].Token, "secret123")
	}

	if cfg.Shards[0].Port != 9600 {
		t.Errorf("shard[0].port = %d, want 9600", cfg.Shards[0].Port)
	}

	if cfg.Shards[1].Port != 9601 {
		t.Errorf("shard[1].port = %d, want 9601", cfg.Shards[1].Port)
	}
}

func TestDefaults(t *testing.T) {
	tmp := t.TempDir()
	dbPath := filepath.Join(tmp, "test.db")
	if err := os.WriteFile(dbPath, []byte{}, 0644); err != nil {
		t.Fatal(err)
	}

	yaml := `
shards:
  - name: s1
    database: ` + dbPath + `
`
	cfgPath := filepath.Join(tmp, "config.yaml")
	if err := os.WriteFile(cfgPath, []byte(yaml), 0644); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(cfgPath)
	if err != nil {
		t.Fatalf("Load(): %v", err)
	}

	if cfg.Global.LogLevel != "info" {
		t.Errorf("default log_level = %q, want info", cfg.Global.LogLevel)
	}

	if cfg.Listener.BindHost != "0.0.0.0" {
		t.Errorf("default bind_host = %q, want 0.0.0.0", cfg.Listener.BindHost)
	}

	if cfg.Listener.PortStart != 9491 {
		t.Errorf("default port_start = %d, want 9491", cfg.Listener.PortStart)
	}

	if cfg.Listener.HealthPath != "/" {
		t.Errorf("default health_path = %q, want /", cfg.Listener.HealthPath)
	}

	if cfg.Listener.HealthInterval.String() != "5s" {
		t.Errorf("default health_interval = %v, want 5s", cfg.Listener.HealthInterval)
	}
}

func TestValidationErrors(t *testing.T) {
	tmp := t.TempDir()
	dbPath := filepath.Join(tmp, "test.db")
	if err := os.WriteFile(dbPath, []byte{}, 0644); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name    string
		yaml    string
		wantErr string
	}{
		{
			name:    "no shards",
			yaml:    `shards: []`,
			wantErr: "at least one shard is required",
		},
		{
			name: "missing name",
			yaml: `
shards:
  - database: ` + dbPath,
			wantErr: "name is required",
		},
		{
			name: "invalid port",
			yaml: `
shards:
  - name: s1
    database: ` + dbPath + `
    port: 99999`,
			wantErr: "invalid port",
		},
		{
			name: "duplicate port",
			yaml: `
shards:
  - name: s1
    database: ` + dbPath + `
    port: 9500
  - name: s2
    database: ` + dbPath + `
    port: 9500`,
			wantErr: "duplicate port",
		},
		{
			name: "nonexistent db",
			yaml: `
shards:
  - name: s1
    database: /nonexistent/path.db`,
			wantErr: "database file not found",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfgPath := filepath.Join(tmp, "config.yaml")
			os.WriteFile(cfgPath, []byte(tt.yaml), 0644)

			_, err := Load(cfgPath)
			if err == nil {
				t.Fatal("expected error, got nil")
			}
			errStr := err.Error()
			if !contains(errStr, tt.wantErr) {
				t.Errorf("error %q does not contain %q", errStr, tt.wantErr)
			}
		})
	}
}

func TestProxyDefaults(t *testing.T) {
	tmp := t.TempDir()
	dbPath := filepath.Join(tmp, "test.db")
	if err := os.WriteFile(dbPath, []byte{}, 0644); err != nil {
		t.Fatal(err)
	}

	yaml := `
shards:
  - name: s1
    database: ` + dbPath + `
proxy:
  enabled: true
  output: /tmp/haproxy.cfg
`
	cfgPath := filepath.Join(tmp, "config.yaml")
	if err := os.WriteFile(cfgPath, []byte(yaml), 0644); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(cfgPath)
	if err != nil {
		t.Fatalf("Load(): %v", err)
	}

	if cfg.Proxy == nil {
		t.Fatal("proxy config is nil")
	}

	if cfg.Proxy.Mode != "roundrobin" {
		t.Errorf("proxy mode = %q, want roundrobin", cfg.Proxy.Mode)
	}
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(s) > len(substr) &&
		(s[:len(substr)] == substr || s[len(s)-len(substr):] == substr ||
			searchSubstring(s, substr)))
}

func searchSubstring(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
