//go:build windows

package main

// Windows 无 rlimit 体系;连接数上限由 Winsock 决定,不在此处理。
func raiseNofile() (soft, hard uint64) { return 0, 0 }
