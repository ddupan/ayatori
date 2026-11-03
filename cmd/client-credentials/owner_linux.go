package main

import (
	"os"
	"syscall"
)

func owner(dir string) (int, error) {
	info, err := os.Stat(dir)
	if err != nil {
		return 0, err
	}
	return int(info.Sys().(*syscall.Stat_t).Uid), nil
}
