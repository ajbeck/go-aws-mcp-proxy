package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/ajbeck/go-aws-mcp-proxy/proxy"
)

func writeDiagnosticJSON(w io.Writer, report proxy.DiagnosticReport) error {
	encoder := json.NewEncoder(w)
	encoder.SetEscapeHTML(false)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(report); err != nil {
		return fmt.Errorf("write diagnostic JSON: %w", err)
	}
	return nil
}

func writeDiagnosticText(w io.Writer, report proxy.DiagnosticReport) error {
	result := "unhealthy"
	if report.Healthy != nil && *report.Healthy {
		result = "healthy"
	}

	var b strings.Builder
	_, _ = fmt.Fprintf(&b, "AWS MCP proxy doctor\nResult: %s\nEndpoint: %s\n\n", result, value(report.Endpoint)) // strings.Builder writes never fail.
	for _, check := range report.Checks {
		status := strings.ToUpper(string(value(check.Status)))
		name := value(check.Name)
		if check.Profile != nil {
			name += " [" + *check.Profile + "]"
		}
		_, _ = fmt.Fprintf(&b, "%s %s: %s\n", status, name, value(check.Message)) // strings.Builder writes never fail.
		if check.Source != nil {
			_, _ = fmt.Fprintf(&b, "  Credential source: %s\n", *check.Source) // strings.Builder writes never fail.
		}
		if check.ExpiresAt != nil {
			_, _ = fmt.Fprintf(&b, "  Expires: %s\n", check.ExpiresAt.Format("2006-01-02T15:04:05Z07:00")) // strings.Builder writes never fail.
		}
		if check.Account != nil {
			_, _ = fmt.Fprintf(&b, "  Account: %s\n", *check.Account) // strings.Builder writes never fail.
		}
		if check.ARN != nil {
			_, _ = fmt.Fprintf(&b, "  ARN: %s\n", *check.ARN) // strings.Builder writes never fail.
		}
		if check.UserID != nil {
			_, _ = fmt.Fprintf(&b, "  User ID: %s\n", *check.UserID) // strings.Builder writes never fail.
		}
		if check.ToolCount != nil {
			_, _ = fmt.Fprintf(&b, "  Tools: %d\n", *check.ToolCount) // strings.Builder writes never fail.
		}
		if check.Detail != nil {
			_, _ = fmt.Fprintf(&b, "  Technical detail: %s\n", *check.Detail) // strings.Builder writes never fail.
		}
		if check.Remediation != nil {
			_, _ = fmt.Fprintf(&b, "  Next step: %s\n", *check.Remediation) // strings.Builder writes never fail.
		}
	}

	if _, err := io.WriteString(w, b.String()); err != nil {
		return fmt.Errorf("write diagnostic report: %w", err)
	}
	return nil
}

func diagnosticExitCode(report proxy.DiagnosticReport) int {
	for _, check := range report.Checks {
		if check.Status == nil || *check.Status != proxy.DiagnosticFail || check.Category == nil {
			continue
		}
		switch *check.Category {
		case proxy.DiagnosticConfiguration:
			return exitConfiguration
		case proxy.DiagnosticCredentials, proxy.DiagnosticIdentity:
			return exitCredentials
		case proxy.DiagnosticNetwork:
			return exitNetwork
		default:
			return exitUpstream
		}
	}
	return exitOK
}
