// auth.go - 服务端鉴权协议与握手实现(v1.0.6,-pass 启用)
//
// 服务端设了密码后,连接必须先通过单行文本握手才计入统计:
//
//	server → client:  "AUTH?\n"       连接建立即发(仅服务端设了密码时)
//	client → server:  "AUTH <密码>\n"
//	server → client:  "AUTH OK\n"     通过,转正常计数
//	                   "AUTH ERR\n"    拒绝,断开,不计任何数
//
// 双端都未配 -pass 时协议完全不出现,行为与旧版逐字节一致。
// 明文单行协议,防的是公网误用/白嫖占 fd,不是密码学对抗 —— 公网部署
// 仍建议安全组限源(见 README 安全注意)。

package main

import (
	"errors"
	"fmt"
	"log"
	"net"
	"time"
)

const (
	authPrefix   = "AUTH "      // 客户端 → 服务端:"AUTH <密码>\n"
	authBanner   = "AUTH?\n"    // 服务端 → 客户端:要求鉴权
	authOK       = "AUTH OK\n"  // 通过
	authERR      = "AUTH ERR\n" // 拒绝
	authLineMax  = 160          // 握手单行上限(含 \n),防恶意超长
	authClientTO = 5 * time.Second
	authServerTO = 10 * time.Second
)

var errAuthFail = errors.New("auth failed")

// validatePass 密码约束:非空时 1-128 字节,不含空白/控制字符
// (密码走单行文本协议,空白会破坏行格式)
func validatePass(p string) error {
	if p == "" {
		return nil
	}
	if len(p) > 128 {
		return fmt.Errorf("密码最长 128 字节")
	}
	for _, r := range p {
		if r <= ' ' || r == 0x7f {
			return fmt.Errorf("密码不能包含空白或控制字符")
		}
	}
	return nil
}

// readAuthLine 读一行(到 \n),握手专用:1 字节循环,量小无碍
func readAuthLine(c net.Conn) (string, error) {
	b := make([]byte, 0, authLineMax)
	one := make([]byte, 1)
	for len(b) < authLineMax {
		n, err := c.Read(one)
		if n > 0 {
			b = append(b, one[0])
			if one[0] == '\n' {
				return string(b), nil
			}
		}
		if err != nil {
			return "", err
		}
	}
	return "", fmt.Errorf("auth line too long")
}

// ---------------- 客户端侧 ----------------

// clientAuth 建连后的握手;仅客户端配置了 -pass 时调用。
// t.noAuth:该 target 已确认服务器不要求鉴权(无 -pass 服务端/旧版二进制),
// 后续连接跳过探测,不再空等 5s banner 超时。
// 状态记在 target 上而非全局(旧版全局标志在混合列表下互相污染:
// 无密码服务器把状态置位后,带密码服务器的握手也被跳过,正确密码被误杀)。
func clientAuth(conn net.Conn, pass string, t *target) error {
	if t.noAuth.Load() {
		return nil
	}
	_ = conn.SetReadDeadline(time.Now().Add(authClientTO))
	line, err := readAuthLine(conn)
	if err != nil || line != authBanner {
		// 读超时/EOF/非 banner 内容:该服务器不要求鉴权(或为旧版二进制)
		if t.noAuth.CompareAndSwap(false, true) {
			log.Printf("server %s 未要求鉴权,-pass 已忽略", t.addr)
		}
		_ = conn.SetReadDeadline(time.Time{})
		return nil
	}
	if pass == "" {
		_ = conn.SetReadDeadline(time.Time{})
		return errAuthFail
	}
	_ = conn.SetWriteDeadline(time.Now().Add(authClientTO))
	_, werr := conn.Write([]byte(authPrefix + pass + "\n"))
	_ = conn.SetWriteDeadline(time.Time{})
	if werr != nil {
		return errAuthFail
	}
	_ = conn.SetReadDeadline(time.Now().Add(authClientTO))
	resp, rerr := readAuthLine(conn)
	_ = conn.SetReadDeadline(time.Time{}) // 必须清掉,否则 hold 的读会超时
	if rerr != nil || resp != authOK {
		return errAuthFail
	}
	return nil
}

// ---------------- 服务端侧(Go 实现;C 实现见 csrc/server.c) ----------------

// serverAuth 服务端握手;通过返回 true(连接转正常)。
// 注意成功后 SetDeadline 已由 defer 清零。
func serverAuth(c net.Conn, pass string) bool {
	_ = c.SetDeadline(time.Now().Add(authServerTO))
	defer func() { _ = c.SetDeadline(time.Time{}) }()
	if _, err := c.Write([]byte(authBanner)); err != nil {
		return false
	}
	line, err := readAuthLine(c)
	if err != nil {
		return false
	}
	if line == authPrefix+pass+"\n" {
		_, werr := c.Write([]byte(authOK))
		return werr == nil
	}
	_, _ = c.Write([]byte(authERR))
	return false
}
