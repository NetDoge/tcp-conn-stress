# 家用宽带极限 TCP 连接测试 (C/S)

测试家用宽带 / 路由器 / NAT 设备在不交换业务数据的前提下,能维持多少个并发 TCP 长连接。
- 服务端:C + epoll,多端口监听,极致内存
- 客户端:Go,token-bucket 限速,自动多端口轮询

---

## 文件清单

| 文件 | 说明 |
| --- | --- |
| `server.c` | 多端口 epoll 服务端,KeepAlive 抗 NAT 老化 |
| `Makefile` | C 端编译脚本 |
| `client.go` | Go 客户端,轮询打满多端口 |

---

## 一、Linux 系统调优(必做,否则根本起不来 10w+)

> 这些是**单台机器**的内核上限,需要在**服务端**和**客户端**两台机器都改(或至少改服务端)。

```bash
# 1) 用户态 fd 上限 - 当前会话 + 持久化
ulimit -n 1048576
echo '* soft nofile 1048576'  > /etc/security/limits.conf
echo '* hard nofile 1048576'  >> /etc/security/limits.conf

# 2) 系统级 fd 总数
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

# 5) 本地端口范围 - 默认 32768-60999(~28k);要 10w+ 必须扩
sysctl -w net.ipv4.ip_local_port_range='1024 65535'

# 6) conntrack 表(路由器/网关或开了 NAT 的机器需要)
#    实测 10w 连接至少给 30w,留 3x 余量
sysctl -w net.netfilter.nf_conntrack_max=524288
# 已分配的 conntrack 哈希表大小(老内核才需要)
sysctl -w net.netfilter.nf_conntrack_buckets=131072
```

> 重启后 `sysctl.conf` / `limits.conf` 自动生效;已开 ssh 的会话用 `ulimit -n 1048576` 立即抬升。

**验证:**
```bash
cat /proc/sys/fs/file-nr        # 第一列应远小于 file-max
ulimit -n                         # 1048576
ss -s                            # TCP 各状态连接数
```

---

## 二、编译

### 2.1 C 服务端

```bash
make              # gcc -O2 -Wall -pthread
./server 8888 8889 8890        # 起三个监听端口
```

### 2.2 Go 客户端

```bash
go build -o client client.go
./client -h                     # 看参数
```

需要 Go 1.18+(`sync/atomic.Uint64`)。

---

## 三、运行示例

### 3.1 单机自测(本机到本机,验证工具链)

```bash
# 终端 A:服务端,4 个端口
./server 18888 18889 18890 18891

# 终端 B:客户端,打到 5 万连接,400 conn/s
./client -servers "127.0.0.1:18888,127.0.0.1:18889,127.0.0.1:18890,127.0.0.1:18891" \
          -target 50000 -rate 400 -stats 1s
```

### 3.2 跨机器测家用宽带(典型用法)

服务端放在受测线路的**内网服务器**(或树莓派)上:

```bash
# 内网服务端:多端口
./server 18888 18889 18890 18891 18892 18893 18894 18895
```

另一台机器(可同内网、可公网 VPS)当客户端:

```bash
# 跨网:目标打 10w,默认 200 conn/s(避开运营商 QoS 突发限速)
./client -servers "192.168.1.100:18888,192.168.1.100:18889,...,192.168.1.100:18895" \
          -target 100000 -rate 200

# 多 WAN / 策略路由:指定出口 IP
./client -bind 192.168.10.5 \
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
[STAT] try=14613 ok=14613 active=14612 | +try/s=95 +ok/s=95 | fail t/o=0 rst=0 addr-full=0 eof=0 other=0
```

**关注:**
- `+try/s` ≈ `+ok/s`:网络通畅
- `addr-full` 持续涨 → **客户端端口耗尽**,加端口或加网卡
- `t/o` 持续涨 → **光猫/NAT 老化或对端丢包**,调大客户端 `-keepalive`
- 服务端 `alive` 稳态不增 → 客户端打到目标值或被 QoS 限速

---

## 四、设计要点速览

### 服务端(`server.c`)
- 一个 `epoll_create1` 实例管理**所有**监听 fd,`accept4` 一次性 accept loop 到 `EAGAIN`
- `accept4(SOCK_NONBLOCK)` 后**不挂回 epoll** — 只 hold 连接、不收发,epoll 无意义
- `SO_RCVBUF/SO_SNDBUF=2048`,10w 连接 fd 内存 ≈ `sizeof(struct file)` 内核侧 + 用户态接近 0
- `SO_KEEPALIVE` + `TCP_KEEPIDLE=60 / KEEPINTVL=10 / KEEPCNT=3` → 客户端静默 60s 后开始探测,30s 内判定对端死,触发本端发 RST
  - 与运营商 NAT 老化(典型 120-300s)留出余量

### 客户端(`client.go`)
- `net.Dialer.Control` 走 `syscall.RawConn.Control(fd)` → 在内核 fd 上 `SetsockoptInt` 压缓冲区
- `KeepAlive: 30s`:在 NAT 老化之前从本端发心跳包,**对端(本服务端)60s 内必收探测包**
- 令牌桶:每秒按 `-rate` 注入令牌,worker 抢令牌去 `DialContext`
- 失败分类:`timeout` / `refused` / `cannot assign requested address`(端口耗尽)/ `EOF` / 其他
- 端口耗尽会**自动切下一个 target**,不卡死

---

## 五、本仓库冒烟记录

- 容器内回环:单端口 18888,2 秒内 accept **9315/s**,峰值活跃 **14613**。
- 验证 `epoll` + `accept4` 主循环 + keepalive + 统计线程均工作正常。
- 容器 ulimit 524288、内核无 conntrack 限制。