// client.go - 家用宽带极限 TCP 连接测试客户端
//
// 编译:  go build -o client client.go
// 运行:  ./client -servers "192.168.1.100:8888,192.168.1.100:8889" \
//                 -target 100000 -rate 200
//
// 特性:
//   - 多端口轮询,每端口打到内核临时端口上限时切下一个
//   - 自定义 net.Dialer:KeepAlive 30s + 小读写缓冲
//   - 令牌桶限速,默认 200 conn/s,可调
//   - 实时统计:尝试/成功/活跃/失败 类别分布

package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"os/signal"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

type target struct {
	ip   string
	port int
}

type stats struct {
	try      atomic.Uint64
	ok       atomic.Uint64
	active   atomic.Int64
	fail     atomic.Uint64
	// 失败分类
	failTO   atomic.Uint64 // i/o timeout
	failRST  atomic.Uint64 // connection refused
	failAddr atomic.Uint64 // cannot assign requested address (端口耗尽)
	failEOF  atomic.Uint64 // EOF
	failOther atomic.Uint64
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
		var pn int
		if _, err := fmt.Sscanf(port, "%d", &pn); err != nil || pn <= 0 || pn > 65535 {
			return nil, fmt.Errorf("bad port in %q", p)
		}
		out = append(out, target{ip: host, port: pn})
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

// 轮询 dial:成功 -> 维持到 ctx 结束或断线
func worker(ctx context.Context, wg *sync.WaitGroup, targets []target, d *net.Dialer) {
	defer wg.Done()
	tlen := len(targets)
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}
		// 轮询每个端口:某端口端口耗尽会换下一个
		for i := 0; i < tlen; i++ {
			t := targets[i]
			addr := fmt.Sprintf("%s:%d", t.ip, t.port)
			st.try.Add(1)
			conn, err := d.DialContext(ctx, "tcp", addr)
			if err != nil {
				classifyErr(err)
				// 端口耗尽时短暂 sleep,避免狂打日志/狂吃 CPU
				if strings.Contains(err.Error(), "cannot assign requested address") {
					time.Sleep(50 * time.Millisecond)
				}
				continue
			}
			st.ok.Add(1)
			st.active.Add(1)
			// 维持连接:直到对端关或 ctx 取消
			go holdConn(ctx, conn)
			// 一次成功就换下一个 target,均衡各端口
		}
	}
}

// holdConn 维持连接:不读不写,让 keepalive 兜底;检测断线时回收计数
func holdConn(ctx context.Context, conn net.Conn) {
	defer func() {
		_ = conn.Close()
		st.active.Add(-1)
	}()
	// 设读超时,以便尽快感知对端断
	_ = conn.SetReadDeadline(time.Now().Add(120 * time.Second))
	buf := make([]byte, 64)
	done := make(chan struct{})
	go func() {
		for {
			_, err := conn.Read(buf)
			if err != nil {
				close(done)
				return
			}
			// 拉长读超时,避免 keepalive 周期内被自己掐掉
			_ = conn.SetReadDeadline(time.Now().Add(120 * time.Second))
		}
	}()
	select {
	case <-ctx.Done():
		return
	case <-done:
		return
	}
}

func main() {
	var (
		servers  = flag.String("servers", "127.0.0.1:8888", "目标地址列表,逗号分隔,格式 IP:Port")
		bind     = flag.String("bind", "", "本地出口 IP(多 WAN/策略路由时指定)")
		target   = flag.Uint64("target", 10000, "目标总连接数(到达后保持)")
		rate     = flag.Int("rate", 200, "每秒建连速率上限")
		keepAlive = flag.Duration("keepalive", 30*time.Second, "TCP KeepAlive 间隔")
		statsInt = flag.Duration("stats", 1*time.Second, "统计打印周期")
	)
	flag.Parse()

	targets, err := parseTargets(*servers)
	if err != nil {
		log.Fatal(err)
	}
	log.Printf("targets:")
	for _, t := range targets {
		log.Printf("  - %s:%d", t.ip, t.port)
	}
	if *bind != "" {
		log.Printf("bind local ip: %s", *bind)
	}

	ctx, cancel := context.WithCancel(context.Background())
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigCh
		log.Println("signal received, draining...")
		cancel()
	}()

	d := newDialer(*bind, *keepAlive)

	// 令牌桶
	tokens := make(chan struct{}, *rate)
	go func() {
		t := time.NewTicker(time.Second / time.Duration(*rate))
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

	// 启动 worker 数 = rate,每个吃令牌后循环 dial
	var wg sync.WaitGroup
	workerN := *rate
	if workerN > 2000 {
		workerN = 2000
	}
	if workerN < 16 {
		workerN = 16
	}
	log.Printf("workers=%d, rate=%d conn/s, target=%d", workerN, *rate, *target)

	for i := 0; i < workerN; i++ {
		wg.Add(1)
		go func() {
			for {
				select {
				case <-ctx.Done():
					wg.Done()
					return
				case <-tokens:
					if st.active.Load() >= int64(*target) {
						// 已达目标,不再加新连,只等 ctx 结束
						time.Sleep(100 * time.Millisecond)
						continue
					}
					worker(ctx, &wg, targets, d)
				}
			}
		}()
	}

	// 统计打印
	go func() {
		t := time.NewTicker(*statsInt)
		defer t.Stop()
		var lastTry, lastOk uint64
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				curTry := st.try.Load()
				curOk := st.ok.Load()
				dt := curTry - lastTry
				do := curOk - lastOk
				lastTry, lastOk = curTry, curOk
				log.Printf("[STAT] try=%d ok=%d active=%d | +try/s=%d +ok/s=%d | "+
					"fail t/o=%d rst=%d addr-full=%d eof=%d other=%d",
					curTry, curOk, st.active.Load(), dt, do,
					st.failTO.Load(), st.failRST.Load(), st.failAddr.Load(),
					st.failEOF.Load(), st.failOther.Load())
			}
		}
	}()

	wg.Wait()
	log.Printf("final: try=%d ok=%d active=%d", st.try.Load(), st.ok.Load(), st.active.Load())
}