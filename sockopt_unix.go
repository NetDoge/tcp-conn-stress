//go:build !windows

package main

import "syscall"

// setSmallBuf 在已建立但尚未握手的 socket 上压小读写缓冲。
// Unix 系内核的 setsockopt 取 fd 为 int。
func setSmallBuf(fd uintptr) {
	f := int(fd)
	// 2K 读写缓冲,极致省内存
	_ = syscall.SetsockoptInt(f, syscall.SOL_SOCKET, syscall.SO_RCVBUF, 2048)
	_ = syscall.SetsockoptInt(f, syscall.SOL_SOCKET, syscall.SO_SNDBUF, 2048)
	// 关闭 Nagle:我们不持续发数据,避免小包被延迟
	_ = syscall.SetsockoptInt(f, syscall.IPPROTO_TCP, syscall.TCP_NODELAY, 1)
}
