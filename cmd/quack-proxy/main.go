package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/alitrack/quack-proxy/internal/config"
	"github.com/alitrack/quack-proxy/internal/proxy"
	"github.com/alitrack/quack-proxy/internal/supervisor"
)

// version is overridable at build time:
//
//	go build -ldflags "-X main.version=v1.2.3" ./cmd/quack-proxy
var version = "dev"

func main() {
	flag.Usage = usage
	flag.Parse()

	args := flag.Args()
	if len(args) < 1 {
		usage()
		os.Exit(1)
	}

	cfgPath := findConfigArg(args)

	switch args[0] {
	case "start":
		runStart(cfgPath)
	case "stop":
		runStop(cfgPath)
	case "status":
		runStatus(cfgPath)
	case "reload":
		runReload(cfgPath)
	case "gen-proxy":
		runGenProxy(cfgPath)
	case "version":
		fmt.Println("quack-proxy " + version)
	default:
		fmt.Fprintf(os.Stderr, "unknown command: %s\n", args[0])
		usage()
		os.Exit(1)
	}
}

// windowsUnsupportedMessage is emitted by `start` on Windows. Before r9 the
// binary printed "duckdb_bin ... not found on PATH or not executable" (a
// POSIX-mode-bit false negative) or died inside a missing bash, making the
// published artifact look merely misconfigured instead of unsupported.
const windowsUnsupportedMessage = `quack-proxy: Windows is not a supported runtime platform (R-18).
Shard hosting needs a POSIX shell (bash, cat, tail -f /dev/null), /dev/null
and process-group semantics; the Windows build provides none of them.
Use the Linux or macOS binary, or run the Linux binary under WSL2.
See docs/SCOPE.md and docs/REMEDIATION.md (R-18).`

// runtimeSupported is split out so the platform policy can be unit-tested on
// any host (CI itself runs on Linux).
func runtimeSupported(goos string) bool {
	return goos != "windows"
}

func findConfigArg(args []string) string {
	for i, a := range args {
		if a == "-c" && i+1 < len(args) {
			return args[i+1]
		}
		if strings.HasPrefix(a, "-c=") {
			return strings.TrimPrefix(a, "-c=")
		}
	}
	return "quack-proxy.yaml"
}

func usage() {
	fmt.Fprint(os.Stderr, `quack-proxy — DuckDB Quack cluster manager

Usage:
  quack-proxy start [-c config.yaml]     Start daemon
  quack-proxy stop [-c config.yaml]       Stop daemon
  quack-proxy status [--json] [--show-tokens] [-c config.yaml]
                                        Show shard status (tokens redacted by default)
  quack-proxy reload [-c config.yaml]     Hot-reload configuration
  quack-proxy gen-proxy [-c config.yaml]  Generate HAProxy config
  quack-proxy version                     Print version
`)
}

func runStart(cfgPath string) {
	// Fail with the real reason instead of letting a Windows user chase a
	// bogus "duckdb_bin not executable" or a silently dead shard.
	if !runtimeSupported(runtime.GOOS) {
		fmt.Fprintln(os.Stderr, windowsUnsupportedMessage)
		os.Exit(1)
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to load config: %v\n", err)
		os.Exit(1)
	}
	logger := newLogger(cfg.Global.LogLevel)
	if err := writePID(cfg.Global.PIDFile); err != nil {
		logger.Error("failed to write pid file", "error", err)
		os.Exit(1)
	}
	sup := supervisor.New(cfg, logger)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := sup.StartAll(ctx); err != nil {
		logger.Error("failed to start shards; cleaning up any that started", "error", err)
		sup.StopAll()
		os.Remove(cfg.Global.PIDFile)
		os.Remove(cfg.Global.StatusFile)
		os.Exit(1)
	}
	go sup.HealthLoop(ctx)
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	for sig := range sigCh {
		switch sig {
		case syscall.SIGHUP:
			logger.Info("reloading config")
			newCfg, err := config.Load(cfgPath)
			if err != nil {
				logger.Error("reload failed; keeping current configuration", "error", err)
				continue
			}
			added, removed, restarted := sup.ApplyConfig(ctx, newCfg)
			cfg = newCfg // keep shutdown cleanup pointing at the live paths
			logger.Info("config reloaded", "added", len(added), "removed", len(removed), "restarted", len(restarted))
			// R-07 (partial): keep the generated LB config in sync with the
			// reloaded shard list. HAProxy itself must still be reloaded by
			// the operator; quack-proxy does not manage that process.
			if newCfg.Proxy != nil && newCfg.Proxy.Enabled && newCfg.Proxy.Output != "" {
				if err := proxy.GenerateHAProxy(newCfg, newCfg.Proxy.Output); err != nil {
					logger.Warn("reload: HAProxy config not regenerated", "error", err)
				} else {
					logger.Warn("reload: HAProxy config regenerated; reload HAProxy manually to apply", "path", newCfg.Proxy.Output)
				}
			}
		default:
			logger.Info("shutting down", "signal", sig)
			cancel()
			sup.StopAll()
			os.Remove(cfg.Global.PIDFile)
			os.Remove(cfg.Global.StatusFile)
			return
		}
	}
}

func runStop(cfgPath string) {
	pid, ok := runningPID(getPIDFile(cfgPath))
	if !ok {
		fmt.Fprintln(os.Stderr, "quack-proxy is not running (no live PID file)")
		os.Exit(1)
	}
	proc, err := os.FindProcess(pid)
	if err != nil || proc.Signal(syscall.SIGTERM) != nil {
		fmt.Fprintf(os.Stderr, "failed to send SIGTERM to pid %d: %v\n", pid, err)
		os.Exit(1)
	}
	fmt.Printf("sent SIGTERM to quack-proxy (pid %d)\n", pid)
}

// statusJSON is the documented shape of `status --json` (PRD §5.1): the
// daemon writes this data to its status file every health-check cycle.
type statusJSON struct {
	Shards               []shardJSON `json:"shards"`
	CoordinatorAttachSQL string      `json:"coordinator_attach_sql"`
}

type shardJSON struct {
	Name            string `json:"name"`
	Port            int    `json:"port"`
	Status          string `json:"status"`
	PID             int    `json:"pid"`
	Uptime          string `json:"uptime"`
	Restarts        int    `json:"restarts"`
	Token           string `json:"token,omitempty"`
	Database        string `json:"database"`
	LastHealthCheck string `json:"last_health_check,omitempty"`
}

// buildStatus maps the on-disk status file to the CLI's output shape. Tokens
// are redacted unless explicitly requested: the tokens are the ONLY
// authentication credential a Quack endpoint has, and `status` is runnable by
// any local user able to read the config (R-05).
func buildStatus(sf *supervisor.StatusFile, showTokens bool) statusJSON {
	var out statusJSON
	for _, r := range sf.Shards {
		sj := shardJSON{
			Name:            r.Name,
			Port:            r.Port,
			Status:          r.Status,
			PID:             r.PID,
			Restarts:        r.Restarts,
			Database:        r.Database,
			LastHealthCheck: r.LastCheck,
		}
		if showTokens {
			sj.Token = r.Token
		}
		if start, err := time.Parse(time.RFC3339, r.StartTime); err == nil {
			sj.Uptime = time.Since(start).Round(time.Second).String()
		}
		out.Shards = append(out.Shards, sj)
		if r.AttachSQL != "" {
			out.CoordinatorAttachSQL += r.AttachSQL + "\n"
		}
	}
	return out
}

// tokenPersistenceNotice returns the stderr explanation for a --show-tokens
// request on a status file that carries no persisted credentials. Keeping it
// a pure function lets the CLI regression suite assert that the operator is
// told WHY the field is empty (and how to opt back in), rather than seeing a
// silent blank that looks like a broken command.
func tokenPersistenceNotice(sf *supervisor.StatusFile, showTokens bool) string {
	if !showTokens {
		return ""
	}
	missing := 0
	for _, r := range sf.Shards {
		if r.Token == "" {
			missing++
		}
	}
	if missing == 0 {
		return ""
	}
	return fmt.Sprintf(
		"quack-proxy: %d shard token(s) are not persisted (global.persist_tokens is false by default, PRD §4.2). "+
			"Set an explicit token in the config to manage it yourself, or set global.persist_tokens: true to allow 0600-persisted tokens in status.json.\n",
		missing)
}

func runStatus(cfgPath string) {
	cfg, err := config.Load(cfgPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "config error: %v\n", err)
		os.Exit(1)
	}
	sf, err := supervisor.ReadStatusFile(cfg.Global.StatusFile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "quack-proxy is not running (no status file at %s)\n", cfg.Global.StatusFile)
		os.Exit(1)
	}

	jsonFlag, showTokens := false, false
	for _, arg := range flag.Args() {
		switch arg {
		case "--json":
			jsonFlag = true
		case "--show-tokens":
			showTokens = true
		}
	}
	// PRD §4.2: with the default global.persist_tokens=false the status file
	// carries no tokens at all. --show-tokens then has nothing to reveal; say
	// so on stderr (never inside the JSON stream) instead of silently
	// returning empty strings that look like a broken command.
	if notice := tokenPersistenceNotice(sf, showTokens); notice != "" {
		fmt.Fprint(os.Stderr, notice)
	}
	out := buildStatus(sf, showTokens)
	if jsonFlag {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		enc.Encode(out)
		return
	}

	fmt.Printf("%-16s %-6s %-10s %-10s %-8s %s\n", "NAME", "PORT", "STATUS", "UPTIME", "RESTARTS", "DATABASE")
	for _, s := range out.Shards {
		fmt.Printf("%-16s %-6d %-10s %-10s %-8d %s\n",
			s.Name, s.Port, s.Status, s.Uptime, s.Restarts, s.Database)
	}
}

func runReload(cfgPath string) {
	pid, ok := runningPID(getPIDFile(cfgPath))
	if !ok {
		fmt.Fprintln(os.Stderr, "quack-proxy is not running (no live PID file)")
		os.Exit(1)
	}
	proc, err := os.FindProcess(pid)
	if err != nil || proc.Signal(syscall.SIGHUP) != nil {
		fmt.Fprintf(os.Stderr, "failed to send SIGHUP to pid %d: %v\n", pid, err)
		os.Exit(1)
	}
	fmt.Printf("sent SIGHUP to quack-proxy (pid %d)\n", pid)
}

func runGenProxy(cfgPath string) {
	cfg, err := config.Load(cfgPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "config error: %v\n", err)
		os.Exit(1)
	}
	if cfg.Proxy == nil || cfg.Proxy.Output == "" {
		fmt.Fprintln(os.Stderr, "proxy.output not configured")
		os.Exit(1)
	}
	if err := proxy.GenerateHAProxy(cfg, cfg.Proxy.Output); err != nil {
		fmt.Fprintf(os.Stderr, "generate failed: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("HAProxy config written to %s\n", cfg.Proxy.Output)
}

// writePrivateFile writes data to path with 0600 via a temp file + rename,
// so readers never see a partial file and an existing world-readable file is
// replaced rather than silently kept (R-11).
func writePrivateFile(path string, data []byte) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".quack-proxy-*.tmp")
	if err != nil {
		return err
	}
	tmp := f.Name()
	if _, err := f.Write(data); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Chmod(tmp, 0600); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

func writePID(path string) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return fmt.Errorf("create pid dir %s: %w", dir, err)
	}
	return writePrivateFile(path, []byte(fmt.Sprintf("%d\n", os.Getpid())))
}

func getPIDFile(cfgPath string) string {
	cfg, _ := config.Load(cfgPath)
	if cfg != nil {
		return cfg.Global.PIDFile
	}
	return "/var/run/quack-proxy/quack-proxy.pid"
}

// runningPID loads the daemon pid from the pid file and reports whether that
// process is alive. A missing file, garbage contents, a non-positive pid
// (signalling pid 0 hits the caller's own process group), or a dead pid all
// mean "not running".
func runningPID(pidFile string) (int, bool) {
	data, err := os.ReadFile(pidFile)
	if err != nil {
		return 0, false
	}
	var pid int
	if _, err := fmt.Sscanf(strings.TrimSpace(string(data)), "%d", &pid); err != nil || pid <= 0 {
		return 0, false
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return 0, false
	}
	err = proc.Signal(syscall.Signal(0))
	if err == nil {
		return pid, true
	}
	if errno, ok := err.(syscall.Errno); ok && errno == syscall.EPERM {
		return pid, true // alive, but owned by another user
	}
	return 0, false // ESRCH: no such process
}

func levelFromString(level string) slog.Level {
	switch level {
	case "debug":
		return slog.LevelDebug
	case "warn":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

func newLogger(level string) *slog.Logger {
	var w io.Writer = os.Stdout
	return slog.New(slog.NewTextHandler(w, &slog.HandlerOptions{Level: levelFromString(level)}))
}
