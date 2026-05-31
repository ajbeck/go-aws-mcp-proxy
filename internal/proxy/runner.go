package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"sync"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/ajbeck/go-aws-mcp-proxy/internal/proxyconfig"
)

const (
	defaultName         = "aws-mcp-proxy"
	defaultTitle        = "MCP Proxy for AWS"
	defaultInstructions = "MCP Proxy for AWS provides access to SigV4 protected MCP servers through a single interface."
)

type UpstreamConnector interface {
	Connect(context.Context, proxyconfig.Config, *mcp.InitializeParams) (UpstreamSession, error)
}

type UpstreamSession interface {
	CallTool(context.Context, *mcp.CallToolParams) (*mcp.CallToolResult, error)
	Close() error
	InitializeResult() *mcp.InitializeResult
	ListTools(context.Context, *mcp.ListToolsParams) (*mcp.ListToolsResult, error)
}

type Runner struct {
	Connector UpstreamConnector
	Logger    *slog.Logger
	Transport mcp.Transport
	Version   string
}

func (r Runner) RunProxy(ctx context.Context, cfg proxyconfig.Config) error {
	runtime := Runtime{
		config:    cfg,
		connector: r.Connector,
		logger:    r.Logger,
		transport: r.Transport,
		version:   r.Version,
	}
	return runtime.Run(ctx)
}

type Runtime struct {
	config    proxyconfig.Config
	connector UpstreamConnector
	logger    *slog.Logger
	transport mcp.Transport
	version   string

	server   *mcp.Server
	upstream upstreamState
}

func (r *Runtime) Run(ctx context.Context) error {
	server := r.newServer()
	r.server = server
	server.AddReceivingMiddleware(r.initializeMiddleware())

	transport := r.transport
	if transport == nil {
		transport = &mcp.StdioTransport{}
	}

	defer r.upstream.Close()
	return server.Run(ctx, transport)
}

func (r *Runtime) newServer() *mcp.Server {
	version := r.version
	if version == "" {
		version = "dev"
	}

	return mcp.NewServer(&mcp.Implementation{
		Name:    defaultName,
		Title:   defaultTitle,
		Version: version,
	}, &mcp.ServerOptions{
		Instructions: defaultInstructions,
		Logger:       r.logger,
		Capabilities: &mcp.ServerCapabilities{
			Tools: &mcp.ToolCapabilities{ListChanged: true},
		},
	})
}

func (r *Runtime) initializeMiddleware() mcp.Middleware {
	return func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
			if method != "initialize" {
				return next(ctx, method, req)
			}

			params, ok := req.GetParams().(*mcp.InitializeParams)
			if !ok {
				return nil, fmt.Errorf("initialize params have unexpected type %T", req.GetParams())
			}

			upstream, err := r.connectUpstream(ctx, params)
			if err != nil {
				return nil, err
			}
			if err := r.registerUpstreamTools(ctx, upstream); err != nil {
				_ = r.upstream.Close()
				return nil, err
			}

			result, err := next(ctx, method, req)
			if err != nil {
				_ = r.upstream.Close()
				return nil, err
			}

			initializeResult, ok := result.(*mcp.InitializeResult)
			if !ok {
				return result, nil
			}
			applyUpstreamCapabilities(initializeResult, upstream.InitializeResult())
			return initializeResult, nil
		}
	}
}

func (r *Runtime) registerUpstreamTools(ctx context.Context, upstream UpstreamSession) error {
	result, err := upstream.ListTools(ctx, &mcp.ListToolsParams{})
	if err != nil {
		return err
	}
	if result == nil {
		return nil
	}

	for _, tool := range filterTools(result.Tools, r.config.ReadOnly) {
		r.registerTool(tool, upstream)
	}
	return nil
}

func (r *Runtime) registerTool(tool *mcp.Tool, upstream UpstreamSession) {
	if tool == nil || tool.Name == "" {
		return
	}

	localTool := cloneTool(tool)
	localTool.InputSchema = normalizedInputSchema(localTool.InputSchema)
	if localTool.OutputSchema != nil && !schemaIsObject(localTool.OutputSchema) {
		localTool.OutputSchema = nil
	}

	r.server.AddTool(localTool, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		callCtx := ctx
		cancel := func() {}
		if r.config.ToolTimeout > 0 {
			callCtx, cancel = context.WithTimeout(ctx, r.config.ToolTimeout)
		}
		defer cancel()

		result, err := upstream.CallTool(callCtx, &mcp.CallToolParams{
			Name:      req.Params.Name,
			Arguments: rawArguments(req.Params.Arguments),
		})
		if err != nil {
			return toolErrorResult(req.Params.Name, err), nil
		}
		return result, nil
	})
}

func (r *Runtime) connectUpstream(ctx context.Context, params *mcp.InitializeParams) (UpstreamSession, error) {
	connector := r.connector
	if connector == nil {
		connector = MCPUpstreamConnector{Version: r.version}
	}

	session, err := connector.Connect(ctx, r.config, params)
	if err != nil {
		return nil, err
	}
	r.upstream.Set(session, params.ClientInfo)
	return session, nil
}

func applyUpstreamCapabilities(local, upstream *mcp.InitializeResult) {
	if local == nil || upstream == nil || upstream.Capabilities == nil {
		return
	}
	local.Capabilities = upstream.Capabilities
}

func filterTools(tools []*mcp.Tool, readOnly bool) []*mcp.Tool {
	if !readOnly {
		return tools
	}

	var filtered []*mcp.Tool
	for _, tool := range tools {
		if tool != nil && tool.Annotations != nil && tool.Annotations.ReadOnlyHint {
			filtered = append(filtered, tool)
		}
	}
	return filtered
}

func cloneTool(tool *mcp.Tool) *mcp.Tool {
	clone := *tool
	if tool.Annotations != nil {
		annotations := *tool.Annotations
		clone.Annotations = &annotations
	}
	return &clone
}

func normalizedInputSchema(schema any) any {
	if schemaIsObject(schema) {
		return schema
	}
	return map[string]any{"type": "object"}
}

func schemaIsObject(schema any) bool {
	switch s := schema.(type) {
	case map[string]any:
		return s["type"] == "object"
	case json.RawMessage:
		var decoded map[string]any
		if err := json.Unmarshal(s, &decoded); err != nil {
			return false
		}
		return decoded["type"] == "object"
	case []byte:
		var decoded map[string]any
		if err := json.Unmarshal(s, &decoded); err != nil {
			return false
		}
		return decoded["type"] == "object"
	default:
		return false
	}
}

func rawArguments(arguments json.RawMessage) any {
	if len(arguments) == 0 {
		return map[string]any{}
	}
	return arguments
}

func toolErrorResult(toolName string, err error) *mcp.CallToolResult {
	return &mcp.CallToolResult{
		Content: []mcp.Content{
			&mcp.TextContent{Text: fmt.Sprintf("Tool call %q failed: %v. Please retry.", toolName, err)},
		},
		IsError: true,
	}
}

type MCPUpstreamConnector struct {
	HTTPClient *http.Client
	Version    string
}

func (c MCPUpstreamConnector) Connect(ctx context.Context, cfg proxyconfig.Config, _ *mcp.InitializeParams) (UpstreamSession, error) {
	version := c.Version
	if version == "" {
		version = "dev"
	}

	client := mcp.NewClient(&mcp.Implementation{
		Name:    defaultName,
		Title:   defaultTitle,
		Version: version,
	}, nil)

	return client.Connect(ctx, &mcp.StreamableClientTransport{
		Endpoint:   cfg.Endpoint,
		HTTPClient: c.HTTPClient,
	}, nil)
}

type upstreamState struct {
	mu         sync.Mutex
	clientInfo *mcp.Implementation
	session    UpstreamSession
}

func (s *upstreamState) Set(session UpstreamSession, clientInfo *mcp.Implementation) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.session != nil && s.session != session {
		_ = s.session.Close()
	}
	s.session = session
	s.clientInfo = clientInfo
}

func (s *upstreamState) ClientInfo() *mcp.Implementation {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.clientInfo
}

func (s *upstreamState) Close() error {
	s.mu.Lock()
	session := s.session
	s.session = nil
	s.mu.Unlock()

	if session == nil {
		return nil
	}
	return session.Close()
}
