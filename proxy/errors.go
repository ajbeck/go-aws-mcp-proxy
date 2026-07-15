package proxy

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
)

type errorCategory string

const (
	categoryAgentFixable         errorCategory = "agent_fixable"
	categoryUserAction           errorCategory = "user_action_required"
	categoryRetryable            errorCategory = "retryable"
	categoryConfiguration        errorCategory = "configuration_error"
	reasonInvalidProfile         string        = "invalid_profile"
	reasonMissingEndpoint        string        = "missing_endpoint"
	reasonMissingService         string        = "missing_service"
	reasonMissingRegion          string        = "missing_region"
	reasonUnsafeEndpoint         string        = "unsafe_endpoint"
	reasonCredentialUnavailable  string        = "credential_unavailable"
	reasonCredentialUnauthorized string        = "credential_unauthorized"
	reasonCredentialForbidden    string        = "credential_forbidden"
	reasonUpstreamJSONRPC        string        = "upstream_jsonrpc_error"
	reasonUpstreamHTTP           string        = "upstream_http_error"
	reasonUpstreamRetryableHTTP  string        = "upstream_retryable_http"
	reasonUpstreamTimeout        string        = "upstream_timeout"
	reasonUnknown                string        = "unknown"
)

type proxyError struct {
	category errorCategory
	reason   string
	problem  string
	next     string
	detail   string
	err      error
}

func (e *proxyError) Error() string {
	return renderError(e)
}

func (e *proxyError) Unwrap() error {
	return e.err
}

func newProxyError(category errorCategory, reason, problem, next, detail string, err error) *proxyError {
	if detail == "" && err != nil {
		detail = err.Error()
	}
	return &proxyError{
		category: category,
		reason:   reason,
		problem:  problem,
		next:     next,
		detail:   detail,
		err:      err,
	}
}

func classifyError(err error) *proxyError {
	if err == nil {
		return nil
	}
	if proxyErr, ok := errors.AsType[*proxyError](err); ok {
		return proxyErr
	}
	if httpErr, ok := errors.AsType[*upstreamHTTPError](err); ok {
		return classifyUpstreamHTTPError(httpErr)
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return newProxyError(
			categoryRetryable,
			reasonUpstreamTimeout,
			"the upstream MCP request timed out",
			"Retry the request. If it keeps timing out, ask the user to check the endpoint, network, and timeout settings.",
			"",
			err,
		)
	}
	return newProxyError(
		categoryUserAction,
		reasonUnknown,
		"the proxy encountered an upstream error it could not classify",
		"Ask the user to inspect the proxy logs and open an issue in the GitHub repo if the problem is not clear.",
		"",
		err,
	)
}

func classifyUpstreamHTTPError(err *upstreamHTTPError) *proxyError {
	switch err.statusCode {
	case http.StatusUnauthorized:
		return newProxyError(
			categoryUserAction,
			reasonCredentialUnauthorized,
			"the upstream MCP endpoint rejected the request as unauthorized",
			"Ask the user to refresh or configure AWS credentials, verify the selected profile, and confirm the endpoint accepts those credentials.",
			upstreamHTTPDetail(err, true),
			err,
		)
	case http.StatusForbidden:
		return newProxyError(
			categoryUserAction,
			reasonCredentialForbidden,
			"the upstream MCP endpoint rejected the signed request as forbidden",
			"Ask the user to verify IAM permissions, AWS account access, region, profile, and endpoint policy.",
			upstreamHTTPDetail(err, true),
			err,
		)
	case http.StatusTooManyRequests, http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return newProxyError(
			categoryRetryable,
			reasonUpstreamRetryableHTTP,
			fmt.Sprintf("the upstream MCP endpoint returned retryable HTTP status %d", err.statusCode),
			"Retry the request later. If it keeps failing, ask the user to check upstream service health and throttling.",
			upstreamHTTPDetail(err, err.jsonrpcMessage != ""),
			err,
		)
	default:
		reason := reasonUpstreamHTTP
		if err.jsonrpcMessage != "" {
			reason = reasonUpstreamJSONRPC
		}
		return newProxyError(
			categoryUserAction,
			reason,
			fmt.Sprintf("the upstream MCP endpoint returned HTTP status %d", err.statusCode),
			"Ask the user to inspect the proxy logs and upstream MCP endpoint configuration.",
			upstreamHTTPDetail(err, err.jsonrpcMessage != ""),
			err,
		)
	}
}

func upstreamHTTPDetail(err *upstreamHTTPError, includeBody bool) string {
	var parts []string
	parts = append(parts, fmt.Sprintf("HTTP %d %s", err.statusCode, http.StatusText(err.statusCode)))
	if err.jsonrpcMessage != "" {
		parts = append(parts, fmt.Sprintf("JSON-RPC error %d: %s", value(err.jsonrpcCode), err.jsonrpcMessage))
	}
	if err.jsonrpcData != "" {
		parts = append(parts, "JSON-RPC data: "+err.jsonrpcData)
	}
	if includeBody && err.bodyExcerpt != "" && err.jsonrpcMessage == "" {
		parts = append(parts, "response excerpt: "+err.bodyExcerpt)
	}
	return strings.Join(parts, "; ")
}

func renderError(err *proxyError) string {
	if err == nil {
		return ""
	}

	var b strings.Builder
	fmt.Fprintf(&b, "Problem: %s\n", err.problem)
	fmt.Fprintf(&b, "Can the agent fix this? %s\n", agentFixText(err.category))
	fmt.Fprintf(&b, "Recommended next step: %s\n", err.next)
	if err.detail != "" {
		fmt.Fprintf(&b, "Technical detail: %s\n", err.detail)
	}
	fmt.Fprintf(&b, "Category: %s\n", err.category)
	fmt.Fprintf(&b, "Reason: %s", err.reason)
	return b.String()
}

func agentFixText(category errorCategory) string {
	switch category {
	case categoryAgentFixable:
		return "Yes. The agent should correct the request and retry."
	case categoryRetryable:
		return "Yes. The agent can retry later or with a longer timeout."
	case categoryConfiguration:
		return "No. Ask the user to update the MCP server configuration."
	default:
		return "No. Ask the user to take action."
	}
}

func credentialUnavailableError(err error) *proxyError {
	return newProxyError(
		categoryUserAction,
		reasonCredentialUnavailable,
		"AWS credentials are not available for signing upstream MCP requests",
		"Ask the user to configure AWS credentials, choose a valid AWS profile, refresh SSO credentials, or run with --skip-auth if the endpoint supports unsigned requests.",
		"",
		err,
	)
}

func missingConfigError(name, reason string) *proxyError {
	return newProxyError(
		categoryConfiguration,
		reason,
		fmt.Sprintf("%s is required before the proxy can connect to the upstream MCP endpoint", name),
		fmt.Sprintf("Ask the user to set %s in the MCP server configuration.", name),
		"",
		nil,
	)
}
