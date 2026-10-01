package shim

import "syscall"

// childSysProcAttr makes the kernel kill the child if the shim itself is
// killed outright (SIGKILL), so killing "rclone" still stops rclone.
func childSysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL}
}
