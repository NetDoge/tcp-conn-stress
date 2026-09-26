//go:build windows

package main

import "os"

// interactiveTTY 判断"真人在控制台裸运行":
// stdin/stdout 均为字符设备且 CONIN$ 可打开(仅真实控制台会话)。
// 管道 / 重定向 / CI 不满足,仍走 usage exit 2。
func interactiveTTY() bool {
	sfi, err := os.Stdin.Stat()
	if err != nil || sfi.Mode()&os.ModeCharDevice == 0 {
		return false
	}
	ofi, err := os.Stdout.Stat()
	if err != nil || ofi.Mode()&os.ModeCharDevice == 0 {
		return false
	}
	f, err := os.Open("CONIN$")
	if err != nil {
		return false
	}
	_ = f.Close()
	return true
}
