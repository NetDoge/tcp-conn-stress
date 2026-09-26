//go:build !windows

package main

import "syscall"

// raiseNofile 把 fd 软上限抬到硬上限并返回 (soft, hard)。
//
// 动机:发行版默认 soft 通常 1024,不抬的话万级连接目标直接 EMFILE;
// 而抬 soft 到 hard 是普通权限操作,不需要 root。这一步把"ulimit -n"
// 从必做的部署步骤变成程序自动完成,用户只剩"hard 本身太低"一种情况
// 才需要找管理员(systemd LimitNOFILE / limits.conf)。
//
// 失败静默(darwin 上 soft 抬到 unlimited hard 可能 EINVAL),保持原值。
func raiseNofile() (soft, hard uint64) {
	var rl syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_NOFILE, &rl); err != nil {
		return 0, 0
	}
	if rl.Cur < rl.Max {
		rl.Cur = rl.Max
		_ = syscall.Setrlimit(syscall.RLIMIT_NOFILE, &rl)
		_ = syscall.Getrlimit(syscall.RLIMIT_NOFILE, &rl) // 读回实际生效值
	}
	return rl.Cur, rl.Max
}
