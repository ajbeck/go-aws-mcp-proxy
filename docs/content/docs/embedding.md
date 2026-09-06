---

title: Embed the proxy
description: Run the complete SigV4 MCP proxy inside a Go process.
weight: 35
kicker: Documentation
---
The supported embedding API is `proxy.Run`. It keeps authentication, metadata,
tool filtering, profile switching, recovery, and MCP forwarding on the same path
as the command-line application.

```go
package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"

	"github.com/ajbeck/go-aws-mcp-proxy/proxy"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	endpoint := "https://aws-mcp.us-east-1.api.aws/mcp"
	service := "aws-mcp"
	region := "us-east-1"
	if err := proxy.Run(ctx, proxy.Config{
		Endpoint: &endpoint,
		Service:  &service,
		Region:   &region,
	}, proxy.RunOptions{
		Logger: slog.Default(),
	}); err != nil {
		slog.Error("proxy stopped", "error", err)
		os.Exit(1)
	}
}
```

`Run` uses stdio by default. `RunOptions` can instead accept a custom MCP
transport, logger, base HTTP client, or upstream connector. Unlike the CLI,
library callers provide the SigV4 service and region explicitly; this keeps the
configuration contract deterministic for embedders.

The repository includes a runnable example that uses an in-memory MCP transport
and connector:

```bash
go test ./proxy -run '^ExampleRun$' -v
```

The package intentionally does not expose a raw signed `http.Client`. Metadata
injection, read-only enforcement, dynamic tool reconciliation, profile routing,
and lazy recovery operate above HTTP; bypassing `Run` would omit those proxy
semantics.
