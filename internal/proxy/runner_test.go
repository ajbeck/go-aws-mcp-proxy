package proxy

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/ajbeck/go-aws-mcp-proxy/internal/proxyconfig"
)

type fakeConnector struct {
	called bool
	cfg    proxyconfig.Config
	params *mcp.InitializeParams
	err    error
	sess   *fakeSession
}

func (c *fakeConnector) Connect(_ context.Context, cfg proxyconfig.Config, params *mcp.InitializeParams) (UpstreamSession, error) {
	c.called = true
	c.cfg = cfg
	c.params = params
	if c.err != nil {
		return nil, c.err
	}
	return c.sess, nil
}

type fakeSession struct {
	closed bool
	result *mcp.InitializeResult
}

func (s *fakeSession) Close() error {
	s.closed = true
	return nil
}

func (s *fakeSession) InitializeResult() *mcp.InitializeResult {
	return s.result
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
		errs <- runner.RunProxy(ctx, proxyconfig.Config{Endpoint: "https://service.us-east-1.api.aws/mcp"})
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
		errs <- runner.RunProxy(ctx, proxyconfig.Config{Endpoint: "https://service.us-east-1.api.aws/mcp"})
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
