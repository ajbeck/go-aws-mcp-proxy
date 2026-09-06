package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"

	"github.com/ajbeck/go-aws-mcp-proxy/proxy"
)

func TestRunDispatchesDoctorWithoutStartingProxy(t *testing.T) {
	proxyCalled := false
	doctorCalled := false
	var gotConfig proxy.Config
	var gotOptions proxy.DiagnoseOptions
	var stdout bytes.Buffer
	var stderr bytes.Buffer

	code := Run(t.Context(), []string{
		"doctor",
		"https://aws-mcp.us-east-1.api.aws/mcp",
		"--profile", "production", "development",
		"--probe",
		"--json",
	}, Options{
		LookupEnv: lookupEnv(nil),
		RunDoctor: func(_ context.Context, cfg proxy.Config, options proxy.DiagnoseOptions) proxy.DiagnosticReport {
			doctorCalled = true
			gotConfig = cfg
			gotOptions = options
			return healthyDiagnosticReport("https://aws-mcp.us-east-1.api.aws/mcp", true)
		},
		RunProxy: func(context.Context, proxy.Config, *slog.Logger) error {
			proxyCalled = true
			return nil
		},
		Stdout:  &stdout,
		Stderr:  &stderr,
		Version: "v0.5.0-test",
	})

	if code != exitOK {
		t.Fatalf("Run(doctor) code = %d, stderr = %q", code, stderr.String())
	}
	if !doctorCalled || proxyCalled {
		t.Fatalf("Run(doctor) doctorCalled = %v, proxyCalled = %v", doctorCalled, proxyCalled)
	}
	if gotConfig.Endpoint == nil || *gotConfig.Endpoint != "https://aws-mcp.us-east-1.api.aws/mcp" {
		t.Fatalf("Run(doctor) Endpoint = %#v", gotConfig.Endpoint)
	}
	if gotConfig.Service == nil || *gotConfig.Service != "aws-mcp" || gotConfig.Region == nil || *gotConfig.Region != "us-east-1" {
		t.Fatalf("Run(doctor) signing config = %#v", gotConfig)
	}
	if gotConfig.Profiles == nil || strings.Join(*gotConfig.Profiles, ",") != "production,development" {
		t.Fatalf("Run(doctor) Profiles = %#v", gotConfig.Profiles)
	}
	if !gotOptions.Probe || gotOptions.Version != "v0.5.0-test" {
		t.Fatalf("Run(doctor) DiagnoseOptions = %#v", gotOptions)
	}
	if gotOptions.Logger != nil {
		t.Fatalf("Run(doctor) Logger = %#v, want nil without --log-level", gotOptions.Logger)
	}
	if stderr.Len() != 0 {
		t.Fatalf("Run(doctor) stderr = %q, want empty", stderr.String())
	}
	var report proxy.DiagnosticReport
	if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
		t.Fatalf("json.Unmarshal(doctor output) error = %v, output = %q", err, stdout.String())
	}
	if report.Healthy == nil || !*report.Healthy {
		t.Fatalf("doctor JSON Healthy = %#v, want true", report.Healthy)
	}
}

func TestRunPreservesRootProxyGrammarWithDoctorAvailable(t *testing.T) {
	run := &fakeProxyRun{}
	var stderr bytes.Buffer
	code := Run(t.Context(), []string{
		"https://doctor.us-east-1.api.aws/mcp",
		"--skip-auth",
	}, Options{
		LookupEnv: lookupEnv(nil),
		RunProxy:  run.call,
		Stderr:    &stderr,
	})

	if code != exitOK {
		t.Fatalf("Run(root proxy) code = %d, stderr = %q", code, stderr.String())
	}
	if !run.called || run.config.Endpoint == nil || *run.config.Endpoint != "https://doctor.us-east-1.api.aws/mcp" {
		t.Fatalf("Run(root proxy) config = %#v", run.config)
	}
}

func TestRunDoctorUsesStableFailureExitCodes(t *testing.T) {
	tests := []struct {
		name     string
		category proxy.DiagnosticCategory
		wantCode int
	}{
		{name: "configuration", category: proxy.DiagnosticConfiguration, wantCode: exitConfiguration},
		{name: "credentials", category: proxy.DiagnosticCredentials, wantCode: exitCredentials},
		{name: "identity", category: proxy.DiagnosticIdentity, wantCode: exitCredentials},
		{name: "network", category: proxy.DiagnosticNetwork, wantCode: exitNetwork},
		{name: "upstream", category: proxy.DiagnosticUpstream, wantCode: exitUpstream},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var stdout bytes.Buffer
			var stderr bytes.Buffer
			code := Run(t.Context(), []string{
				"doctor",
				"https://aws-mcp.us-east-1.api.aws/mcp",
			}, Options{
				LookupEnv: lookupEnv(nil),
				RunDoctor: func(context.Context, proxy.Config, proxy.DiagnoseOptions) proxy.DiagnosticReport {
					return failedDiagnosticReport(test.category)
				},
				Stdout: &stdout,
				Stderr: &stderr,
			})

			if code != test.wantCode {
				t.Fatalf("Run(doctor %s failure) code = %d, want %d", test.name, code, test.wantCode)
			}
			if stderr.Len() != 0 {
				t.Fatalf("Run(doctor %s failure) stderr = %q, want empty", test.name, stderr.String())
			}
			if !strings.Contains(stdout.String(), "Result: unhealthy") || !strings.Contains(stdout.String(), "FAIL check") {
				t.Fatalf("Run(doctor %s failure) stdout = %q", test.name, stdout.String())
			}
		})
	}
}

func TestRunDoctorReportsMissingEndpointAsConfigurationFailure(t *testing.T) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	code := Run(t.Context(), []string{"doctor", "--skip-auth", "--json"}, Options{
		LookupEnv: lookupEnv(nil),
		Stdout:    &stdout,
		Stderr:    &stderr,
	})

	if code != exitConfiguration {
		t.Fatalf("Run(doctor without endpoint) code = %d, want %d; stderr = %q", code, exitConfiguration, stderr.String())
	}
	if stdout.Len() != 0 || !strings.Contains(stderr.String(), "expected") {
		t.Fatalf("Run(doctor without endpoint) stdout = %q, stderr = %q", stdout.String(), stderr.String())
	}
}

func healthyDiagnosticReport(endpoint string, probe bool) proxy.DiagnosticReport {
	return proxy.DiagnosticReport{
		Healthy:        new(true),
		Endpoint:       new(endpoint),
		ProbeRequested: new(probe),
		Checks: []proxy.DiagnosticCheck{{
			Name:     new("configuration"),
			Status:   new(proxy.DiagnosticPass),
			Category: new(proxy.DiagnosticConfiguration),
			Message:  new("Configuration is valid."),
		}},
	}
}

func failedDiagnosticReport(category proxy.DiagnosticCategory) proxy.DiagnosticReport {
	return proxy.DiagnosticReport{
		Healthy:        new(false),
		Endpoint:       new("https://aws-mcp.us-east-1.api.aws/mcp"),
		ProbeRequested: new(false),
		Checks: []proxy.DiagnosticCheck{{
			Name:        new("check"),
			Status:      new(proxy.DiagnosticFail),
			Category:    new(category),
			Message:     new("The check failed."),
			Remediation: new("Correct it and retry."),
		}},
	}
}
