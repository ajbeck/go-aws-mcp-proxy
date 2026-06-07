package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/ajbeck/go-aws-mcp-proxy/internal/cli"
)

var version = "dev"

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	os.Exit(cli.Run(ctx, os.Args[1:], cli.Options{
		Stderr:  os.Stderr,
		Stdout:  os.Stdout,
		Version: version,
	}))
}
