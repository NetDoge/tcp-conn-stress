//go:build darwin

package main

import (
	"syscall"
	"unsafe"
)

// interactiveTTY 用 ioctl(TIOCGETA) 做 isatty 判定(同 linux 版,常量名不同)。
func isTTYFd(fd uintptr) bool {
	var t syscall.Termios
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, fd,
		uintptr(syscall.TIOCGETA), uintptr(unsafe.Pointer(&t)))
	return errno == 0
}

func interactiveTTY() bool { return isTTYFd(0) && isTTYFd(1) }
