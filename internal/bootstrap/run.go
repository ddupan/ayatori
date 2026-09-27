// Package bootstrap 负责 controller-manager 的参数解析、依赖装配与启动。
package bootstrap

import (
	"context"
	"flag"
	"fmt"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
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
	if err := setupOpenBaoAuthentication(manager, options.openBao); err != nil {
		return fmt.Errorf("set up OpenBao authentication: %w", err)
	}
	cleanup, err := setupDatabase(context.Background(), manager, options.database)
	if err != nil {
		return err
	}
	// manager 的 worker 完全停止后才释放组件持有的资源。
	defer cleanup()

	setupLog.Info("Starting manager")
	if err := manager.Start(ctrl.SetupSignalHandler()); err != nil {
		return fmt.Errorf("run controller manager: %w", err)
	}
	return nil
}
