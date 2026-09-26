# 家用宽带极限 TCP 连接测试 (tcp-stress)

测试家用宽带 / 路由器 / NAT 设备在不交换业务数据的前提下,能维持多少个并发 TCP 长连接。
**v1.0.1 起合并为单二进制 `tcp-stress`**:
- `-s` 服务器模式:C + epoll(cgo 内嵌),多端口监听,极致内存,仅 Linux
- `-c` 客户端模式:纯 Go,token-bucket 限速,自动多端口轮询,全平台

```bash
tcp-stress -s 8888 8889 8890                # 服务器(受测线路那端)
tcp-stress -c -servers "1.2.3.4:8888,1.2.3.4:8889" -target 100000 -rate 200
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
| `server_stub.go` | 非 Linux / 纯 Go 构建的服务端占位(`-s` 明确报错) |
| `client.go` | Go 客户端主逻辑,轮询打满多端口 |
| `sockopt_unix.go` / `sockopt_windows.go` | 平台相关的底层 socket 调优(按 build tag 二选一) |
| `Makefile` | 构建脚本(静态 cgo 构建) |
| `go.mod` | Go module 定义 |
| `.github/workflows/release.yml` | 云编译 + 自动发 Release |

> C 源码放 `csrc/` 子目录是刻意的:目录里直接有 `.c` 文件时,
> `CGO_ENABLED=0 go build .` 会报 "C source files not allowed",
> 所有纯 Go 构建(交叉编译 darwin/windows)都会炸。由 cgo 前导
> `#include "csrc/server.c"` 引入,非 Linux 构建完全不碰它。

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

## 一b、大规模调优(可选,10 万+ 连接才需要)

> 程序已自动抬 fd 软上限;以下项不影响"能不能跑",只影响 10 万+ 规模的
> 成功率与性能。两台机器都改(或至少改服务端)。

```bash
# 1) 系统级 fd 总数
sysctl -w fs.file-max=2097152
echo 'fs.file-max = 2097152' >> /etc/sysctl.conf

# 3) 内核 TCP 内存档 - 10w+ 连接需要抬高三档
sysctl -w net.ipv4.tcp_mem='131072 262144 524288'
sysctl -w net.ipv4.tcp_wmem='4096 8192 16384'
sysctl -w net.ipv4.tcp_rmem='4096 8192 16384'

# 4) 允许 TIME_WAIT 复用 + 扩大半连接 / 全连接队列
sysctl -w net.ipv4.tcp_tw_reuse=1
sysctl -w net.ipv4.tcp_max_syn_backlog=262144
sysctl -w net.core.somaxconn=262144

# 5) 本地端口范围 - 默认 32768-60999(~28k/端口);单目标 10w+ 必须扩,
#    或者直接给 -servers 多配几个端口(每端口独立 ~28k,程序自动轮询)
sysctl -w net.ipv4.ip_local_port_range='1024 65535'

# 6) conntrack 表(路由器/网关或开了 NAT 的机器需要)
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

需要 gcc + Go 1.19+。手动等价:

```bash
CGO_ENABLED=1 go build -trimpath -tags 'osusergo netgo' \
  -ldflags '-s -w -extldflags -static' -o tcp-stress .
```

### 2.2 其他平台(仅客户端)

```bash
CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -trimpath -ldflags '-s -w' -o tcp-stress.exe .
CGO_ENABLED=0 GOOS=darwin  GOARCH=arm64 go build -trimpath -ldflags '-s -w' -o tcp-stress     .
```

epoll 是 Linux 独有,darwin / windows 的二进制不含服务端,`-s` 会明确报错。

### 2.3 直接用 Release 里的预编译产物

每次打 tag 由 GitHub Actions 云编译并发布到 Releases,无需本地工具链:

| 产物 | 内容 | 适用 |
| --- | --- | --- |
| `tcp-conn-stress-linux-amd64.tar.gz` | `tcp-stress`(全功能) | x86_64 Linux |
| `tcp-conn-stress-linux-arm64.tar.gz` | `tcp-stress`(全功能) | ARM64 Linux(树莓派等) |
| `tcp-conn-stress-darwin-amd64.tar.gz` | `tcp-stress`(仅客户端) | Intel Mac |
| `tcp-conn-stress-darwin-arm64.tar.gz` | `tcp-stress`(仅客户端) | Apple Silicon Mac |
| `tcp-conn-stress-windows-amd64.zip` | `tcp-stress.exe`(仅客户端) | Windows |
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
[STAT] alive=14613 try=14613 ok=14613 closed=0 | +try/s=95 +ok/s=95 | fail t/o=0 rst=0 addr-full=0 eof=0 other=0
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

- 服务端监听 **0.0.0.0 且无任何认证**,任何能路由到端口的客户端都能建立连接、占用 fd 槽位
- **公网 VPS 部署必须用安全组 / 防火墙限源 IP**,只放行测试客户端的出口地址,用完即关
- `-s` 模式的端口只用于压测,不要复用已有服务的端口段;测试期间这些端口等于对外开放
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
- Go 侧 `runServer` 把端口列表拼成 argv 传给 `tcp_server_main(argc, argv)`,进入后**不再回到 Go 运行时**(C 主循环自带 SIGINT/SIGTERM 处理)
- `server_stub.go`(`//go:build !linux || !cgo`)保证纯 Go 构建可编译、`-s` 报错清晰

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

2026-09-26 审计轮(修复后复验):

- 165s 长测(跨过旧 120s bug 阈值):`try=300 = target`,零重拨 — 旧版同场景 `try=600`
- `kill -9` 客户端:服务端 3s 内 `alive=0`、`total_close=200`、分端口全归零
- 同端口双实例:第二实例 `Address already in use` 拒绝(移除 `SO_REUSEPORT`)
- 参数校验:`-rate 50001` / `-keepalive -1s` 均拒绝
- CI 含同款长测,每次发版自动跑

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
