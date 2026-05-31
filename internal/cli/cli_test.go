package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/ajbeck/go-aws-mcp-proxy/internal/proxyconfig"
)

type fakeRunner struct {
	called bool
	config proxyconfig.Config
}

func (r *fakeRunner) RunProxy(_ context.Context, config proxyconfig.Config) error {
	r.called = true
	r.config = config
	return nil
}

func TestRunParsesUpstreamCompatibleRootCommand(t *testing.T) {
	runner := &fakeRunner{}
	var stdout, stderr bytes.Buffer

	code := Run(context.Background(), []string{
		"https://bedrock-agentcore.us-east-1.amazonaws.com/mcp",
		"--profile", "default", "dev",
		"--ca-bundle", "/tmp/company-ca.pem",
		"--metadata", "team=platform", "AWS_REGION=us-west-2",
		"--read-only",
		"--log-level", "DEBUG",
		"--retries", "3",
		"--timeout", "10.5",
		"--connect-timeout", "2",
		"--read-timeout", "3",
		"--write-timeout", "4",
		"--tool-timeout", "5",
		"--disable-telemetry",
		"--skip-auth",
	}, Options{
		Env:     proxyconfig.MapEnv{},
		Runner:  runner,
		Stderr:  &stderr,
		Stdout:  &stdout,
		Version: "test",
	})

	if code != exitOK {
		t.Fatalf("Run() code = %d, stderr = %q", code, stderr.String())
	}
	if !runner.called {
		t.Fatal("runner was not called")
	}

	cfg := runner.config
	if cfg.Endpoint != "https://bedrock-agentcore.us-east-1.amazonaws.com/mcp" {
		t.Fatalf("Endpoint = %q", cfg.Endpoint)
	}
	if cfg.Service != "bedrock-agentcore" {
		t.Fatalf("Service = %q", cfg.Service)
	}
	if cfg.Region != "us-east-1" {
		t.Fatalf("Region = %q", cfg.Region)
	}
	if got := strings.Join(cfg.Profiles, ","); got != "default,dev" {
		t.Fatalf("Profiles = %q", got)
	}
	if cfg.CaBundle != "/tmp/company-ca.pem" {
		t.Fatalf("CaBundle = %q", cfg.CaBundle)
	}
	if cfg.Metadata["team"] != "platform" {
		t.Fatalf("Metadata team = %q", cfg.Metadata["team"])
	}
	if cfg.Metadata["AWS_REGION"] != "us-west-2" {
		t.Fatalf("Metadata AWS_REGION = %q", cfg.Metadata["AWS_REGION"])
	}
	if !cfg.ReadOnly || cfg.LogLevel != "DEBUG" || cfg.Retries != 3 {
		t.Fatalf("unexpected config: %+v", cfg)
	}
	if cfg.Timeout != 10500*time.Millisecond {
		t.Fatalf("Timeout = %s", cfg.Timeout)
	}
	if cfg.ConnectTimeout != 2*time.Second || cfg.ReadTimeout != 3*time.Second ||
		cfg.WriteTimeout != 4*time.Second || cfg.ToolTimeout != 5*time.Second {
		t.Fatalf("unexpected timeouts: %+v", cfg)
	}
	if !cfg.DisableTelemetry || !cfg.SkipAuth {
		t.Fatalf("expected disable telemetry and skip auth: %+v", cfg)
	}
}

func TestRunUsesVersionFlag(t *testing.T) {
	runner := &fakeRunner{}
	var stdout, stderr bytes.Buffer

	code := Run(context.Background(), []string{"--version"}, Options{
		Env:     proxyconfig.MapEnv{},
		Runner:  runner,
		Stderr:  &stderr,
		Stdout:  &stdout,
		Version: "1.2.3",
	})

	if code != exitOK {
		t.Fatalf("Run() code = %d, stderr = %q", code, stderr.String())
	}
	if runner.called {
		t.Fatal("runner should not be called for --version")
	}
	if strings.TrimSpace(stdout.String()) != "1.2.3" {
		t.Fatalf("stdout = %q", stdout.String())
	}
}

func TestRunReturnsUsageForParseErrors(t *testing.T) {
	var stdout, stderr bytes.Buffer

	code := Run(context.Background(), []string{"--profile"}, Options{
		Env:     proxyconfig.MapEnv{},
		Runner:  &fakeRunner{},
		Stderr:  &stderr,
		Stdout:  &stdout,
		Version: "test",
	})

	if code != exitUsage {
		t.Fatalf("Run() code = %d, stderr = %q", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "--profile") {
		t.Fatalf("stderr = %q, want profile error", stderr.String())
	}
}

func TestRunReturnsUsageForConfigValidationErrors(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want string
	}{
		{
			name: "metadata",
			args: []string{"https://service.us-east-1.api.aws/mcp", "--metadata", "invalid"},
			want: "metadata must be in key=value format",
		},
		{
			name: "retries",
			args: []string{"https://service.us-east-1.api.aws/mcp", "--retries", "11"},
			want: "retries must be between 0 and 10",
		},
		{
			name: "negative timeout",
			args: []string{"https://service.us-east-1.api.aws/mcp", "--timeout", "-1"},
			want: "timeout must be >= 0",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer

			code := Run(context.Background(), tt.args, Options{
				Env:     proxyconfig.MapEnv{},
				Runner:  &fakeRunner{},
				Stderr:  &stderr,
				Stdout:  &stdout,
				Version: "test",
			})

			if code != exitUsage {
				t.Fatalf("Run() code = %d, stderr = %q", code, stderr.String())
			}
			if !strings.Contains(stderr.String(), tt.want) {
				t.Fatalf("stderr = %q, want %q", stderr.String(), tt.want)
			}
		})
	}
}
