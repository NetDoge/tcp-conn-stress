// client.go - 家用宽带极限 TCP 连接测试客户端(纯 Go)
//
// v1.0.1 起与 C 服务端合并为单二进制 tcp-stress,本文件是客户端模式(-c)的实现,
// 入口与 flag 定义见 main.go:
//   ./tcp-stress -c -servers "192.168.1.100:8888,192.168.1.100:8889" \
//                      -target 100000 -rate 200
//
// 特性:
//   - 多端口轮询,每端口打到内核临时端口上限时切下一个
//   - 自定义 net.Dialer:KeepAlive 30s + 小读写缓冲
//   - 令牌桶限速,默认 200 conn/s,可调
//   - 实时统计:尝试/成功/活跃/断开/失败 类别分布

package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"math"
	"net"
	"os"
	"os/signal"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// clientConfig 由 main.go 的 flag 填好传入;参数校验在 runClient 内做。
type clientConfig struct {
	servers   string
	bind      string
	target    uint64
	rate      int
	keepAlive time.Duration
	statsInt  time.Duration
	pass      string // 服务端鉴权密码;-pass 启用,空 = 不鉴权
}

type target struct {
	ip   string
	port int
	addr string // net.JoinHostPort 结果,IPv6 自带方括号
}

type stats struct {
	try    atomic.Uint64
	ok     atomic.Uint64
	active atomic.Int64
	// 失败分类
	failAuth  atomic.Uint64 // 鉴权失败(密码不对/服务器要求而未提供)
	failTO    atomic.Uint64 // i/o timeout
	failRST   atomic.Uint64 // connection refused
	failAddr  atomic.Uint64 // cannot assign requested address (端口耗尽)
	failFD    atomic.Uint64 // too many open files (fd 耗尽)
	failEOF   atomic.Uint64 // EOF
	failOther atomic.Uint64
	// 维持期间被断开(对端关/链路死):观察 NAT 老化的关键指标
	closed atomic.Uint64
}

var st stats

// authConsecFail 连续鉴权失败计数;任一成功清零
var authConsecFail atomic.Uint64

func parseTargets(s string) ([]target, error) {
	parts := strings.Split(s, ",")
	out := make([]target, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		host, port, err := net.SplitHostPort(p)
		if err != nil {
			return nil, fmt.Errorf("bad target %q: %v", p, err)
		}
		var pn int
		if _, err := fmt.Sscanf(port, "%d", &pn); err != nil || pn <= 0 || pn > 65535 {
			return nil, fmt.Errorf("bad port in %q", p)
		}
		out = append(out, target{ip: host, port: pn,
			addr: net.JoinHostPort(host, fmt.Sprintf("%d", pn))})
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no targets")
	}
	return out, nil
}

// 自定义 socket:小缓冲 + keepalive
func dialControl(network, addr string, c syscall.RawConn) error {
	return c.Control(func(fd uintptr) {
		setSmallBuf(fd)
	})
}

// dialerWithBind 生成绑本地出口 IP 的 Dialer
func newDialer(bindIP string, keepAlive time.Duration) *net.Dialer {
	d := &net.Dialer{
		Timeout:   5 * time.Second,
		KeepAlive: keepAlive,
		Control:   dialControl,
	}
	if bindIP != "" {
		ip := net.ParseIP(bindIP)
		if ip == nil {
			log.Fatalf("invalid -bind ip: %s", bindIP)
		}
		d.LocalAddr = &net.TCPAddr{IP: ip}
	}
	return d
}

// classifyErr 把错误归类到 stats 的原子字段
func classifyErr(err error) {
	if err == nil {
		return
	}
	msg := err.Error()
	switch {
	case isTimeoutErr(err):
		st.failTO.Add(1)
	case strings.Contains(msg, "connection refused"):
		st.failRST.Add(1)
	case strings.Contains(msg, "cannot assign requested address"):
		st.failAddr.Add(1)
	case strings.Contains(msg, "too many open files"):
		st.failFD.Add(1)
	case strings.Contains(strings.ToLower(msg), "eof"):
		st.failEOF.Add(1)
	default:
		st.failOther.Add(1)
		if st.failOther.Load() < 20 {
			log.Printf("fail other: %v", err)
		}
	}
}

func isTimeoutErr(err error) bool {
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return true
	}
	return false
}

// dialer 负责拨号一次;返回的 conn 已建立。
// portIdx: 失败切到下一个 target 时使用;成功则由调用方持有。
// 失败时返回 err;成功时返回 conn 与下一个要试的 portIdx。
// 返回值用于在多端口之间轮询。
func dialOnce(ctx context.Context, targets []target, startIdx int, d *net.Dialer, pass string) (net.Conn, int) {
	tlen := len(targets)
	for off := 0; off < tlen; off++ {
		idx := (startIdx + off) % tlen
		t := targets[idx]
		st.try.Add(1)
		conn, err := d.DialContext(ctx, "tcp", t.addr)
		if err != nil {
			classifyErr(err)
			if strings.Contains(err.Error(), "cannot assign requested address") ||
				strings.Contains(err.Error(), "too many open files") {
				// 端口/fd 耗尽,不要狂打;也跳过当前端口试下一个
				time.Sleep(50 * time.Millisecond)
			}
			continue
		}
		if pass != "" {
			if err := clientAuth(conn, pass); err != nil {
				_ = conn.Close()
				st.failAuth.Add(1)
				if n := authConsecFail.Add(1); n >= 10 {
					log.Fatalf("鉴权连续失败 %d 次(密码不正确或服务器要求鉴权),已中止;检查两端 -pass", n)
				}
				continue
			}
			authConsecFail.Store(0)
		}
		st.ok.Add(1)
		st.active.Add(1)
		return conn, (idx + 1) % tlen
	}
	// 全部失败:保持起点,下个 token 从同一位置再轮
	return nil, startIdx
}

// holdConn 维持连接:不收发数据,阻塞等待断开或 ctx 取消。
// 断开感知的三个出口:
//  1. 对端 FIN/RST → Read 返回 EOF / reset
//  2. 对端死透(NAT 老化拔线) → keepalive 探测失败,内核报 ETIMEDOUT
//  3. ctx 取消 → defer Close() 解除阻塞的 Read
//
// 注意:绝不设读 deadline —— 对端本就不发数据,deadline 只会把
// 健康连接当死链掐掉(历史 bug:每条连接活不过 120s,全体旋转木马)。
func holdConn(ctx context.Context, conn net.Conn) {
	defer func() {
		_ = conn.Close()
		st.active.Add(-1)
	}()
	buf := make([]byte, 64)
	done := make(chan struct{})
	go func() {
		n, _ := conn.Read(buf)
		// 服务器鉴权 banner:只有客户端未配 -pass 时才会漏到这里
		// (配了密码的话 banner 在 clientAuth 握手里就被消费掉了)。
		// 前缀匹配:banner 即使分段到达也能识别。
		if n > 0 && n <= len(authBanner) && string(buf[:n]) == authBanner[:n] {
			log.Fatalf("服务器要求鉴权而握手未完成(本端未配 -pass 或网络延迟过高),已中止")
		}
		close(done)
	}()
	select {
	case <-ctx.Done():
		return
	case <-done:
		// ctx 取消引发的 Close 也会让 Read 出错;那不算失败
		if ctx.Err() == nil {
			st.closed.Add(1)
		}
		return
	}
}

func runClient(cfg clientConfig) {
	rate, target := cfg.rate, cfg.target
	keepAlive, statsInt := cfg.keepAlive, cfg.statsInt

	if rate <= 0 {
		log.Fatalf("rate must be > 0")
	}
	if target == 0 {
		log.Fatalf("target must be > 0")
	}
	if statsInt <= 0 {
		log.Fatalf("stats interval must be > 0")
	}
	if rate > 50000 {
		log.Fatalf("rate too large (max 50000)")
	}
	if keepAlive < 0 {
		log.Fatalf("keepalive must be >= 0 (0 = Go default 15s)")
	}
	if keepAlive > 10*time.Minute {
		log.Fatalf("keepalive too long (max 10m)")
	}
	if target > math.MaxInt64 {
		log.Fatalf("target too large (max %d)", uint64(math.MaxInt64))
	}

	targets, err := parseTargets(cfg.servers)
	if err != nil {
		log.Fatal(err)
	}
	if err := validatePass(cfg.pass); err != nil {
		log.Fatalf("-pass %v", err)
	}
	if cfg.pass != "" {
		log.Printf("auth: enabled")
	}
	log.Printf("targets:")
	for _, t := range targets {
		log.Printf("  - %s", t.addr)
	}
	if cfg.bind != "" {
		log.Printf("bind local ip: %s", cfg.bind)
	}

	ctx, cancel := context.WithCancel(context.Background())
	sigCh := make(chan os.Signal, 1)
	// SIGHUP 一并优雅处理:ssh 断开时也能打完 final 统计再退
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	go func() {
		<-sigCh
		log.Println("signal received, draining...")
		cancel()
	}()

	d := newDialer(cfg.bind, keepAlive)

	// 令牌桶
	tokens := make(chan struct{}, rate)
	go func() {
		t := time.NewTicker(time.Second / time.Duration(rate))
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				select {
				case tokens <- struct{}{}:
				default:
				}
			}
		}
	}()

	// worker 数:与 rate 解耦。少点也不会拖慢,多了只是空转抢 token。
	workerN := rate
	if workerN > 256 {
		workerN = 256
	}
	if workerN < 16 {
		workerN = 16
	}
	log.Printf("workers=%d, rate=%d conn/s, target=%d", workerN, rate, target)

	// hold 计数:每次成功 dial 起一个 holdConn goroutine,它在退出前 Done 一次。
	// 主线程 hold.Wait() 等所有连接清理完再打 final。
	var hold sync.WaitGroup

	for i := 0; i < workerN; i++ {
		hold.Add(1)
		go func(startIdx int) {
			defer hold.Done()
			portIdx := startIdx
			for {
				select {
				case <-ctx.Done():
					return
				case <-tokens:
					if ctx.Err() != nil {
						return
					}
					if st.active.Load() >= int64(target) {
						// 已达目标,让出 CPU,等 ctx 结束
						time.Sleep(100 * time.Millisecond)
						continue
					}
					conn, next := dialOnce(ctx, targets, portIdx, d, cfg.pass)
					portIdx = next
					if conn == nil {
						continue
					}
					hold.Add(1)
					go func(c net.Conn) {
						defer hold.Done()
						holdConn(ctx, c)
					}(conn)
				}
			}
		}(i % len(targets))
	}

	// 统计打印
	go func() {
		t := time.NewTicker(statsInt)
		defer t.Stop()
		var lastTry, lastOk uint64
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				curTry := st.try.Load()
				curOk := st.ok.Load()
				// 换算每秒速率:-stats 非 1s 时 +/s 才是真实值
				dt := uint64(math.Round(float64(curTry-lastTry) / statsInt.Seconds()))
				do := uint64(math.Round(float64(curOk-lastOk) / statsInt.Seconds()))
				lastTry, lastOk = curTry, curOk
				log.Printf("[STAT] alive=%d try=%d ok=%d closed=%d | +try/s=%d +ok/s=%d | "+
					"fail t/o=%d rst=%d addr-full=%d fd-full=%d eof=%d auth=%d other=%d",
					st.active.Load(), curTry, curOk, st.closed.Load(), dt, do,
					st.failTO.Load(), st.failRST.Load(), st.failAddr.Load(),
					st.failFD.Load(), st.failEOF.Load(), st.failAuth.Load(), st.failOther.Load())
			}
		}
	}()

	hold.Wait()
	log.Printf("final: try=%d ok=%d closed=%d active=%d",
		st.try.Load(), st.ok.Load(), st.closed.Load(), st.active.Load())
}
