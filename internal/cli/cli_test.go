package cli

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/ajbeck/go-aws-mcp-proxy/internal/proxy"
)

type fakeRunner struct {
	called bool
	config proxy.Config
	logger *slog.Logger
}

func (r *fakeRunner) RunProxy(_ context.Context, config proxy.Config, logger *slog.Logger) error {
	r.called = true
	r.config = config
	r.logger = logger
	return nil
}

func TestRunParsesUpstreamCompatibleRootCommand(t *testing.T) {
	runner := &fakeRunner{}
	var stdout, stderr bytes.Buffer

	code := Run(t.Context(), []string{
		"https://bedrock-agentcore.us-east-1.amazonaws.com/mcp",
		"--profile", "default",
		"--profile", "dev",
		"--ca-bundle", "/tmp/company-ca.pem",
		"--metadata", "team=platform",
		"--metadata", "AWS_REGION=us-west-2",
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
	if runner.logger == nil {
		t.Fatal("logger was not passed to runner")
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

func TestLoggerHonorsLogLevelAndWritesToStderr(t *testing.T) {
	runner := &fakeRunner{}
	var stdout, stderr bytes.Buffer

	code := Run(t.Context(), []string{
		"https://service.us-east-1.api.aws/mcp",
		"--log-level", "DEBUG",
	}, Options{
		Runner:  runner,
		Stderr:  &stderr,
		Stdout:  &stdout,
		Version: "test",
	})
	if code != exitOK {
		t.Fatalf("Run() code = %d, stderr = %q", code, stderr.String())
	}

	runner.logger.Debug("debug message")
	if stdout.Len() != 0 {
		t.Fatalf("stdout = %q, want empty", stdout.String())
	}
	if !strings.Contains(stderr.String(), "debug message") {
		t.Fatalf("stderr = %q, want debug log", stderr.String())
	}
}

func TestRunUsesVersionFlag(t *testing.T) {
	var stdout, stderr bytes.Buffer

	code := Run(t.Context(), []string{"--version"}, Options{
		Stderr:  &stderr,
		Stdout:  &stdout,
		Version: "1.2.3",
	})

	if code != exitOK {
		t.Fatalf("Run() code = %d, stderr = %q", code, stderr.String())
	}
	if strings.TrimSpace(stdout.String()) != "1.2.3" {
		t.Fatalf("stdout = %q", stdout.String())
	}
}

func TestRunRequiresRunner(t *testing.T) {
	var stdout, stderr bytes.Buffer

	code := Run(t.Context(), []string{"https://service.us-east-1.api.aws/mcp"}, Options{
		Stderr:  &stderr,
		Stdout:  &stdout,
		Version: "test",
	})

	if code != exitError {
		t.Fatalf("Run() code = %d, stderr = %q", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "runner is required") {
		t.Fatalf("stderr = %q, want runner error", stderr.String())
	}
}

func TestRunReturnsUsageForParseErrors(t *testing.T) {
	var stdout, stderr bytes.Buffer

	code := Run(t.Context(), []string{"--profile"}, Options{
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

func TestAppConfigUsesEndpointAndEnvironmentFallbacks(t *testing.T) {
	cfg := app{
		Endpoint: "https://service.example.com/mcp",
		Metadata: map[string]string{
			"team": "platform",
		},
	}.config(lookupEnv(map[string]string{"AWS_REGION": "eu-west-1"}))

	if cfg.Service != "service" {
		t.Fatalf("Service = %q", cfg.Service)
	}
	if cfg.Region != "eu-west-1" {
		t.Fatalf("Region = %q", cfg.Region)
	}
	if cfg.Metadata["team"] != "platform" {
		t.Fatalf("Metadata team = %q", cfg.Metadata["team"])
	}
}

func TestAppConfigPreservesExplicitMetadata(t *testing.T) {
	cfg := app{
		Endpoint: "https://service.us-east-1.api.aws/mcp",
		Metadata: map[string]string{
			"AWS_REGION": "us-west-2",
		},
	}.config(lookupEnv(nil))

	if cfg.Region != "us-east-1" {
		t.Fatalf("Region = %q", cfg.Region)
	}
	if cfg.Metadata["AWS_REGION"] != "us-west-2" {
		t.Fatalf("Metadata AWS_REGION = %q", cfg.Metadata["AWS_REGION"])
	}
}

func TestAppConfigDedupesProfiles(t *testing.T) {
	cfg := app{
		Endpoint: "https://service.us-east-1.api.aws/mcp",
		Profiles: []string{
			"default",
			"dev",
			"default",
			"",
		},
	}.config(lookupEnv(nil))

	if got := strings.Join(cfg.Profiles, ","); got != "default,dev" {
		t.Fatalf("Profiles = %q", got)
	}
}

func lookupEnv(values map[string]string) proxy.LookupEnv {
	return func(name string) (string, bool) {
		value, ok := values[name]
		return value, ok
	}
}
