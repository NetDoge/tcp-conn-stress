/*
 * server.c - 多端口 epoll TCP 连接压力测试服务端
 *
 * 特性:
 *   - 命令行接收多个监听端口,通过单个 epoll 实例统一管理
 *   - accept 后立即压低 RCVBUF/SNDBUF,10w+ 连接省内存
 *   - 已建连 socket 也入 epoll(只盯 RDHUP/ERR/HUP),对端断开即回收
 *   - 启用 SO_KEEPALIVE + TCP_KEEPIDLE/INTVL/CNT,60s 抗 NAT 老化
 *   - 独立统计线程:每秒打印总数/分端口/每秒增/减
 *
 * 用法(已合并进 tcp-stress 单二进制,本文件由 cgo 前导 include):
 *   ./tcp-stress -s <port1> [port2] [port3] ...
 *   ./tcp-stress -s 8888 8889 8890
 *   ./tcp-stress -s -pass <密码> 8888 8890    (v1.0.6 鉴权)
 *
 * 鉴权(v1.0.6,-pass 启用;协议见 auth.go,与纯 Go 服务端一致):
 *   accept → 立即发 "AUTH?\n",连接进入"鉴权中",epoll 额外盯 EPOLLIN;
 *   收到 "AUTH <密码>\n" 匹配 → 回 "AUTH OK\n",转已建连(开始计数),
 *   只盯 RDHUP/ERR/HUP;不匹配/超时(10s)/断开 → 回 "AUTH ERR\n" 关闭,
 *   计 auth_fail,不进任何连接统计。密码错误连接不占 alive/fd 长期。
 *
 * 退出:
 *   SIGINT / SIGTERM 触发优雅退出,关闭所有 fd。
 */

/* cgo 构建时 -D_GNU_SOURCE=1 由命令行注入,这里 guard 住避免重定义告警 */
#ifndef _GNU_SOURCE
#define _GNU_SOURCE
#endif
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <unistd.h>
#include <errno.h>
#include <signal.h>
#include <time.h>
#include <pthread.h>
#include <stdint.h>
#include <fcntl.h>
#include <sys/epoll.h>
#include <sys/socket.h>
#include <sys/types.h>
#include <netinet/in.h>
#include <netinet/tcp.h>
#include <arpa/inet.h>
#include <dirent.h>
#include <sys/stat.h>

/* 默认缓冲区阈值 - 压到内核最小,只为维持连接 */
#define DEFAULT_RCVBUF  2048
#define DEFAULT_SNDBUF  2048
/* 默认 keepalive:60s 空闲后首次探测,每 10s 一次,共 3 次 */
#define DEFAULT_KEEPIDLE  60
#define DEFAULT_KEEPINTVL 10
#define DEFAULT_KEEPCNT   3

/* epoll 单次最多处理事件 */
#define MAX_EVENTS 256
/* 监听 fd 上限 */
#define MAX_LISTENERS 64
/* 连接 fd 的 epoll data 标记(bit63);监听 fd 的高 32 位是端口索引(<64),bit63 恒 0 */
#define CONN_TAG ((uint64_t)1 << 63)

/* 分端口计数 */
static uint32_t g_per_port[MAX_LISTENERS];
static int      g_port_count = 0;
static uint16_t g_ports[MAX_LISTENERS];

/* 全局计数 - 累计与实时 */
static volatile uint64_t g_total_alive   = 0; /* 当前活跃 */
static volatile uint64_t g_total_acc     = 0; /* 累计接受 */
static volatile uint64_t g_total_close   = 0; /* 累计关闭 */
static volatile int g_stop = 0;
/* EMFILE 告警限频(每 10s 最多一条) */
static time_t g_last_emfile_log = 0;

/* ---- v1.0.6 鉴权状态 ----
 * g_pass == NULL:鉴权关闭,行为与旧版逐字节一致。
 * 鉴权中的连接挤在紧凑数组里(上限 AUTH_MAX,超出直接关);
 * 单线程事件循环内访问,无锁。 */
#define AUTH_MAX     4096        /* 并发鉴权中连接上限(防恶意挂连接耗 fd) */
#define AUTH_TIMEOUT 10          /* 鉴权超时秒数(与 Go 实现一致) */
typedef struct { int fd; time_t deadline; char buf[160]; int len; } auth_ent;
static auth_ent g_auth_q[AUTH_MAX];
static int g_auth_n = 0;
static const char *g_pass = NULL;          /* NULL = 不鉴权 */
static volatile uint64_t g_auth_fail = 0;  /* 鉴权失败累计 */
static time_t g_last_authfail_log = 0;
static time_t g_last_evict_log = 0;

/* 密码经此 setter 注入(堆拷贝),不进 argv —— /proc/<pid>/cmdline 不落密码。
 * argv 的 -pass 解析保留(独立编译 server.c 的旧用法),Go 侧不再走 argv。 */
static char *g_pass_heap = NULL;
__attribute__((unused)) static void tcp_server_set_pass(const char *p) {
    if (!p) return;
    char *dup = strdup(p);
    if (!dup) {
        /* fail-closed:要求鉴权却因内存不足静默关掉更糟 */
        fprintf(stderr, "tcp_server_set_pass: out of memory\n");
        exit(1);
    }
    free(g_pass_heap);
    g_pass_heap = dup;
    g_pass = dup;
}

static const char AUTH_BANNER_S[] = "AUTH?\n";
static const char AUTH_OK_S[]     = "AUTH OK\n";
static const char AUTH_ERR_S[]    = "AUTH ERR\n";

/* ---- v1.0.8 本批待关队列(驱逐受害者延迟 close)----
 * epoll_wait 返回的 events[] 是批快照:受害者的 RDHUP 事件可能已在其中。
 * 立即 close 会让 accept4 复用该 fd 号,陈旧事件随后打到无辜新连接 ——
 * 轻则误杀,重则计数器下溢(实测 alive 永久变 2^64-1)。
 * 受害者先进本队列,批内凭 in_close_q() 跳过其陈旧事件;下一批处理前
 * (上一批事件已全部消费)由 closeq_drain() 统一 close,批内 fd 号不可能
 * 被复用,下溢路径被整体消除。 */
#define CLOSEQ_MAX 4096 /* 单批驱逐上限(=AUTH_MAX);超出退回立即 close */
static int g_close_q[CLOSEQ_MAX];
static int g_close_n = 0;
static int in_close_q(int fd) {
    for (int i = 0; i < g_close_n; i++)
        if (g_close_q[i] == fd) return 1;
    return 0;
}
static void closeq_drain(void) {
    for (int i = 0; i < g_close_n; i++) close(g_close_q[i]);
    g_close_n = 0;
}

/* 紧凑数组三件套:add / find / remove(swap 删,保持稠密) */
static void auth_remove_at(int i);
static void auth_fail_bump(void);
static int auth_add(int fd, time_t deadline) {
    if (g_auth_n >= AUTH_MAX) {
        /* 队列满:驱逐 deadline 最早的停滞连接,把槽位让给新连接。
         * 旧逻辑直接拒绝新连接 —— 攻击者用一批连上但不回密码的连接
         * 即可占满队列,把携带正确密码的合法客户端整个拒之门外(DoS)。
         * 驱逐后:合法客户端毫秒级完成握手,永远轮不到被驱逐;攻击者的
         * 停滞连接自相轮换,合法流量始终进得来。 */
        /* 扫描范围取队头前缀窗口而非全队列:受害者只需是"任意停滞
         * 连接",队头是最早入队的一批;全扫 O(AUTH_MAX) 会被驱逐风暴
         * 打成 CPU 放大器(实测 5857 conn/s 洪水烧掉 87% 单核)。
         * 合法客户端毫秒级完成握手,在持续满员的队列里活不到队头。 */
        int oldest = 0, scan = g_auth_n < 64 ? g_auth_n : 64;
        for (int i = 1; i < scan; i++)
            if (g_auth_q[i].deadline < g_auth_q[oldest].deadline) oldest = i;
        int victim = g_auth_q[oldest].fd;
        auth_remove_at(oldest);
        if (g_close_n < CLOSEQ_MAX) {
            g_close_q[g_close_n++] = victim; /* 延迟 close,见 CLOSEQ 注释 */
        } else {
            /* 单批驱逐数超上限(理论极端):退回立即 close,
             * 残余风险是低概率误杀一个新连接(客户端有重试兜底) */
            close(victim);
        }
        auth_fail_bump();
        time_t now = time(NULL);
        if (now - g_last_evict_log >= 10) {
            g_last_evict_log = now;
            fprintf(stderr, "auth: queue full (%d), evicted oldest pending\n",
                    AUTH_MAX);
        }
    }
    g_auth_q[g_auth_n].fd = fd;
    g_auth_q[g_auth_n].deadline = deadline;
    g_auth_q[g_auth_n].len = 0;
    g_auth_n++;
    return 0;
}
static int auth_find(int fd) {
    for (int i = 0; i < g_auth_n; i++)
        if (g_auth_q[i].fd == fd) return i;
    return -1;
}
static void auth_remove_at(int i) {
    g_auth_q[i] = g_auth_q[--g_auth_n];
}
static void auth_remove(int fd) { /* 仅 accept 路径 epoll_ctl 失败时用(冷路径) */
    int i = auth_find(fd);
    if (i >= 0) auth_remove_at(i);
}
static void auth_fail_bump(void) {
    __sync_add_and_fetch(&g_auth_fail, 1);
    time_t now = time(NULL);
    if (now - g_last_authfail_log >= 10) {
        g_last_authfail_log = now;
        fprintf(stderr, "auth: rejected (total_fail=%lu)\n",
                (unsigned long)g_auth_fail);
    }
}



static void on_signal(int s) {
    (void)s;
    g_stop = 1;
}

/* 优化 socket:小缓冲 + keepalive */
static int tune_socket(int fd) {
    int one = 1;
    int sz;

    /* nodelay - 我们不交换数据,关掉 Nagle 也无意义,但关闭延迟 */
    setsockopt(fd, IPPROTO_TCP, TCP_NODELAY, &one, sizeof(one));

    /* 极致省内存 */
    sz = DEFAULT_RCVBUF;
    setsockopt(fd, SOL_SOCKET, SO_RCVBUF, &sz, sizeof(sz));
    sz = DEFAULT_SNDBUF;
    setsockopt(fd, SOL_SOCKET, SO_SNDBUF, &sz, sizeof(sz));

    /* Keepalive - 抗 NAT 老化 */
    setsockopt(fd, SOL_SOCKET, SO_KEEPALIVE, &one, sizeof(one));
    sz = DEFAULT_KEEPIDLE;
    setsockopt(fd, IPPROTO_TCP, TCP_KEEPIDLE, &sz, sizeof(sz));
    sz = DEFAULT_KEEPINTVL;
    setsockopt(fd, IPPROTO_TCP, TCP_KEEPINTVL, &sz, sizeof(sz));
    sz = DEFAULT_KEEPCNT;
    setsockopt(fd, IPPROTO_TCP, TCP_KEEPCNT, &sz, sizeof(sz));
    return 0;
}

/* 端口串严格校验:纯数字、长度 1-5、无前导零、值 1-65535。
 * 与 Go 侧 isValidPort 同语义;旧版 strtol 会把 "8888x"/" 8888"/"0080"
 * 静默解析成 8888/8888/80(独立编译 legacy 用法下的输入面) */
static int valid_port_str(const char *s) {
    if (!s || !*s || strlen(s) > 5) return 0;
    if (s[0] == '0' && s[1] != '\0') return 0; /* 前导零拒绝 */
    long v = 0;
    for (const char *p = s; *p; p++) {
        if (*p < '0' || *p > '9') return 0;
        v = v * 10 + (*p - '0');
    }
    return v >= 1 && v <= 65535;
}

/* 创建并 bind 监听 socket */
static int make_listener(uint16_t port) {
    int fd, yes = 1;
    struct sockaddr_in addr;

    fd = socket(AF_INET, SOCK_STREAM | SOCK_NONBLOCK | SOCK_CLOEXEC, 0);
    if (fd < 0) {
        fprintf(stderr, "socket() failed for port %u: %s\n", port, strerror(errno));
        return -1;
    }
    setsockopt(fd, SOL_SOCKET, SO_REUSEADDR, &yes, sizeof(yes));

    memset(&addr, 0, sizeof(addr));
    addr.sin_family = AF_INET;
    addr.sin_addr.s_addr = htonl(INADDR_ANY);
    addr.sin_port = htons(port);

    if (bind(fd, (struct sockaddr *)&addr, sizeof(addr)) < 0) {
        fprintf(stderr, "bind() %u failed: %s\n", port, strerror(errno));
        close(fd);
        return -1;
    }
    if (listen(fd, 1024) < 0) {
        fprintf(stderr, "listen() %u failed: %s\n", port, strerror(errno));
        close(fd);
        return -1;
    }
    return fd;
}

/* 统计线程:每秒打印一次 */
static void *stats_thread(void *arg) {
    (void)arg;
    uint64_t prev_acc = 0, prev_close = 0;
    while (!g_stop) {
        sleep(1);
        uint64_t now_acc   = g_total_acc;
        uint64_t now_close = g_total_close;
        uint64_t alive     = g_total_alive;
        uint64_t acc_rate   = now_acc   - prev_acc;
        uint64_t close_rate = now_close - prev_close;
        prev_acc   = now_acc;
        prev_close = now_close;

        /* 用 stderr 避免与 epoll 线程 stdout 抢 */
        fprintf(stderr, "[STATS t=%lus] alive=%lu | total_acc=%lu total_close=%lu "
                        "| +%lu/s -%lu/s | auth_fail=%lu\n",
                (unsigned long)time(NULL), (unsigned long)alive,
                (unsigned long)now_acc, (unsigned long)now_close,
                (unsigned long)acc_rate, (unsigned long)close_rate,
                (unsigned long)g_auth_fail);
        fprintf(stderr, "         ports:");
        for (int i = 0; i < g_port_count; i++) {
            fprintf(stderr, " %u=%u", g_ports[i], g_per_port[i]);
        }
        fprintf(stderr, "\n");
        fflush(stderr);

    }
    return NULL;
}

/* 入口由 Go 侧 main.go 经 cgo 调用。
 * static: cgo 会把本前导复制进多个生成的 C 文件,static 保证各编译单元私有,
 *         不会在链接期撞重复符号;unused 压掉未调用 TU 的告警。 */
__attribute__((unused)) static int tcp_server_main(int argc, char **argv) {
    int argi = 1;
    /* v1.0.6:可选 "-pass <密码>" 前缀(Go 侧已校验格式,这里只取值) */
    if (argc >= 3 && strcmp(argv[1], "-pass") == 0) {
        g_pass = argv[2];
        argi = 3;
    }
    if (argc < argi + 1) {
        fprintf(stderr, "Usage: %s -s [-pass <密码>] <port1> [port2] ...\n", argv[0]);
        return 1;
    }
    if (argc - argi > MAX_LISTENERS) {
        fprintf(stderr, "too many ports, limit=%d\n", MAX_LISTENERS);
        return 1;
    }

    /* 解析端口:严格校验(拒绝尾部垃圾/前导空格/前导零) */
    for (int i = argi; i < argc; i++) {
        if (!valid_port_str(argv[i])) {
            fprintf(stderr, "invalid port: %s\n", argv[i]);
            return 1;
        }
        g_ports[g_port_count++] = (uint16_t)atoi(argv[i]);
    }

    /* 信号 */
    struct sigaction sa = {0};
    sa.sa_handler = on_signal;
    sigaction(SIGINT,  &sa, NULL);
    sigaction(SIGTERM, &sa, NULL);
    sigaction(SIGHUP,  &sa, NULL);   /* ssh 断开也走优雅退出,保住 final 统计 */
    sigaction(SIGQUIT, &sa, NULL);   /* Ctrl-\ 同样优雅退出,final 统计不丢 */
    signal(SIGPIPE, SIG_IGN);

    /* epoll */
    int ep = epoll_create1(EPOLL_CLOEXEC);
    if (ep < 0) {
        perror("epoll_create1");
        return 1;
    }

    /* 创建所有监听 socket 并加入 epoll */
    struct epoll_event ev;
    int listeners[MAX_LISTENERS];
    for (int i = 0; i < g_port_count; i++) {
        int lfd = make_listener(g_ports[i]);
        if (lfd < 0) return 1;
        listeners[i] = lfd;
        memset(&ev, 0, sizeof(ev));
        ev.events = EPOLLIN;
        ev.data.u64 = ((uint64_t)i << 32) | (uint32_t)lfd;
        if (epoll_ctl(ep, EPOLL_CTL_ADD, lfd, &ev) < 0) {
            perror("epoll_ctl ADD listener");
            return 1;
        }
        fprintf(stderr, "listening on 0.0.0.0:%u (fd=%d)\n", g_ports[i], lfd);
    }

    /* 启动统计线程 */
    pthread_t st_tid;
    pthread_create(&st_tid, NULL, stats_thread, NULL);
    pthread_detach(st_tid);

    /* 主事件循环 */
    struct epoll_event events[MAX_EVENTS];
    /* EMFILE 时按端口记录 listener 摘除截止时间 */
    time_t pause_until[MAX_LISTENERS];
    memset(pause_until, 0, sizeof(pause_until));
    if (g_pass) {
        fprintf(stderr, "auth: enabled (password set)\n");
    }
    fprintf(stderr, "server running, ctrl-c to stop.\n");
    while (!g_stop) {
        /* 摘除的 listener 到时挂回 */
        time_t now = time(NULL);

        /* 批首清空上一批待关队列:此刻上一批事件已全部消费,这些
         * fd 关掉后即使被 accept4 复用,新连接事件只会出现在下一批 */
        closeq_drain();

        /* 鉴权超时扫描:挂着不发的连接 10s 后关闭(防 fd 耗尽) */
        for (int k = 0; k < g_auth_n; ) {
            if (now >= g_auth_q[k].deadline) {
                close(g_auth_q[k].fd); /* close 自动从 epoll 摘除 */
                auth_fail_bump();
                auth_remove_at(k);
            } else {
                k++;
            }
        }
        for (int j = 0; j < g_port_count; j++) {
            if (pause_until[j] && now >= pause_until[j]) {
                memset(&ev, 0, sizeof(ev));
                ev.events = EPOLLIN;
                ev.data.u64 = ((uint64_t)j << 32) | (uint32_t)listeners[j];
                if (epoll_ctl(ep, EPOLL_CTL_ADD, listeners[j], &ev) < 0)
                    perror("epoll_ctl re-ADD listener");
                pause_until[j] = 0;
            }
        }

        int n = epoll_wait(ep, events, MAX_EVENTS, 500);
        if (n < 0) {
            if (errno == EINTR) continue;
            perror("epoll_wait");
            break;
        }
        for (int i = 0; i < n; i++) {
            uint64_t dat = events[i].data.u64;

            if (dat & CONN_TAG) {
                /* 已建连 socket:对端 FIN/RST 或 keepalive 判死 → 回收 */
                int cfd = (int)(uint32_t)(dat & 0xffffffff);
                uint32_t pidx = (uint32_t)((dat >> 32) & 0x7fffffff);

                /* 本批待关 fd(驱逐受害者)的陈旧事件:直接跳过。
                 * 它已被逐出鉴权队列且尚未 close,既不能按"已建连
                 * 断开"处理(计数器会下溢),也不能误当鉴权连接去读。 */
                if (in_close_q(cfd)) continue;

                int ai = g_pass ? auth_find(cfd) : -1; /* 鉴权中?(一次查清) */
                int pending = ai >= 0;

                /* 鉴权中 + 有数据:逐次累积 "AUTH <密码>\n" 再比对。
                 * 单次 read 不保证整行到齐(TCP 分段),必须攒到 \n 才判。 */
                if (pending && (events[i].events & EPOLLIN)) {
                    auth_ent *ae = &g_auth_q[ai];
                    char expect[152];
                    int el = snprintf(expect, sizeof(expect), "AUTH %s\n", g_pass);
                    ssize_t r = read(cfd, ae->buf + ae->len,
                                     sizeof(ae->buf) - (size_t)ae->len);
                    if (r > 0) {
                        ae->len += (int)r;
                        char *nl = memchr(ae->buf, '\n', (size_t)ae->len);
                        if (nl) {
                            /* 第一行到齐:只比第一行,行后多余字节忽略
                             * (与已建连忽略数据语义一致;旧版整缓冲比对
                             * 会把 "AUTH pw\nGARBAGE" 判 ERR,与 Go 实现分叉) */
                            size_t linelen = (size_t)(nl - ae->buf) + 1;
                            if (linelen == (size_t)el &&
                                    memcmp(ae->buf, expect, (size_t)el) == 0 &&
                                    write(cfd, AUTH_OK_S, sizeof(AUTH_OK_S) - 1)
                                        == (ssize_t)(sizeof(AUTH_OK_S) - 1)) {
                                /* 转已建连:摘 EPOLLIN,开始计数(ai 已知,免二次扫描) */
                                auth_remove_at(ai);
                                pending = 0;
                                memset(&ev, 0, sizeof(ev));
                                ev.events = EPOLLRDHUP | EPOLLERR | EPOLLHUP;
                                ev.data.u64 = dat;
                                epoll_ctl(ep, EPOLL_CTL_MOD, cfd, &ev);
                                __sync_add_and_fetch(&g_per_port[pidx], 1);
                                __sync_add_and_fetch(&g_total_alive, 1);
                                __sync_add_and_fetch(&g_total_acc, 1);
                                if (!(events[i].events &
                                        (EPOLLRDHUP | EPOLLERR | EPOLLHUP))) {
                                    continue; /* 健康,等下一个事件 */
                                }
                                /* 事件同时带断开标志:落入下方关闭路径(刚入账,计数对) */
                            } else {
                                (void)!write(cfd, AUTH_ERR_S, sizeof(AUTH_ERR_S) - 1);
                                close(cfd);
                                auth_remove_at(ai);
                                auth_fail_bump();
                                continue;
                            }
                        } else if (ae->len >= (int)sizeof(ae->buf)) {
                            /* 攒满仍无换行:超长,拒绝 */
                            (void)!write(cfd, AUTH_ERR_S, sizeof(AUTH_ERR_S) - 1);
                            close(cfd);
                            auth_remove_at(ai);
                            auth_fail_bump();
                            continue;
                        }
                        /* 行未到齐:保持鉴权中,等下一个 EPOLLIN */
                    } else if (r == 0) {
                        /* EOF:鉴权中途断开 */
                        close(cfd);
                        auth_remove_at(ai);
                        auth_fail_bump();
                        continue;
                    } else if (errno != EAGAIN && errno != EWOULDBLOCK) {
                        close(cfd);
                        auth_remove_at(ai);
                        auth_fail_bump();
                        continue;
                    }
                    /* EAGAIN 或行未到齐:落入下方统一判定 */
                }

                /* 事件无断开/错误标志的两种"未终结"状态,都继续等:
                 * - 鉴权中:密码行未到齐,等下一个 EPOLLIN
                 *   (修复:旧逻辑此处落 close,分段到达的密码行在首字节
                 *    后就被误杀 —— B5 逐字节握手实测复现)
                 * - 已建连:杂散可读事件(兴趣集已无 EPOLLIN,防御性忽略) */
                if (!(events[i].events & (EPOLLRDHUP | EPOLLERR | EPOLLHUP))) {
                    continue;
                }

                close(cfd); /* close 自动从 epoll 摘除 */
                if (pending) {
                    /* 鉴权中途对端断开:不算连接关闭,算鉴权失败 */
                    auth_remove_at(ai);
                    auth_fail_bump();
                    continue;
                }
                __sync_sub_and_fetch(&g_total_alive, 1);
                __sync_add_and_fetch(&g_total_close, 1);
                __sync_sub_and_fetch(&g_per_port[pidx], 1);
                continue;
            }

            int fd = (int)(dat & 0xffffffff);
            uint32_t port_idx = (uint32_t)(dat >> 32);

            if (events[i].events & (EPOLLERR | EPOLLHUP)) {
                /* 监听 fd 出错 - 罕见 */
                fprintf(stderr, "listener fd=%d err/hup\n", fd);
                continue;
            }
            /* accept loop */
            while (1) {
                struct sockaddr_in cli;
                socklen_t cl = sizeof(cli);
                int cfd = accept4(fd, (struct sockaddr *)&cli, &cl,
                                  SOCK_NONBLOCK | SOCK_CLOEXEC);
                if (cfd < 0) {
                    if (errno == EAGAIN || errno == EWOULDBLOCK) break;
                    if (errno == EINTR) continue;
                    if (errno == ECONNABORTED || errno == ECONNRESET) continue;
                    if (errno == EMFILE || errno == ENFILE) {
                        /* fd 耗尽:必须摘下 listener,
                         * 否则电平触发空转烧 CPU + 刷爆日志 */
                        epoll_ctl(ep, EPOLL_CTL_DEL, fd, NULL);
                        pause_until[port_idx] = time(NULL) + 1;
                        if (time(NULL) - g_last_emfile_log >= 10) {
                            g_last_emfile_log = time(NULL);
                            fprintf(stderr, "accept4: %s - fd exhausted, "
                                    "listener %u paused 1s\n",
                                    strerror(errno), g_ports[port_idx]);
                        }
                        break;
                    }
                    perror("accept4");
                    break;
                }
                tune_socket(cfd);
                /* v1.0.6 鉴权:accept 即发 banner,连接进"鉴权中"状态;
                 * 通过前不计 alive/acc,失败关连接只计 auth_fail。 */
                if (g_pass) {
                    if (write(cfd, AUTH_BANNER_S, sizeof(AUTH_BANNER_S) - 1)
                            != (ssize_t)(sizeof(AUTH_BANNER_S) - 1) ||
                        auth_add(cfd, time(NULL) + AUTH_TIMEOUT) != 0) {
                        close(cfd);
                        auth_fail_bump();
                        continue;
                    }
                    memset(&ev, 0, sizeof(ev));
                    ev.events = EPOLLIN | EPOLLRDHUP | EPOLLERR | EPOLLHUP;
                    ev.data.u64 = CONN_TAG | ((uint64_t)port_idx << 32) | (uint32_t)cfd;
                    if (epoll_ctl(ep, EPOLL_CTL_ADD, cfd, &ev) < 0) {
                        perror("epoll_ctl ADD conn");
                        auth_remove(cfd);
                        close(cfd);
                        continue;
                    }
                    continue; /* 等鉴权行,不计任何连接数 */
                }
                /* 连接 fd 挂 epoll:只盯断开/错误,不监听 EPOLLIN(不收数据) */
                memset(&ev, 0, sizeof(ev));
                ev.events = EPOLLRDHUP | EPOLLERR | EPOLLHUP;
                ev.data.u64 = CONN_TAG | ((uint64_t)port_idx << 32) | (uint32_t)cfd;
                if (epoll_ctl(ep, EPOLL_CTL_ADD, cfd, &ev) < 0) {
                    perror("epoll_ctl ADD conn");
                    close(cfd);
                    continue;
                }
                __sync_add_and_fetch(&g_per_port[port_idx], 1);
                __sync_add_and_fetch(&g_total_alive, 1);
                __sync_add_and_fetch(&g_total_acc, 1);
            }
        }
    }

    fprintf(stderr, "shutting down...\n");
    /* 优雅关闭:
     * 1) 先关 listener,epoll 再不会分发新的 accept 事件
     * 2) 遍历 /proc/self/fd,把所有 socket 类型的 fd 都 close 掉(已 accept 的连接)
     *    客户端会收到 FIN,而不是被内核强 RST
     */
    for (int i = 0; i < g_port_count; i++) close(listeners[i]);
    DIR *d = opendir("/proc/self/fd");
    if (d) {
        struct dirent *de;
        int dfd = dirfd(d); /* 退出遍历不可 close 正在 readdir 的目录 fd(理论 UB); */
        while ((de = readdir(d)) != NULL) {
            if (de->d_name[0] == '.') continue;
            int fd = atoi(de->d_name);
            if (fd <= 2) continue;          /* stdin/stdout/stderr */
            if (fd == ep || fd == dfd) continue;
            struct stat st;
            if (fstat(fd, &st) == 0 && S_ISSOCK(st.st_mode)) {
                close(fd);
                /* 不用减 g_total_alive:这是退出路径,final alive 取关闭瞬间值即可 */
            }
        }
        closedir(d);
    }
    close(ep);
    fprintf(stderr, "bye. final alive=%lu\n", (unsigned long)g_total_alive);
    return 0;
}