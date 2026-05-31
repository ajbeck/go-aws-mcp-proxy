package main

import (
	"context"
	"os"

	"github.com/ajbeck/go-aws-mcp-proxy/internal/cli"
	"github.com/ajbeck/go-aws-mcp-proxy/internal/proxy"
	"github.com/ajbeck/go-aws-mcp-proxy/internal/proxyconfig"
)

var version = "dev"

func main() {
	os.Exit(cli.Run(context.Background(), os.Args[1:], cli.Options{
		Env:     proxyconfig.OSEnv{},
		Runner:  proxy.Runner{Version: version},
		Stderr:  os.Stderr,
		Stdout:  os.Stdout,
		Version: version,
	}))
}
