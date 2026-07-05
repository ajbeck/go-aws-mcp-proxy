package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	defaultName         = "aws-mcp-proxy"
	defaultTitle        = "MCP Proxy for AWS"
	defaultInstructions = "MCP Proxy for AWS provides access to SigV4 protected MCP servers through a single interface."
)

// UpstreamConnector opens an MCP client session to the configured upstream.
type UpstreamConnector interface {
	Connect(context.Context, Config, *mcp.InitializeParams) (UpstreamSession, error)
}

// UpstreamSession is the upstream MCP session behavior used by the proxy.
type UpstreamSession interface {
	CallTool(context.Context, *mcp.CallToolParams) (*mcp.CallToolResult, error)
	Close() error
	InitializeResult() *mcp.InitializeResult
	ListTools(context.Context, *mcp.ListToolsParams) (*mcp.ListToolsResult, error)
}

// RunOptions configures embedding and test seams for Run.
type RunOptions struct {
	// Connector replaces the default Streamable HTTP upstream connector.
	Connector UpstreamConnector
	// HTTPClient is used by the default upstream connector.
	HTTPClient *http.Client
	// Logger receives proxy and request logs.
	Logger *slog.Logger
	// Transport is the MCP server transport. It defaults to stdio.
	Transport mcp.Transport
	// Version is reported in MCP implementation metadata and user-agent data.
	Version string
}

// Run starts the proxy and blocks until the MCP server transport exits or the
// context is canceled.
func Run(ctx context.Context, cfg Config, options RunOptions) error {
	logger := options.Logger
	run := proxyRun{
		config:     cfg,
		connector:  options.Connector,
		httpClient: options.HTTPClient,
		logger:     logger,
		transport:  options.Transport,
		version:    options.Version,
	}
	return run.run(ctx)
}

type proxyRun struct {
	config     Config
	connector  UpstreamConnector
	httpClient *http.Client
	logger     *slog.Logger
	transport  mcp.Transport
	version    string

	server   *mcp.Server
	profiles profileSessions
	upstream upstreamState
}

func (r *proxyRun) run(ctx context.Context) error {
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

func (r *proxyRun) newServer() *mcp.Server {
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

func (r *proxyRun) initializeMiddleware() mcp.Middleware {
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

func (r *proxyRun) registerUpstreamTools(ctx context.Context, upstream UpstreamSession) error {
	result, err := r.listUpstreamTools(ctx, upstream)
	if err != nil {
		return err
	}
	if result == nil {
		return nil
	}

	readOnly := enabled(r.config.ReadOnly)
	for _, tool := range filterTools(result.Tools, readOnly) {
		r.registerTool(tool, upstream)
	}
	if r.logger != nil {
		r.logger.Info("registered upstream tools", "count", len(result.Tools), "read_only", readOnly)
	}
	return nil
}

func (r *proxyRun) listUpstreamTools(ctx context.Context, upstream UpstreamSession) (*mcp.ListToolsResult, error) {
	retries := retryCount(r.config.Retries)
	for attempt := 0; ; attempt++ {
		result, err := upstream.ListTools(ctx, &mcp.ListToolsParams{})
		if err == nil {
			return result, nil
		}
		if !shouldRetry(ctx, attempt, retries) {
			return nil, err
		}
		if r.logger != nil {
			r.logger.Warn("retrying upstream tools/list", "attempt", attempt, "next_attempt", attempt+1, "error", err)
		}
		if err := waitForRetry(ctx, retryDelay(attempt)); err != nil {
			return nil, err
		}
	}
}

func (r *proxyRun) registerTool(tool *mcp.Tool, upstream UpstreamSession) {
	if tool == nil || tool.Name == "" {
		return
	}

	localTool := cloneTool(tool)
	localTool.InputSchema = normalizedInputSchema(localTool.InputSchema)
	profiles := value(r.config.Profiles)
	if len(profiles) > 0 && authRequiringTool(localTool.Name) {
		localTool.InputSchema = inputSchemaWithProfile(localTool.InputSchema, profiles)
	}
	if localTool.OutputSchema != nil && !schemaIsObject(localTool.OutputSchema) {
		localTool.OutputSchema = nil
	}

	r.server.AddTool(localTool, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		start := time.Now()
		callCtx := ctx
		cancel := func() {}
		if positiveDuration(r.config.ToolTimeout) {
			callCtx, cancel = context.WithTimeout(ctx, *r.config.ToolTimeout)
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

func (r *proxyRun) callUpstreamTool(ctx context.Context, upstream UpstreamSession, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args, profile, err := argumentsAndProfile(req.Params.Arguments)
	if err != nil {
		return nil, err
	}
	if profile == "" {
		return r.callSessionTool(ctx, upstream, &mcp.CallToolParams{
			Name:      req.Params.Name,
			Arguments: rawArguments(req.Params.Arguments),
		})
	}

	if !authRequiringTool(req.Params.Name) {
		if r.logger != nil {
			r.logger.Warn("ignoring aws_profile on non-auth tool", "tool", req.Params.Name)
		}
		return r.callSessionTool(ctx, upstream, &mcp.CallToolParams{
			Name:      req.Params.Name,
			Arguments: args,
		})
	}

	profiles := value(r.config.Profiles)
	if !allowedProfile(profile, profiles) {
		return nil, fmt.Errorf("profile %q is not in the allowed list; allowed profiles: %s", profile, profileList(profiles))
	}
	defaultProfile := defaultProfile(r.config.Profiles)
	if defaultProfile != nil && profile == *defaultProfile {
		return r.callSessionTool(ctx, upstream, &mcp.CallToolParams{
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
	return r.callSessionTool(ctx, session, &mcp.CallToolParams{
		Name:      req.Params.Name,
		Arguments: args,
	})
}

func (r *proxyRun) callSessionTool(ctx context.Context, session UpstreamSession, params *mcp.CallToolParams) (*mcp.CallToolResult, error) {
	retries := retryCount(r.config.Retries)
	for attempt := 0; ; attempt++ {
		result, err := session.CallTool(ctx, params)
		if err == nil {
			return result, nil
		}
		if !shouldRetry(ctx, attempt, retries) {
			return nil, err
		}
		if r.logger != nil {
			r.logger.Warn("retrying upstream tool call", "tool", params.Name, "attempt", attempt, "next_attempt", attempt+1, "error", err)
		}
		if err := waitForRetry(ctx, retryDelay(attempt)); err != nil {
			return nil, err
		}
	}
}

func retryCount(retries *int) int {
	if retries == nil {
		return 3
	}
	return *retries
}

func shouldRetry(ctx context.Context, attempt, retries int) bool {
	return retries > 0 && attempt < retries && ctx.Err() == nil
}

func retryDelay(attempt int) time.Duration {
	delay := 100 * time.Millisecond * (1 << attempt)
	if delay > 2*time.Second {
		return 2 * time.Second
	}
	return delay
}

func waitForRetry(ctx context.Context, delay time.Duration) error {
	if delay <= 0 {
		return nil
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func (r *proxyRun) connectUpstream(ctx context.Context, params *mcp.InitializeParams) (UpstreamSession, error) {
	connector := r.connector
	if connector == nil {
		connector = mcpUpstreamConnector{HTTPClient: r.httpClient, Version: r.version}
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

func defaultProfile(profiles *[]string) *string {
	if profiles == nil || len(*profiles) == 0 {
		return nil
	}
	return &(*profiles)[0]
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

type mcpUpstreamConnector struct {
	HTTPClient *http.Client
	Version    string
}

func (c mcpUpstreamConnector) Connect(ctx context.Context, cfg Config, params *mcp.InitializeParams) (UpstreamSession, error) {
	endpoint, err := required(cfg.Endpoint, "endpoint")
	if err != nil {
		return nil, err
	}
	if err := validateEndpoint(endpoint); err != nil {
		return nil, err
	}

	version := c.Version
	if version == "" {
		version = "dev"
	}
	options := clientOptions{
		Version: version,
	}
	if params != nil && params.ClientInfo != nil {
		options.ClientName = params.ClientInfo.Name
		options.ClientVersion = params.ClientInfo.Version
	}
	httpClient, err := newClient(ctx, cfg, c.HTTPClient, options)
	if err != nil {
		return nil, err
	}

	client := mcp.NewClient(&mcp.Implementation{
		Name:    defaultName,
		Title:   defaultTitle,
		Version: version,
	}, nil)
	if metadata := requestMetadata(cfg); len(metadata) > 0 {
		client.AddSendingMiddleware(metadataMiddleware(metadata))
	}

	retries := retryCount(cfg.Retries)
	for attempt := 0; ; attempt++ {
		session, err := client.Connect(ctx, &mcp.StreamableClientTransport{
			Endpoint:   endpoint,
			HTTPClient: httpClient,
		}, nil)
		if err == nil {
			return session, nil
		}
		if !shouldRetry(ctx, attempt, retries) {
			return nil, err
		}
		if err := waitForRetry(ctx, retryDelay(attempt)); err != nil {
			return nil, err
		}
	}
}

func requestMetadata(cfg Config) map[string]string {
	metadata := make(map[string]string)
	if cfg.Region != nil && *cfg.Region != "" {
		metadata["AWS_REGION"] = *cfg.Region
	}
	for key, value := range value(cfg.Metadata) {
		metadata[key] = value
	}
	return metadata
}

func metadataMiddleware(metadata map[string]string) mcp.Middleware {
	return func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
			params := req.GetParams()
			if params == nil {
				return next(ctx, method, req)
			}
			existing := params.GetMeta()
			meta := make(map[string]any, len(metadata)+len(existing))
			for key, value := range metadata {
				meta[key] = value
			}
			for key, value := range existing {
				meta[key] = value
			}
			params.SetMeta(meta)
			return next(ctx, method, req)
		}
	}
}

func validateEndpoint(endpoint string) error {
	parsed, err := url.Parse(endpoint)
	if err != nil {
		return fmt.Errorf("invalid endpoint URL %q: %w", endpoint, err)
	}
	if parsed.Scheme == "" {
		return fmt.Errorf("invalid endpoint URL %q: missing URL scheme; use https:// for secure connections", endpoint)
	}
	if parsed.Host == "" {
		return fmt.Errorf("invalid endpoint URL %q: missing URL host", endpoint)
	}

	switch strings.ToLower(parsed.Scheme) {
	case "https":
		return nil
	case "http":
		if localEndpointHost(parsed.Hostname()) {
			return nil
		}
		return fmt.Errorf("invalid endpoint URL %q: HTTP is not allowed for remote endpoints; use https:// instead", endpoint)
	default:
		return fmt.Errorf("invalid endpoint URL %q: unsupported scheme %q; only https is supported for remote endpoints", endpoint, parsed.Scheme)
	}
}

func localEndpointHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	addr, err := netip.ParseAddr(host)
	return err == nil && addr.IsLoopback()
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

func (s *profileSessions) Get(ctx context.Context, profile string, run *proxyRun) (UpstreamSession, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cache != nil {
		if session := s.cache[profile]; session != nil {
			return session, nil
		}
	}
	params := s.params

	cfg := run.config
	cfg.Profiles = &[]string{profile}
	connector := run.connector
	if connector == nil {
		connector = mcpUpstreamConnector{HTTPClient: run.httpClient, Version: run.version}
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
