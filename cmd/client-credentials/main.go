// client-credentials 替不会自行续签的控制面组件（ayatori-manager、Flux、ESO、kube-state-metrics）
// 管理客户端证书（ADR-0021）：每个身份一个目录，证书文件属于目录的所有者，组件按路径引用并自动重新加载。
//
// client-credentials approve-bootstrap 是部署方的首次签发：核对以某个 bootstrap token 提交的 CSR 后批准。
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"

	"git.ddupan.top/panxiao81/ayatori/internal/infra/clientcert"
)

type identities []clientcert.Identity

func (l *identities) String() string { return fmt.Sprint(*l) }

// Set 解析 <用户名>=<目录>；证书的 CN 为用户名，文件属于目录的所有者。
func (l *identities) Set(v string) error {
	name, dir, ok := strings.Cut(v, "=")
	if !ok || name == "" || dir == "" {
		return fmt.Errorf("want <user>=<dir>, got %q", v)
	}
	uid, err := owner(dir)
	if err != nil {
		return err
	}
	*l = append(*l, clientcert.Identity{Dir: dir, CommonName: name, UID: uid})
	return nil
}

func main() {
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if len(os.Args) > 1 && os.Args[1] == "approve-bootstrap" {
		if err := approveBootstrap(ctx, os.Args[2:]); err != nil {
			logger.Error("approve bootstrap CSR", "error", err)
			os.Exit(1)
		}
		return
	}
	var ids identities
	flag.Var(&ids, "identity", "<user>=<dir>，可重复")
	flag.Parse()
	for _, id := range ids {
		stopRenewal, err := clientcert.Start(ctx, id)
		if err != nil {
			logger.Error("start certificate manager", "identity", id.CommonName, "error", err)
			os.Exit(1)
		}
		defer stopRenewal()
		logger.Info("managing client certificate", "identity", id.CommonName, "dir", id.Dir)
	}
	<-ctx.Done()
}

func approveBootstrap(ctx context.Context, args []string) error {
	flags := flag.NewFlagSet("approve-bootstrap", flag.ExitOnError)
	kubeconfig := flags.String("kubeconfig", "", "管理员 kubeconfig")
	tokenID := flags.String("token-id", "", "bootstrap token 的 ID")
	user := flags.String("user", "", "要签发的用户名（证书 CN）")
	var groups stringList
	flags.Var(&groups, "group", "除 "+clientcert.Group+" 外的组（证书 O），可重复")
	timeout := flags.Duration("timeout", 5*time.Minute, "等待 CSR 的时间")
	_ = flags.Parse(args)
	config, err := clientcmd.BuildConfigFromFlags("", *kubeconfig)
	if err != nil {
		return err
	}
	admin, err := kubernetes.NewForConfig(config)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, *timeout)
	defer cancel()
	return clientcert.ApproveBootstrap(ctx, admin, *tokenID, *user, groups)
}

type stringList []string

func (l *stringList) String() string     { return strings.Join(*l, ",") }
func (l *stringList) Set(v string) error { *l = append(*l, v); return nil }
