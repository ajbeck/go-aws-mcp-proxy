package proxy

import (
	"context"
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
	Close() error
	InitializeResult() *mcp.InitializeResult
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

	upstream upstreamState
}

func (r *Runtime) Run(ctx context.Context) error {
	server := r.newServer()
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
