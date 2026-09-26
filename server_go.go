//go:build !linux || !cgo

// server_go.go - 纯 Go 服务端实现(darwin / windows,及未开 cgo 的构建)。
//
// v1.0.5 起所有平台都能跑 -s:Linux+cgo 走 C/epoll 实现(server_linux.go),
// 其余构建走本文件。行为与 C 版对齐:
//   - 多端口监听(AF_INET),统一计数 alive / total_acc / total_close / 分端口
//   - 小缓冲(2K)+ TCP_NODELAY + KeepAlive(60s idle / 10s interval / 3 次,平台支持范围内)
//   - STATS 每秒输出,格式与 C 版逐字一致
//   - SIGINT/SIGTERM/SIGHUP 优雅退出:先关 listener,再逐个 close 连接(对端收 FIN)
//   - 主动关闭不计入 total_close(final alive 取关闭瞬间值,同 C 版语义)
//
// 实现:每连接一个 goroutine 阻塞 Read 感知断开(约 8KB/连接),
// 适合功能使用与中小规模;10w+ 大规模压测仍建议 Linux C 服务端。

package main

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

const (
	kaIdle     = 60 * time.Second
	kaInterval = 10 * time.Second
	kaCount    = 3
)

type goServer struct {
	alive      atomic.Int64
	totalAcc   atomic.Uint64
	totalClose atomic.Uint64
	authFail   atomic.Uint64
	perPort    []atomic.Int64
	ports      []int
	conns      sync.Map // net.Conn -> int(portIdx)
}

func (s *goServer) tune(c *net.TCPConn) {
	_ = c.SetNoDelay(true)
	_ = c.SetReadBuffer(2048)
	_ = c.SetWriteBuffer(2048)
	_ = c.SetKeepAliveConfig(net.KeepAliveConfig{
		Enable:   true,
		Idle:     kaIdle,
		Interval: kaInterval,
		Count:    kaCount,
	})
}

// hold 阻塞直到对端断开(FIN/RST)、keepalive 判死或本端显式关闭。
// 主动关闭(优雅退出)不计数 —— 与 C 版关闭路径语义一致。
func (s *goServer) hold(ctx context.Context, c net.Conn, idx int) {
	buf := make([]byte, 64)
	_, _ = c.Read(buf)
	_ = c.Close()
	s.conns.Delete(c)
	if ctx.Err() != nil {
		return // 本端主动关闭,不算自然断开
	}
	s.alive.Add(-1)
	s.totalClose.Add(1)
	s.perPort[idx].Add(-1)
}

func runServer(ports []string, pass string) error {
	if len(ports) == 0 {
		return fmt.Errorf("服务器模式需要至少一个端口: tcp-stress -s <port1> [port2] ...")
	}
	if err := validatePass(pass); err != nil {
		return fmt.Errorf("-pass %v", err)
	}
	if len(ports) > 64 {
		return fmt.Errorf("too many ports, limit=64")
	}
	pn := make([]int, 0, len(ports))
	for _, p := range ports {
		if !isValidPort(p) {
			return fmt.Errorf("invalid port: %s", p)
		}
		v, _ := strconv.Atoi(p)
		pn = append(pn, v)
	}

	// 信号 → ctx:INT/TERM/HUP 均触发优雅退出(与 C 版/客户端对齐)
	ctx, stop := signal.NotifyContext(context.Background(),
		syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	defer stop()

	s := &goServer{ports: pn, perPort: make([]atomic.Int64, len(pn))}

	var wg sync.WaitGroup
	var listeners []net.Listener
	for i, p := range pn {
		l, err := net.Listen("tcp4", fmt.Sprintf("0.0.0.0:%d", p))
		if err != nil {
			for _, x := range listeners {
				_ = x.Close()
			}
			return err
		}
		listeners = append(listeners, l)
		fmt.Fprintf(os.Stderr, "listening on 0.0.0.0:%d\n", p)

		wg.Add(1)
		go func(idx int, ln net.Listener) {
			defer wg.Done()
			for {
				c, err := ln.Accept()
				if err != nil {
					if ctx.Err() != nil {
						return // 优雅退出,listener 已关
					}
					// 瞬时错误(如 fd 耗尽):退避重试,绝不退出 listener
					time.Sleep(200 * time.Millisecond)
					continue
				}
				if ctx.Err() != nil {
					_ = c.Close()
					return
				}
				if tc, ok := c.(*net.TCPConn); ok {
					s.tune(tc)
				}
				wg.Add(1)
				go func() {
					defer wg.Done()
					// 鉴权:通过才计数(失败连接不进 conns 表、不占统计)
					if pass != "" {
						if !serverAuth(c, pass) {
							s.authFail.Add(1)
							_ = c.Close()
							return
						}
					}
					s.conns.Store(c, idx)
					s.alive.Add(1)
					s.totalAcc.Add(1)
					s.perPort[idx].Add(1)
					s.hold(ctx, c, idx)
				}()
			}
		}(i, l)
	}

	// STATS:每秒,格式与 C 版逐字对齐
	wg.Add(1)
	go func() {
		defer wg.Done()
		t := time.NewTicker(time.Second)
		defer t.Stop()
		var prevAcc, prevClose uint64
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				acc := s.totalAcc.Load()
				cl := s.totalClose.Load()
				var pb strings.Builder
				for i, p := range s.ports {
					fmt.Fprintf(&pb, " %d=%d", p, s.perPort[i].Load())
				}
				fmt.Fprintf(os.Stderr,
					"[STATS t=%ds] alive=%d | total_acc=%d total_close=%d | +%d/s -%d/s | auth_fail=%d\n"+
						"         ports:%s\n",
					time.Now().Unix(), s.alive.Load(), acc, cl,
					acc-prevAcc, cl-prevClose, s.authFail.Load(), pb.String())
				prevAcc, prevClose = acc, cl
			}
		}
	}()

	if pass != "" {
		fmt.Fprintln(os.Stderr, "auth: enabled (password set)")
	}
	fmt.Fprintln(os.Stderr, "server running, ctrl-c to stop.")
	<-ctx.Done()
	fmt.Fprintln(os.Stderr, "shutting down...")

	// 1) 先关 listener:不再接受新连接
	for _, l := range listeners {
		_ = l.Close()
	}
	// 2) 逐个 close 已建连:对端收 FIN 而非 RST
	s.conns.Range(func(k, _ any) bool {
		_ = k.(net.Conn).Close()
		return true
	})
	// 3) 等 hold goroutine 清理完(兜底 5s)
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
	}
	fmt.Fprintf(os.Stderr, "bye. final alive=%d\n", s.alive.Load())
	return nil
}
