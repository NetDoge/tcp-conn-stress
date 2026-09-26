//go:build linux

package main

import (
	"syscall"
	"unsafe"
)

// interactiveTTY 用 ioctl(TCGETS) 做 isatty 判定(与 x/term 同语义):
// fd 0 与 fd 1 都是终端才允许进入向导。管道、/dev/null、重定向文件、
// socket 均返回 ENOTTY。零外部依赖(stdlib syscall + unsafe)。
//
// 踩坑记录:曾尝试 Stat+ModeCharDevice(挡不住 /dev/null——它也是字符
// 设备)与 /dev/fd/N readlink 路径比对(打开 /dev/tty 的 fd 其 readlink
// 显示 "/dev/tty" 本身,内核不解析重定向设备,比对恒 false),全部证伪;
// ioctl 才是唯一可靠判据。
func isTTYFd(fd uintptr) bool {
	var t syscall.Termios
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, fd,
		uintptr(syscall.TCGETS), uintptr(unsafe.Pointer(&t)))
	return errno == 0
}

func interactiveTTY() bool { return isTTYFd(0) && isTTYFd(1) }
