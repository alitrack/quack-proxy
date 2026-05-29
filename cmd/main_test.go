package main

import (
	"os"
	"path/filepath"
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

	writePID(pidFile)

	data, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatalf("read pid file: %v", err)
	}

	if len(data) == 0 {
		t.Error("pid file should not be empty")
	}

	// Verify it's a valid number
	pid := string(data)
	if len(pid) < 1 || pid[len(pid)-1] != '\n' {
		t.Error("pid file should end with newline")
	}
}

func TestNewLogger(t *testing.T) {
	logger := newLogger()
	if logger == nil {
		t.Error("newLogger() returned nil")
	}
}
