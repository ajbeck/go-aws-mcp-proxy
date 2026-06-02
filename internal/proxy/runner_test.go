package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
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
	if len(cfg.Profiles) > 0 && c.sessionsByProfile != nil {
		if sess := c.sessionsByProfile[cfg.Profiles[0]]; sess != nil {
			return sess, nil
		}
	}
	return c.sess, nil
}

type fakeSession struct {
	closed             bool
	result             *mcp.InitializeResult
	tools              []*mcp.Tool
	callName           string
	callArgs           any
	callErr            error
	callResult         *mcp.CallToolResult
	waitForContextDone bool
}

func (s *fakeSession) CallTool(ctx context.Context, params *mcp.CallToolParams) (*mcp.CallToolResult, error) {
	s.callName = params.Name
	s.callArgs = params.Arguments
	if s.waitForContextDone {
		<-ctx.Done()
		return nil, ctx.Err()
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
	return &mcp.ListToolsResult{Tools: s.tools}, nil
}

func TestRuntimeConnectsUpstreamDuringInitialize(t *testing.T) {
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	upstreamCaps := &mcp.ServerCapabilities{
		Tools:     &mcp.ToolCapabilities{ListChanged: true},
		Resources: &mcp.ResourceCapabilities{ListChanged: true},
	}
	session := &fakeSession{result: &mcp.InitializeResult{Capabilities: upstreamCaps}}
	connector := &fakeConnector{sess: session}
	runner := Runner{
		Connector: connector,
		Transport: serverTransport,
		Version:   "test-version",
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	errs := make(chan error, 1)
	go func() {
		errs <- runner.RunProxy(ctx, Config{Endpoint: "https://service.us-east-1.api.aws/mcp"}, nil)
	}()

	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "1.0.0"}, nil)
	clientSession, err := client.Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatalf("client.Connect() error = %v", err)
	}

	if !connector.called {
		t.Fatal("upstream connector was not called")
	}
	if connector.cfg.Endpoint != "https://service.us-east-1.api.aws/mcp" {
		t.Fatalf("connector endpoint = %q", connector.cfg.Endpoint)
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
			t.Fatalf("runner returned error = %v", err)
		}
	case <-ctx.Done():
		t.Fatal("runner did not exit after client session closed")
	}
	if !session.closed {
		t.Fatal("upstream session was not closed")
	}
}

func TestRuntimeRegistersAndForwardsUpstreamTools(t *testing.T) {
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
	runner := Runner{
		Connector: &fakeConnector{sess: session},
		Transport: serverTransport,
		Version:   "test-version",
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	errs := make(chan error, 1)
	go func() {
		errs <- runner.RunProxy(ctx, Config{
			Endpoint:    "https://service.us-east-1.api.aws/mcp",
			ToolTimeout: 5 * time.Second,
		}, nil)
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
	waitForRunnerExit(t, ctx, errs)
}

func TestRuntimeInjectsAWSProfileIntoAuthToolSchema(t *testing.T) {
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	session := &fakeSession{
		result: &mcp.InitializeResult{Capabilities: &mcp.ServerCapabilities{Tools: &mcp.ToolCapabilities{ListChanged: true}}},
		tools: []*mcp.Tool{
			{Name: "aws___call_aws", InputSchema: map[string]any{"type": "object", "properties": map[string]any{}}},
			{Name: "aws___search_documentation", InputSchema: map[string]any{"type": "object", "properties": map[string]any{}}},
		},
	}
	runner := Runner{Connector: &fakeConnector{sess: session}, Transport: serverTransport, Version: "test-version"}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	errs := make(chan error, 1)
	go func() {
		errs <- runner.RunProxy(ctx, Config{
			Endpoint: "https://service.us-east-1.api.aws/mcp",
			Profiles: []string{"default", "dev"},
		}, nil)
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
	waitForRunnerExit(t, ctx, errs)
}

func TestRuntimeRoutesAWSProfileOverrideToDedicatedSession(t *testing.T) {
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
	runner := Runner{Connector: connector, Transport: serverTransport, Version: "test-version"}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	errs := make(chan error, 1)
	go func() {
		errs <- runner.RunProxy(ctx, Config{
			Endpoint: "https://service.us-east-1.api.aws/mcp",
			Profiles: []string{"default", "dev"},
		}, nil)
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
	if len(connector.configs) != 2 || strings.Join(connector.configs[1].Profiles, ",") != "dev" {
		t.Fatalf("connector configs = %#v", connector.configs)
	}

	if err := clientSession.Close(); err != nil {
		t.Fatalf("clientSession.Close() error = %v", err)
	}
	waitForRunnerExit(t, ctx, errs)
	if !devSession.closed {
		t.Fatal("profile override session was not closed")
	}
}

func TestRuntimeRoutesDefaultAWSProfileThroughDefaultSession(t *testing.T) {
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	defaultSession := &fakeSession{
		result: &mcp.InitializeResult{Capabilities: &mcp.ServerCapabilities{Tools: &mcp.ToolCapabilities{ListChanged: true}}},
		tools:  []*mcp.Tool{{Name: "aws___call_aws", InputSchema: map[string]any{"type": "object"}}},
	}
	connector := &fakeConnector{sess: defaultSession}
	runner := Runner{Connector: connector, Transport: serverTransport, Version: "test-version"}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	errs := make(chan error, 1)
	go func() {
		errs <- runner.RunProxy(ctx, Config{
			Endpoint: "https://service.us-east-1.api.aws/mcp",
			Profiles: []string{"default", "dev"},
		}, nil)
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
	waitForRunnerExit(t, ctx, errs)
}

func TestRuntimeRejectsDisallowedAWSProfile(t *testing.T) {
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	session := &fakeSession{
		result: &mcp.InitializeResult{Capabilities: &mcp.ServerCapabilities{Tools: &mcp.ToolCapabilities{ListChanged: true}}},
		tools:  []*mcp.Tool{{Name: "aws___call_aws", InputSchema: map[string]any{"type": "object"}}},
	}
	runner := Runner{Connector: &fakeConnector{sess: session}, Transport: serverTransport, Version: "test-version"}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	errs := make(chan error, 1)
	go func() {
		errs <- runner.RunProxy(ctx, Config{
			Endpoint: "https://service.us-east-1.api.aws/mcp",
			Profiles: []string{"default", "dev"},
		}, nil)
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
	waitForRunnerExit(t, ctx, errs)
}

func TestRuntimeStripsAWSProfileFromNonAuthTool(t *testing.T) {
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	session := &fakeSession{
		result: &mcp.InitializeResult{Capabilities: &mcp.ServerCapabilities{Tools: &mcp.ToolCapabilities{ListChanged: true}}},
		tools:  []*mcp.Tool{{Name: "aws___search_documentation", InputSchema: map[string]any{"type": "object"}}},
	}
	connector := &fakeConnector{sess: session}
	runner := Runner{Connector: connector, Transport: serverTransport, Version: "test-version"}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	errs := make(chan error, 1)
	go func() {
		errs <- runner.RunProxy(ctx, Config{
			Endpoint: "https://service.us-east-1.api.aws/mcp",
			Profiles: []string{"default", "dev"},
		}, nil)
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
	waitForRunnerExit(t, ctx, errs)
}

func TestRuntimeFiltersReadOnlyTools(t *testing.T) {
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
	runner := Runner{
		Connector: &fakeConnector{sess: session},
		Transport: serverTransport,
		Version:   "test-version",
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	errs := make(chan error, 1)
	go func() {
		errs <- runner.RunProxy(ctx, Config{
			Endpoint: "https://service.us-east-1.api.aws/mcp",
			ReadOnly: true,
		}, nil)
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
	waitForRunnerExit(t, ctx, errs)
}

func TestRuntimeReturnsToolVisibleErrorOnUpstreamCallFailure(t *testing.T) {
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	session := &fakeSession{
		result:  &mcp.InitializeResult{Capabilities: &mcp.ServerCapabilities{Tools: &mcp.ToolCapabilities{ListChanged: true}}},
		tools:   []*mcp.Tool{{Name: "failing", InputSchema: map[string]any{"type": "object"}}},
		callErr: errors.New("upstream unavailable"),
	}
	runner := Runner{
		Connector: &fakeConnector{sess: session},
		Transport: serverTransport,
		Version:   "test-version",
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	errs := make(chan error, 1)
	go func() {
		errs <- runner.RunProxy(ctx, Config{Endpoint: "https://service.us-east-1.api.aws/mcp"}, nil)
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
	waitForRunnerExit(t, ctx, errs)
}

func TestRuntimeAppliesToolTimeout(t *testing.T) {
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	session := &fakeSession{
		result: &mcp.InitializeResult{Capabilities: &mcp.ServerCapabilities{Tools: &mcp.ToolCapabilities{ListChanged: true}}},
		tools: []*mcp.Tool{
			{Name: "slow", InputSchema: map[string]any{"type": "object"}},
		},
		waitForContextDone: true,
	}
	runner := Runner{
		Connector: &fakeConnector{sess: session},
		Transport: serverTransport,
		Version:   "test-version",
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	errs := make(chan error, 1)
	go func() {
		errs <- runner.RunProxy(ctx, Config{
			Endpoint:    "https://service.us-east-1.api.aws/mcp",
			ToolTimeout: time.Millisecond,
		}, nil)
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
	waitForRunnerExit(t, ctx, errs)
}

func TestRuntimeReturnsUpstreamConnectErrorDuringInitialize(t *testing.T) {
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	wantErr := errors.New("connect failed")
	connector := &fakeConnector{err: wantErr}
	runner := Runner{
		Connector: connector,
		Transport: serverTransport,
		Version:   "test-version",
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	errs := make(chan error, 1)
	go func() {
		errs <- runner.RunProxy(ctx, Config{Endpoint: "https://service.us-east-1.api.aws/mcp"}, nil)
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
		t.Fatal("runner did not exit after context cancellation")
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

func waitForRunnerExit(t *testing.T, ctx context.Context, errs <-chan error) {
	t.Helper()

	select {
	case err := <-errs:
		if err != nil {
			t.Fatalf("runner returned error = %v", err)
		}
	case <-ctx.Done():
		t.Fatal("runner did not exit after client session closed")
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
