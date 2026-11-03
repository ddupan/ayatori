#!/bin/sh
# 把共享 fixture 复位到刚启动的状态：初始配置、无 transaction、新的 dataplaneapi 进程
# （其 reload 记录只在内存中）。等旧进程退出后返回，start.sh 的循环随即拉起新进程。
set -e
cp /fixture/haproxy.cfg /usr/local/etc/haproxy/haproxy.cfg
rm -rf /tmp/haproxy-transactions
kill -USR2 "$(cat /var/run/haproxy.pid)"
old=$(pgrep -x dataplaneapi)
kill "$old"
while kill -0 "$old" 2>/dev/null; do sleep 0.1; done
