// Package config parses and validates quack-proxy.yaml.
package config

import (
	"fmt"
	"os"
	"time"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Global   GlobalConfig    `yaml:"global"`
	Listener ListenerConfig  `yaml:"listener"`
	Shards   []ShardConfig   `yaml:"shards"`
	Proxy    *ProxyConfig     `yaml:"proxy"`
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
	ReadOnly bool   `yaml:"readonly"`
	// InitSQL runs BEFORE quack_serve in the shard's DuckDB process.
	// Use it to install/load extra extensions and register server-side
	// hooks, e.g. the luajit RBAC policy engine:
	//
	//   init_sql: |
	//     INSTALL luajit FROM community; LOAD luajit;
	//     SET GLOBAL quack_authentication_function = 'rbac_authn';
	//
	// NOTE: init_sql is piped through a shell echo, so keep it to
	// single-line statements. For multi-line payloads (e.g. a Lua policy
	// source with comments), put the SQL in a file and use init_sql_file.
	InitSQL string `yaml:"init_sql"`
	// InitSQLFile is a path to a .sql file whose contents are used as
	// init_sql (file contents are piped verbatim; init_sql, when both are
	// set, runs first).
	InitSQLFile string `yaml:"init_sql_file"`
}

type ProxyConfig struct {
	Enabled   bool       `yaml:"enabled"`
	Output    string     `yaml:"output"`
	BindPort  int        `yaml:"bind_port"`
	Mode      string     `yaml:"mode"`
	SSL       *SSLConfig `yaml:"ssl"`
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

func (c *Config) Validate() error {
	if len(c.Shards) == 0 {
		return fmt.Errorf("at least one shard is required")
	}

	ports := make(map[int]bool)
	for i, s := range c.Shards {
		if s.Name == "" {
			return fmt.Errorf("shard[%d]: name is required", i)
		}
		if s.Database == "" {
			return fmt.Errorf("shard[%d] (%s): database path is required", i, s.Name)
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
	}

	return nil
}
