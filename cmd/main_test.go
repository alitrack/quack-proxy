package main

import (
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
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
