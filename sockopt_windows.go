//go:build windows

package main

import "syscall"

// setSmallBuf 在已建立但尚未握手的 socket 上压小读写缓冲。
// Windows(Winsock)的 setsockopt 取 socket 句柄为 syscall.Handle。
func setSmallBuf(fd uintptr) {
	h := syscall.Handle(fd)
	_ = syscall.SetsockoptInt(h, syscall.SOL_SOCKET, syscall.SO_RCVBUF, 2048)
	_ = syscall.SetsockoptInt(h, syscall.SOL_SOCKET, syscall.SO_SNDBUF, 2048)
	_ = syscall.SetsockoptInt(h, syscall.IPPROTO_TCP, syscall.TCP_NODELAY, 1)
}
