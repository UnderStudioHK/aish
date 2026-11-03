//go:build linux

package servermgmt

import "syscall"

// childAttrs makes the model server die with aish (no orphan servers).
func childAttrs() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Pdeathsig: syscall.SIGTERM}
}
