// main.go - tcp-conn-stress 单二进制入口
//
// 单二进制 tcp-stress,三种用法:
//   tcp-stress -s [-pass 密码] <port1> [port2] ...   服务器模式(Linux 走 C/epoll,其余纯 Go)
//   tcp-stress -c [-servers ...] [-target N] ...     客户端模式(纯 Go)
//   tcp-stress                                          终端里裸运行 → 交互式向导(TUI)
//
// - 两种模式互斥;非终端环境(管道/CI)裸运行仍打印用法 exit 2
// - -pass:服务端鉴权密码;双端一致才计入统计(详见 auth.go)

package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// 版本号:构建时注入(make VERSION=v1.0.3,CI 从 tag 取),默认 dev
var version = "dev"

// isValidPort 纯数字 1-65535(两种服务端实现与入口校验共用)
func isValidPort(s string) bool {
	if s == "" || len(s) > 5 {
		return false
	}
	n := 0
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
		n = n*10 + int(c-'0')
	}
	return n >= 1 && n <= 65535
}

// parsePortSpec 展开端口说明串(空格分隔,单端口或 a-b 闭区间)为端口列表。
// 只接受纯数字与 '-' 范围符;单端口 1-65535,范围须 a<=b 且 a>=1 b<=65535,
// 且不得超出 64 个(与服务端监听上限一致,客户端同样套用)。
// 返回 (list, err):list 为展开后的端口字符串;err 非空时 list 为空。
// 空输入(全空白)返回空 list,nil —— 由调用方决定是否报错。
func parsePortSpec(spec string) ([]string, error) {
	fields := strings.Fields(spec)
	if len(fields) == 0 {
		return nil, nil
	}
	if len(fields) > 64 {
		return nil, fmt.Errorf("端口段太多(空格分隔,最多 64 段): %d", len(fields))
	}
	var out []string
	for _, f := range fields {
		lo, hi := f, f
		if i := strings.IndexByte(f, '-'); i >= 0 {
			lo, hi = f[:i], f[i+1:]
			if lo == "" || hi == "" {
				return nil, fmt.Errorf("端口范围格式错误: %q(应为 a-b,如 8888-8895)", f)
			}
			a, ea := strconv.Atoi(lo)
			b, eb := strconv.Atoi(hi)
			if ea != nil || eb != nil || a <= 0 || a > 65535 || b <= 0 || b > 65535 {
				return nil, fmt.Errorf("非法端口: %q", f)
			}
			if a > b {
				return nil, fmt.Errorf("端口范围起始大于结束: %q", f)
			}
			if len(out)+(b-a+1) > 64 {
				return nil, fmt.Errorf("端口总数超出上限 64")
			}
			for p := a; p <= b; p++ {
				out = append(out, strconv.Itoa(p))
			}
			continue
		}
		// 单端口
		if !isValidPort(f) {
			return nil, fmt.Errorf("非法端口: %q", f)
		}
		if len(out)+1 > 64 {
			return nil, fmt.Errorf("端口总数超出上限 64")
		}
		out = append(out, f)
	}
	return out, nil
}

func main() {
	// fd 软上限自动抬到硬上限:免掉"ulimit -n"部署步骤(普通权限,无需 root)
	if soft, hard := raiseNofile(); soft > 0 {
		// 简报放 stderr,不污染 -v/-h 的 stdout 协议
		fmt.Fprintf(os.Stderr, "fd limit: soft=%d hard=%d\n", soft, hard)
	}

	var (
		showVersion bool
		showHelp    bool
		serverMode  bool
		clientMode  bool
		pass        string
		passFile    string
		cfg         clientConfig
	)
	flag.BoolVar(&showVersion, "v", false, "打印版本号并退出")
	flag.BoolVar(&showVersion, "version", false, "打印版本号并退出(同 -v)")
	flag.BoolVar(&showHelp, "h", false, "打印帮助信息并退出")
	flag.BoolVar(&showHelp, "help", false, "打印帮助信息并退出(同 -h)")
	flag.BoolVar(&serverMode, "s", false, "服务器模式:位置参数为监听端口列表(全平台)")
	flag.StringVar(&pass, "pass", "", "鉴权密码:服务端启用后,客户端须提供相同密码(1-128 字节,无空白)。注意:密码会出现在 ps/进程列表里,敏感场景用 -passfile")
	flag.StringVar(&passFile, "passfile", "", "从文件读鉴权密码(取首行;文件建议 chmod 600)。不落进程命令行,ps 不可见")
	flag.BoolVar(&clientMode, "c", false, "客户端模式")
	flag.StringVar(&cfg.servers, "servers", "127.0.0.1:8888", "客户端:目标地址列表(≤64,须为 tcp-stress 服务端),逗号分隔,格式 IP:Port")
	flag.StringVar(&cfg.bind, "bind", "", "客户端:本地出口 IP(多 WAN/策略路由时指定)")
	flag.Uint64Var(&cfg.target, "target", 10000, "客户端:目标总连接数(到达后保持)")
	flag.IntVar(&cfg.rate, "rate", 200, "客户端:每秒建连速率上限")
	flag.DurationVar(&cfg.keepAlive, "keepalive", 30*time.Second, "客户端:TCP KeepAlive 间隔")
	flag.DurationVar(&cfg.statsInt, "stats", 1*time.Second, "客户端:统计打印周期(最小 100ms)")
	flag.Usage = func() {
		w := flag.CommandLine.Output()
		fmt.Fprintf(w, `tcp-stress — 家用宽带极限 TCP 长连接压测(单二进制)

仅供测试自有/授权的设备;客户端只与 tcp-stress 服务端建连(身份握手,详见 README 使用政策)

用法:
  tcp-stress -s <port1> [port2] ...          服务器模式(Linux 为 C/epoll,其余为纯 Go)
  tcp-stress -c [客户端参数...]               客户端模式

示例:
  tcp-stress -s 8888 8889 8890
  tcp-stress -s -pass 秘密 8888 8890              # 带鉴权的服务端
  tcp-stress -s -passfile /etc/tcp-stress.pass 8888   # 密码从文件读(不落 ps)
  tcp-stress -c -servers "192.168.1.100:8888,192.168.1.100:8889" -target 100000 -rate 200
  tcp-stress                                          # 终端里裸运行,进入交互向导

参数:
`)
		flag.PrintDefaults()
	}
	flag.Parse()

	// -passfile:从文件读密码 —— 密码不进 argv,不落 /proc/<pid>/cmdline
	// (-pass 的密码 ps aux 可见,敏感场景应改用本参数)
	if passFile != "" {
		if pass != "" {
			fmt.Fprintln(os.Stderr, "error: -pass 与 -passfile 不能同时使用")
			os.Exit(2)
		}
		// 限读 129 字节(密码上限 128 + 换行):os.ReadFile 会把整个文件
		// 先读进内存才报"密码最长 128 字节",1GB 文件实测 RSS 冲到 2GB;
		// 指向 /dev/zero 一类无 EOF 设备更是无界读
		f, err := os.Open(passFile)
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: 读 -passfile %s: %v\n", passFile, err)
			os.Exit(2)
		}
		// 宽权限(组/其他可读)告警:密码文件被同机其他账户读到即泄漏
		// (0600/0400 视为收紧;stat 在读前,出错不拦使用)
		if fi, ferr := f.Stat(); ferr == nil {
			if fi.Mode().Perm()&0077 != 0 {
				fmt.Fprintf(os.Stderr, "warning: -passfile %s 权限为 %o,组/其他用户可读,建议 chmod 600\n",
					passFile, fi.Mode().Perm())
			}
		}
		b, err := io.ReadAll(io.LimitReader(f, 129))
		_ = f.Close()
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: 读 -passfile %s: %v\n", passFile, err)
			os.Exit(2)
		}
		s := string(b)
		if i := strings.IndexByte(s, '\n'); i >= 0 {
			s = s[:i] // 只取首行
		}
		s = strings.TrimSpace(s)
		if s == "" {
			fmt.Fprintf(os.Stderr, "error: -passfile %s 首行为空\n", passFile)
			os.Exit(2)
		}
		pass = s
	}

	switch {
	case showHelp:
		// 显式求助走 stdout + exit 0;解析错误/裸调用仍走 stderr + exit 2
		flag.CommandLine.SetOutput(os.Stdout)
		flag.Usage()
		os.Exit(0)
	case showVersion:
		fmt.Printf("tcp-stress %s (%s/%s)\n", version, runtime.GOOS, runtime.GOARCH)
		os.Exit(0)
	case serverMode && clientMode:
		fmt.Fprintln(os.Stderr, "error: -s 与 -c 不能同时使用")
		os.Exit(2)
	case serverMode, clientMode:
		if err := validatePass(pass); err != nil {
			fmt.Fprintf(os.Stderr, "error: -pass %v\n", err)
			os.Exit(2)
		}
		if serverMode {
			// 位置参数支持端口范围(如 "8888-8895"):展开成单个端口列表再交给 runServer
			ports, perr := parsePortSpec(strings.Join(flag.Args(), " "))
			if perr != nil {
				fmt.Fprintln(os.Stderr, "error:", perr)
				os.Exit(2)
			}
			if len(ports) == 0 {
				fmt.Fprintln(os.Stderr, "error: 服务器模式需要至少一个端口: tcp-stress -s <port1> [port2] ...")
				os.Exit(2)
			}
			if err := runServer(ports, pass); err != nil {
				fmt.Fprintln(os.Stderr, "error:", err)
				os.Exit(1)
			}
		} else {
			if flag.NArg() > 0 {
				fmt.Fprintf(os.Stderr, "error: 客户端模式不支持位置参数: %v\n", flag.Args())
				os.Exit(2)
			}
			cfg.pass = pass
			runClient(cfg)
		}
	default:
		// 完全不带参数 + 真终端:进交互向导;
		// 带了 flag 却没选模式仍是错误;管道/CI 裸调用仍 usage exit 2
		if len(os.Args) == 1 && interactiveTTY() {
			runTUI()
			return
		}
		if flag.NArg() > 0 {
			fmt.Fprintf(os.Stderr, "error: 未指定模式(-s/-c);模式参数必须写在最前面,收到的位置参数: %v\n", flag.Args())
		}
		flag.Usage()
		os.Exit(2)
	}
}
