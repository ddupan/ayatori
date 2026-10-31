# 集成测试用的 libvirtd：只用 test:///default 驱动，不需要 KVM 或特权。
# TCP 监听不做认证，只映射到 127.0.0.1。
FROM ubuntu@sha256:534baea6a22c03a63003dbc8dbe78fe34bc0d7e595d9a9dc9834884ff530eb55
RUN apt-get update \
 && DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends libvirt-daemon libvirt-clients \
 && rm -rf /var/lib/apt/lists/* \
 && (groupadd -r kvm || true) && (useradd -r -g kvm libvirt-qemu || true) \
 && printf 'listen_tls = 0\nlisten_tcp = 1\nauth_tcp = "none"\n' >> /etc/libvirt/libvirtd.conf \
 && mkdir -p /run/libvirt
EXPOSE 16509
CMD ["libvirtd", "--listen"]
