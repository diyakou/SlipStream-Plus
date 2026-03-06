//go:build !windows

package engine

import "syscall"

// procAttr returns SysProcAttr to create a new process group on Unix.
func procAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setpgid: true}
}

// killProcessGroup kills the entire process group on Unix.
func killProcessGroup(pid int) {
	syscall.Kill(-pid, syscall.SIGKILL)
}
