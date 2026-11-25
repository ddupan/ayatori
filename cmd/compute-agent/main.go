// compute-agent 在计算节点上按 Ayatori API 下发 libvirt network、pool 与 domain（ADR-0015）。
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/spiffe/go-spiffe/v2/spiffeid"
	"github.com/spiffe/go-spiffe/v2/workloadapi"

	"go.ddupan.top/ayatori/internal/compute/agent"
	"go.ddupan.top/ayatori/internal/compute/console"
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
	serveErr := make(chan error, 2)
	if config.MetricsAddress != "" {
		mux := http.NewServeMux()
		mux.Handle("/metrics", promhttp.HandlerFor(registry, promhttp.HandlerOpts{}))
		server := &http.Server{Addr: config.MetricsAddress, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
		go func() { serveErr <- server.ListenAndServe() }()
		defer func() { _ = server.Close() }()
	}
	if config.Console != nil {
		go func() { serveErr <- serveConsole(ctx, config.Console, a, logger) }()
	}
	done := make(chan struct{})
	go func() { a.Run(ctx); close(done) }()
	select {
	case <-done:
		return nil
	case err := <-serveErr:
		// 指标或中继端口无法监听是部署错误；退出让 systemd 报告。
		stop()
		<-done
		if err == nil || errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}

// serveConsole 运行控制台中继。SVID 在拿到之前一直等待：SPIRE 晚于 Agent 就绪时不影响 Agent 的其他工作。
func serveConsole(ctx context.Context, c *agent.ConsoleConfig, a *agent.Agent, logger *slog.Logger) error {
	source, err := workloadapi.NewX509Source(ctx, workloadapi.WithClientOptions(workloadapi.WithAddr(c.WorkloadAPI)))
	if err != nil {
		if ctx.Err() != nil {
			return nil
		}
		return fmt.Errorf("console relay: %w", err)
	}
	defer func() { _ = source.Close() }()
	logger.Info("console relay listening", "address", c.Address)
	return console.Serve(ctx, c.Address, source, source, spiffeid.RequireFromString(c.ProxyID),
		console.Handler(a.OpenConsole, logger.With("component", "console")))
}
