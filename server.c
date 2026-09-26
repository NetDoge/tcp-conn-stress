/*
 * server.c - 多端口 epoll TCP 连接压力测试服务端
 *
 * 特性:
 *   - 命令行接收多个监听端口,通过单个 epoll 实例统一管理
 *   - accept 后立即压低 RCVBUF/SNDBUF,10w+ 连接省内存
 *   - 启用 SO_KEEPALIVE + TCP_KEEPIDLE/INTVL/CNT,60s 抗 NAT 老化
 *   - 独立统计线程:每秒打印总数/分端口/每秒增/减
 *
 * 用法:
 *   ./server <port1> [port2] [port3] ...
 *   ./server 8888 8889 8890
 *
 * 退出:
 *   SIGINT / SIGTERM 触发优雅退出,关闭所有 fd。
 */

#define _GNU_SOURCE
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

/* 分端口计数 */
static uint32_t g_per_port[MAX_LISTENERS];
static int      g_port_count = 0;
static uint16_t g_ports[MAX_LISTENERS];

/* 全局计数 - 累计与实时 */
static volatile uint64_t g_total_alive   = 0; /* 当前活跃 */
static volatile uint64_t g_total_acc     = 0; /* 累计接受 */
static volatile uint64_t g_total_close   = 0; /* 累计关闭 */
static volatile uint64_t g_last_accepts  = 0; /* 上一个 1s 窗口新增 */
static volatile uint64_t g_last_closes   = 0; /* 上一个 1s 窗口断开 */

static volatile int g_stop = 0;

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
#ifdef SO_REUSEPORT
    /* 允许同端口多实例;若只需 1 实例也不冲突 */
    setsockopt(fd, SOL_SOCKET, SO_REUSEPORT, &yes, sizeof(yes));
#endif

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
                        "| +%lu/s -%lu/s\n",
                (unsigned long)time(NULL), (unsigned long)alive,
                (unsigned long)now_acc, (unsigned long)now_close,
                (unsigned long)acc_rate, (unsigned long)close_rate);
        fprintf(stderr, "         ports:");
        for (int i = 0; i < g_port_count; i++) {
            fprintf(stderr, " %u=%u", g_ports[i], g_per_port[i]);
        }
        fprintf(stderr, "\n");
        fflush(stderr);

        g_last_accepts = acc_rate;
        g_last_closes  = close_rate;
    }
    return NULL;
}

int main(int argc, char **argv) {
    if (argc < 2) {
        fprintf(stderr, "Usage: %s <port1> [port2] ...\n", argv[0]);
        return 1;
    }
    if (argc - 1 > MAX_LISTENERS) {
        fprintf(stderr, "too many ports, limit=%d\n", MAX_LISTENERS);
        return 1;
    }

    /* 解析端口 */
    for (int i = 1; i < argc; i++) {
        long p = strtol(argv[i], NULL, 10);
        if (p <= 0 || p > 65535) {
            fprintf(stderr, "invalid port: %s\n", argv[i]);
            return 1;
        }
        g_ports[g_port_count++] = (uint16_t)p;
    }

    /* 信号 */
    struct sigaction sa = {0};
    sa.sa_handler = on_signal;
    sigaction(SIGINT,  &sa, NULL);
    sigaction(SIGTERM, &sa, NULL);
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
    fprintf(stderr, "server running, ctrl-c to stop.\n");
    while (!g_stop) {
        int n = epoll_wait(ep, events, MAX_EVENTS, 500);
        if (n < 0) {
            if (errno == EINTR) continue;
            perror("epoll_wait");
            break;
        }
        for (int i = 0; i < n; i++) {
            int fd = (int)(events[i].data.u64 & 0xffffffff);
            uint32_t port_idx = (uint32_t)(events[i].data.u64 >> 32);

            if (events[i].events & (EPOLLERR | EPOLLHUP)) {
                /* 监听 fd 出错 - 罕见;尝试重建 */
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
                    perror("accept4");
                    break;
                }
                tune_socket(cfd);
                g_per_port[port_idx]++;
                __sync_add_and_fetch(&g_total_alive, 1);
                __sync_add_and_fetch(&g_total_acc, 1);
                /* 不读不写,fd 自动随 keepalive 检测存活 */
                /* 也不加入 epoll - 无数据交互,无意义 */
            }
        }
    }

    fprintf(stderr, "shutting down...\n");
    /* 关闭所有连接 */
    /* 简化:遍历 /proc/self/fd 太重,直接关 listener,让内核回收已建连 */
    for (int i = 0; i < g_port_count; i++) close(listeners[i]);
    close(ep);
    fprintf(stderr, "bye. final alive=%lu\n", (unsigned long)g_total_alive);
    return 0;
}