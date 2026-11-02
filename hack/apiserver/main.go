// Command apiserver 在 go test 之外启动一次 envtest 的 etcd 与 kube-apiserver，供集成测试共用。
// 启动在负载高的 runner 上可能要一分钟以上，放在测试外才能与编译并行，并给足超时；
// 写出 kubeconfig 即表示就绪，收到 SIGTERM 或 SIGINT 时停止。
package main

import (
	"context"
	"flag"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

func main() {
	kubeconfig := flag.String("write-kubeconfig", "", "就绪后写出 admin kubeconfig 的路径")
	crds := flag.String("crds", "config/crd/bases", "启动时安装的 CRD 目录")
	flag.Parse()
	if *kubeconfig == "" {
		log.Fatal("需要 -write-kubeconfig")
	}
	environment := &envtest.Environment{
		CRDDirectoryPaths: []string{*crds}, ErrorIfCRDPathMissing: true, ControlPlaneStartTimeout: 5 * time.Minute,
	}
	if _, err := environment.Start(); err != nil {
		log.Fatal(err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	defer stop()
	// 先写临时文件再改名：等待方看到文件时内容已完整。
	if err := os.WriteFile(*kubeconfig+".tmp", environment.KubeConfig, 0o600); err != nil {
		log.Fatal(err)
	}
	if err := os.Rename(*kubeconfig+".tmp", *kubeconfig); err != nil {
		log.Fatal(err)
	}
	<-ctx.Done()
	if err := environment.Stop(); err != nil {
		log.Fatal(err)
	}
}
