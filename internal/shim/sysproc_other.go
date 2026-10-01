//go:build unix && !linux

package shim

import "syscall"

func childSysProcAttr() *syscall.SysProcAttr { return nil }
