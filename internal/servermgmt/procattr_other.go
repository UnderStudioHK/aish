//go:build !linux

package servermgmt

import "syscall"

func childAttrs() *syscall.SysProcAttr { return nil }
