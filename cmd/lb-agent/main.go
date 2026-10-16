// lb-agent 在 LB 主机上按本机 HAProxy 的 frontend 状态宣告 VIP（ADR-0014）。
package main

import (
	"context"
	"errors"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"git.ddupan.top/panxiao81/ayatori/internal/loadbalancer/agent"
)

func main() {
	path := flag.String("config", "/etc/ayatori/lb-agent.yaml", "本地配置文件")
	flag.Parse()
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	if err := run(*path, logger); err != nil {
		logger.Error("lb-agent exited", "error", err)
		os.Exit(1)
	}
}

func run(path string, logger *slog.Logger) error {
	config, err := agent.LoadConfig(path)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	registry := prometheus.NewRegistry()
	a, err := agent.New(ctx, config, registry, logger)
	if err != nil {
		return err
	}
	metricsErr := make(chan error, 1)
	if config.MetricsAddress != "" {
		server := &http.Server{
			Addr:              config.MetricsAddress,
			Handler:           promhttp.HandlerFor(registry, promhttp.HandlerOpts{}),
			ReadHeaderTimeout: 5 * time.Second,
		}
		go func() { metricsErr <- server.ListenAndServe() }()
		defer func() { _ = server.Close() }()
	}
	done := make(chan struct{})
	go func() { a.Run(ctx); close(done) }()
	select {
	case <-done:
		return nil
	case err := <-metricsErr:
		// 指标端口无法监听是部署错误；退出让 systemd 报告，会话随进程断开。
		stop()
		<-done
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}
