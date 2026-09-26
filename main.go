// main.go - tcp-conn-stress 单二进制入口
//
// v1.0.1 起 server(C/epoll)与 client(Go)合并为一个二进制 tcp-stress:
//   tcp-stress -s <port1> [port2] ...              服务器模式(cgo 调用 C 实现,仅 Linux)
//   tcp-stress -c [-servers ...] [-target N] ...   客户端模式(纯 Go)
//
// - 非 Linux 平台的构建不含 C 服务端,-s 会明确报错(见 server_stub.go)
// - 两种模式互斥;不带模式参数打印用法并退出

package main

import (
	"flag"
	"fmt"
	"os"
	"time"
)

func main() {
	var (
		serverMode bool
		clientMode bool
		cfg        clientConfig
	)
	flag.BoolVar(&serverMode, "s", false, "服务器模式:位置参数为监听端口列表")
	flag.BoolVar(&clientMode, "c", false, "客户端模式")
	flag.StringVar(&cfg.servers, "servers", "127.0.0.1:8888", "客户端:目标地址列表,逗号分隔,格式 IP:Port")
	flag.StringVar(&cfg.bind, "bind", "", "客户端:本地出口 IP(多 WAN/策略路由时指定)")
	flag.Uint64Var(&cfg.target, "target", 10000, "客户端:目标总连接数(到达后保持)")
	flag.IntVar(&cfg.rate, "rate", 200, "客户端:每秒建连速率上限")
	flag.DurationVar(&cfg.keepAlive, "keepalive", 30*time.Second, "客户端:TCP KeepAlive 间隔")
	flag.DurationVar(&cfg.statsInt, "stats", 1*time.Second, "客户端:统计打印周期")
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, `tcp-stress — 家用宽带极限 TCP 长连接压测(单二进制)

用法:
  tcp-stress -s <port1> [port2] ...          服务器模式(仅 Linux 构建)
  tcp-stress -c [客户端参数...]               客户端模式

示例:
  tcp-stress -s 8888 8889 8890
  tcp-stress -c -servers "192.168.1.100:8888,192.168.1.100:8889" -target 100000 -rate 200

参数:
`)
		flag.PrintDefaults()
	}
	flag.Parse()

	switch {
	case serverMode && clientMode:
		fmt.Fprintln(os.Stderr, "error: -s 与 -c 不能同时使用")
		os.Exit(2)
	case serverMode:
		if err := runServer(flag.Args()); err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
	case clientMode:
		runClient(cfg)
	default:
		flag.Usage()
		os.Exit(2)
	}
}
