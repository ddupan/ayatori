#!/bin/sh
# 生产由 systemd 分别运行 haproxy 与 dataplaneapi；fixture 在同一容器内模拟。
set -e
cp /fixture/haproxy.cfg /usr/local/etc/haproxy/haproxy.cfg
cp /fixture/dataplaneapi.yml /usr/local/etc/haproxy/dataplaneapi.yml
haproxy -W -D -S /var/run/haproxy-master.sock -p /var/run/haproxy.pid -f /usr/local/etc/haproxy/haproxy.cfg
# 循环运行：测试可重启 dataplaneapi，模拟其重启后丢失已排期的 reload。
while :; do dataplaneapi -f /usr/local/etc/haproxy/dataplaneapi.yml; done
