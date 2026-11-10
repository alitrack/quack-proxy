// Package config parses and validates quack-proxy.yaml.
package config

import (
	"fmt"
	"os"
	"time"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Global   GlobalConfig   `yaml:"global"`
	Listener ListenerConfig `yaml:"listener"`
	Shards   []ShardConfig  `yaml:"shards"`
	Proxy    *ProxyConfig   `yaml:"proxy"`
}

type GlobalConfig struct {
	LogLevel   string `yaml:"log_level"`
	PIDFile    string `yaml:"pid_file"`
	StatusFile string `yaml:"status_file"`
	// DuckDBBin overrides the duckdb CLI binary used for shards.
	// Pin a known-good version: some alphas have broken quack token auth
	// (verified 2026-11-09: v2.0.0-alpha39998 rejects even the correct
	// server token; 2.0.0-alpha48730 works).
	DuckDBBin string `yaml:"duckdb_bin"`
}

type ListenerConfig struct {
	BindHost       string        `yaml:"bind_host"`
	PortStart      int           `yaml:"port_start"`
	HealthPath     string        `yaml:"health_path"`
	HealthInterval time.Duration `yaml:"health_interval"`
}

type ShardConfig struct {
	Name     string `yaml:"name"`
	Database string `yaml:"database"`
	Port     int    `yaml:"port"`
	Token    string `yaml:"token"`
	// InitSQL runs BEFORE quack_serve in the shard's DuckDB process.
	// Use it to install/load extra extensions and register server-side
	// hooks, e.g. the luajit RBAC policy engine:
	//
	//   init_sql: |
	//     INSTALL luajit FROM community; LOAD luajit;
	//     SET GLOBAL quack_authentication_function = 'rbac_authn';
	//
	// The SQL is delivered to the child verbatim through a temp file, so
	// multi-line bodies (Lua policies, dollar-quoted sources) are safe.
	InitSQL string `yaml:"init_sql"`
	// InitSQLFile is a path to a .sql file whose contents are appended
	// after init_sql (which runs first when both are set). The file must
	// exist: a missing file fails shard startup loudly.
	InitSQLFile string `yaml:"init_sql_file"`
}

type ProxyConfig struct {
	Enabled  bool       `yaml:"enabled"`
	Output   string     `yaml:"output"`
	BindPort int        `yaml:"bind_port"`
	Mode     string     `yaml:"mode"`
	SSL      *SSLConfig `yaml:"ssl"`
}

type SSLConfig struct {
	Enabled bool   `yaml:"enabled"`
	Cert    string `yaml:"cert"`
}

func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}

	cfg := &Config{}
	if err := yaml.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}

	cfg.applyDefaults()
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("validate config: %w", err)
	}

	return cfg, nil
}

func (c *Config) applyDefaults() {
	if c.Global.LogLevel == "" {
		c.Global.LogLevel = "info"
	}
	if c.Global.DuckDBBin == "" {
		c.Global.DuckDBBin = "duckdb"
	}
	if c.Global.PIDFile == "" {
		c.Global.PIDFile = "/var/run/quack-proxy/quack-proxy.pid"
	}
	if c.Global.StatusFile == "" {
		c.Global.StatusFile = "/var/run/quack-proxy/status.json"
	}
	if c.Listener.BindHost == "" {
		c.Listener.BindHost = "0.0.0.0"
	}
	if c.Listener.PortStart == 0 {
		c.Listener.PortStart = 9491
	}
	if c.Listener.HealthPath == "" {
		c.Listener.HealthPath = "/"
	}
	if c.Listener.HealthInterval == 0 {
		c.Listener.HealthInterval = 5 * time.Second
	}
	for i := range c.Shards {
		if c.Shards[i].Port == 0 {
			c.Shards[i].Port = c.Listener.PortStart + i
		}
	}
	if c.Proxy != nil && c.Proxy.Mode == "" {
		c.Proxy.Mode = "roundrobin"
	}
}

var validLogLevels = map[string]bool{"debug": true, "info": true, "warn": true, "error": true}
var validProxyModes = map[string]bool{"roundrobin": true, "leastconn": true}

// hasQuoteOrNewline reports whether s contains a single quote or a line
// break. Such values are interpolated by the supervisor into two kinds of
// single-quoted context — a `bash -c '…'` word and a SQL string literal
// (`token = '…'`, `quack:'…'`) — where a `'` escapes the quoting (a shell or
// SQL injection, and at minimum a broken boot) and a newline splits the
// command. Rejecting them at load time turns a footgun into a clear config
// error.
func hasQuoteOrNewline(s string) bool {
	for _, r := range s {
		if r == '\'' || r == '\n' || r == '\r' {
			return true
		}
	}
	return false
}

func (c *Config) Validate() error {
	if !validLogLevels[c.Global.LogLevel] {
		return fmt.Errorf("global.log_level %q is not one of debug, info, warn, error", c.Global.LogLevel)
	}
	if c.Listener.HealthInterval <= 0 {
		return fmt.Errorf("listener.health_interval must be positive")
	}
	if c.Proxy != nil && !validProxyModes[c.Proxy.Mode] {
		return fmt.Errorf("proxy.mode %q is not one of roundrobin, leastconn", c.Proxy.Mode)
	}

	// Fields that the supervisor interpolates into single-quoted shell words
	// and SQL string literals must not carry quotes or newlines.
	if hasQuoteOrNewline(c.Global.DuckDBBin) {
		return fmt.Errorf("global.duckdb_bin %q may not contain a single quote or newline", c.Global.DuckDBBin)
	}
	if hasQuoteOrNewline(c.Listener.BindHost) {
		return fmt.Errorf("listener.bind_host %q may not contain a single quote or newline", c.Listener.BindHost)
	}

	if len(c.Shards) == 0 {
		return fmt.Errorf("at least one shard is required")
	}

	ports := make(map[int]bool)
	names := make(map[string]bool)
	for i, s := range c.Shards {
		if s.Name == "" {
			return fmt.Errorf("shard[%d]: name is required", i)
		}
		if names[s.Name] {
			return fmt.Errorf("shard[%d]: duplicate name %q", i, s.Name)
		}
		names[s.Name] = true
		if hasQuoteOrNewline(s.Name) {
			return fmt.Errorf("shard[%d] (%s): name may not contain a single quote or newline", i, s.Name)
		}
		if s.Database == "" {
			return fmt.Errorf("shard[%d] (%s): database path is required", i, s.Name)
		}
		if hasQuoteOrNewline(s.Database) {
			return fmt.Errorf("shard[%d] (%s): database path may not contain a single quote or newline", i, s.Name)
		}
		if _, err := os.Stat(s.Database); os.IsNotExist(err) {
			return fmt.Errorf("shard[%d] (%s): database file not found: %s", i, s.Name, s.Database)
		}
		if s.Port <= 0 || s.Port > 65535 {
			return fmt.Errorf("shard[%d] (%s): invalid port %d", i, s.Name, s.Port)
		}
		if ports[s.Port] {
			return fmt.Errorf("shard[%d] (%s): duplicate port %d", i, s.Name, s.Port)
		}
		ports[s.Port] = true
		if hasQuoteOrNewline(s.Token) {
			return fmt.Errorf("shard[%d] (%s): token may not contain a single quote or newline", i, s.Name)
		}
	}

	return nil
}
