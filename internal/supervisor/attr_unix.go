//go:build !windows

package supervisor

import "syscall"

func setProcessGroup() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setpgid: true}
}
