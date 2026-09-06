package proxy_test

import (
	"context"
	"fmt"

	"github.com/ajbeck/go-aws-mcp-proxy/proxy"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// ExampleRun demonstrates embedding the complete proxy with a custom MCP
// transport and upstream connector. Production embedders can omit both to use
// the standard stdio and SigV4 Streamable HTTP paths.
func ExampleRun() {
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	endpoint := "https://example.invalid/mcp"
	errs := make(chan error, 1)
	go func() {
		errs <- proxy.Run(ctx, proxy.Config{Endpoint: &endpoint}, proxy.RunOptions{
			Connector: exampleConnector{},
			Transport: serverTransport,
			Version:   "example",
		})
	}()

	client := mcp.NewClient(&mcp.Implementation{Name: "embedded-client", Version: "1.0.0"}, nil)
	session, err := client.Connect(ctx, clientTransport, nil)
	if err != nil {
		panic(err)
	}

	result, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "hello"})
	if err != nil {
		panic(err)
	}
	fmt.Println(result.Content[0].(*mcp.TextContent).Text)

	if err := session.Close(); err != nil {
		panic(err)
	}
	if err := <-errs; err != nil {
		panic(err)
	}

	// Output:
	// hello from embedded upstream
}

type exampleConnector struct{}

func (exampleConnector) Connect(context.Context, proxy.Config, *mcp.InitializeParams) (proxy.UpstreamSession, error) {
	return exampleSession{}, nil
}

type exampleSession struct{}

func (exampleSession) CallTool(context.Context, *mcp.CallToolParams) (*mcp.CallToolResult, error) {
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "hello from embedded upstream"}}}, nil
}

func (exampleSession) Close() error { return nil }

func (exampleSession) InitializeResult() *mcp.InitializeResult {
	return &mcp.InitializeResult{Capabilities: &mcp.ServerCapabilities{Tools: &mcp.ToolCapabilities{}}}
}

func (exampleSession) ListTools(context.Context, *mcp.ListToolsParams) (*mcp.ListToolsResult, error) {
	return &mcp.ListToolsResult{Tools: []*mcp.Tool{{
		Name:        "hello",
		Description: "Return a greeting from the embedded upstream.",
		InputSchema: map[string]any{"type": "object"},
	}}}, nil
}
