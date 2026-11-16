//go:build windows

package supervisor

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// executableFile reports whether a directory entry can be launched on
// Windows. Windows has no POSIX mode bits: os.Stat returns 0666/0444 for
// ordinary files, so the old `mode&0o111` check rejected EVERY configured
// engine, including perfectly valid .exe paths. Executability is determined
// by the extension listed in PATHEXT instead. Note this only fixes the
// lookup/diagnosis path; the daemon still refuses to run on Windows (R-18)
// because shard hosting needs a POSIX shell and process-group semantics.
func executableFile(fi os.FileInfo) bool {
	if fi.IsDir() {
		return false
	}
	ext := strings.ToLower(filepath.Ext(fi.Name()))
	if ext == "" {
		return false
	}
	for _, allowed := range filepath.SplitList(os.Getenv("PATHEXT")) {
		if allowed != "" && strings.EqualFold(ext, strings.ToLower(allowed)) {
			return true
		}
	}
	// PATHEXT is normally set, but a stripped environment must not turn a
	// valid .exe into "not executable".
	return ext == ".exe" || ext == ".com" || ext == ".cmd" || ext == ".bat"
}

// setProcessGroup is a no-op on Windows: there is no process-group id to
// target, so stopShardLocked falls back to signalling the wrapper alone.
func setProcessGroup() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{}
}

// signalProcessGroup has no group semantics on Windows; it signals the
// wrapper process itself.
func signalProcessGroup(pid int, sig syscall.Signal) error {
	proc, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	return proc.Signal(sig)
}

// groupAlive cannot be implemented without Windows process-group semantics.
// Returning false keeps stopShard's confirmation step a no-op there; Windows
// support is a documented non-goal (R-18).
func groupAlive(pid int) bool {
	return false
}
