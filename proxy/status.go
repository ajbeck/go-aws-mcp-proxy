package proxy

import (
	"context"
	"fmt"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	proxyStatusToolName   = "aws___proxy_status"
	reasonReconnectNeeded = "reconnect_required"
)

type degradedUpstreamSession struct {
	err error
}

func (s degradedUpstreamSession) CallTool(context.Context, *mcp.CallToolParams) (*mcp.CallToolResult, error) {
	return nil, s.err
}

func (s degradedUpstreamSession) Close() error {
	return nil
}

func (s degradedUpstreamSession) InitializeResult() *mcp.InitializeResult {
	return &mcp.InitializeResult{
		Capabilities: &mcp.ServerCapabilities{
			Tools: &mcp.ToolCapabilities{ListChanged: true},
		},
	}
}

func (s degradedUpstreamSession) ListTools(context.Context, *mcp.ListToolsParams) (*mcp.ListToolsResult, error) {
	return &mcp.ListToolsResult{}, nil
}

func (s degradedUpstreamSession) degradedError() error {
	return s.err
}

func (r *proxyRun) registerProxyStatusTool() {
	r.server.AddTool(&mcp.Tool{
		Name:        proxyStatusToolName,
		Title:       "AWS MCP proxy status",
		Description: "Report the local AWS MCP proxy connection and credential status.",
		InputSchema: map[string]any{"type": "object"},
		OutputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"status":                map[string]any{"type": "string"},
				"endpoint":              map[string]any{"type": "string"},
				"signing_mode":          map[string]any{"type": "string"},
				"credentials_required":  map[string]any{"type": "boolean"},
				"credentials_available": map[string]any{"type": []string{"boolean", "null"}},
				"category":              map[string]any{"type": []string{"string", "null"}},
				"reason":                map[string]any{"type": []string{"string", "null"}},
				"recommended_action":    map[string]any{"type": "string"},
			},
		},
		Annotations: &mcp.ToolAnnotations{
			ReadOnlyHint: true,
			Title:        "AWS MCP proxy status",
		},
	}, func(ctx context.Context, _ *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return r.proxyStatus(ctx), nil
	})
}

func (r *proxyRun) proxyStatus(ctx context.Context) *mcp.CallToolResult {
	status := "connected"
	endpoint := value(r.config.Endpoint)
	signingMode := "signed"
	credentialsRequired := !enabled(r.config.SkipAuth)
	var credentialsAvailable any
	var proxyErr *proxyError

	if err := r.checkSigningCredentials(ctx); err != nil {
		credentialsAvailable = false
		proxyErr = classifyError(err)
		if enabled(r.config.SkipAuth) && proxyErr.reason == reasonCredentialUnavailable {
			signingMode = "unsigned"
			proxyErr = nil
		} else {
			status = "degraded"
		}
	} else {
		credentialsAvailable = true
		if err := r.upstream.DegradedError(); err != nil {
			status = "degraded"
			proxyErr = newProxyError(
				categoryUserAction,
				reasonReconnectNeeded,
				"AWS credentials are now available, but upstream tools were not discovered for this MCP session",
				"Ask the user to restart or reconnect the MCP server so the proxy can discover the upstream tools.",
				"",
				err,
			)
		}
	}

	action := "The proxy is connected and ready."
	if proxyErr != nil {
		action = proxyErr.next
	}
	structured := map[string]any{
		"status":                status,
		"endpoint":              endpoint,
		"signing_mode":          signingMode,
		"credentials_required":  credentialsRequired,
		"credentials_available": credentialsAvailable,
		"recommended_action":    action,
	}

	text := "Status: " + status + "\n"
	text += "Endpoint: " + endpoint + "\n"
	text += "Signing mode: " + signingMode + "\n"
	text += fmt.Sprintf("Credentials required: %t\n", credentialsRequired)
	if credentialsAvailable == nil {
		text += "Credentials available: not checked\n"
	} else {
		text += fmt.Sprintf("Credentials available: %t\n", credentialsAvailable)
	}
	if proxyErr == nil {
		structured["category"] = nil
		structured["reason"] = nil
		text += "Problem: none\n"
		text += "Can the agent fix this? No action is required.\n"
		text += "Recommended next step: " + action + "\n"
	} else {
		structured["category"] = string(proxyErr.category)
		structured["reason"] = proxyErr.reason
		text += renderError(proxyErr)
	}

	return &mcp.CallToolResult{
		Content:           []mcp.Content{&mcp.TextContent{Text: text}},
		StructuredContent: structured,
	}
}

func (r *proxyRun) checkSigningCredentials(ctx context.Context) error {
	var caBundle []byte
	if r.config.CaBundle != nil {
		var err error
		caBundle, err = readCABundle(*r.config.CaBundle)
		if err != nil {
			return err
		}
	}
	provider, err := signingCredentialsProvider(ctx, r.config, caBundle, clientOptions{Credentials: r.credentials})
	if err != nil {
		return err
	}
	if err := preflightSigningCredentials(ctx, provider); err != nil {
		return err
	}
	if r.config.Service == nil {
		return missingConfigError("service", reasonMissingService)
	}
	if r.config.Region == nil {
		return missingConfigError("region", reasonMissingRegion)
	}
	return nil
}

func (s *upstreamState) DegradedError() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	session, ok := s.session.(interface{ degradedError() error })
	if !ok {
		return nil
	}
	return session.degradedError()
}
