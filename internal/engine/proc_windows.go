//go:build windows

package engine

import "syscall"

// procAttr returns nil on Windows (no process groups via Setpgid).
func procAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{}
}

// killProcessGroup on Windows just kills the process (no group support).
func killProcessGroup(pid int) {
	// Windows doesn't support process groups the same way.
	// The process will be killed via cmd.Process.Kill() in Stop().
}
