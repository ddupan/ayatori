// compute-agent 在计算节点上按 Ayatori API 下发 libvirt network、pool 与 domain（ADR-0015）。
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

	"go.ddupan.top/ayatori/internal/compute/agent"
)

func main() {
	path := flag.String("config", "/etc/ayatori/compute-agent.yaml", "本地配置文件")
	flag.Parse()
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	if err := run(*path, logger); err != nil {
		logger.Error("compute-agent exited", "error", err)
		os.Exit(1)
	}
}

func run(path string, logger *slog.Logger) error {
	config, err := agent.LoadConfig(path)
	if err != nil {
		return err
	}
	registry := prometheus.NewRegistry()
	a, err := agent.New(config, registry, logger)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	metricsErr := make(chan error, 1)
	if config.MetricsAddress != "" {
		mux := http.NewServeMux()
		mux.Handle("/metrics", promhttp.HandlerFor(registry, promhttp.HandlerOpts{}))
		server := &http.Server{Addr: config.MetricsAddress, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
		go func() { metricsErr <- server.ListenAndServe() }()
		defer func() { _ = server.Close() }()
	}
	done := make(chan struct{})
	go func() { a.Run(ctx); close(done) }()
	select {
	case <-done:
		return nil
	case err := <-metricsErr:
		// 指标端口无法监听是部署错误；退出让 systemd 报告。
		stop()
		<-done
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}
