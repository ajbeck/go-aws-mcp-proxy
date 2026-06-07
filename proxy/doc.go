// Package proxy runs an MCP stdio proxy for SigV4-protected AWS MCP
// servers.
//
// The public entry point is Run. Callers provide a Config describing the
// upstream MCP endpoint and optional RunOptions for embedding concerns such as
// logging, custom MCP transports, a shared HTTP client, or a replacement
// upstream connector for tests.
//
// A minimal embedded proxy run looks like:
//
//	err := proxy.Run(ctx, proxy.Config{
//		Endpoint: "https://service.us-east-1.api.aws/mcp",
//		Region:   "us-east-1",
//		Service:  "service",
//	}, proxy.RunOptions{
//		Logger: logger,
//	})
package proxy
