//go:build linux && cgo

// server_linux.go - cgo 桥:把 C 实现的 epoll 服务端(csrc/server.c)接进单二进制。
//
// 仅 Linux 构建参与(epoll 是 Linux 独有);server.c 全部符号为 static,
// 经前导 #include 编入 cgo 生成的编译单元,不产生包级符号。
// CFLAGS 说明:-O2 保证 accept 循环性能;server.c 本身 -Wall -Wextra 干净。

package main

/*
#cgo CFLAGS: -O2 -D_GNU_SOURCE=1
#cgo LDFLAGS: -pthread

#include <stdlib.h>

#include "csrc/server.c"
*/
import "C"

import (
	"fmt"
	"unsafe"
)

// runServer 进入 C 服务端主循环(等价旧版 ./server),阻塞至 SIGINT/SIGTERM。
// pass 非空时经 setter 注入 C 侧堆内存(不进 argv,
// /proc/<pid>/cmdline 不落密码 —— ps aux 不可见)。
func runServer(ports []string, pass string) error {
	if len(ports) == 0 {
		return fmt.Errorf("服务器模式需要至少一个端口: tcp-stress -s <port1> [port2] ...")
	}
	if len(ports) > 64 {
		return fmt.Errorf("too many ports, limit=64")
	}
	if err := validatePass(pass); err != nil {
		return fmt.Errorf("-pass %v", err)
	}
	for _, p := range ports {
		if !isValidPort(p) {
			return fmt.Errorf("invalid port: %s", p)
		}
	}

	// C 侧沿用旧版 argv 约定:argv[0]=程序名,端口从 argv[1] 起
	// (密码不走 argv,见 tcp_server_set_pass)
	argv := make([]*C.char, 0, len(ports)+1)
	argv = append(argv, C.CString("tcp-stress"))
	for _, p := range ports {
		argv = append(argv, C.CString(p))
	}
	defer func() {
		for _, a := range argv {
			C.free(unsafe.Pointer(a))
		}
	}()

	if pass != "" {
		cPass := C.CString(pass)
		defer C.free(unsafe.Pointer(cPass))
		C.tcp_server_set_pass(cPass)
	}

	rc := C.tcp_server_main(C.int(len(argv)), (**C.char)(unsafe.Pointer(&argv[0])))
	if rc != 0 {
		return fmt.Errorf("server exited with code %d", int(rc))
	}
	return nil
}
