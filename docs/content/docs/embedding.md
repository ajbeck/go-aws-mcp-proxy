---

title: Embed the proxy
description: Run the complete SigV4 MCP proxy inside a Go process.
weight: 35
kicker: Documentation
---
The supported runtime embedding API is `proxy.Run`. It keeps authentication, metadata,
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

## Run preflight diagnostics

`proxy.Diagnose` returns the same redacted configuration and AWS identity report
as the `doctor` command. It does not contact the configured MCP endpoint unless
`DiagnoseOptions.Probe` is true:

```go
report := proxy.Diagnose(ctx, proxy.Config{
	Endpoint: new("https://aws-mcp.us-east-1.api.aws/mcp"),
	Service:  new("aws-mcp"),
	Region:   new("us-east-1"),
}, proxy.DiagnoseOptions{})
if report.Healthy == nil || !*report.Healthy {
	// Render or marshal report.Checks for the user.
}
```

The report includes credential source and expiration metadata plus the account,
ARN, and user ID returned by STS `GetCallerIdentity`. It never contains access
keys, secret keys, or session tokens.

The repository includes a runnable offline diagnostic example:

```bash
go test ./proxy -run '^ExampleDiagnose$' -v
```
