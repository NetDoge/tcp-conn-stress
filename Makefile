# Makefile - tcp-conn-stress 单二进制构建
#
#   make        静态构建(发布形态:cgo 内嵌 C 服务端,不挑 glibc)
#   make dyn    动态构建(本机调试,编译快)
#   make clean  清理
#
# 非 Linux 平台没有 epoll,服务端不参与编译(server_stub.go 接管),
# 那边交叉编译用:
#   CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 go build -trimpath -ldflags '-s -w' -o tcp-stress .

BINARY = tcp-stress

.PHONY: all dyn clean

all:
	CGO_ENABLED=1 go build -trimpath -tags 'osusergo netgo' \
	  -ldflags '-s -w -extldflags -static' -o $(BINARY) .

dyn:
	CGO_ENABLED=1 go build -o $(BINARY) .

clean:
	rm -f $(BINARY)
