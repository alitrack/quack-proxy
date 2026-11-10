//go:build windows

package supervisor

import (
	"os"
	"syscall"
)

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
