//go:build !windows && !linux && !darwin

package main

import "os"

// interactiveTTY 兜底实现(无发布产物的其余 unix):
// stdin/stdout 均为字符设备即认为交互。不追求精确,只保证可编译。
func interactiveTTY() bool {
	for _, f := range []*os.File{os.Stdin, os.Stdout} {
		fi, err := f.Stat()
		if err != nil || fi.Mode()&os.ModeCharDevice == 0 {
			return false
		}
	}
	return true
}
