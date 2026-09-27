// main_test.go - 参数解析单元测试(v1.2.1 起入库;此前开发期单测为临时脚本未入库,
// 2026-09-27 审计 P1-3:64 端口上限是防端口扫描声明的一部分,必须有回归网)
package main

import (
	"strings"
	"testing"
)

func TestParsePortSpec(t *testing.T) {
	cases := []struct {
		name    string
		in      string
		want    []string // nil = 期望错误
		wantErr string   // 错误子串(空 = 不校验子串)
	}{
		{"单端口", "8888", []string{"8888"}, ""},
		{"多端口空格", "8888 8889", []string{"8888", "8889"}, ""},
		{"范围", "8888-8891", []string{"8888", "8889", "8890", "8891"}, ""},
		{"单位点范围", "8888-8888", []string{"8888"}, ""},
		{"混合", "8888 8890-8891", []string{"8888", "8890", "8891"}, ""},
		{"空输入", "   ", nil, ""}, // 空 list 无错,由调用方决定
		{"非法字符", "888x", nil, "invalid port"},
		{"超界", "65536", nil, "invalid port"},
		{"零端口", "0", nil, "invalid port"},
		{"前导零单端口", "0080", nil, "invalid port"},
		{"前导零范围(审计P1-1)", "08888-08890", nil, "invalid port"},
		{"加号范围(审计P1-1)", "+8888-8890", nil, "invalid port"},
		{"尾部垃圾范围", "8888-8890x", nil, "invalid port"},
		{"倒序范围", "8890-8888", nil, "起始大于结束"},
		{"空范围端", "8888-", nil, "范围格式错误"},
		{"超64展开", "1-65", nil, "超出上限"},
		{"恰好64", "1-64", []string{}, ""}, // want 由下方动态校验长度
		{"重复端口(审计P2-2)", "8888 8888", nil, "重复端口"},
		{"重叠范围(审计P2-2)", "8888-8890 8890", nil, "重复端口"},
		{"范围展开重复段", "8888-8889 8888-8889", nil, "重复端口"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := parsePortSpec(c.in)
			if c.want == nil {
				if c.name == "空输入" {
					if err != nil || len(got) != 0 {
						t.Fatalf("空输入应返回空 list 无错,得 %v %v", got, err)
					}
					return
				}
				if err == nil {
					t.Fatalf("期望错误,得 %v", got)
				}
				if c.wantErr != "" && !strings.Contains(err.Error(), c.wantErr) {
					t.Fatalf("错误 %q 不含 %q", err.Error(), c.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("意外错误: %v", err)
			}
			if c.name == "恰好64" {
				if len(got) != 64 || got[0] != "1" || got[63] != "64" {
					t.Fatalf("1-64 应展开 64 个,得 %d 个(%v..%v)", len(got), got[0], got[len(got)-1])
				}
				return
			}
			if len(got) != len(c.want) {
				t.Fatalf("得 %v,期望 %v", got, c.want)
			}
			for i := range got {
				if got[i] != c.want[i] {
					t.Fatalf("第 %d 项 %s != %s", i, got[i], c.want[i])
				}
			}
		})
	}
}

func TestParseTargets(t *testing.T) {
	cases := []struct {
		name    string
		in      string
		n       int    // 期望 target 数(0 = 期望错误)
		first   string // 第一个 addr
		wantErr string // 错误子串
	}{
		{"单 target", "127.0.0.1:8888", 1, "127.0.0.1:8888", ""},
		{"逗号列表", "127.0.0.1:8888,127.0.0.1:8889", 2, "127.0.0.1:8888", ""},
		{"端口范围", "127.0.0.1:18888-18891", 4, "127.0.0.1:18888", ""},
		{"IPv6 范围", "[::1]:18800-18802", 3, "[::1]:18800", ""},
		{"混合范围+单端口", "127.0.0.1:8888-8889,127.0.0.1:9000", 3, "127.0.0.1:8888", ""},
		{"范围跨逗号非法", "127.0.0.1:8888,127.0.0.1:8889-8888", 0, "", "bad port"},
		{"范围前导零", "127.0.0.1:08888-08890", 0, "", "bad port"},
		{"范围加号", "127.0.0.1:+8888-8890", 0, "", "bad port"},
		{"范围尾部垃圾", "127.0.0.1:8888-8890x", 0, "", "bad port"},
		{"范围倒序", "127.0.0.1:8890-8888", 0, "", "bad port"},
		{"空 host", ":8888", 0, "", "空 host"},
		{"IPv6 空 host", "[::]:8888", 0, "", "空 host"},
		{"超 64 target", strings.Repeat("10.0.0.1:8888,", 64) + "10.0.0.1:8889", 0, "", "too many targets"},
		{"恰 64 target(范围展开)", "10.0.0.1:9000-9063", 64, "10.0.0.1:9000", ""},
		{"范围展开超 64", "10.0.0.1:9000-9064", 0, "", "too many targets"},
		{"空串", "", 0, "", "no targets"},
		{"尾部逗号宽容", "127.0.0.1:8888,", 1, "127.0.0.1:8888", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := parseTargets(c.in)
			if c.n == 0 {
				if err == nil {
					t.Fatalf("期望错误,得 %d target", len(got))
				}
				if c.wantErr != "" && !strings.Contains(err.Error(), c.wantErr) {
					t.Fatalf("错误 %q 不含 %q", err.Error(), c.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("意外错误: %v", err)
			}
			if len(got) != c.n {
				t.Fatalf("得 %d target,期望 %d", len(got), c.n)
			}
			if got[0].addr != c.first {
				t.Fatalf("首 addr %s != %s", got[0].addr, c.first)
			}
		})
	}
}
