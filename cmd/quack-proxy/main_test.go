package main

import (
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"

	"github.com/alitrack/quack-proxy/internal/supervisor"
)

func TestFindConfigArg(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want string
	}{
		{
			name: "no flags",
			args: []string{"start"},
			want: "quack-proxy.yaml",
		},
		{
			name: "space form",
			args: []string{"start", "-c", "/etc/quack.yaml"},
			want: "/etc/quack.yaml",
		},
		{
			name: "equals form",
			args: []string{"start", "-c=/etc/quack.yaml"},
			want: "/etc/quack.yaml",
		},
		{
			name: "equals form no value",
			args: []string{"start", "-c="},
			want: "",
		},
		{
			name: "other flags ignored",
			args: []string{"start", "--verbose", "-c", "mycfg.yaml"},
			want: "mycfg.yaml",
		},
		{
			name: "status with config",
			args: []string{"status", "--json", "-c", "cfg.yaml"},
			want: "cfg.yaml",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := findConfigArg(tt.args)
			if got != tt.want {
				t.Errorf("findConfigArg(%v) = %q, want %q", tt.args, got, tt.want)
			}
		})
	}
}

func TestWritePIDFile(t *testing.T) {
	tmp := t.TempDir()
	pidFile := filepath.Join(tmp, "quack-proxy.pid")

	if err := writePID(pidFile); err != nil {
		t.Fatalf("writePID: %v", err)
	}

	data, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatalf("read pid file: %v", err)
	}

	// The pid file must contain exactly this process's pid, newline-terminated.
	want := fmt.Sprintf("%d\n", os.Getpid())
	if string(data) != want {
		t.Errorf("pid file = %q, want %q", data, want)
	}
}

// TestWritePIDCustomName verifies the pid file's directory is derived with
// filepath.Dir, not by slicing off the default "quack-proxy.pid" suffix. The
// old slicing corrupted any non-default name (e.g. "my-pid" -> "my-pi").
func TestWritePIDCustomName(t *testing.T) {
	tmp := t.TempDir()
	pidFile := filepath.Join(tmp, "my-pid")

	if err := writePID(pidFile); err != nil {
		t.Fatalf("writePID: %v", err)
	}
	if _, err := os.Stat(pidFile); err != nil {
		t.Fatalf("expected pid file at %s: %v", pidFile, err)
	}
	// No spurious "my-pi" directory should have been created.
	if _, err := os.Stat(filepath.Join(tmp, "my-pi")); !os.IsNotExist(err) {
		t.Errorf("a mangled 'my-pi' path should not exist: %v", err)
	}
}

func TestRunningPID(t *testing.T) {
	tmp := t.TempDir()

	// No file at all.
	if _, ok := runningPID(filepath.Join(tmp, "nope.pid")); ok {
		t.Error("expected not-running for missing pid file")
	}

	// Garbage / zero / negative pids must be rejected — signalling pid 0
	// would target the caller's own process group.
	for _, content := range []string{"", "0\n", "-1\n", "abc\n"} {
		p := filepath.Join(tmp, "bad.pid")
		if err := os.WriteFile(p, []byte(content), 0644); err != nil {
			t.Fatal(err)
		}
		if _, ok := runningPID(p); ok {
			t.Errorf("content %q should not report running", content)
		}
	}

	// signal(0) liveness probing and child reaping are POSIX-only. The
	// Windows build is a documented non-goal (R-18), so stop here on it.
	if runtime.GOOS == "windows" {
		t.Skip("signal(0) liveness probing is POSIX-only (R-18)")
	}

	// A dead but plausible pid is rejected (signal 0 -> ESRCH). Use a real
	// process that we start and then wait on, so the pid is genuinely gone.
	cmd := exec.Command("true")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	dead := cmd.Process.Pid
	cmd.Wait()
	p := filepath.Join(tmp, "dead.pid")
	os.WriteFile(p, []byte(strconv.Itoa(dead)+"\n"), 0644)
	if _, ok := runningPID(p); ok {
		t.Error("expected not-running for a dead pid")
	}

	// Our own pid is reported running.
	own := filepath.Join(tmp, "own.pid")
	os.WriteFile(own, []byte(fmt.Sprintf("%d\n", os.Getpid())), 0644)
	if got, ok := runningPID(own); !ok || got != os.Getpid() {
		t.Errorf("expected own pid %d running, got %d/%v", os.Getpid(), got, ok)
	}
}

func TestNewLogger(t *testing.T) {
	logger := newLogger("info")
	if logger == nil {
		t.Error("newLogger() returned nil")
	}
}

func TestLevelFromString(t *testing.T) {
	cases := []struct {
		in   string
		want slog.Level
	}{
		{"debug", slog.LevelDebug},
		{"info", slog.LevelInfo},
		{"warn", slog.LevelWarn},
		{"error", slog.LevelError},
		{"", slog.LevelInfo},         // default
		{"nonsense", slog.LevelInfo}, // unknown falls back to info
	}
	for _, c := range cases {
		if got := levelFromString(c.in); got != c.want {
			t.Errorf("levelFromString(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

// R-05: the token is the only Quack credential, so `status` must not print
// it unless the operator explicitly asks.
func TestBuildStatusRedactsTokens(t *testing.T) {
	sf := &supervisor.StatusFile{Shards: []supervisor.StatusFileRecord{
		{Name: "s1", Port: 9491, Status: "healthy", Token: "SUPER-SECRET", StartTime: "2026-11-13T00:00:00Z"},
	}}
	redacted := buildStatus(sf, false)
	if len(redacted.Shards) != 1 || redacted.Shards[0].Token != "" {
		t.Errorf("default status must not expose tokens: %+v", redacted.Shards)
	}
	shown := buildStatus(sf, true)
	if shown.Shards[0].Token != "SUPER-SECRET" {
		t.Errorf("--show-tokens must expose tokens, got %+v", shown.Shards)
	}
}

func TestWritePIDIsPrivate(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX permission bits are not enforced on Windows")
	}
	tmp := t.TempDir()
	pidFile := filepath.Join(tmp, "quack-proxy.pid")
	// Simulate a stale, world-readable pid file left by an older version:
	// rewriting it must not preserve the old 0644 mode.
	if err := os.WriteFile(pidFile, []byte("0\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := writePID(pidFile); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(pidFile)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0600 {
		t.Errorf("pid file mode = %04o, want 0600", perm)
	}
}

// R-18 (r9): the platform policy must be explicit and testable on any host,
// not buried in a runtime.GOOS check inside runStart.
func TestRuntimeSupported(t *testing.T) {
	for _, tc := range []struct {
		goos string
		want bool
	}{
		{"linux", true},
		{"darwin", true},
		{"windows", false},
	} {
		if got := runtimeSupported(tc.goos); got != tc.want {
			t.Errorf("runtimeSupported(%q) = %v, want %v", tc.goos, got, tc.want)
		}
	}
}

func TestWindowsUnsupportedMessageIsActionable(t *testing.T) {
	for _, want := range []string{"Windows", "not a supported runtime platform", "R-18", "WSL2"} {
		if !strings.Contains(windowsUnsupportedMessage, want) {
			t.Errorf("windows message missing %q", want)
		}
	}
}
