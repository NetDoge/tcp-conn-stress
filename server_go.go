//go:build !linux || !cgo

// server_go.go - 纯 Go 服务端实现(darwin / windows,及未开 cgo 的构建)。
//
// v1.0.5 起所有平台都能跑 -s:Linux+cgo 走 C/epoll 实现(server_linux.go),
// 其余构建走本文件。行为与 C 版对齐:
//   - 多端口监听(AF_INET),统一计数 alive / total_acc / total_close / 分端口
//   - v1.1.0:accept 即发身份行("AUTH?\n" 带密码 / "STRESS\n" 开放),与 C 版对齐
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

	// authPendingMax: 鉴权中连接上限,与 C 侧 AUTH_MAX 对齐。
	// 满时驱逐一条鉴权中连接给新连接让位(纯 Go 版 v1.0.8 前无上限:
	// 实测 8000 停滞连接全收,ΔRSS 43.5MB,零驱逐)。
	authPendingMax = 4096
)

type goServer struct {
	alive      atomic.Int64
	totalAcc   atomic.Uint64
	totalClose atomic.Uint64
	authFail   atomic.Uint64
	perPort    []atomic.Int64
	ports      []int
	conns      sync.Map // net.Conn -> int(portIdx)

	mu           sync.Mutex            // 保护 pending / lastEvictLog
	pending      map[net.Conn]struct{} // 鉴权中连接:上限 authPendingMax,满时驱逐(与 C 版对齐)
	lastEvictLog time.Time
}

// evictLog 驱逐日志 10s 限频(对齐 C 版 g_last_evict_log;调用方须持有 mu)
func (s *goServer) evictLog() {
	if time.Since(s.lastEvictLog) >= 10*time.Second {
		s.lastEvictLog = time.Now()
		fmt.Fprintf(os.Stderr, "auth: pending full (%d), evicted one stalled connection\n", authPendingMax)
	}
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
// 收到数据一律丢弃继续等 —— 与 C 服务端语义对齐(旧版 Read 一次返回
// 就计 close,收到任意数据即断连,与 C 版行为分叉)。
// 主动关闭(优雅退出)不计数 —— 与 C 版关闭路径语义一致。
func (s *goServer) hold(ctx context.Context, c net.Conn, idx int) {
	buf := make([]byte, 2048)
	for {
		_, err := c.Read(buf)
		if err != nil {
			break // EOF / RST / 本端关闭
		}
		// 数据丢弃,继续等断开
	}
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
		// isValidPort 已拒尾部垃圾;再拒前导零:"08080" 会被 Atoi
		// 解析成 8080 静默换绑另一端口(C 版 valid_port_str 已拒,
		// 双实现须一致;实测旧版纯 Go 版 08080 实际监听 8080)
		if !isValidPort(p) || (len(p) > 1 && p[0] == '0') {
			return fmt.Errorf("invalid port: %s", p)
		}
		v, _ := strconv.Atoi(p)
		pn = append(pn, v)
	}

	// 信号 → ctx:INT/TERM/HUP/QUIT 均触发优雅退出(与 C 版/客户端对齐)
	ctx, stop := signal.NotifyContext(context.Background(),
		syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP, syscall.SIGQUIT)
	defer stop()

	s := &goServer{
		ports:   pn,
		perPort: make([]atomic.Int64, len(pn)),
		pending: make(map[net.Conn]struct{}),
	}

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
			var lastErrLog time.Time
			for {
				c, err := ln.Accept()
				if err != nil {
					if ctx.Err() != nil {
						return // 优雅退出,listener 已关
					}
					// 瞬时错误(如 fd 耗尽):退避重试,绝不退出 listener;
					// 10s 限频留日志,否则静默吞掉排障线索(对齐 C 版 EMFILE 日志)
					if time.Since(lastErrLog) > 10*time.Second {
						lastErrLog = time.Now()
						fmt.Fprintf(os.Stderr, "accept: %v (retrying with backoff)\n", err)
					}
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
					// 先入 conns 表再鉴权:shutdown 的逐个 close 才能命中
					// "鉴权中"的连接 —— 旧版它们卡在 10s 读 deadline 上,
					// 退出恒烧满 5s 兜底(实测 SIGINT 后固定 5.0s 才 bye)
					s.conns.Store(c, idx)
					if pass == "" {
						// v1.1.0 开放模式身份行:客户端据此区分"tcp-stress
						// 服务端"与"任意第三方服务",第三方滥用在建连
						// 阶段即被客户端拒掉;写失败(对端已断)按未建连处理
						if _, werr := c.Write([]byte(srvBanner)); werr != nil {
							s.conns.Delete(c)
							_ = c.Close()
							return
						}
					}
					if pass != "" {
						// 鉴权中队列上限 + 驱逐(v1.0.9,对齐 C 版语义):
						// 连上不回密码的停滞连接可无限占槽耗 fd/内存;直接拒绝
						// 新连接会让停滞洪水永久锁死正确密码的合法客户端,
						// 驱逐一条让位。入队与驱逐同一临界区,上限不变式严格成立。
						s.mu.Lock()
						if len(s.pending) >= authPendingMax {
							for v := range s.pending {
								delete(s.pending, v)
								s.evictLog()
								_ = v.Close() // 受害者 serverAuth 读失败自行走失败清理
								break
							}
						}
						s.pending[c] = struct{}{}
						s.mu.Unlock()
						ok := serverAuth(c, pass)
						s.mu.Lock()
						delete(s.pending, c)
						s.mu.Unlock()
						if !ok {
							if ctx.Err() == nil {
								s.authFail.Add(1) // 退出期关闭不算鉴权失败
							}
							s.conns.Delete(c)
							_ = c.Close()
							return
						}
					}
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
	} else {
		fmt.Fprintln(os.Stderr, "warning: 未设 -pass,任何能路由到本端口的客户端都能占用连接槽位;公网部署建议加 -pass 并限源")
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
