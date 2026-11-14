package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"duet/internal/duet"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	os.Exit(duet.RunCLI(ctx, os.Args[1:], os.Stdout, os.Stderr))
}
