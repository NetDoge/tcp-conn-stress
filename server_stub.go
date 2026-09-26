//go:build !linux || !cgo

// server_stub.go - 非 Linux 平台 / 未开 cgo 的构建里,服务端不可用。
//
// epoll 是 Linux 独有,darwin/windows 包只含客户端;
// 此桩保证 -s 给出明确报错,而不是符号缺失导致编译失败。
// (CGO_ENABLED=0 的 Linux 构建同样落在这里:server_linux.go 带 linux && cgo 标签被剔除)

package main

import "fmt"

func runServer(ports []string) error {
	return fmt.Errorf("服务器模式(-s)仅在 Linux 的 cgo 构建中可用;此二进制只支持客户端模式(-c)")
}
