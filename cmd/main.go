package main

import (
	"os"

	"go.ddupan.top/ayatori/internal/bootstrap"
	ctrl "sigs.k8s.io/controller-runtime"
)

// version 由发布构建以 -ldflags "-X main.version=<tag>" 写入（Dockerfile 的 VERSION）；本地构建是 dev。
var version = "dev"

func main() {
	if err := bootstrap.Run(version); err != nil {
		ctrl.Log.WithName("setup").Error(err, "Controller manager exited")
		os.Exit(1)
	}
}
