package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type fakeConnector struct {
	called            bool
	cfg               Config
	configs           []Config
	params            *mcp.InitializeParams
	err               error
	sess              *fakeSession
	sessionsByProfile map[string]*fakeSession
}

func (c *fakeConnector) Connect(_ context.Context, cfg Config, params *mcp.InitializeParams) (UpstreamSession, error) {
	c.called = true
	c.cfg = cfg
	c.configs = append(c.configs, cfg)
	c.params = params
	if c.err != nil {
		return nil, c.err
	}
	profiles := value(cfg.Profiles)
	if len(profiles) > 0 && c.sessionsByProfile != nil {
		if sess := c.sessionsByProfile[profiles[0]]; sess != nil {
			return sess, nil
		}
	}
	return c.sess, nil
}

type fakeSession struct {
	closed             bool
	result             *mcp.InitializeResult
	tools              []*mcp.Tool
	listCount          int
	listErrs           []error
	callCount          int
	callName           string
	callArgs           any
	callErr            error
	callErrs           []error
	callResult         *mcp.CallToolResult
	waitForContextDone bool
}

func (s *fakeSession) CallTool(ctx context.Context, params *mcp.CallToolParams) (*mcp.CallToolResult, error) {
	s.callCount++
	s.callName = params.Name
	s.callArgs = params.Arguments
	if s.waitForContextDone {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	if index := s.callCount - 1; index < len(s.callErrs) && s.callErrs[index] != nil {
		return nil, s.callErrs[index]
	}
	if s.callErr != nil {
		return nil, s.callErr
	}
	if s.callResult != nil {
		return s.callResult, nil
	}
	return &mcp.CallToolResult{
		Content: []mcp.Content{&mcp.TextContent{Text: "ok"}},
	}, ctx.Err()
}

func (s *fakeSession) Close() error {
	s.closed = true
	return nil
}

func (s *fakeSession) InitializeResult() *mcp.InitializeResult {
	return s.result
}

func (s *fakeSession) ListTools(context.Context, *mcp.ListToolsParams) (*mcp.ListToolsResult, error) {
	s.listCount++
	if index := s.listCount - 1; index < len(s.listErrs) && s.listErrs[index] != nil {
		return nil, s.listErrs[index]
	}
	return &mcp.ListToolsResult{Tools: s.tools}, nil
}

type headerRoundTripper struct {
	base  http.RoundTripper
	name  string
	value string
}

func (rt headerRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	req.Header.Set(rt.name, rt.value)
	return rt.base.RoundTrip(req)
}

func TestRunConnectsUpstreamDuringInitialize(t *testing.T) {
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	upstreamCaps := &mcp.ServerCapabilities{
		Tools:     &mcp.ToolCapabilities{ListChanged: true},
		Resources: &mcp.ResourceCapabilities{ListChanged: true},
	}
	session := &fakeSession{result: &mcp.InitializeResult{Capabilities: upstreamCaps}}
	connector := &fakeConnector{sess: session}
	options := RunOptions{
		Connector: connector,
		Transport: serverTransport,
		Version:   "test-version",
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	errs := make(chan error, 1)
	go func() {
		errs <- Run(ctx, Config{Endpoint: new("https://service.us-east-1.api.aws/mcp")}, options)
	}()

	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "1.0.0"}, nil)
	clientSession, err := client.Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatalf("client.Connect() error = %v", err)
	}

	if !connector.called {
		t.Fatal("upstream connector was not called")
	}
	if connector.cfg.Endpoint == nil || *connector.cfg.Endpoint != "https://service.us-east-1.api.aws/mcp" {
		t.Fatalf("connector endpoint = %#v", connector.cfg.Endpoint)
	}
	if connector.params == nil || connector.params.ClientInfo == nil {
		t.Fatalf("connector initialize params = %#v", connector.params)
	}
	if connector.params.ClientInfo.Name != "test-client" {
		t.Fatalf("client info name = %q", connector.params.ClientInfo.Name)
	}
	gotCaps := clientSession.InitializeResult().Capabilities
	if gotCaps == nil || gotCaps.Tools == nil || gotCaps.Resources == nil {
		t.Fatalf("capabilities were not replaced with upstream capabilities: %#v", gotCaps)
	}
	if !gotCaps.Tools.ListChanged || !gotCaps.Resources.ListChanged {
		t.Fatalf("unexpected capabilities: %#v", gotCaps)
	}

	if err := clientSession.Close(); err != nil {
		t.Fatalf("clientSession.Close() error = %v", err)
	}

	select {
	case err := <-errs:
		if err != nil {
			t.Fatalf("proxy run returned error = %v", err)
		}
	case <-ctx.Done():
		t.Fatal("proxy did not exit after client session closed")
	}
	if !session.closed {
		t.Fatal("upstream session was not closed")
	}
}

func TestRunOptionsHTTPClientIsUsedByDefaultConnector(t *testing.T) {
	const headerName = "X-Test-Proxy-Client"
	const headerValue = "custom"

	upstream := mcp.NewServer(&mcp.Implementation{Name: "upstream", Version: "1.0.0"}, nil)
	handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server {
		return upstream
	}, &mcp.StreamableHTTPOptions{JSONResponse: true})

	var sawCustomClient atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Header.Get(headerName) == headerValue {
			sawCustomClient.Store(true)
		}
		handler.ServeHTTP(w, req)
	}))
	t.Cleanup(server.Close)

	client := &http.Client{
		Transport: headerRoundTripper{
			base:  http.DefaultTransport,
			name:  headerName,
			value: headerValue,
		},
	}

	run := proxyRun{
		config: Config{
			Endpoint: new(server.URL),
			SkipAuth: new(true),
		},
		httpClient: client,
		version:    "test-version",
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	session, err := run.connectUpstream(ctx, &mcp.InitializeParams{
		ClientInfo: &mcp.Implementation{Name: "test-client", Version: "1.0.0"},
	})
	if err != nil {
		t.Fatalf("connectUpstream() error = %v", err)
	}
	if err := session.Close(); err != nil {
		t.Fatalf("session.Close() error = %v", err)
	}
	if !sawCustomClient.Load() {
		t.Fatal("upstream did not receive a request through the custom HTTP client")
	}
}

func TestDefaultConnectorRetriesInitialize(t *testing.T) {
	upstream := mcp.NewServer(&mcp.Implementation{Name: "upstream", Version: "1.0.0"}, nil)
	handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server {
		return upstream
	}, &mcp.StreamableHTTPOptions{JSONResponse: true})

	var requests atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if requests.Add(1) == 1 {
			http.Error(w, "temporary failure", http.StatusServiceUnavailable)
			return
		}
		handler.ServeHTTP(w, req)
	}))
	t.Cleanup(server.Close)

	session, err := mcpUpstreamConnector{}.Connect(t.Context(), Config{
		Endpoint: new(server.URL),
		Retries:  new(1),
		SkipAuth: new(true),
	}, &mcp.InitializeParams{
		ClientInfo: &mcp.Implementation{Name: "test-client", Version: "1.0.0"},
	})
	if err != nil {
		t.Fatalf("Connect() error = %v", err)
	}
	if err := session.Close(); err != nil {
		t.Fatalf("session.Close() error = %v", err)
	}
	if requests.Load() < 2 {
		t.Fatalf("upstream requests = %d, want at least 2", requests.Load())
	}
}

func TestDefaultConnectorRequiresEndpoint(t *testing.T) {
	_, err := mcpUpstreamConnector{}.Connect(t.Context(), Config{
		SkipAuth: new(true),
	}, nil)
	if err == nil || !strings.Contains(err.Error(), "endpoint is required") {
		t.Fatalf("Connect() error = %v, want endpoint required", err)
	}
}

func TestRegisterUpstreamToolsRetriesListTools(t *testing.T) {
	upstream := &fakeSession{
		listErrs: []error{errors.New("temporary tools/list failure")},
		tools:    []*mcp.Tool{{Name: "aws___search_documentation", InputSchema: map[string]any{"type": "object"}}},
	}
	run := proxyRun{
		config: Config{
			Retries: new(1),
		},
		server: mcp.NewServer(&mcp.Implementation{Name: "proxy", Version: "test"}, nil),
	}

	if err := run.registerUpstreamTools(t.Context(), upstream); err != nil {
		t.Fatalf("registerUpstreamTools() error = %v", err)
	}
	if upstream.listCount != 2 {
		t.Fatalf("ListTools count = %d, want 2", upstream.listCount)
	}
}

func TestRunRegistersAndForwardsUpstreamTools(t *testing.T) {
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	upstreamResult := &mcp.CallToolResult{
		Content: []mcp.Content{&mcp.TextContent{Text: "forwarded"}},
	}
	session := &fakeSession{
		result: &mcp.InitializeResult{Capabilities: &mcp.ServerCapabilities{Tools: &mcp.ToolCapabilities{ListChanged: true}}},
		tools: []*mcp.Tool{
			{
				Name:        "aws___call_aws",
				Description: "Call AWS APIs.",
				InputSchema: map[string]any{
					"type": "object",
				},
			},
		},
		callResult: upstreamResult,
	}
	options := RunOptions{
		Connector: &fakeConnector{sess: session},
		Transport: serverTransport,
		Version:   "test-version",
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	errs := make(chan error, 1)
	go func() {
		errs <- Run(ctx, Config{
			Endpoint:    new("https://service.us-east-1.api.aws/mcp"),
			ToolTimeout: new(5 * time.Second),
		}, options)
	}()

	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "1.0.0"}, nil)
	clientSession, err := client.Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatalf("client.Connect() error = %v", err)
	}

	tools, err := clientSession.ListTools(ctx, &mcp.ListToolsParams{})
	if err != nil {
		t.Fatalf("ListTools() error = %v", err)
	}
	if len(tools.Tools) != 1 || tools.Tools[0].Name != "aws___call_aws" {
		t.Fatalf("tools = %#v", tools.Tools)
	}

	result, err := clientSession.CallTool(ctx, &mcp.CallToolParams{
		Name:      "aws___call_aws",
		Arguments: map[string]any{"service": "s3"},
	})
	if err != nil {
		t.Fatalf("CallTool() error = %v", err)
	}
	if result.IsError || len(result.Content) != 1 {
		t.Fatalf("result = %#v", result)
	}
	text, ok := result.Content[0].(*mcp.TextContent)
	if !ok || text.Text != "forwarded" {
		t.Fatalf("content = %#v", result.Content)
	}
	if session.callName != "aws___call_aws" {
		t.Fatalf("upstream call name = %q", session.callName)
	}
	var args map[string]any
	raw, ok := session.callArgs.(json.RawMessage)
	if !ok {
		t.Fatalf("upstream call args type = %T", session.callArgs)
	}
	if err := json.Unmarshal(raw, &args); err != nil {
		t.Fatalf("unmarshal call args: %v", err)
	}
	if args["service"] != "s3" {
		t.Fatalf("upstream call args = %#v", args)
	}

	if err := clientSession.Close(); err != nil {
		t.Fatalf("clientSession.Close() error = %v", err)
	}
	waitForProxyRunExit(t, ctx, errs)
}

func TestRunInjectsAWSProfileIntoAuthToolSchema(t *testing.T) {
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	session := &fakeSession{
		result: &mcp.InitializeResult{Capabilities: &mcp.ServerCapabilities{Tools: &mcp.ToolCapabilities{ListChanged: true}}},
		tools: []*mcp.Tool{
			{Name: "aws___call_aws", InputSchema: map[string]any{"type": "object", "properties": map[string]any{}}},
			{Name: "aws___search_documentation", InputSchema: map[string]any{"type": "object", "properties": map[string]any{}}},
		},
	}
	options := RunOptions{Connector: &fakeConnector{sess: session}, Transport: serverTransport, Version: "test-version"}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	errs := make(chan error, 1)
	go func() {
		errs <- Run(ctx, Config{
			Endpoint: new("https://service.us-east-1.api.aws/mcp"),
			Profiles: new([]string{"default", "dev"}),
		}, options)
	}()

	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "1.0.0"}, nil)
	clientSession, err := client.Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatalf("client.Connect() error = %v", err)
	}

	tools, err := clientSession.ListTools(ctx, &mcp.ListToolsParams{})
	if err != nil {
		t.Fatalf("ListTools() error = %v", err)
	}
	authTool := findTool(tools.Tools, "aws___call_aws")
	if authTool == nil {
		t.Fatal("auth tool not found")
	}
	profileSchema := schemaProperty(t, authTool.InputSchema, "aws_profile")
	enum, ok := profileSchema["enum"].([]any)
	if !ok || len(enum) != 2 || enum[0] != "default" || enum[1] != "dev" {
		t.Fatalf("aws_profile enum = %#v", profileSchema["enum"])
	}
	docsTool := findTool(tools.Tools, "aws___search_documentation")
	if docsTool == nil {
		t.Fatal("docs tool not found")
	}
	if propertyExists(docsTool.InputSchema, "aws_profile") {
		t.Fatal("aws_profile was injected into a non-auth tool")
	}

	if err := clientSession.Close(); err != nil {
		t.Fatalf("clientSession.Close() error = %v", err)
	}
	waitForProxyRunExit(t, ctx, errs)
}

func TestRunRoutesAWSProfileOverrideToDedicatedSession(t *testing.T) {
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	defaultSession := &fakeSession{
		result: &mcp.InitializeResult{Capabilities: &mcp.ServerCapabilities{Tools: &mcp.ToolCapabilities{ListChanged: true}}},
		tools:  []*mcp.Tool{{Name: "aws___call_aws", InputSchema: map[string]any{"type": "object"}}},
	}
	devSession := &fakeSession{
		result:     defaultSession.result,
		callResult: &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "dev"}}},
	}
	connector := &fakeConnector{
		sess: defaultSession,
		sessionsByProfile: map[string]*fakeSession{
			"dev": devSession,
		},
	}
	options := RunOptions{Connector: connector, Transport: serverTransport, Version: "test-version"}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	errs := make(chan error, 1)
	go func() {
		errs <- Run(ctx, Config{
			Endpoint: new("https://service.us-east-1.api.aws/mcp"),
			Profiles: new([]string{"default", "dev"}),
		}, options)
	}()

	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "1.0.0"}, nil)
	clientSession, err := client.Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatalf("client.Connect() error = %v", err)
	}

	result, err := clientSession.CallTool(ctx, &mcp.CallToolParams{
		Name: "aws___call_aws",
		Arguments: map[string]any{
			"cli_command": "aws sts get-caller-identity",
			"aws_profile": "dev",
		},
	})
	if err != nil {
		t.Fatalf("CallTool() error = %v", err)
	}
	if result.IsError {
		t.Fatalf("result = %#v", result)
	}
	if defaultSession.callName != "" {
		t.Fatalf("default session was called: %q", defaultSession.callName)
	}
	if devSession.callName != "aws___call_aws" {
		t.Fatalf("dev session call name = %q", devSession.callName)
	}
	args := decodeRawArgs(t, devSession.callArgs)
	if args["cli_command"] != "aws sts get-caller-identity" {
		t.Fatalf("dev call args = %#v", args)
	}
	if _, ok := args["aws_profile"]; ok {
		t.Fatalf("aws_profile was forwarded: %#v", args)
	}
	if len(connector.configs) != 2 || strings.Join(value(connector.configs[1].Profiles), ",") != "dev" {
		t.Fatalf("connector configs = %#v", connector.configs)
	}

	if err := clientSession.Close(); err != nil {
		t.Fatalf("clientSession.Close() error = %v", err)
	}
	waitForProxyRunExit(t, ctx, errs)
	if !devSession.closed {
		t.Fatal("profile override session was not closed")
	}
}

func TestRunRoutesDefaultAWSProfileThroughDefaultSession(t *testing.T) {
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	defaultSession := &fakeSession{
		result: &mcp.InitializeResult{Capabilities: &mcp.ServerCapabilities{Tools: &mcp.ToolCapabilities{ListChanged: true}}},
		tools:  []*mcp.Tool{{Name: "aws___call_aws", InputSchema: map[string]any{"type": "object"}}},
	}
	connector := &fakeConnector{sess: defaultSession}
	options := RunOptions{Connector: connector, Transport: serverTransport, Version: "test-version"}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	errs := make(chan error, 1)
	go func() {
		errs <- Run(ctx, Config{
			Endpoint: new("https://service.us-east-1.api.aws/mcp"),
			Profiles: new([]string{"default", "dev"}),
		}, options)
	}()

	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "1.0.0"}, nil)
	clientSession, err := client.Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatalf("client.Connect() error = %v", err)
	}

	_, err = clientSession.CallTool(ctx, &mcp.CallToolParams{
		Name: "aws___call_aws",
		Arguments: map[string]any{
			"cli_command": "aws s3 ls",
			"aws_profile": "default",
		},
	})
	if err != nil {
		t.Fatalf("CallTool() error = %v", err)
	}
	args := decodeRawArgs(t, defaultSession.callArgs)
	if args["cli_command"] != "aws s3 ls" {
		t.Fatalf("default call args = %#v", args)
	}
	if _, ok := args["aws_profile"]; ok {
		t.Fatalf("aws_profile was forwarded: %#v", args)
	}
	if len(connector.configs) != 1 {
		t.Fatalf("created unexpected profile session configs: %#v", connector.configs)
	}

	if err := clientSession.Close(); err != nil {
		t.Fatalf("clientSession.Close() error = %v", err)
	}
	waitForProxyRunExit(t, ctx, errs)
}

func TestCallSessionToolRetriesTransientErrors(t *testing.T) {
	session := &fakeSession{
		callErrs: []error{errors.New("temporary failure")},
		callResult: &mcp.CallToolResult{
			Content: []mcp.Content{&mcp.TextContent{Text: "retried"}},
		},
	}
	run := proxyRun{
		config: Config{
			Retries: new(1),
		},
	}

	result, err := run.callSessionTool(t.Context(), session, &mcp.CallToolParams{Name: "aws___call_aws"})
	if err != nil {
		t.Fatalf("callSessionTool() error = %v", err)
	}
	if session.callCount != 2 {
		t.Fatalf("CallTool count = %d, want 2", session.callCount)
	}
	text, ok := result.Content[0].(*mcp.TextContent)
	if !ok || text.Text != "retried" {
		t.Fatalf("result content = %#v", result.Content)
	}
}

func TestRetryCountDefaultsToThreeAndAllowsDisable(t *testing.T) {
	if got := retryCount(nil); got != 3 {
		t.Fatalf("retryCount(nil) = %d, want 3", got)
	}
	if got := retryCount(new(0)); got != 0 {
		t.Fatalf("retryCount(0) = %d, want 0", got)
	}
}

func TestMetadataMiddlewareAddsRequestMetadata(t *testing.T) {
	handler := metadataMiddleware(map[string]string{
		"AWS_REGION": "us-east-1",
		"team":       "platform",
	})(func(_ context.Context, _ string, req mcp.Request) (mcp.Result, error) {
		params := req.GetParams().(*mcp.CallToolParams)
		if params.Meta["AWS_REGION"] != "us-east-1" {
			t.Fatalf("AWS_REGION metadata = %#v", params.Meta["AWS_REGION"])
		}
		if params.Meta["team"] != "client" {
			t.Fatalf("team metadata = %#v", params.Meta["team"])
		}
		return nil, nil
	})

	params := &mcp.CallToolParams{
		Name: "aws___call_aws",
		Meta: mcp.Meta{
			"team": "client",
		},
	}
	if _, err := handler(t.Context(), "tools/call", &mcp.ClientRequest[*mcp.CallToolParams]{Params: params}); err != nil {
		t.Fatalf("metadata middleware handler error = %v", err)
	}
}

func TestRunRejectsDisallowedAWSProfile(t *testing.T) {
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	session := &fakeSession{
		result: &mcp.InitializeResult{Capabilities: &mcp.ServerCapabilities{Tools: &mcp.ToolCapabilities{ListChanged: true}}},
		tools:  []*mcp.Tool{{Name: "aws___call_aws", InputSchema: map[string]any{"type": "object"}}},
	}
	options := RunOptions{Connector: &fakeConnector{sess: session}, Transport: serverTransport, Version: "test-version"}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	errs := make(chan error, 1)
	go func() {
		errs <- Run(ctx, Config{
			Endpoint: new("https://service.us-east-1.api.aws/mcp"),
			Profiles: new([]string{"default", "dev"}),
		}, options)
	}()

	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "1.0.0"}, nil)
	clientSession, err := client.Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatalf("client.Connect() error = %v", err)
	}

	result, err := clientSession.CallTool(ctx, &mcp.CallToolParams{
		Name:      "aws___call_aws",
		Arguments: map[string]any{"aws_profile": "prod"},
	})
	if err != nil {
		t.Fatalf("CallTool() protocol error = %v", err)
	}
	if !result.IsError {
		t.Fatalf("IsError = false, result = %#v", result)
	}
	text, ok := result.Content[0].(*mcp.TextContent)
	if !ok || !strings.Contains(text.Text, "not in the allowed list") {
		t.Fatalf("content = %#v", result.Content)
	}

	if err := clientSession.Close(); err != nil {
		t.Fatalf("clientSession.Close() error = %v", err)
	}
	waitForProxyRunExit(t, ctx, errs)
}

func TestRunStripsAWSProfileFromNonAuthTool(t *testing.T) {
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	session := &fakeSession{
		result: &mcp.InitializeResult{Capabilities: &mcp.ServerCapabilities{Tools: &mcp.ToolCapabilities{ListChanged: true}}},
		tools:  []*mcp.Tool{{Name: "aws___search_documentation", InputSchema: map[string]any{"type": "object"}}},
	}
	connector := &fakeConnector{sess: session}
	options := RunOptions{Connector: connector, Transport: serverTransport, Version: "test-version"}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	errs := make(chan error, 1)
	go func() {
		errs <- Run(ctx, Config{
			Endpoint: new("https://service.us-east-1.api.aws/mcp"),
			Profiles: new([]string{"default", "dev"}),
		}, options)
	}()

	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "1.0.0"}, nil)
	clientSession, err := client.Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatalf("client.Connect() error = %v", err)
	}

	_, err = clientSession.CallTool(ctx, &mcp.CallToolParams{
		Name: "aws___search_documentation",
		Arguments: map[string]any{
			"search_phrase": "s3",
			"aws_profile":   "dev",
		},
	})
	if err != nil {
		t.Fatalf("CallTool() error = %v", err)
	}
	args := decodeRawArgs(t, session.callArgs)
	if args["search_phrase"] != "s3" {
		t.Fatalf("call args = %#v", args)
	}
	if _, ok := args["aws_profile"]; ok {
		t.Fatalf("aws_profile was forwarded: %#v", args)
	}
	if len(connector.configs) != 1 {
		t.Fatalf("created unexpected profile session configs: %#v", connector.configs)
	}

	if err := clientSession.Close(); err != nil {
		t.Fatalf("clientSession.Close() error = %v", err)
	}
	waitForProxyRunExit(t, ctx, errs)
}

func TestRunFiltersReadOnlyTools(t *testing.T) {
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	session := &fakeSession{
		result: &mcp.InitializeResult{Capabilities: &mcp.ServerCapabilities{Tools: &mcp.ToolCapabilities{ListChanged: true}}},
		tools: []*mcp.Tool{
			{
				Name:        "read",
				InputSchema: map[string]any{"type": "object"},
				Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
			},
			{
				Name:        "write",
				InputSchema: map[string]any{"type": "object"},
				Annotations: &mcp.ToolAnnotations{ReadOnlyHint: false},
			},
			{
				Name:        "unknown",
				InputSchema: map[string]any{"type": "object"},
			},
		},
	}
	options := RunOptions{
		Connector: &fakeConnector{sess: session},
		Transport: serverTransport,
		Version:   "test-version",
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	errs := make(chan error, 1)
	go func() {
		errs <- Run(ctx, Config{
			Endpoint: new("https://service.us-east-1.api.aws/mcp"),
			ReadOnly: new(true),
		}, options)
	}()

	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "1.0.0"}, nil)
	clientSession, err := client.Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatalf("client.Connect() error = %v", err)
	}

	tools, err := clientSession.ListTools(ctx, &mcp.ListToolsParams{})
	if err != nil {
		t.Fatalf("ListTools() error = %v", err)
	}
	if len(tools.Tools) != 1 || tools.Tools[0].Name != "read" {
		t.Fatalf("tools = %#v", tools.Tools)
	}

	if err := clientSession.Close(); err != nil {
		t.Fatalf("clientSession.Close() error = %v", err)
	}
	waitForProxyRunExit(t, ctx, errs)
}

func TestRunReturnsToolVisibleErrorOnUpstreamCallFailure(t *testing.T) {
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	session := &fakeSession{
		result:  &mcp.InitializeResult{Capabilities: &mcp.ServerCapabilities{Tools: &mcp.ToolCapabilities{ListChanged: true}}},
		tools:   []*mcp.Tool{{Name: "failing", InputSchema: map[string]any{"type": "object"}}},
		callErr: errors.New("upstream unavailable"),
	}
	options := RunOptions{
		Connector: &fakeConnector{sess: session},
		Transport: serverTransport,
		Version:   "test-version",
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	errs := make(chan error, 1)
	go func() {
		errs <- Run(ctx, Config{Endpoint: new("https://service.us-east-1.api.aws/mcp")}, options)
	}()

	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "1.0.0"}, nil)
	clientSession, err := client.Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatalf("client.Connect() error = %v", err)
	}

	result, err := clientSession.CallTool(ctx, &mcp.CallToolParams{Name: "failing"})
	if err != nil {
		t.Fatalf("CallTool() protocol error = %v", err)
	}
	if !result.IsError {
		t.Fatalf("IsError = false, result = %#v", result)
	}
	if len(result.Content) != 1 {
		t.Fatalf("content = %#v", result.Content)
	}
	text, ok := result.Content[0].(*mcp.TextContent)
	if !ok || !strings.Contains(text.Text, "upstream unavailable") {
		t.Fatalf("content = %#v", result.Content)
	}

	if err := clientSession.Close(); err != nil {
		t.Fatalf("clientSession.Close() error = %v", err)
	}
	waitForProxyRunExit(t, ctx, errs)
}

func TestRunAppliesToolTimeout(t *testing.T) {
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	session := &fakeSession{
		result: &mcp.InitializeResult{Capabilities: &mcp.ServerCapabilities{Tools: &mcp.ToolCapabilities{ListChanged: true}}},
		tools: []*mcp.Tool{
			{Name: "slow", InputSchema: map[string]any{"type": "object"}},
		},
		waitForContextDone: true,
	}
	options := RunOptions{
		Connector: &fakeConnector{sess: session},
		Transport: serverTransport,
		Version:   "test-version",
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	errs := make(chan error, 1)
	go func() {
		errs <- Run(ctx, Config{
			Endpoint:    new("https://service.us-east-1.api.aws/mcp"),
			ToolTimeout: new(time.Millisecond),
		}, options)
	}()

	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "1.0.0"}, nil)
	clientSession, err := client.Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatalf("client.Connect() error = %v", err)
	}

	result, err := clientSession.CallTool(ctx, &mcp.CallToolParams{Name: "slow"})
	if err != nil {
		t.Fatalf("CallTool() protocol error = %v", err)
	}
	if !result.IsError {
		t.Fatalf("IsError = false, result = %#v", result)
	}
	text, ok := result.Content[0].(*mcp.TextContent)
	if !ok || !strings.Contains(text.Text, "context deadline exceeded") {
		t.Fatalf("content = %#v", result.Content)
	}

	if err := clientSession.Close(); err != nil {
		t.Fatalf("clientSession.Close() error = %v", err)
	}
	waitForProxyRunExit(t, ctx, errs)
}

func TestRunReturnsUpstreamConnectErrorDuringInitialize(t *testing.T) {
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	wantErr := errors.New("connect failed")
	connector := &fakeConnector{err: wantErr}
	options := RunOptions{
		Connector: connector,
		Transport: serverTransport,
		Version:   "test-version",
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	errs := make(chan error, 1)
	go func() {
		errs <- Run(ctx, Config{Endpoint: new("https://service.us-east-1.api.aws/mcp")}, options)
	}()

	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "1.0.0"}, nil)
	_, err := client.Connect(ctx, clientTransport, nil)
	if err == nil {
		t.Fatal("client.Connect() error = nil")
	}
	if !connector.called {
		t.Fatal("upstream connector was not called")
	}

	cancel()
	select {
	case <-errs:
	case <-time.After(5 * time.Second):
		t.Fatal("proxy did not exit after context cancellation")
	}
}

func TestApplyUpstreamCapabilitiesKeepsLocalWhenUpstreamCapabilitiesMissing(t *testing.T) {
	localCaps := &mcp.ServerCapabilities{Tools: &mcp.ToolCapabilities{ListChanged: true}}
	local := &mcp.InitializeResult{Capabilities: localCaps}

	applyUpstreamCapabilities(local, &mcp.InitializeResult{})

	if local.Capabilities != localCaps {
		t.Fatal("local capabilities changed")
	}
}

func waitForProxyRunExit(t *testing.T, ctx context.Context, errs <-chan error) {
	t.Helper()

	select {
	case err := <-errs:
		if err != nil {
			t.Fatalf("proxy run returned error = %v", err)
		}
	case <-ctx.Done():
		t.Fatal("proxy did not exit after client session closed")
	}
}

func findTool(tools []*mcp.Tool, name string) *mcp.Tool {
	for _, tool := range tools {
		if tool != nil && tool.Name == name {
			return tool
		}
	}
	return nil
}

func schemaProperty(t *testing.T, schema any, name string) map[string]any {
	t.Helper()

	object, ok := schema.(map[string]any)
	if !ok {
		t.Fatalf("schema type = %T", schema)
	}
	properties, ok := object["properties"].(map[string]any)
	if !ok {
		t.Fatalf("schema properties = %#v", object["properties"])
	}
	property, ok := properties[name].(map[string]any)
	if !ok {
		t.Fatalf("schema property %q = %#v", name, properties[name])
	}
	return property
}

func propertyExists(schema any, name string) bool {
	object, ok := schema.(map[string]any)
	if !ok {
		return false
	}
	properties, ok := object["properties"].(map[string]any)
	if !ok {
		return false
	}
	_, ok = properties[name]
	return ok
}

func decodeRawArgs(t *testing.T, args any) map[string]any {
	t.Helper()

	raw, ok := args.(json.RawMessage)
	if !ok {
		t.Fatalf("args type = %T", args)
	}
	var decoded map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("unmarshal args: %v", err)
	}
	return decoded
}
