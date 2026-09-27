// tui.go - 零参数交互式向导(v1.0.6)
//
// 终端里不带任何参数运行 tcp-stress 进入本向导:
// 选模式 → 逐项问参数(回车用默认值)→ 展示等价命令行 → 确认运行。
// 目标是零学习成本用上全部功能,并顺带学会对应 CLI 写法。
//
// 进入条件:完全无参数 + stdin/stdout 都是控制终端(interactiveTTY)。
// 管道 / /dev/null 重定向 / CI / docker 无 -t 时仍走旧路径:
// 打印用法 exit 2,不会挂住等待输入。
//
// 刻意不做 raw mode / 密码回显遮蔽:保持零依赖与跨平台简单性。
// 向导阶段 Ctrl-C 直接结束;测试运行阶段走各模式自身的优雅退出。

package main

import (
	"bufio"
	"flag"
	"fmt"
	"net"
	"os"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// ask 打印提示读一行;空输入(直接回车 / EOF)返回默认值
func ask(r *bufio.Reader, prompt, def string) string {
	if def != "" {
		fmt.Printf("%s [默认 %s]: ", prompt, def)
	} else {
		fmt.Printf("%s: ", prompt)
	}
	line, err := r.ReadString('\n')
	if err != nil && line == "" {
		return def
	}
	return strings.TrimSpace(line)
}

func askInt(r *bufio.Reader, prompt string, def int64) int64 {
	for {
		s := ask(r, prompt, strconv.FormatInt(def, 10))
		v, err := strconv.ParseInt(s, 10, 64)
		if err == nil && v > 0 {
			return v
		}
		fmt.Println("  需要正整数,重新输入")
	}
}

func askPorts(r *bufio.Reader) []string {
	for {
		s := ask(r, "监听端口(空格分隔,1-65535,最多 64 个;支持范围如 8888-8895)", "8888 8889")
		ports, err := parsePortSpec(s)
		if err != nil {
			fmt.Printf("  %v,重新输入\n", err)
			continue
		}
		if len(ports) == 0 {
			fmt.Println("  至少需要一个端口,重新输入")
			continue
		}
		return ports
	}
}

// askPass 输入两次一致的合法密码;直接回车返回 ""(不使用密码)
func askPass(r *bufio.Reader, hint string) string {
	for {
		p := ask(r, hint+"(直接回车 = 不使用)", "")
		if p == "" {
			return ""
		}
		if err := validatePass(p); err != nil {
			fmt.Printf("  %v,重新输入\n", err)
			continue
		}
		p2 := ask(r, "  再输入一遍确认", "")
		if p2 == p {
			return p
		}
		fmt.Println("  两次不一致,重来")
	}
}

func confirm(r *bufio.Reader, cmd string) bool {
	fmt.Printf("\n等价命令行(记住它,下次可直接用):\n  %s\n\n确认运行? (y/n) ", cmd)
	line, err := r.ReadString('\n')
	if err != nil && line == "" {
		return false
	}
	s := strings.TrimSpace(line)
	return s == "y" || s == "Y"
}

// runTUI 向导主循环;选定的模式跑完即返回(由 main 结束进程)
func runTUI() {
	fmt.Printf("tcp-stress %s (%s/%s) — TCP 长连接压测向导\n", version, runtime.GOOS, runtime.GOARCH)
	fmt.Println("提示:向导中 Ctrl-C 直接退出;测试运行中 Ctrl-C 优雅退出并打印统计")
	fmt.Println("本工具仅供测试自有/授权设备;客户端只与 tcp-stress 服务端建连")
	r := bufio.NewReader(os.Stdin)
	for {
		fmt.Println(`
  1) 服务器模式 — 放在受测线路那端,接受连接
  2) 客户端模式 — 发起连接并维持,测线路极限
  3) 查看帮助
  q) 退出`)
		fmt.Print("选择: ")
		line, err := r.ReadString('\n')
		if err != nil && line == "" {
			return // EOF / Ctrl-D
		}
		switch strings.TrimSpace(line) {
		case "1":
			ports := askPorts(r)
			pass := askPass(r, "鉴权密码")
			cmd := fmt.Sprintf("tcp-stress -s %s", strings.Join(ports, " "))
			if pass != "" {
				cmd = fmt.Sprintf("tcp-stress -s -pass <密码> %s", strings.Join(ports, " "))
			}
			if !confirm(r, cmd) {
				continue
			}
			fmt.Println("运行中…(Ctrl-C 优雅退出)")
			if err := runServer(ports, pass); err != nil {
				fmt.Fprintln(os.Stderr, "error:", err)
				os.Exit(1)
			}
			return
		case "2":
			var servers, bind string
			for {
				s := ask(r, "服务器地址列表(须为 tcp-stress 服务端,IP:Port 逗号分隔,最多 64 个;端口支持范围如 127.0.0.1:18888-18895)", "127.0.0.1:8888")
				if _, err := parseTargets(s); err == nil {
					servers = s
					break
				}
				fmt.Println("  格式: IP:Port,逗号分隔,如 192.168.1.100:8888,192.168.1.100:8889")
			}
			target := askInt(r, "目标连接数", 10000)
			var rate int64
			for {
				rate = askInt(r, "建连速率上限(个/秒,最大 50000)", 200)
				if rate <= 50000 {
					break
				}
				fmt.Println("  上限 50000,重新输入")
			}
			for {
				b := ask(r, "本地出口 IP(多 WAN/策略路由用,回车跳过)", "")
				if b == "" || net.ParseIP(b) != nil {
					bind = b
					break
				}
				fmt.Println("  不是合法 IP,重新输入")
			}
			pass := askPass(r, "鉴权密码(服务器设置了才填)")
			cmd := fmt.Sprintf("tcp-stress -c -servers %q -target %d -rate %d",
				servers, target, rate)
			if bind != "" {
				cmd += fmt.Sprintf(" -bind %s", bind)
			}
			if pass != "" {
				cmd += " -pass <密码>"
			}
			if !confirm(r, cmd) {
				continue
			}
			fmt.Println("运行中…(Ctrl-C 优雅退出)")
			runClient(clientConfig{
				servers:   servers,
				bind:      bind,
				target:    uint64(target),
				rate:      int(rate),
				keepAlive: 30 * time.Second,
				statsInt:  time.Second,
				pass:      pass,
			})
			return
		case "3":
			flag.CommandLine.SetOutput(os.Stdout)
			flag.Usage()
		case "q", "Q":
			return
		}
	}
}
