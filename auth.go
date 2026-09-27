// auth.go - 服务端身份握手与鉴权协议(v1.1.0;-pass 鉴权自 v1.0.6)
//
// v1.1.0 起服务端在连接建立后必须先自报身份,客户端只与能完成握手的
// tcp-stress 服务端维持连接 —— 指向任意第三方服务(nginx/SSH/IoT 等)
// 的连接会在握手阶段被拒并中止。本工具因此无法被直接当作对第三方的
// 连接耗尽攻击工具使用(见 README「使用政策」)。
//
// 协议(服务端 → 客户端,连接建立即发一行):
//   "AUTH?\n"   服务端设了 -pass,要求密码
//   "STRESS\n"  服务端开放模式(v1.1.0 新增)
// 客户端按身份行走对应分支:
//   AUTH?  → 客户端发 "AUTH <密码>\n",服务端回 "AUTH OK\n" / "AUTH ERR\n"
//   STRESS → 客户端配了 -pass 属该 target 配置错配:提示一次后忽略,
//            连接照常保持(混合"开放+鉴权"列表是合法用法,不中止)
// 任何其他内容/超时/EOF = 非 tcp-stress 服务端(或 v1.0.x 及更早的开放
// 模式服务端),按 target 连续失败计数,达到上限即整个客户端中止
// (防把滥用失败当正常丢包无限重试;按 target 计数则防混合列表里
// 一台真服务端的成功不断清零计数、掩护假 target 逃过中止)。
//
// 明文单行协议,防的是误用与第三方滥用,不是密码学对抗 —— 公网部署
// 仍建议安全组限源(见 README 安全注意)。

package main

import (
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
	srvBanner    = "STRESS\n"   // 服务端 → 客户端:开放模式身份行(v1.1.0)
	authLineMax  = 160          // 握手单行上限(含 \n),防恶意超长
	authClientTO = 5 * time.Second
	authServerTO = 10 * time.Second
)

// hsConsecFail:同一 target 连续握手失败达到该次数后,客户端整体中止
const hsConsecFail = 10

// hsAbort 立即中止类错误(两端配置错配,重试无意义)
type hsAbort struct{ msg string }

func (e *hsAbort) Error() string { return e.msg }

// hsReject 可重试失败(对端非 tcp-stress / 密码不对);由 dialOnce 按
// target 计连续次数,达到 hsConsecFail 用 fatal 文案中止。
// failAuth:归入 failAuth 统计(鉴权失败);否则归 failOther(身份不符)
type hsReject struct {
	fatal    string
	failAuth bool
}

func (e *hsReject) Error() string { return e.fatal }

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

// clientHandshake 建连后的身份握手;每个连接都走(v1.1.0 起不再有
// "本端未配 -pass 就跳过握手"的路径 —— 那条路正是把客户端指向任意
// 第三方服务时零拦截的根因)。成功返回 nil,连接转 hold。
func clientHandshake(conn net.Conn, pass string, t *target) error {
	_ = conn.SetReadDeadline(time.Now().Add(authClientTO))
	line, err := readAuthLine(conn)
	_ = conn.SetReadDeadline(time.Time{})
	if err != nil {
		// 超时/EOF/超长行:对端不是 tcp-stress 服务端
		// (或 v1.0.x 及更早的开放模式服务端,须升级)
		return &hsReject{fatal: notStressFatalMsg(t.addr)}
	}
	switch line {
	case authBanner:
		if pass == "" {
			return &hsAbort{msg: fmt.Sprintf(
				"服务器 %s 要求鉴权但握手未完成(本端未配 -pass / 两端配置不一致 / 网络延迟过高),已中止", t.addr)}
		}
		_ = conn.SetWriteDeadline(time.Now().Add(authClientTO))
		_, werr := conn.Write([]byte(authPrefix + pass + "\n"))
		_ = conn.SetWriteDeadline(time.Time{})
		if werr != nil {
			return &hsReject{fatal: authFatalMsg(t.addr), failAuth: true}
		}
		_ = conn.SetReadDeadline(time.Now().Add(authClientTO))
		resp, rerr := readAuthLine(conn)
		_ = conn.SetReadDeadline(time.Time{}) // 必须清掉,否则 hold 的读会超时
		if rerr != nil || resp != authOK {
			return &hsReject{fatal: authFatalMsg(t.addr), failAuth: true}
		}
		return nil
	case srvBanner:
		if pass != "" && t.noAuthLogged.CompareAndSwap(false, true) {
			log.Printf("server %s 未要求鉴权,-pass 已忽略", t.addr)
		}
		return nil
	default:
		return &hsReject{fatal: notStressFatalMsg(t.addr)}
	}
}

// authFatalMsg 鉴权类连续失败的中止文案
func authFatalMsg(addr string) string {
	return fmt.Sprintf("服务器 %s 鉴权连续失败 %d 次(密码不正确或两端配置不一致),已中止;检查两端 -pass",
		addr, hsConsecFail)
}

// notStressFatalMsg 非 tcp-stress 对端连续失败的中止文案(防滥用核心)
func notStressFatalMsg(addr string) string {
	return fmt.Sprintf("服务器 %s 连续 %d 次未通过 tcp-stress 身份握手,已中止;"+
		"本工具仅限测试自有/授权的 tcp-stress 服务端"+
		"(对端若为 v1.0.x 及更早版本的开放模式服务端,请先升级两端)", addr, hsConsecFail)
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
