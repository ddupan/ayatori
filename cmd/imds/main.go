// imds 是实例的元数据服务 ayatori-imds（ADR-0016），每台计算节点运行一个。
package main

import (
	"context"
	"flag"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"go.ddupan.top/ayatori/internal/compute/imds"
)

func main() {
	stateDir := flag.String("state-dir", "/var/lib/ayatori/compute-agent", "Compute Agent 的本地状态目录，只读")
	flag.Parse()
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	s := &imds.Service{StateDir: *stateDir, Logger: logger}
	if err := s.Run(ctx); err != nil {
		logger.Error("imds exited", "error", err)
		os.Exit(1)
	}
}
