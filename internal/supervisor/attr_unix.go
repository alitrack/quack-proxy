//go:build !windows

package supervisor

import (
	"os"
	"syscall"
)

// executableFile reports whether a directory entry can be executed. On
// POSIX the mode bits are authoritative; a file without any x bit is not an
// executable even if it has a familiar name.
func executableFile(fi os.FileInfo) bool {
	return !fi.IsDir() && fi.Mode()&0o111 != 0
}

// setProcessGroup puts each shard's whole process tree (the bash wrapper and
// every descendant, including duckdb and the `tail -f /dev/null` keep-alive)
// into its own process group so the group can be signalled as a unit. The
// group id equals the leader's pid (the bash wrapper), which is also
// cmd.Process.Pid.
func setProcessGroup() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setpgid: true}
}

// signalProcessGroup delivers sig to the entire process group led by pid.
// Signalling only the leader (cmd.Process) would kill the bash wrapper while
// its children survive, keep the port bound, and leak.
func signalProcessGroup(pid int, sig syscall.Signal) error {
	return syscall.Kill(-pid, sig)
}

// groupAlive reports whether the process group still has members. A zero
// signal only probes for existence; it never delivers anything.
func groupAlive(pid int) bool {
	return syscall.Kill(-pid, 0) == nil
}
