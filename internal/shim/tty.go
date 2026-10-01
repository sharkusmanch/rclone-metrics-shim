package shim

import (
	"os"
	"syscall"
	"unsafe"
)

// inForegroundOfTTY reports whether the shim's process group is the
// foreground group of its controlling terminal. When it is, the terminal
// delivers Ctrl-C and Ctrl-\ to the child directly (same group), so relaying
// them as well would hit the child twice.
func inForegroundOfTTY() bool {
	tty, err := os.OpenFile("/dev/tty", os.O_RDONLY|syscall.O_NOCTTY, 0)
	if err != nil {
		return false // no controlling terminal (cron, containers)
	}
	defer tty.Close()
	var pgrp int32
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, tty.Fd(), uintptr(syscall.TIOCGPGRP), uintptr(unsafe.Pointer(&pgrp)))
	return errno == 0 && int(pgrp) == syscall.Getpgrp()
}
