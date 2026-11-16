// Command kvctl is the terminal client for the v2 distributed KV cluster.
//
//	kvctl -nodes 127.0.0.1:7001,127.0.0.1:7002,127.0.0.1:7003 get user:1
//	kvctl -meta 127.0.0.1:9090 ranges
//	kvctl -meta 127.0.0.1:9090 split 1 m
//	kvctl -nodes 127.0.0.1:7001,127.0.0.1:7002,127.0.0.1:7003 backup -out ./backup
//
// Run `kvctl help` for commands and global flags.
package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"distributed-db/internal/kvctl"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	os.Exit(kvctl.NewRunner(os.Stdin, os.Stdout, os.Stderr).Run(ctx, os.Args[1:]))
}
