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
//   - v1.1.0 起每个连接必做 tcp-stress 身份握手,非 tcp-stress 服务端
//     连续失败即中止 —— 无法直接用于对第三方的连接耗尽攻击

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
	"strconv"
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
	// v1.1.0 身份握手:每个连接必走,免握手路径已删除(防第三方滥用)
	// hsFail:该 target 连续握手失败次数,任一成功清零 —— 按 target
	// 计数而非全局:混合列表里一台真服务端的成功不能替假 target
	// 清零计数(防"混入一台真服务端绕过整体中止")
	// noAuthLogged:"未要求鉴权,-pass 已忽略"每 target 只提示一次
	hsFail       atomic.Uint64
	noAuthLogged atomic.Bool
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
		// 空 host(":8888" / "[::]:8888")会被 Dial 静默解成本机,
		// 笔误时测错对象而无任何提示(实测旧版 try=3 ok=3 连上本机);
		// IPv4 需显式写 IP 或 0.0.0.0,IPv6 写 [::1] 等具体地址
		if host == "" || host == "::" {
			return nil, fmt.Errorf("bad target %q: 空 host,请写具体地址(如 127.0.0.1:8888 或 [::1]:8888)", p)
		}
		// 回环校验拒绝尾部垃圾与前导零/正负号("8888x"/"0080"/"+80"),
		// 与服务端 isValidPort 同等严格(旧版 fmt.Sscanf 不查尾部,静默拨错端口)
		pn, err := strconv.Atoi(port)
		if err != nil || pn <= 0 || pn > 65535 || strconv.Itoa(pn) != port {
			return nil, fmt.Errorf("bad port in %q", p)
		}
		out = append(out, target{ip: host, port: pn, addr: net.JoinHostPort(host, port)})
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no targets")
	}
	// v1.1.0:目标数上限 64 —— 封住把客户端当端口扫描器用的面
	// (每个 target 连续握手失败 10 次才中止,上限即最大扫描面)
	if len(out) > 64 {
		return nil, fmt.Errorf("too many targets, limit=64")
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
// 失败时返回 err;成功时返回 conn、命中的 target 与下一个要试的 portIdx。
// 返回值用于在多端口之间轮询。
func dialOnce(ctx context.Context, targets []target, startIdx int, d *net.Dialer, pass string) (net.Conn, *target, int) {
	tlen := len(targets)
	for off := 0; off < tlen; off++ {
		idx := (startIdx + off) % tlen
		t := &targets[idx] // 指针:握手失败计数/忽略提示写回 target 本体
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
		// v1.1.0 身份握手:每个连接必走。旧版仅在客户端配了 -pass 时
		// 才探测鉴权,把客户端指向任意第三方服务(nginx/SSH 等)时
		// 零拦截 —— 实测假静默服务器 50 连全持有,可当 slowloris 用。
		if err := clientHandshake(conn, pass, t); err != nil {
			_ = conn.Close()
			var rej *hsReject
			if errors.As(err, &rej) {
				if rej.failAuth {
					st.failAuth.Add(1)
				} else {
					st.failOther.Add(1)
				}
				if n := t.hsFail.Add(1); n >= hsConsecFail {
					log.Fatal(rej.fatal)
				}
				continue
			}
			var ab *hsAbort
			if errors.As(err, &ab) {
				log.Fatal(ab.msg)
			}
			log.Fatal(err) // 不可达:握手错误只有 hsReject/hsAbort 两类
		}
		t.hsFail.Store(0)
		st.ok.Add(1)
		// active 的计入由调用方(worker)预留完成:拨号+鉴权中的连接
		// 也占 target 名额,防多 worker 竞态过冲(实测 max_alive=target+1)
		return conn, t, (idx + 1) % tlen
	}
	// 全部失败:保持起点,下个 token 从同一位置再轮
	return nil, nil, startIdx
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
		defer close(done)
		// v1.1.0:身份握手在 dialOnce 已完成,服务端此后不再发任何
		// 数据;Read 任何返回(对端 FIN/RST、keepalive 判死、异常收到
		// 数据)都按断开处理。旧版"banner 迟到补握手自愈"逻辑随免
		// 握手路径一并删除:服务端身份行建连即发,握手阶段必然已
		// 读完,不存在身份行漏到 hold 的时序。
		conn.Read(buf)
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
	rate, targetN := cfg.rate, cfg.target
	keepAlive, statsInt := cfg.keepAlive, cfg.statsInt

	if rate <= 0 {
		log.Fatalf("rate must be > 0")
	}
	if targetN == 0 {
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
	if targetN > math.MaxInt64 {
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
	for i := range targets {
		log.Printf("  - %s", targets[i].addr)
	}
	if cfg.bind != "" {
		log.Printf("bind local ip: %s", cfg.bind)
	}

	ctx, cancel := context.WithCancel(context.Background())
	sigCh := make(chan os.Signal, 1)
	// SIGHUP(ssh 断开)与 SIGQUIT(Ctrl-\)一并优雅处理:
	// 都能打完 final 统计再退(QUIT 默认行为是打栈退出 rc=2,统计全丢)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP, syscall.SIGQUIT)
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
	log.Printf("workers=%d, rate=%d conn/s, target=%d", workerN, rate, targetN)

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
					// 预留槽位后拨号:active 语义 = 已建立 + 拨号中(含鉴权)。
					// 旧版"先查后拨"在多 worker 竞态下会过冲(max_alive=target+1);
					// 原子预留保证 active 恒 <= target。
					if n := st.active.Add(1); n > int64(targetN) {
						st.active.Add(-1)
						// 已达目标,让出 CPU,等 ctx 结束
						time.Sleep(100 * time.Millisecond)
						continue
					}
					conn, _, next := dialOnce(ctx, targets, portIdx, d, cfg.pass)
					portIdx = next
					if conn == nil {
						st.active.Add(-1) // 拨号/鉴权全失败,释放预留
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
