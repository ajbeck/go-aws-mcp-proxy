package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"reflect"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	defaultName          = "aws-mcp-proxy"
	defaultTitle         = "MCP Proxy for AWS"
	defaultInstructions  = "MCP Proxy for AWS provides access to SigV4 protected MCP servers through a single interface."
	jsonRPCServerClosing = -32004
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

	credentials credentialsProvider
}

// Run starts the proxy and blocks until the MCP server transport exits or the
// context is canceled.
func Run(ctx context.Context, cfg Config, options RunOptions) error {
	logger := options.Logger
	run := proxyRun{
		config:      cfg,
		connector:   options.Connector,
		credentials: options.credentials,
		httpClient:  options.HTTPClient,
		logger:      logger,
		transport:   options.Transport,
		version:     options.Version,
	}
	return run.run(ctx)
}

type proxyRun struct {
	config      Config
	connectMu   sync.Mutex
	connector   UpstreamConnector
	credentials credentialsProvider
	httpClient  *http.Client
	logger      *slog.Logger
	transport   mcp.Transport
	version     string

	server     *mcp.Server
	downstream activeDownstreamSessions
	profiles   profileSessions
	tools      upstreamTools
	upstream   upstreamState
}

func (r *proxyRun) run(ctx context.Context) error {
	server := r.newServer()
	r.server = server
	r.registerProxyStatusTool()
	server.AddReceivingMiddleware(r.initializeMiddleware(), r.ensureUpstreamMiddleware())

	transport := r.transport
	if transport == nil {
		transport = &mcp.StdioTransport{}
	}

	defer r.profiles.Close()
	defer r.upstream.Close()
	err := server.Run(ctx, transport)
	if isServerClosingError(err) {
		return nil
	}
	return err
}

func isServerClosingError(err error) bool {
	if errors.Is(err, mcp.ErrConnectionClosed) || errors.Is(err, io.ErrClosedPipe) {
		return true
	}
	for current := err; current != nil; current = errors.Unwrap(current) {
		encoded, marshalErr := json.Marshal(current)
		if marshalErr != nil {
			continue
		}
		var rpcError struct {
			Code int64 `json:"code"`
		}
		if json.Unmarshal(encoded, &rpcError) == nil && rpcError.Code == jsonRPCServerClosing {
			return true
		}
	}
	return false
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

			if enabled(r.config.LazyConnect) || deferredInitializeClient(params) {
				r.profiles.SetInitializeParams(params)
				if r.logger != nil {
					var clientName, clientVersion string
					if params.ClientInfo != nil {
						clientName = params.ClientInfo.Name
						clientVersion = params.ClientInfo.Version
					}
					r.logger.Info("deferring upstream connect for MCP client", "client_name", clientName, "client_version", clientVersion, "configured", enabled(r.config.LazyConnect))
				}
			} else {
				if _, err := r.ensureUpstreamReady(ctx, params); err != nil {
					return nil, classifyError(err)
				}
			}

			result, err := next(ctx, method, req)
			if err != nil {
				_ = r.upstream.Close()
				return nil, err
			}

			return result, nil
		}
	}
}

func (r *proxyRun) ensureUpstreamMiddleware() mcp.Middleware {
	return func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
			if method != "tools/list" && method != "tools/call" {
				return next(ctx, method, req)
			}
			upstream, err := r.ensureUpstreamReady(ctx, initializeParamsForRequest(req))
			if err != nil {
				return nil, classifyError(err)
			}
			if method == "tools/list" || r.shouldRefreshForToolCall(req) {
				if err := r.registerUpstreamTools(ctx, upstream); err != nil {
					return nil, classifyError(err)
				}
			}
			return next(ctx, method, req)
		}
	}
}

func (r *proxyRun) shouldRefreshForToolCall(req mcp.Request) bool {
	params, ok := req.GetParams().(*mcp.CallToolParamsRaw)
	return ok && params.Name != proxyStatusToolName && !r.tools.contains(params.Name)
}

// initializeParamsForRequest returns the client metadata for either supported
// MCP protocol era. Legacy clients provide it in the initialize request;
// modern clients provide it in each request's _meta field.
func initializeParamsForRequest(req mcp.Request) *mcp.InitializeParams {
	if params, ok := req.GetParams().(*mcp.InitializeParams); ok {
		return params
	}

	type clientMetadataRequest interface {
		ClientCapabilities() *mcp.ClientCapabilities
		ClientInfo() *mcp.Implementation
		ProtocolVersion() string
	}
	if request, ok := req.(clientMetadataRequest); ok {
		return &mcp.InitializeParams{
			Capabilities:    request.ClientCapabilities(),
			ClientInfo:      request.ClientInfo(),
			ProtocolVersion: request.ProtocolVersion(),
		}
	}

	if session, ok := req.GetSession().(*mcp.ServerSession); ok {
		return session.InitializeParams()
	}
	return nil
}

func (r *proxyRun) ensureUpstreamReady(ctx context.Context, params *mcp.InitializeParams) (UpstreamSession, error) {
	if upstream := r.upstream.Session(); upstream != nil && !degradedSession(upstream) {
		return upstream, nil
	}

	r.connectMu.Lock()
	defer r.connectMu.Unlock()
	if upstream := r.upstream.Session(); upstream != nil {
		if !degradedSession(upstream) {
			return upstream, nil
		}
		if err := r.upstream.Invalidate(upstream); err != nil && r.logger != nil {
			r.logger.Warn("failed to close degraded upstream session", "error", err)
		}
	}

	upstream, err := r.connectUpstream(ctx, params)
	if err != nil {
		return nil, err
	}
	if err := r.registerUpstreamTools(ctx, upstream); err != nil {
		_ = r.upstream.Close()
		return nil, err
	}
	return upstream, nil
}

func deferredInitializeClient(params *mcp.InitializeParams) bool {
	if params == nil || params.ClientInfo == nil {
		return false
	}
	name := strings.ToLower(params.ClientInfo.Name)
	return strings.Contains(name, "kiro cli") ||
		strings.Contains(name, "q dev cli") ||
		strings.Contains(name, "q developer cli")
}

func (r *proxyRun) registerUpstreamTools(ctx context.Context, upstream UpstreamSession) error {
	r.tools.lock()
	defer r.tools.unlock()

	result, err := r.discoverUpstreamTools(ctx, upstream)
	if err != nil {
		return err
	}
	if result == nil {
		return nil
	}

	desired := make(map[string]*mcp.Tool)
	for _, tool := range filterTools(result.Tools, enabled(r.config.ReadOnly)) {
		if tool == nil || tool.Name == "" {
			continue
		}
		desired[tool.Name] = r.prepareTool(tool)
	}

	var removed []string
	for name := range r.tools.registered {
		if desired[name] == nil {
			removed = append(removed, name)
		}
	}
	if len(removed) > 0 {
		r.server.RemoveTools(removed...)
	}

	changed := 0
	sessionChanged := r.tools.session != upstream
	for name, tool := range desired {
		if !sessionChanged && reflect.DeepEqual(r.tools.registered[name], tool) {
			continue
		}
		r.addTool(tool, upstream)
		changed++
	}
	r.tools.registered = desired
	r.tools.session = upstream
	if len(desired) > 0 {
		r.tools.discovered = true
	}
	if r.logger != nil {
		r.logger.Info("reconciled upstream tools", "count", len(desired), "added_or_updated", changed, "removed", len(removed), "read_only", enabled(r.config.ReadOnly))
	}
	return nil
}

func (r *proxyRun) discoverUpstreamTools(ctx context.Context, upstream UpstreamSession) (*mcp.ListToolsResult, error) {
	result, err := r.listUpstreamTools(ctx, upstream)
	if err != nil {
		return nil, err
	}
	if enabled(r.config.AllowEmptyTools) || r.tools.discovered || degradedSession(upstream) || !emptyToolList(result) {
		return result, nil
	}

	retries := retryCount(r.config.Retries)
	for attempt := 0; attempt < retries; attempt++ {
		if r.logger != nil {
			r.logger.Warn("retrying suspicious empty initial tools/list", "attempt", attempt, "next_attempt", attempt+1)
		}
		if err := waitForRetry(ctx, retryDelay(attempt, nil)); err != nil {
			return nil, err
		}
		result, err = r.listUpstreamTools(ctx, upstream)
		if err != nil {
			return nil, err
		}
		if !emptyToolList(result) {
			return result, nil
		}
	}
	return nil, newProxyError(
		categoryRetryable,
		reasonUpstreamEmptyTools,
		"the upstream MCP endpoint returned an empty initial tool list",
		"Retry after staggering concurrent proxy startup, or configure --allow-empty-tools if this endpoint intentionally has no tools.",
		fmt.Sprintf("tools/list remained empty after %d attempt(s)", retries+1),
		nil,
	)
}

func degradedSession(upstream UpstreamSession) bool {
	_, ok := upstream.(interface{ degradedError() error })
	return ok
}

func emptyToolList(result *mcp.ListToolsResult) bool {
	return result == nil || len(result.Tools) == 0
}

func (r *proxyRun) listUpstreamTools(ctx context.Context, upstream UpstreamSession) (*mcp.ListToolsResult, error) {
	var tools []*mcp.Tool
	seen := make(map[string]bool)
	params := &mcp.ListToolsParams{}
	for {
		result, err := r.listUpstreamToolsPage(ctx, upstream, params)
		if err != nil {
			return nil, err
		}
		if result == nil {
			if len(tools) == 0 {
				return nil, nil
			}
			return nil, fmt.Errorf("upstream tools/list returned a nil result while paginating")
		}
		tools = append(tools, result.Tools...)
		if result.NextCursor == "" {
			return &mcp.ListToolsResult{Tools: tools}, nil
		}
		if seen[result.NextCursor] {
			return nil, fmt.Errorf("upstream tools/list repeated cursor %q", result.NextCursor)
		}
		seen[result.NextCursor] = true
		params = &mcp.ListToolsParams{Cursor: result.NextCursor}
	}
}

func (r *proxyRun) listUpstreamToolsPage(ctx context.Context, upstream UpstreamSession, params *mcp.ListToolsParams) (*mcp.ListToolsResult, error) {
	retries := retryCount(r.config.Retries)
	for attempt := 0; ; attempt++ {
		result, err := upstream.ListTools(ctx, params)
		if err == nil {
			return result, nil
		}
		if !shouldRetry(ctx, attempt, retries, err) {
			return nil, err
		}
		if r.logger != nil {
			r.logger.Warn("retrying upstream tools/list", "attempt", attempt, "next_attempt", attempt+1, "error", err)
		}
		if err := waitForRetry(ctx, retryDelay(attempt, err)); err != nil {
			return nil, err
		}
	}
}

func (r *proxyRun) prepareTool(tool *mcp.Tool) *mcp.Tool {
	localTool := cloneTool(tool)
	localTool.InputSchema = normalizedInputSchema(localTool.InputSchema)
	profiles := value(r.config.Profiles)
	if len(profiles) > 0 && authRequiringTool(localTool.Name) {
		localTool.InputSchema = inputSchemaWithProfile(localTool.InputSchema, profiles)
	}
	if localTool.OutputSchema != nil && !schemaIsObject(localTool.OutputSchema) {
		localTool.OutputSchema = nil
	}
	return localTool
}

func (r *proxyRun) addTool(localTool *mcp.Tool, upstream UpstreamSession) {
	r.server.AddTool(localTool, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		r.downstream.begin(req.Session)
		defer r.downstream.end(req.Session)

		start := time.Now()
		callCtx := ctx
		cancel := func() {}
		if positiveDuration(r.config.ToolTimeout) {
			callCtx, cancel = context.WithTimeout(ctx, *r.config.ToolTimeout)
		}
		defer cancel()

		result, err := r.callUpstreamTool(callCtx, upstream, req)
		if err != nil {
			proxyErr := classifyError(err)
			if r.logger != nil {
				r.logger.Error("upstream tool call failed", "tool", req.Params.Name, "duration_ms", time.Since(start).Milliseconds(), "error_category", proxyErr.category, "error_reason", proxyErr.reason, "error", err)
			}
			return toolErrorResult(req.Params.Name, proxyErr), nil
		}
		if r.logger != nil {
			r.logger.Debug("upstream tool call completed", "tool", req.Params.Name, "duration_ms", time.Since(start).Milliseconds(), "is_error", result != nil && result.IsError)
		}
		return result, nil
	})
}

type upstreamTools struct {
	discovered bool
	mu         sync.Mutex
	registered map[string]*mcp.Tool
	session    UpstreamSession
}

type activeDownstreamSessions struct {
	mu       sync.Mutex
	refCount map[*mcp.ServerSession]int
}

func (s *activeDownstreamSessions) begin(session *mcp.ServerSession) {
	if session == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.refCount == nil {
		s.refCount = make(map[*mcp.ServerSession]int)
	}
	s.refCount[session]++
}

func (s *activeDownstreamSessions) end(session *mcp.ServerSession) {
	if session == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.refCount[session] <= 1 {
		delete(s.refCount, session)
		return
	}
	s.refCount[session]--
}

func (s *activeDownstreamSessions) session() (*mcp.ServerSession, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.refCount) != 1 {
		return nil, false
	}
	for session := range s.refCount {
		return session, true
	}
	return nil, false
}

func (t *upstreamTools) lock() {
	t.mu.Lock()
	if t.registered == nil {
		t.registered = make(map[string]*mcp.Tool)
	}
}

func (t *upstreamTools) unlock() {
	t.mu.Unlock()
}

func (t *upstreamTools) contains(name string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.registered[name] != nil
}

func (r *proxyRun) callUpstreamTool(ctx context.Context, upstream UpstreamSession, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args, profile, err := argumentsAndProfile(req.Params.Arguments)
	if err != nil {
		return nil, err
	}
	if profile == "" {
		return r.callDefaultSessionTool(ctx, upstream, forwardedToolParams(req, rawArguments(req.Params.Arguments)))
	}

	if !authRequiringTool(req.Params.Name) {
		if r.logger != nil {
			r.logger.Warn("ignoring aws_profile on non-auth tool", "tool", req.Params.Name)
		}
		return r.callDefaultSessionTool(ctx, upstream, forwardedToolParams(req, args))
	}

	profiles := value(r.config.Profiles)
	if !allowedProfile(profile, profiles) {
		return nil, newProxyError(
			categoryAgentFixable,
			reasonInvalidProfile,
			fmt.Sprintf("AWS profile %q is not in the allowed list", profile),
			"Retry with one of the allowed AWS profiles: "+profileList(profiles)+".",
			"",
			nil,
		)
	}
	defaultProfile := defaultProfile(r.config.Profiles)
	if defaultProfile != nil && profile == *defaultProfile {
		return r.callDefaultSessionTool(ctx, upstream, forwardedToolParams(req, args))
	}

	session, err := r.profiles.Get(ctx, profile, r)
	if err != nil {
		return nil, fmt.Errorf("failed to create connection for profile %q; check that the profile is configured and credentials are valid: %w", profile, err)
	}
	if r.logger != nil {
		r.logger.Info("routing tool call through profile override", "tool", req.Params.Name, "profile", profile)
	}
	result, err := r.callSessionTool(ctx, session, forwardedToolParams(req, args))
	if err != nil {
		if closeErr := r.profiles.Invalidate(profile, session); closeErr != nil && r.logger != nil {
			r.logger.Warn("failed to close invalidated profile session", "profile", profile, "error", closeErr)
		}
	}
	return result, err
}

func (r *proxyRun) callDefaultSessionTool(ctx context.Context, session UpstreamSession, params *mcp.CallToolParams) (*mcp.CallToolResult, error) {
	result, err := r.callSessionTool(ctx, session, params)
	if err != nil && invalidatesSession(err) {
		if closeErr := r.upstream.Invalidate(session); closeErr != nil && r.logger != nil {
			r.logger.Warn("failed to close invalidated upstream session", "error", closeErr)
		}
	}
	return result, err
}

func invalidatesSession(err error) bool {
	if errors.Is(err, mcp.ErrConnectionClosed) || errors.Is(err, mcp.ErrSessionMissing) {
		return true
	}
	if httpErr, ok := errors.AsType[*upstreamHTTPError](err); ok {
		return httpErr.statusCode == http.StatusUnauthorized || httpErr.statusCode == http.StatusForbidden
	}
	return false
}

func forwardedToolParams(req *mcp.CallToolRequest, arguments any) *mcp.CallToolParams {
	return &mcp.CallToolParams{
		Meta:           forwardedMeta(req.Params.Meta),
		Name:           req.Params.Name,
		Arguments:      arguments,
		InputResponses: req.Params.InputResponses,
		RequestState:   req.Params.RequestState,
	}
}

func forwardedMeta(meta mcp.Meta) mcp.Meta {
	if len(meta) == 0 {
		return nil
	}
	forwarded := make(mcp.Meta, len(meta))
	for key, value := range meta {
		forwarded[key] = value
	}
	delete(forwarded, mcp.MetaKeyProtocolVersion)
	delete(forwarded, mcp.MetaKeyClientInfo)
	delete(forwarded, mcp.MetaKeyClientCapabilities)
	if len(forwarded) == 0 {
		return nil
	}
	return forwarded
}

func (r *proxyRun) callSessionTool(ctx context.Context, session UpstreamSession, params *mcp.CallToolParams) (*mcp.CallToolResult, error) {
	return session.CallTool(ctx, params)
}

func retryCount(retries *int) int {
	if retries == nil {
		return 3
	}
	return *retries
}

func transportRetryCount(retries *int) int {
	count := retryCount(retries)
	if count == 0 {
		return -1
	}
	return count
}

func shouldRetry(ctx context.Context, attempt, retries int, err error) bool {
	return retries > 0 && attempt < retries && ctx.Err() == nil && retryableError(err)
}

func retryableError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, mcp.ErrConnectionClosed) ||
		errors.Is(err, mcp.ErrSessionMissing) ||
		errors.Is(err, io.EOF) ||
		errors.Is(err, io.ErrUnexpectedEOF) ||
		errors.Is(err, syscall.ECONNREFUSED) ||
		errors.Is(err, syscall.ECONNRESET) ||
		errors.Is(err, syscall.EPIPE) {
		return true
	}
	if httpErr, ok := errors.AsType[*upstreamHTTPError](err); ok {
		if httpErr.jsonrpcMessage != "" {
			return false
		}
		switch httpErr.statusCode {
		case http.StatusRequestTimeout,
			http.StatusTooManyRequests,
			http.StatusInternalServerError,
			http.StatusBadGateway,
			http.StatusServiceUnavailable,
			http.StatusGatewayTimeout:
			return true
		default:
			return false
		}
	}
	if netErr, ok := errors.AsType[net.Error](err); ok {
		return netErr.Timeout()
	}
	return false
}

const (
	initialRetryDelay = 100 * time.Millisecond
	maxBackoffDelay   = 2 * time.Second
)

func retryDelay(attempt int, err error) time.Duration {
	return retryDelayWithJitter(attempt, retryAfter(err), rand.Int64N)
}

func retryDelayWithJitter(attempt int, minimum time.Duration, jitter func(int64) int64) time.Duration {
	backoff := initialRetryDelay
	for range attempt {
		if backoff >= maxBackoffDelay {
			break
		}
		backoff *= 2
	}
	if backoff > maxBackoffDelay {
		backoff = maxBackoffDelay
	}

	half := backoff / 2
	delay := half + time.Duration(jitter(int64(backoff-half)+1))
	if minimum > delay {
		delay = minimum
	}
	if delay > maxRetryDelay {
		return maxRetryDelay
	}
	return delay
}

func retryAfter(err error) time.Duration {
	if httpErr, ok := errors.AsType[*upstreamHTTPError](err); ok {
		return httpErr.retryAfter
	}
	return 0
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
		connector = mcpUpstreamConnector{
			Credentials:        r.credentials,
			HTTPClient:         r.httpClient,
			Logger:             r.logger,
			Version:            r.version,
			ElicitationHandler: r.forwardElicitation,
			ToolListChangedHandler: func(notificationCtx context.Context, upstream UpstreamSession) {
				if err := r.registerUpstreamTools(notificationCtx, upstream); err != nil && r.logger != nil {
					r.logger.Warn("failed to reconcile upstream tool-list change", "error", err)
				}
			},
		}
	}

	session, err := connector.Connect(ctx, r.config, params)
	if err != nil {
		return nil, err
	}
	r.profiles.SetInitializeParams(params)
	var clientInfo *mcp.Implementation
	if params != nil {
		clientInfo = params.ClientInfo
	}
	r.upstream.Set(session, clientInfo)
	return session, nil
}

func (r *proxyRun) forwardElicitation(ctx context.Context, params *mcp.ElicitParams) (*mcp.ElicitResult, error) {
	session, ok := r.downstream.session()
	if !ok {
		return nil, fmt.Errorf("upstream requested elicitation without one active downstream session")
	}
	forwarded := *params
	return session.Elicit(ctx, &forwarded)
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
		return nil, "", newProxyError(
			categoryAgentFixable,
			reasonInvalidProfile,
			"aws_profile must be a non-empty string",
			"Retry the tool call with aws_profile set to one of the allowed profile names.",
			"",
			nil,
		)
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
	proxyErr := classifyError(err)
	return &mcp.CallToolResult{
		Content: []mcp.Content{
			&mcp.TextContent{Text: fmt.Sprintf("Tool call %q failed.\n%s", toolName, renderError(proxyErr))},
		},
		IsError: true,
	}
}

type mcpUpstreamConnector struct {
	Credentials            credentialsProvider
	ElicitationHandler     func(context.Context, *mcp.ElicitParams) (*mcp.ElicitResult, error)
	HTTPClient             *http.Client
	Logger                 *slog.Logger
	ToolListChangedHandler func(context.Context, UpstreamSession)
	Version                string
}

func (c mcpUpstreamConnector) Connect(ctx context.Context, cfg Config, params *mcp.InitializeParams) (UpstreamSession, error) {
	if cfg.Endpoint == nil {
		return nil, missingConfigError("endpoint", reasonMissingEndpoint)
	}
	endpoint := *cfg.Endpoint
	if err := validateEndpoint(endpoint); err != nil {
		return nil, err
	}

	version := c.Version
	if version == "" {
		version = "dev"
	}
	options := clientOptions{
		Credentials: c.Credentials,
		Logger:      c.Logger,
		Version:     version,
	}
	if params != nil && params.ClientInfo != nil {
		options.ClientName = params.ClientInfo.Name
		options.ClientVersion = params.ClientInfo.Version
	}
	var caBundle []byte
	if cfg.CaBundle != nil {
		var err error
		caBundle, err = readCABundle(*cfg.CaBundle)
		if err != nil {
			return nil, err
		}
	}
	mode, err := authModeFor(cfg)
	if err != nil {
		return nil, err
	}
	if mode == authModeRequired {
		if cfg.Service == nil {
			return nil, missingConfigError("service", reasonMissingService)
		}
		if cfg.Region == nil {
			return nil, missingConfigError("region", reasonMissingRegion)
		}
		credentials, err := signingCredentialsProvider(ctx, cfg, caBundle, options)
		if err != nil {
			return nil, err
		}
		if err := preflightSigningCredentials(ctx, credentials); err != nil {
			return degradedUpstreamSession{err: err}, nil
		}
		options.Credentials = credentials
	}
	httpClient, err := newClient(ctx, cfg, c.HTTPClient, options)
	if err != nil {
		return nil, err
	}

	clientOptions := &mcp.ClientOptions{
		Capabilities:   forwardedClientCapabilities(params),
		Logger:         c.Logger,
		MultiRoundTrip: &mcp.MultiRoundTripOptions{Disabled: true},
	}
	if c.ElicitationHandler != nil && supportsElicitation(params) {
		clientOptions.ElicitationHandler = func(ctx context.Context, req *mcp.ElicitRequest) (*mcp.ElicitResult, error) {
			return c.ElicitationHandler(ctx, req.Params)
		}
	}
	if c.ToolListChangedHandler != nil {
		clientOptions.ToolListChangedHandler = func(ctx context.Context, req *mcp.ToolListChangedRequest) {
			if upstream, ok := req.GetSession().(UpstreamSession); ok {
				c.ToolListChangedHandler(ctx, upstream)
			}
		}
	}
	client := mcp.NewClient(&mcp.Implementation{
		Name:    defaultName,
		Title:   defaultTitle,
		Version: version,
	}, clientOptions)
	if metadata := requestMetadata(cfg, metadataRegion(ctx, cfg, caBundle)); len(metadata) > 0 {
		client.AddSendingMiddleware(metadataMiddleware(metadata))
	}

	retries := retryCount(cfg.Retries)
	for attempt := 0; ; attempt++ {
		session, err := client.Connect(ctx, &mcp.StreamableClientTransport{
			Endpoint:   endpoint,
			HTTPClient: httpClient,
			MaxRetries: transportRetryCount(cfg.Retries),
		}, nil)
		if err == nil {
			return session, nil
		}
		if !shouldRetry(ctx, attempt, retries, err) {
			return nil, err
		}
		if err := waitForRetry(ctx, retryDelay(attempt, err)); err != nil {
			return nil, err
		}
	}
}

func forwardedClientCapabilities(params *mcp.InitializeParams) *mcp.ClientCapabilities {
	capabilities := &mcp.ClientCapabilities{}
	if params != nil && params.Capabilities != nil {
		capabilities.Elicitation = params.Capabilities.Elicitation
	}
	return capabilities
}

func supportsElicitation(params *mcp.InitializeParams) bool {
	return params != nil && params.Capabilities != nil && params.Capabilities.Elicitation != nil
}

func requestMetadata(cfg Config, resolvedRegion ...string) map[string]string {
	metadata := make(map[string]string)
	region := value(cfg.Region)
	if len(resolvedRegion) > 0 {
		region = resolvedRegion[0]
	}
	if region != "" {
		metadata["AWS_REGION"] = region
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
		return newProxyError(
			categoryConfiguration,
			reasonUnsafeEndpoint,
			fmt.Sprintf("endpoint URL %q is invalid", endpoint),
			"Ask the user to configure a valid https:// endpoint, or localhost http:// endpoint for local development.",
			"",
			err,
		)
	}
	if parsed.Scheme == "" {
		return newProxyError(
			categoryConfiguration,
			reasonUnsafeEndpoint,
			fmt.Sprintf("endpoint URL %q is missing a URL scheme", endpoint),
			"Ask the user to configure an https:// endpoint, or localhost http:// endpoint for local development.",
			"",
			nil,
		)
	}
	if parsed.Host == "" {
		return newProxyError(
			categoryConfiguration,
			reasonUnsafeEndpoint,
			fmt.Sprintf("endpoint URL %q is missing a URL host", endpoint),
			"Ask the user to configure a complete endpoint URL.",
			"",
			nil,
		)
	}

	switch strings.ToLower(parsed.Scheme) {
	case "https":
		return nil
	case "http":
		if localEndpointHost(parsed.Hostname()) {
			return nil
		}
		return newProxyError(
			categoryConfiguration,
			reasonUnsafeEndpoint,
			fmt.Sprintf("endpoint URL %q uses HTTP for a remote host", endpoint),
			"Ask the user to change the endpoint to https:// or use a localhost HTTP endpoint for local development.",
			"",
			nil,
		)
	default:
		return newProxyError(
			categoryConfiguration,
			reasonUnsafeEndpoint,
			fmt.Sprintf("endpoint URL %q uses unsupported scheme %q", endpoint, parsed.Scheme),
			"Ask the user to configure an https:// endpoint, or localhost http:// endpoint for local development.",
			"",
			nil,
		)
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

func (s *upstreamState) Session() UpstreamSession {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.session
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

func (s *upstreamState) Invalidate(failed UpstreamSession) error {
	s.mu.Lock()
	if s.session != failed {
		s.mu.Unlock()
		return nil
	}
	s.session = nil
	s.mu.Unlock()
	return failed.Close()
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

func (s *profileSessions) InitializeParams() *mcp.InitializeParams {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.params
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
		connector = mcpUpstreamConnector{
			Credentials:        run.credentials,
			ElicitationHandler: run.forwardElicitation,
			HTTPClient:         run.httpClient,
			Logger:             run.logger,
			Version:            run.version,
		}
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

// Invalidate removes a failed profile-specific session. The next tool call for
// the profile will establish a new connection, picking up refreshed credentials
// or a replacement upstream session. It deliberately does not retry the failed
// tool call.
func (s *profileSessions) Invalidate(profile string, failed UpstreamSession) error {
	s.mu.Lock()
	cached := s.cache[profile]
	if cached != failed {
		s.mu.Unlock()
		return nil
	}
	delete(s.cache, profile)
	s.mu.Unlock()
	return failed.Close()
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
