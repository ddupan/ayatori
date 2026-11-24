package main

import (
	"os"

	"go.ddupan.top/ayatori/internal/bootstrap"
	ctrl "sigs.k8s.io/controller-runtime"
)

func main() {
	if err := bootstrap.Run(); err != nil {
		ctrl.Log.WithName("setup").Error(err, "Controller manager exited")
		os.Exit(1)
	}
}
