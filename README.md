# 家用宽带极限 TCP 连接测试 (tcp-stress)

测试家用宽带 / 路由器 / NAT 设备在不交换业务数据的前提下,能维持多少个并发 TCP 长连接。
**单二进制 `tcp-stress`,全平台全功能**:
- `-s` 服务器模式:Linux 用 C + epoll(cgo 内嵌,极致内存);macOS / Windows / armv7l 用纯 Go 等效实现(行为对齐)
- `-c` 客户端模式:纯 Go,token-bucket 限速,自动多端口轮询
- `-pass`:服务端鉴权(v1.0.6)——设了密码的 服务端,客户端必须提供相同密码才计入统计
- 裸运行进 TUI 向导(v1.0.6)——终端里不带参数运行,问答式配好全部参数,零学习成本

```bash
tcp-stress                                    # 终端里裸运行,进入交互向导
tcp-stress -s 8888 8889 8890                # 服务器(受测线路那端)
tcp-stress -s -pass 秘密 8888 8890            # 带鉴权的服务端
tcp-stress -c -servers "1.2.3.4:8888,1.2.3.4:8889" -target 100000 -rate 200
tcp-stress -c -servers "1.2.3.4:8888" -pass 秘密 -target 10000    # 客户端带密码
tcp-stress -v                                # 版本号(-version 同义)
tcp-stress -h                                # 帮助(-help 同义,exit 0)
```

---

## 文件清单

| 文件 | 说明 |
| --- | --- |
| `main.go` | 单二进制入口,`-s`/`-c` 模式分发与 flag 定义 |
| `csrc/server.c` | C 服务端实现(多端口 epoll,KeepAlive 抗 NAT 老化) |
| `server_linux.go` | cgo 桥:Linux 构建把 C 服务端编进二进制 |
| `server_go.go` | 纯 Go 服务端实现(darwin / windows,行为与 C 版对齐) |
| `client.go` | Go 客户端主逻辑,轮询打满多端口 |
| `auth.go` | v1.0.6 鉴权协议(单行文本握手)与客户端/纯 Go 服务端实现 |
| `tui.go` + `tui_tty_*.go` | v1.0.6 零参数交互向导;TTY 判定按平台走 ioctl(isatty) |
| `sockopt_unix.go` / `sockopt_windows.go` | 平台相关的底层 socket 调优(按 build tag 二选一) |
| `rlimit_unix.go` / `rlimit_windows.go` | fd 软上限自动抬升(按 build tag 二选一) |
| `Makefile` | 构建脚本(静态 cgo 构建) |
| `go.mod` | Go module 定义 |
| `.github/workflows/release.yml` | 云编译 + 自动发 Release |

> C 源码放 `csrc/` 子目录是刻意的:目录里直接有 `.c` 文件时,
> `CGO_ENABLED=0 go build .` 会报 "C source files not allowed",
> 所有纯 Go 构建都会炸。由 cgo 前导 `#include "csrc/server.c"` 引入,
> 纯 Go 构建完全不碰它(服务端走 `server_go.go`)。

---

## 一、上手即用(零配置)

**v1.0.4 起无需任何系统调优即可跑**:程序启动时自动把 fd 软上限抬到硬上限
(普通权限操作,不需要 root),发行版默认环境(soft 1024 / hard 数十万)直接用:

```bash
tcp-stress -s 8888 8889
tcp-stress -c -servers "1.2.3.4:8888,1.2.3.4:8889" -target 10000
```

启动时会打印实际生效值确认:`fd limit: soft=1048576 hard=1048576`。

**唯一仍需管理员的场景**:fd **硬**上限本身太低(某些容器 / 老系统只有 4096)。
这不是程序能绕的(soft 只能抬到 hard),需一次性调整:

```bash
ulimit -H -n 1048576                      # 会话级(需 root)
# systemd 服务:  [Service] LimitNOFILE=1048576
# /etc/security/limits.conf:  * hard nofile 1048576
```

---

## 一a、服务端鉴权(-pass,v1.0.6)

公网 VPS 上裸跑服务端,任何扫到端口的人都能连上来占 fd 槽位。`-pass` 给服务端加一道密码:

```bash
# 服务端:设密码(1-128 字节,不能含空白)
tcp-stress -s -pass 我的密码 8888 8889

# 客户端:提供相同密码
tcp-stress -c -servers "1.2.3.4:8888,1.2.3.4:8889" -pass 我的密码 -target 10000
```

**协议与语义**:
- 连接建立后服务端先发 `AUTH?\n`,客户端回 `AUTH <密码>\n`,服务端回 `AUTH OK\n` 或 `AUTH ERR\n`
- 鉴权通过才计入 `alive`/`total_acc`;失败/超时(10s)的连接直接关闭,只计 `auth_fail`(STATS 行可见)
- 密码错误连续 10 次**且期间无任何成功**:客户端主动中止(密码配错当场暴露,不空转刷连接)。注意混合列表(部分对部分错)时任何一次成功都会清零连败计数,继续正常跑、不会中止(实测 12 连败未中止)
- 服务端要鉴权而客户端没配 `-pass`:客户端立即报错退出;服务端无鉴权而客户端带了 `-pass`:忽略并提示(两端版本/配置不匹配不会被静默吞掉)
- 双端都不配 `-pass`:协议完全不出现,行为与旧版逐字节一致

**密码传递**:
- `-pass 密码`:便捷,但密码会出现在进程命令行里(`ps aux` / `/proc/<pid>/cmdline` 可见),同机多用户环境注意
- `-passfile 文件`:从文件首行读密码(建议 `chmod 600`),**不落进程命令行**,敏感场景用这个;与 `-pass` 互斥

**注意**:明文单行协议,防的是公网误用/白嫖,不是密码学对抗;公网部署仍建议配合安全组限源(见「安全注意」)。

---

## 一b、大规模调优(可选,10 万+ 连接才需要)

> 程序已自动抬 fd 软上限;以下项不影响"能不能跑",只影响 10 万+ 规模的
> 成功率与性能。两台机器都改(或至少改服务端)。

```bash
# 1) 系统级 fd 总数
sysctl -w fs.file-max=2097152
echo 'fs.file-max = 2097152' >> /etc/sysctl.conf

# 2) 内核 TCP 内存档 - 10w+ 连接需要抬高三档
sysctl -w net.ipv4.tcp_mem='131072 262144 524288'
sysctl -w net.ipv4.tcp_wmem='4096 8192 16384'
sysctl -w net.ipv4.tcp_rmem='4096 8192 16384'

# 3) 允许 TIME_WAIT 复用 + 扩大半连接 / 全连接队列
sysctl -w net.ipv4.tcp_tw_reuse=1
sysctl -w net.ipv4.tcp_max_syn_backlog=262144
sysctl -w net.core.somaxconn=262144

# 4) 本地端口范围 - 默认 32768-60999(~28k/端口);单目标 10w+ 必须扩,
#    或者直接给 -servers 多配几个端口(每端口独立 ~28k,程序自动轮询)
sysctl -w net.ipv4.ip_local_port_range='1024 65535'

# 5) conntrack 表(路由器/网关或开了 NAT 的机器需要)
#    实测 10w 连接至少给 30w,留 3x 余量
sysctl -w net.netfilter.nf_conntrack_max=524288
# 注意:nf_conntrack_buckets 是只读 sysctl,运行时改不了,只能模块加载时设
#   echo 'options nf_conntrack hashsize=131072' | sudo tee /etc/modprobe.d/nf_conntrack.conf
# (改完需重启;只调 max 不调 hashsize 也能跑,只是哈希偏挤)
```

> 重启后 `sysctl.conf` 自动生效。

**验证:**
```bash
cat /proc/sys/fs/file-nr        # 第一列应远小于 file-max
ss -s                            # TCP 各状态连接数
```

---

## 二、编译

### 2.1 Linux(全功能,含 C 服务端)

```bash
make            # 静态 cgo 构建,产物不挑 glibc,可直接拷到别的机器
make dyn        # 动态构建,本机调试编译快
```

需要 gcc + Go 1.23+。手动等价:

```bash
CGO_ENABLED=1 go build -trimpath -tags 'osusergo netgo' \
  -ldflags '-s -w -extldflags -static' -o tcp-stress .
```

### 2.2 其他平台 / 未装 gcc 的 Linux(纯 Go,全功能)

```bash
CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -trimpath -ldflags '-s -w' -o tcp-stress.exe .
CGO_ENABLED=0 GOOS=darwin  GOARCH=arm64 go build -trimpath -ldflags '-s -w' -o tcp-stress     .
CGO_ENABLED=0 GOOS=linux   GOARCH=arm   GOARM=7 go build -trimpath -ldflags '-s -w' -o tcp-stress-armv7l .
```

epoll 是 Linux 独有,这些构建的服务端走 `server_go.go`(纯 Go):
STATS 格式 / 断连回收 / 优雅退出 / KeepAlive 参数与 C 版对齐,
每连接一个 goroutine(约 8KB/连接),中小规模与功能使用无碍;
**10w+ 大规模压测建议仍用 Linux C 服务端**(epoll + 极致内存)。

### 2.3 直接用 Release 里的预编译产物

每次打 tag 由 GitHub Actions 云编译并发布到 Releases,无需本地工具链:

| 产物 | 内容 | 适用 |
| --- | --- | --- |
| `tcp-conn-stress-linux-amd64.tar.gz` | `tcp-stress`(全功能) | x86_64 Linux |
| `tcp-conn-stress-linux-arm64.tar.gz` | `tcp-stress`(全功能) | ARM64 Linux(树莓派等) |
| `tcp-conn-stress-linux-armv7l.tar.gz` | `tcp-stress`(全功能,纯 Go 服务端) | armv7l 32 位 ARM(老树莓派 / 路由器 / OpenWrt) |
| `tcp-conn-stress-darwin-amd64.tar.gz` | `tcp-stress`(全功能,纯 Go 服务端) | Intel Mac |
| `tcp-conn-stress-darwin-arm64.tar.gz` | `tcp-stress`(全功能,纯 Go 服务端) | Apple Silicon Mac |
| `tcp-conn-stress-windows-amd64.zip` | `tcp-stress.exe`(全功能,纯 Go 服务端) | Windows |
| `SHA256SUMS.txt` | 校验和 | 全部 |

想自己触发一次云编译,推个 tag 即可:

```bash
git tag v1.0.1 && git push origin v1.0.1
```

---

## 三、运行示例

### 3.1 单机自测(本机到本机,验证工具链)

```bash
# 终端 A:服务端,4 个端口
tcp-stress -s 18888 18889 18890 18891

# 终端 B:客户端,打到 5 万连接,400 conn/s
tcp-stress -c -servers "127.0.0.1:18888,127.0.0.1:18889,127.0.0.1:18890,127.0.0.1:18891" \
                 -target 50000 -rate 400 -stats 1s
```

### 3.2 跨机器测家用宽带(典型用法)

服务端放在受测线路的**内网服务器**(或树莓派)上:

```bash
# 内网服务端:多端口
tcp-stress -s 18888 18889 18890 18891 18892 18893 18894 18895
```

另一台机器(可同内网、可公网 VPS)当客户端:

```bash
# 跨网:目标打 10w,默认 200 conn/s(避开运营商 QoS 突发限速)
tcp-stress -c -servers "192.168.1.100:18888,192.168.1.100:18889,...,192.168.1.100:18895" \
                 -target 100000 -rate 200

# 多 WAN / 策略路由:指定出口 IP
tcp-stress -c -bind 192.168.10.5 \
                 -servers "1.2.3.4:18888,1.2.3.4:18889" \
                 -target 80000 -rate 150
```

### 3.3 观察指标

服务端每秒打印:
```
[STATS t=1790409523s] alive=14613 | total_acc=14613 total_close=0 | +9315/s -0/s
         ports: 18888=14613
```

客户端每秒打印:
```
[STAT] alive=14613 try=14613 ok=14613 closed=0 | +try/s=95 +ok/s=95 | fail t/o=0 rst=0 addr-full=0 fd-full=0 eof=0 auth=0 other=0
```

**关注:**
- `+try/s` ≈ `+ok/s`:网络通畅
- `addr-full` 持续涨 → **客户端端口耗尽**,加端口或加网卡
- `t/o` 持续涨 → **光猫/NAT 老化或对端丢包**,调大客户端 `-keepalive`
- `closed` 持续涨(自己没在退出)→ **中间设备在拔连接** — 这就是要测的"家用宽带极限"
- 服务端 `alive` 稳态不增 → 客户端打到目标值或被 QoS 限速

### 3.4 `-rate` 是上限,不是保证

令牌桶只保证**不超速**;实际速率受客户端机器与网络制约。本仓库实测(4 核 arm64):
空载时第一秒即可按 `-rate 3000` 精确打满;同一机器维持 12k 连接的高载下,
`-rate 3000` 实际只能跑出 ~1200 conn/s(worker 上限 256→2048 无改善,瓶颈在机器饱和)。
要高建连速率,用更强的机器或分布式多客户端。

内存量级参考:客户端约 **7KB/连接**(goroutine-per-conn 模型),
100k 连接需预留 ~700MB;服务端用户态几乎不存连接状态,12k 连接 RSS 仅 ~4MB。

---

## 四、安全注意(部署前必读)

- 不设 `-pass` 时,服务端监听 **0.0.0.0 且无任何认证**,任何能路由到端口的客户端都能建立连接、占用 fd 槽位;**公网部署请一律加 `-pass`**
- 服务端(双实现)只监听 **IPv4**(`0.0.0.0`);IPv6 仅客户端 target 支持(如 `[::1]:8888`),v6 服务端监听不在当前目标内
- `-pass` 是明文单行协议(见「服务端鉴权」),能挡扫描器和误用,挡不住嗅探/中间人;**公网 VPS 部署仍建议叠加安全组 / 防火墙限源 IP**,只放行测试客户端的出口地址,用完即关
- `-s` 模式的端口只用于压测,不要复用已有服务的端口段;测试期间这些端口等于对外开放
- 密码传递:`-pass` 会落进程命令行(`ps aux` 可见),敏感/多用户场景用 `-passfile`(v1.0.7)
- 开 `-pass` 的服务端仍有资源面:鉴权中连接队列上限 4096(双实现一致,C 版驱逐延迟到批间 close 杜绝 epoll 陈旧事件下溢;纯 Go 版 v1.0.9 起对齐——旧版无上限,实测 8000 停滞连接全收),满时驱逐挂起连接保证合法客户端进得来;慢速滴流垃圾字节会消耗服务端 CPU(实测 1000 连接 × 50B/s ≈ 单核 28%),公网部署叠加限源才是正解
- 已建连上灌数据的代价,双实现不对称(实测 500 连接 × 10KB/s):**C 版 0.0% 单核** —— 已建连不注册 EPOLLIN,内核缓冲满后自动零窗口,攻击者吞吐被内核钳住;**纯 Go 版约 5% 单核** —— goroutine 丢弃读允许对端全速灌。需要抗数据面滥用时选 Linux C 服务端
- 退出服务端后确认端口已关(`ss -tlnp | grep <port>`),容器/服务化部署另加访问控制

---

## 五、设计要点速览

### 服务端(`csrc/server.c`,经 `server_linux.go` cgo 编入)
- 一个 `epoll_create1` 实例管理**所有**监听 fd,`accept4` 一次性 accept loop 到 `EAGAIN`
- 已建连 socket 也注册进同一个 epoll(只盯 `EPOLLRDHUP/EPOLLERR/EPOLLHUP`,不收数据)→ 对端断开即回收,`alive`/`total_close`/分端口计数都是真值
- `accept4` 遇 `EMFILE/ENFILE`:摘下 listener 1s 再挂回 + 10s 限频日志,fd 耗尽不空转
- `SO_RCVBUF/SO_SNDBUF=2048`,10w 连接 fd 内存 ≈ `sizeof(struct file)` 内核侧 + 用户态接近 0
- `SO_KEEPALIVE` + `TCP_KEEPIDLE=60 / KEEPINTVL=10 / KEEPCNT=3` → 客户端静默 60s 后开始探测,30s 内判定对端死,触发本端发 RST
  - 与运营商 NAT 老化(典型 120-300s)留出余量
- 退出时遍历 `/proc/self/fd` 逐个 close 已建连 socket → 客户端收到 FIN 而非 RST,`active` 优雅归零

### cgo 合并方式
- `server.c` 全部符号 `static`,经 `#include "csrc/server.c"` 编入 cgo 生成的编译单元,不产生包级符号,无重复定义
- `-D_GNU_SOURCE=1` 由 cgo CFLAGS 命令行注入,保证 `accept4` 等扩展在 include 任何头之前就可见
- Go 侧 `runServer` 把端口列表拼成 argv 传给 `tcp_server_main(argc, argv)`,进入后**不再回到 Go 运行时**(C 主循环自带 SIGINT/SIGTERM/SIGHUP/SIGQUIT 优雅退出处理)
- 纯 Go 构建的服务端走 `server_go.go` 全功能实现(旧版 `server_stub.go` 已于 v1.0.5 移除)

### 客户端(`client.go`)
- `net.Dialer.Control` 走 `syscall.RawConn.Control(fd)` → 在内核 fd 上 `SetsockoptInt` 压缓冲区
- `KeepAlive: 30s`:在 NAT 老化之前从本端发心跳包,**对端(本服务端)60s 内必收探测包**
- 令牌桶:每秒按 `-rate` 注入令牌,worker 抢令牌去 `DialContext`
- 失败分类:`timeout` / `refused` / `cannot assign requested address`(端口耗尽)/ `EOF` / 其他
- 端口耗尽会**自动切下一个 target**,不卡死
- `holdConn` **不设读 deadline**:对端不发数据,deadline 只会把健康连接误杀(历史 bug:每条连接活不过 120s,全体旋转木马重拨);死链交 keepalive 内核探测感知
- `closed` 计数:维持期间被断开的连接 — 真实 NAT 老化的直接观测指标

---

## 六、本仓库冒烟记录

- 容器内回环:单端口 18888,2 秒内 accept **9315/s**,峰值活跃 **14613**。
- 验证 `epoll` + `accept4` 主循环 + keepalive + 统计线程均工作正常。
- 容器 ulimit 524288、内核无 conntrack 限制。

2026-09-27 v1.0.9 第三轮审计修复(P1×1 / P2×4):

- **纯 Go 服务端鉴权并发上限(P1)**:accept 后鉴权中的连接上限 4096(与 C 版 AUTH_MAX 对齐),满时驱逐一条挂起连接给新连接让位 —— 旧版无上限,实测 8000 停滞连接全收(ΔRSS 43.5MB),公网暴露的 darwin/windows/armv7l 服务端可被无限挂连接耗 fd/内存;修复后同场景驱逐生效、RSS 封顶、停滞洪水下合法客户端 ok=10/10
- **空 host target 拒绝(P2)**:`-servers ":8888"` / `"[::]:8888"` 从静默连本机(实测 try=3 ok=3,笔误测错对象无提示)改为显式报错;`[::1]:8888` 等具体地址不受影响
- server.c 退出遍历 /proc/self/fd 跳过 dirfd(P2):消除 close(readdir 正在用的目录 fd) 的理论 UB
- README:混合列表中止语义修正(仅全部 target 鉴权失败才中止)、v4-only 监听说明;CI 新增空 host 校验两行与纯 Go 服务端鉴权上限回归步(4200 停滞 + 合法客户端 ok=10/10 + 驱逐日志断言)

2026-09-27 v1.0.8 复审修复轮(P0×1 / P1×3 / P2×3):

- **驱逐陈旧事件下溢(P0)**:满队列驱逐改为"受害者先进本批待关队列、批间统一 close",并跳过其批内陈旧事件 —— 旧版驱逐即 close,同批快照里受害者的 RDHUP 走已建连关闭路径,`g_total_alive` 从未入账就减一,永久下溢 2^64-1(实测 `final alive=18446744073709551615`,per_port 同步下溢)
- **驱逐风暴 CPU 放大(-37%)**:满队列扫描取队头前缀 64 槽最老(不再全队列 O(4096) 扫),断开路径复用已知下标免二次扫描;9500 conn/s 风暴下单连接 CPU 成本 0.149ms→0.093ms,风暴中合法客户端 ok=10/10
- **客户端 banner 迟到自愈**:5s 内未收到 banner 被误判 noAuth 后,完整 banner 迟到时现场补握手并撤销误判 —— 旧版该状态永久固化,高延迟服务端导致带正确密码的客户端整批 Fatal 自杀;未配密码场景仍 Fatal,文案不变
- **-passfile 限读 129 字节**:1GB 文件内存峰值 2055MB→736KB(旧版 `os.ReadFile` 全量读入才报"密码最长 128 字节")
- **纯 Go 服务端退出加速**:鉴权中连接纳入退出关闭范围,SIGINT 后固定 5.0s→即时退出
- **独立编译端口严格校验**:`strtol` 换成与 Go 侧同语义的 `valid_port_str`(拒 `8888x`/`0080`/` 8888`)
- gofmt 全量通过;CI 新增 gofmt 检查步与陈旧事件下溢回归步(4200 停滞 + 6 万洪水 + 配对关最老,断言计数器恒零)

2026-09-27 v1.0.7 全量审计修复轮(P0×2 / P1×6 / P2×4):

- **鉴权槽位 DoS 修复**:C 服务端鉴权队列满(4096)时改为驱逐最早超时的挂起连接 —— 旧版直接拒绝新连接,4200 个"连上不回密码"的停滞连接即可把正确密码的客户端整个拒之门外(实测 ok=0),修复后同场景 ok=10/10
- **混合鉴权列表修复**:`-pass` 状态从全局单标志改为 per-target —— 旧版列表里混有"无密码+有密码"服务器时,无密码服务器毒化全局状态,正确密码也被误杀(误报"本端未配 -pass");修复后两种顺序均 final try=20 ok=20
- 端口解析收紧:`strconv.Atoi` + 回环校验,`8888x`/`0080`/`+80` 一律拒绝(旧版 `fmt.Sscanf` 静默拨 `8888x`→8888)
- 双实现语义对齐:鉴权行尾多余字节忽略(与 Go 版一致);纯 Go 服务端收到数据改为丢弃保持连接(与 C 版一致)
- SIGQUIT(Ctrl-\)双端均优雅退出打 final 统计(旧版客户端打栈退出 rc=2 统计丢)
- 新增 `-passfile`:密码从文件读,不落 `/proc/<pid>/cmdline`
- target 防过冲:worker 拨号前原子预留槽位,`active` 恒 ≤ target(旧版竞态下 max_alive=target+1)
- 文档:`.gitignore` 盖住全部产物变体;README 修正 server_stub 死引用/STATS 示例字段/文件清单/sysctl 编号;CI 新增审计回归步(槽位驱逐、混合列表、端口校验、SIGQUIT、过冲上限)

2026-09-26 审计轮(修复后复验):

- 165s 长测(跨过旧 120s bug 阈值):`try=300 = target`,零重拨 — 旧版同场景 `try=600`
- `kill -9` 客户端:服务端 3s 内 `alive=0`、`total_close=200`、分端口全归零
- 同端口双实例:第二实例 `Address already in use` 拒绝(移除 `SO_REUSEPORT`)
- 参数校验:`-rate 50001` / `-keepalive -1s` 均拒绝
- CI 含同款长测,每次发版自动跑

2026-09-26 v1.0.6 鉴权 + TUI + armv7l:

- `-pass` 服务端鉴权:单行文本协议(`AUTH?` / `AUTH <密码>` / `AUTH OK|ERR`),C/epoll 与纯 Go 双实现语义一致;鉴权通过才计数,失败/超时(10s)计 `auth_fail`;客户端密码错 10 连败中止、配置不匹配当场报错不静默
- C/epoll 侧鉴权走 EPOLLIN 状态机:行累积到 `\n` 才判定(密码行 TCP 分段到达不误杀,实测逐字节发送通过),鉴权中队列 AUTH_MAX=4096 + 超时扫描防 fd 耗尽
- 零参数 TUI 向导:真终端裸运行进入,问答式配置(端口/地址/密码等,回车取默认),展示等价命令行再运行;管道/CI/重定向仍 usage exit 2(isatty ioctl 判定,挡住 /dev/null 这类字符设备)
- 新增 linux-armv7l 产物(GOARM=7 纯 Go,老树莓派/OpenWrt 可用);CI smoke 增加鉴权矩阵与 TUI 冒烟
- 已知边界:freebsd 构建有 rlimit 类型历史问题(无发布产物,不影响)

2026-09-26 v1.0.5 全平台全功能:

- 新增 `server_go.go`:纯 Go 服务端,darwin / windows(及未开 cgo 的 Linux)的 `-s` 可用
- 行为与 C 版对齐:STATS 逐字同格式、kill -9 客户端 3s 内回收(total_close=100 实测)、SIGHUP/INT/TERM 优雅退出、KeepAlive 60/10/3、2K 小缓冲
- 构建标签:`linux && cgo` → C/epoll;其余 → server_go.go;`isValidPort` 上移 main.go 共享
- go.mod 1.19 → 1.23(net.KeepAliveConfig);CI smoke 增加纯 Go 服务端双模式断言

2026-09-26 v1.0.4 零配置可用:

- 启动自动把 fd 软上限抬到硬上限(普通权限,无需 root),默认环境免 `ulimit -n`
- EMFILE 客户端侧显式分类 `fd-full` 并退避(旧版错进 other 且狂打)
- sysctl 调优降级为"10 万+ 可选";README 重写为上手即用
- CI 断言:soft 压 256 下 300 连接全部成功(自动抬升生效)

2026-09-26 v1.0.3 版本/帮助参数:

- `-v` / `--version`:打印 `tcp-stress <版本> (平台/架构)` 并退出;版本号由构建注入(`make VERSION=v1.0.3`,CI 从 tag 取),CI 已断言
- `-h` / `--help`:简要帮助走 stdout + exit 0;裸调用 / 参数错误仍走 stderr + exit 2
- `-v`、`-h` 优先于 `-s`/`-c` 模式参数

2026-09-26 v1.0.2 审计修复轮:

- SIGHUP 优雅退出:ssh 断开不再暴毙,client final / server bye 统计保住(旧版实测直接死、统计全丢)
- IPv6 target:`[::1]:port` 经 `net.JoinHostPort` 重组,拨号进入网络层(旧版 "too many colons")
- `-target` 溢出(≥2^63)明确拒绝(旧版 int64 转负 → 静默零拨号)
- 位置参数防护:`tcp-stress 8888 -c` / `-c extra` 均报错并提示模式参数须在最前(旧版静默落 usage)
- `-stats` 非 1s 时 `+/s` 按周期换算为真实每秒速率
- README 补安全注意(0.0.0.0 无认证,公网必须限源)与 rate 语义(上限非保证)

2026-09-26 v1.0.1 合并轮(单二进制):

- 分发/参数校验 6 项:无参 usage、`-s -c` 互斥、`-s` 无端口/非法端口拒绝、纯 Go 构建下 `-s` 报 stub 错、`-c` 参数校验不变
- 8 端口 800 连接:alive=800 精确 8 等分,双端优雅退出(rc=0)
- `kill -9` 客户端:cgo 化后服务端仍 3s 内 `alive=0 total_close=100`
- 60s 稳态:`try` 峰值 = 200 = target,零重拨
