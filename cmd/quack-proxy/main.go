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
var version = "v0.1.0"

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
  quack-proxy status [-c config.yaml]     Show shard status
  quack-proxy reload [-c config.yaml]     Hot-reload configuration
  quack-proxy gen-proxy [-c config.yaml]  Generate HAProxy config
  quack-proxy version                     Print version
`)
}

func runStart(cfgPath string) {
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
			added, removed := sup.ApplyConfig(ctx, newCfg)
			logger.Info("config reloaded", "added", len(added), "removed", len(removed))
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
	Token           string `json:"token"`
	Database        string `json:"database"`
	LastHealthCheck string `json:"last_health_check,omitempty"`
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

	var out statusJSON
	for _, r := range sf.Shards {
		sj := shardJSON{
			Name:            r.Name,
			Port:            r.Port,
			Status:          r.Status,
			PID:             r.PID,
			Restarts:        r.Restarts,
			Token:           r.Token,
			Database:        r.Database,
			LastHealthCheck: r.LastCheck,
		}
		if start, err := time.Parse(time.RFC3339, r.StartTime); err == nil {
			sj.Uptime = time.Since(start).Round(time.Second).String()
		}
		out.Shards = append(out.Shards, sj)
		if r.AttachSQL != "" {
			out.CoordinatorAttachSQL += r.AttachSQL + "\n"
		}
	}

	jsonFlag := false
	for _, arg := range flag.Args() {
		if arg == "--json" {
			jsonFlag = true
		}
	}
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

func writePID(path string) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("create pid dir %s: %w", dir, err)
	}
	return os.WriteFile(path, []byte(fmt.Sprintf("%d\n", os.Getpid())), 0644)
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
