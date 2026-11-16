// Package config parses and validates quack-proxy.yaml.
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
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
	// AllowUnsignedExtensions re-enables DuckDB's -unsigned flag. It is off
	// by default: an unsigned extension can load arbitrary native code, so
	// only enable it after pre-installing and hash-verifying the extensions.
	AllowUnsignedExtensions bool `yaml:"allow_unsigned_extensions"`
	// PersistTokens opts in to writing shard tokens into status.json (0600).
	// PRD §4.2 (2026-11-13 revision) requires that tokens NOT be persisted
	// in cleartext by default, and that persistence be behind an explicit
	// switch. With the switch off every shard must carry an explicit token in
	// the config: an auto-generated token would otherwise be unrecoverable,
	// because status.json is the only out-of-process channel the daemon has.
	// The operator's own config/secret store is then the distribution channel.
	PersistTokens bool `yaml:"persist_tokens"`
}

type ListenerConfig struct {
	BindHost       string        `yaml:"bind_host"`
	PortStart      int           `yaml:"port_start"`
	HealthPath     string        `yaml:"health_path"`
	HealthInterval time.Duration `yaml:"health_interval"`
	// HealthExpect is an optional substring that the health response body
	// must contain. A bare 2xx from an unrelated HTTP server on the shard
	// port is otherwise a false positive (R-09). Operators who know their
	// Quack greeting can pin it here; empty keeps liveness-only behaviour.
	HealthExpect string `yaml:"health_expect"`
	// AllowOtherHostname relaxes Quack's Host-header check. Off by default;
	// enable only when clients reach a shard through a VIP or DNS name.
	AllowOtherHostname bool `yaml:"allow_other_hostname"`
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
	// Hardened emits `chroot` / `user` / `group` into the generated HAProxy
	// global section instead of merely documenting them as comments (R-23).
	// It is OFF by default on purpose: those directives require a host-local
	// `haproxy` account and a root-owned chroot directory. Enabling it without
	// running scripts/verify-hardening.sh first makes HAProxy refuse to start.
	Hardened bool `yaml:"hardened"`
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
		// Loopback by default: Quack speaks plaintext HTTP and the generated
		// token is the only credential. Reaching the LAN must be an explicit,
		// deliberate change (R-05).
		c.Listener.BindHost = "127.0.0.1"
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

// Shard names become ATTACH aliases, temp-file name components and HAProxy
// server names, so anything outside an identifier is unsafe (path traversal,
// SQL alias corruption, config-line injection). See R-13.
var validShardName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
var validBindHost = regexp.MustCompile(`^[A-Za-z0-9._:-]+$`)

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
	if c.Proxy != nil && c.Proxy.SSL != nil && c.Proxy.SSL.Enabled {
		// Fail loud instead of letting operators believe traffic is encrypted.
		// TLS termination belongs to the external proxy for now (R-05).
		return fmt.Errorf("proxy.ssl.enabled is not implemented: terminate TLS in an external proxy; refusing a config that only looks encrypted")
	}
	if len(c.Listener.HealthExpect) > 256 {
		return fmt.Errorf("listener.health_expect is too long (max 256 bytes)")
	}
	if !validBindHost.MatchString(c.Listener.BindHost) {
		return fmt.Errorf("listener.bind_host %q must be a plain hostname or IP (no userinfo, spaces or slashes)", c.Listener.BindHost)
	}
	// health_path is interpolated into a URL. Reject anything that could
	// change the scheme or host, e.g. "http://127.0.0.1:9491@evil.com/".
	hp := c.Listener.HealthPath
	if !strings.HasPrefix(hp, "/") || strings.ContainsAny(hp, " \t\r\n@\\") || strings.Contains(hp, "://") || strings.Contains(hp, "//") {
		return fmt.Errorf("listener.health_path %q must be a plain absolute path starting with '/' (no scheme, userinfo, whitespace or '//')", hp)
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
		if !validShardName.MatchString(s.Name) {
			return fmt.Errorf("shard[%d] (%s): name must match ^[A-Za-z_][A-Za-z0-9_]*$ (used as an ATTACH alias, temp-file name and HAProxy server name)", i, s.Name)
		}
		if s.Database == "" {
			return fmt.Errorf("shard[%d] (%s): database path is required", i, s.Name)
		}
		if hasQuoteOrNewline(s.Database) {
			return fmt.Errorf("shard[%d] (%s): database path may not contain a single quote or newline", i, s.Name)
		}
		// The .db file itself may not exist yet: DuckDB creates a brand-new
		// database on first open, so a fresh shard is legitimate. But its
		// parent directory must — DuckDB cannot create a file in a missing
		// directory, and a wrong directory is the common typo case. Fail
		// loud on a missing (or non-directory) parent.
		dbDir := filepath.Dir(s.Database)
		if fi, err := os.Stat(dbDir); err != nil || !fi.IsDir() {
			return fmt.Errorf("shard[%d] (%s): database directory not found: %s", i, s.Name, dbDir)
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

	// PRD §4.2: no cleartext token on disk by default. If the operator
	// declines the explicit persist_tokens switch, an omitted token cannot
	// be distributed at all (the daemon would generate one and then refuse
	// to write it anywhere), so fail loud at load time with two actionable
	// alternatives rather than starting an unreachable shard. This mirrors
	// the proxy.ssl.enabled fail-closed pattern. Kept AFTER the per-shard
	// checks so the existing validation error precedence is preserved.
	if !c.Global.PersistTokens {
		var missing []string
		for _, s := range c.Shards {
			if s.Token == "" {
				missing = append(missing, s.Name)
			}
		}
		if len(missing) > 0 {
			return fmt.Errorf("shards without an explicit token (%s) require global.persist_tokens: true: with the default (false) PRD §4.2 forbids persisting credentials in status.json, and an auto-generated token would be unrecoverable; either set an explicit token per shard (recommended) or set global.persist_tokens: true to allow 0600-persisted auto-generated tokens", strings.Join(missing, ", "))
		}
	}

	return nil
}
