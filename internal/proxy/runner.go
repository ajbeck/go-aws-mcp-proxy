package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/ajbeck/go-aws-mcp-proxy/internal/awshttp"
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

func (r Runner) RunProxy(ctx context.Context, cfg proxyconfig.Config, logger *slog.Logger) error {
	if r.Logger != nil {
		logger = r.Logger
	}
	runtime := Runtime{
		config:    cfg,
		connector: r.Connector,
		logger:    logger,
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
	profiles profileSessions
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

	defer r.profiles.Close()
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
	if r.logger != nil {
		r.logger.Info("registered upstream tools", "count", len(result.Tools), "read_only", r.config.ReadOnly)
	}
	return nil
}

func (r *Runtime) registerTool(tool *mcp.Tool, upstream UpstreamSession) {
	if tool == nil || tool.Name == "" {
		return
	}

	localTool := cloneTool(tool)
	localTool.InputSchema = normalizedInputSchema(localTool.InputSchema)
	if len(r.config.Profiles) > 0 && authRequiringTool(localTool.Name) {
		localTool.InputSchema = inputSchemaWithProfile(localTool.InputSchema, r.config.Profiles)
	}
	if localTool.OutputSchema != nil && !schemaIsObject(localTool.OutputSchema) {
		localTool.OutputSchema = nil
	}

	r.server.AddTool(localTool, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		start := time.Now()
		callCtx := ctx
		cancel := func() {}
		if r.config.ToolTimeout > 0 {
			callCtx, cancel = context.WithTimeout(ctx, r.config.ToolTimeout)
		}
		defer cancel()

		result, err := r.callUpstreamTool(callCtx, upstream, req)
		if err != nil {
			if r.logger != nil {
				r.logger.Error("upstream tool call failed", "tool", req.Params.Name, "duration_ms", time.Since(start).Milliseconds(), "error", err)
			}
			return toolErrorResult(req.Params.Name, err), nil
		}
		if r.logger != nil {
			r.logger.Debug("upstream tool call completed", "tool", req.Params.Name, "duration_ms", time.Since(start).Milliseconds(), "is_error", result != nil && result.IsError)
		}
		return result, nil
	})
}

func (r *Runtime) callUpstreamTool(ctx context.Context, upstream UpstreamSession, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args, profile, err := argumentsAndProfile(req.Params.Arguments)
	if err != nil {
		return nil, err
	}
	if profile == "" {
		return upstream.CallTool(ctx, &mcp.CallToolParams{
			Name:      req.Params.Name,
			Arguments: rawArguments(req.Params.Arguments),
		})
	}

	if !authRequiringTool(req.Params.Name) {
		if r.logger != nil {
			r.logger.Warn("ignoring aws_profile on non-auth tool", "tool", req.Params.Name)
		}
		return upstream.CallTool(ctx, &mcp.CallToolParams{
			Name:      req.Params.Name,
			Arguments: args,
		})
	}

	if !allowedProfile(profile, r.config.Profiles) {
		return nil, fmt.Errorf("profile %q is not in the allowed list; allowed profiles: %s", profile, profileList(r.config.Profiles))
	}
	if profile == defaultProfile(r.config.Profiles) {
		return upstream.CallTool(ctx, &mcp.CallToolParams{
			Name:      req.Params.Name,
			Arguments: args,
		})
	}

	session, err := r.profiles.Get(ctx, profile, r)
	if err != nil {
		return nil, fmt.Errorf("failed to create connection for profile %q; check that the profile is configured and credentials are valid: %w", profile, err)
	}
	if r.logger != nil {
		r.logger.Info("routing tool call through profile override", "tool", req.Params.Name, "profile", profile)
	}
	return session.CallTool(ctx, &mcp.CallToolParams{
		Name:      req.Params.Name,
		Arguments: args,
	})
}

func (r *Runtime) connectUpstream(ctx context.Context, params *mcp.InitializeParams) (UpstreamSession, error) {
	connector := r.connector
	if connector == nil {
		connector = MCPUpstreamConnector{Logger: r.logger, Version: r.version}
	}

	session, err := connector.Connect(ctx, r.config, params)
	if err != nil {
		return nil, err
	}
	r.profiles.SetInitializeParams(params)
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

var authRequiringTools = map[string]bool{
	"aws___call_aws":             true,
	"aws___run_script":           true,
	"aws___get_presigned_url":    true,
	"aws___get_tasks":            true,
	"aws___suggest_aws_commands": true,
}

func authRequiringTool(name string) bool {
	return authRequiringTools[name]
}

func inputSchemaWithProfile(schema any, profiles []string) any {
	object := schemaObject(schema)
	properties, _ := object["properties"].(map[string]any)
	if properties == nil {
		properties = map[string]any{}
		object["properties"] = properties
	}
	properties["aws_profile"] = map[string]any{
		"type":        "string",
		"description": "AWS CLI profile to sign this request with. Available profiles: " + profileList(profiles) + ".",
		"enum":        append([]string(nil), profiles...),
	}
	return object
}

func schemaObject(schema any) map[string]any {
	switch value := schema.(type) {
	case map[string]any:
		return deepCopyMap(value)
	case json.RawMessage:
		var decoded map[string]any
		if err := json.Unmarshal(value, &decoded); err == nil && decoded != nil {
			return decoded
		}
	case []byte:
		var decoded map[string]any
		if err := json.Unmarshal(value, &decoded); err == nil && decoded != nil {
			return decoded
		}
	}
	return map[string]any{"type": "object"}
}

func deepCopyMap(value map[string]any) map[string]any {
	encoded, err := json.Marshal(value)
	if err != nil {
		return map[string]any{"type": "object"}
	}
	var decoded map[string]any
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		return map[string]any{"type": "object"}
	}
	return decoded
}

func argumentsAndProfile(arguments json.RawMessage) (any, string, error) {
	if len(arguments) == 0 {
		return map[string]any{}, "", nil
	}
	var decoded map[string]any
	if err := json.Unmarshal(arguments, &decoded); err != nil {
		return arguments, "", nil
	}
	value, ok := decoded["aws_profile"]
	if !ok {
		return arguments, "", nil
	}
	profile, ok := value.(string)
	if !ok || profile == "" {
		return nil, "", fmt.Errorf("aws_profile must be a non-empty string")
	}
	delete(decoded, "aws_profile")
	encoded, err := json.Marshal(decoded)
	if err != nil {
		return nil, "", err
	}
	return json.RawMessage(encoded), profile, nil
}

func allowedProfile(profile string, profiles []string) bool {
	for _, allowed := range profiles {
		if profile == allowed {
			return true
		}
	}
	return false
}

func defaultProfile(profiles []string) string {
	if len(profiles) == 0 {
		return ""
	}
	return profiles[0]
}

func profileList(profiles []string) string {
	if len(profiles) == 0 {
		return ""
	}
	out := profiles[0]
	for _, profile := range profiles[1:] {
		out += ", " + profile
	}
	return out
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
	Logger     *slog.Logger
	Version    string
}

func (c MCPUpstreamConnector) Connect(ctx context.Context, cfg proxyconfig.Config, params *mcp.InitializeParams) (UpstreamSession, error) {
	version := c.Version
	if version == "" {
		version = "dev"
	}
	options := awshttp.ClientOptions{
		Logger:  c.Logger,
		Version: version,
	}
	if params != nil && params.ClientInfo != nil {
		options.ClientName = params.ClientInfo.Name
		options.ClientVersion = params.ClientInfo.Version
	}
	httpClient, err := awshttp.NewClientWithOptions(ctx, cfg, c.HTTPClient, options)
	if err != nil {
		return nil, err
	}

	client := mcp.NewClient(&mcp.Implementation{
		Name:    defaultName,
		Title:   defaultTitle,
		Version: version,
	}, nil)

	return client.Connect(ctx, &mcp.StreamableClientTransport{
		Endpoint:   cfg.Endpoint,
		HTTPClient: httpClient,
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

type profileSessions struct {
	mu     sync.Mutex
	params *mcp.InitializeParams
	cache  map[string]UpstreamSession
}

func (s *profileSessions) SetInitializeParams(params *mcp.InitializeParams) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.params = params
}

func (s *profileSessions) Get(ctx context.Context, profile string, runtime *Runtime) (UpstreamSession, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cache != nil {
		if session := s.cache[profile]; session != nil {
			return session, nil
		}
	}
	params := s.params

	cfg := runtime.config
	cfg.Profiles = []string{profile}
	connector := runtime.connector
	if connector == nil {
		connector = MCPUpstreamConnector{Logger: runtime.logger, Version: runtime.version}
	}

	session, err := connector.Connect(ctx, cfg, params)
	if err != nil {
		return nil, err
	}

	if s.cache == nil {
		s.cache = map[string]UpstreamSession{}
	}
	s.cache[profile] = session
	return session, nil
}

func (s *profileSessions) Close() error {
	s.mu.Lock()
	cache := s.cache
	s.cache = nil
	s.mu.Unlock()

	var firstErr error
	for _, session := range cache {
		if err := session.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}
