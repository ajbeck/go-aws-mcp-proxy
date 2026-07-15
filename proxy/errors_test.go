package proxy

import (
	"net/http"
	"strings"
	"testing"
)

func TestClassifyUpstreamHTTPErrorUnauthorizedIncludesSafeResponseExcerpt(t *testing.T) {
	proxyErr := classifyError(&upstreamHTTPError{
		statusCode:  http.StatusUnauthorized,
		bodyExcerpt: "missing authentication token",
	})

	if proxyErr.category != categoryUserAction {
		t.Fatalf("category = %q, want %q", proxyErr.category, categoryUserAction)
	}
	if proxyErr.reason != reasonCredentialUnauthorized {
		t.Fatalf("reason = %q, want %q", proxyErr.reason, reasonCredentialUnauthorized)
	}
	if !strings.Contains(proxyErr.detail, "HTTP 401 Unauthorized") {
		t.Fatalf("detail = %q, want HTTP status", proxyErr.detail)
	}
	if !strings.Contains(proxyErr.detail, "response excerpt: missing authentication token") {
		t.Fatalf("detail = %q, want response excerpt", proxyErr.detail)
	}
}

func TestClassifyUpstreamHTTPErrorRetryableOmitsGenericResponseExcerpt(t *testing.T) {
	proxyErr := classifyError(&upstreamHTTPError{
		statusCode:  http.StatusServiceUnavailable,
		bodyExcerpt: "temporary maintenance page",
	})

	if proxyErr.category != categoryRetryable {
		t.Fatalf("category = %q, want %q", proxyErr.category, categoryRetryable)
	}
	if proxyErr.reason != reasonUpstreamRetryableHTTP {
		t.Fatalf("reason = %q, want %q", proxyErr.reason, reasonUpstreamRetryableHTTP)
	}
	if strings.Contains(proxyErr.detail, "temporary maintenance page") {
		t.Fatalf("detail = %q, want generic 5xx body omitted", proxyErr.detail)
	}
}

func TestClassifyUpstreamHTTPErrorJSONRPCIncludesMessageAndData(t *testing.T) {
	code := int64(-32001)
	proxyErr := classifyError(&upstreamHTTPError{
		statusCode:     http.StatusInternalServerError,
		jsonrpcCode:    &code,
		jsonrpcMessage: "tool failed",
		jsonrpcData:    `{"hint":"retry with a valid region"}`,
	})

	if proxyErr.category != categoryUserAction {
		t.Fatalf("category = %q, want %q", proxyErr.category, categoryUserAction)
	}
	if proxyErr.reason != reasonUpstreamJSONRPC {
		t.Fatalf("reason = %q, want %q", proxyErr.reason, reasonUpstreamJSONRPC)
	}
	for _, want := range []string{
		"HTTP 500 Internal Server Error",
		"JSON-RPC error -32001: tool failed",
		`JSON-RPC data: {"hint":"retry with a valid region"}`,
	} {
		if !strings.Contains(proxyErr.detail, want) {
			t.Fatalf("detail = %q, want %q", proxyErr.detail, want)
		}
	}
}
