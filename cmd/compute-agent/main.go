// compute-agent 在计算节点上按 Ayatori API 下发 libvirt network、pool 与 domain（ADR-0015）。
package main

import (
	"context"
	"flag"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"go.ddupan.top/ayatori/internal/compute/agent"
)

func main() {
	path := flag.String("config", "/etc/ayatori/compute-agent.yaml", "本地配置文件")
	flag.Parse()
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	config, err := agent.LoadConfig(*path)
	if err != nil {
		logger.Error("load config", "error", err)
		os.Exit(1)
	}
	a, err := agent.New(config, logger)
	if err != nil {
		logger.Error("compute-agent exited", "error", err)
		os.Exit(1)
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	a.Run(ctx)
}
