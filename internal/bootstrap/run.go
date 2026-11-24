// Package bootstrap 负责 controller-manager 的参数解析、依赖装配与启动。
package bootstrap

import (
	"context"
	"flag"
	"fmt"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	"go.ddupan.top/ayatori/internal/infra/clientcert"
)

var setupLog = ctrl.Log.WithName("setup")

// Run 只编排启动顺序；命令行参数与信号处理在进程中初始化一次。
func Run() error {
	var options options
	options.bindFlags(flag.CommandLine)
	flag.Parse()
	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&options.logging)))

	manager, err := newManager(options)
	if err != nil {
		return err
	}
	baoClient, err := setupOpenBaoAuthentication(manager, options.openBao)
	if err != nil {
		return fmt.Errorf("set up OpenBao authentication: %w", err)
	}
	closeDatabaseConnections, err := registerDatabaseControllers(context.Background(), manager, options.database, baoClient)
	if err != nil {
		return err
	}
	// manager 的 worker 完全停止后才释放组件持有的资源。
	defer closeDatabaseConnections()
	if err := registerLoadBalancerControllers(context.Background(), manager, options.lb); err != nil {
		return err
	}
	if err := (&clientcert.RenewalApprover{Client: manager.GetClient()}).SetupWithManager(manager); err != nil {
		return fmt.Errorf("set up client certificate renewal approver: %w", err)
	}

	setupLog.Info("Starting manager")
	if err := manager.Start(ctrl.SetupSignalHandler()); err != nil {
		return fmt.Errorf("run controller manager: %w", err)
	}
	return nil
}
