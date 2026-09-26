# Makefile - 家用宽带极限 TCP 连接测试 - 服务端
#
# 用法:
#   make            # 编译
#   make clean      # 清理

CC      ?= gcc
CFLAGS  ?= -O2 -g -Wall -Wextra -pthread
# 默认静态链接,产物可直接拷到别的机器跑(不挑 glibc 版本)。
# 本地调试想去掉静态链接:make LDFLAGS=-pthread
LDFLAGS ?= -pthread -static

TARGET  = server

.PHONY: all clean run

all: $(TARGET)

$(TARGET): server.c
	$(CC) $(CFLAGS) -o $@ $< $(LDFLAGS)

run: $(TARGET)
	./$(TARGET) 8888 8889 8890

clean:
	rm -f $(TARGET)