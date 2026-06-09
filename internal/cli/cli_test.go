package cli

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/ajbeck/go-aws-mcp-proxy/proxy"
)

type fakeProxyRun struct {
	called bool
	config proxy.Config
	logger *slog.Logger
}

func (r *fakeProxyRun) call(_ context.Context, config proxy.Config, logger *slog.Logger) error {
	r.called = true
	r.config = config
	r.logger = logger
	return nil
}

func TestAppRunBuildsProxyConfigAndLogger(t *testing.T) {
	run := &fakeProxyRun{}
	var stderr bytes.Buffer

	application := &app{
		Endpoint: new("https://bedrock-agentcore.us-east-1.amazonaws.com/mcp"),
		LogLevel: new("DEBUG"),
	}

	err := application.Run(t.Context(), lookupEnv(nil), run.call, &stderr)
	if err != nil {
		t.Fatalf("app.Run() error = %v", err)
	}
	if !run.called {
		t.Fatal("proxy run was not called")
	}
	if run.logger == nil {
		t.Fatal("logger was not passed to proxy run")
	}
	if run.config.Endpoint == nil || *run.config.Endpoint != "https://bedrock-agentcore.us-east-1.amazonaws.com/mcp" {
		t.Fatalf("Endpoint = %#v", run.config.Endpoint)
	}

	run.logger.Debug("debug message")
	if !strings.Contains(stderr.String(), "debug message") {
		t.Fatalf("stderr = %q, want debug log", stderr.String())
	}
}

func TestRunBindsDependenciesIntoAppRun(t *testing.T) {
	run := &fakeProxyRun{}
	var stdout bytes.Buffer
	var stderr bytes.Buffer

	code := Run(t.Context(), []string{
		"https://service.us-east-1.api.aws/mcp",
		"--skip-auth",
	}, Options{
		LookupEnv: lookupEnv(nil),
		RunProxy:  run.call,
		Stderr:    &stderr,
		Stdout:    &stdout,
	})

	if code != exitOK {
		t.Fatalf("Run() code = %d, stderr = %q", code, stderr.String())
	}
	if !run.called {
		t.Fatal("proxy run was not called")
	}
	if run.config.Endpoint == nil || *run.config.Endpoint != "https://service.us-east-1.api.aws/mcp" {
		t.Fatalf("Endpoint = %#v", run.config.Endpoint)
	}
	if run.config.SkipAuth == nil || !*run.config.SkipAuth {
		t.Fatalf("SkipAuth = %#v", run.config.SkipAuth)
	}
}

func TestNewLoggerHonorsLogLevel(t *testing.T) {
	var stderr bytes.Buffer
	logger := newLogger("DEBUG", &stderr)
	logger.Debug("debug message")
	if !strings.Contains(stderr.String(), "debug message") {
		t.Fatalf("stderr = %q, want debug log", stderr.String())
	}
}

func TestAppConfigUsesEndpointAndEnvironmentFallbacks(t *testing.T) {
	cfg := app{
		Endpoint: new("https://service.example.com/mcp"),
		Profiles: []string{
			"default",
			"dev",
		},
		CaBundle: new("/tmp/company-ca.pem"),
		Metadata: map[string]string{
			"team":       "platform",
			"AWS_REGION": "us-west-2",
		},
		ReadOnly:         new(true),
		LogLevel:         new("DEBUG"),
		Retries:          new(3),
		Timeout:          new(10.5),
		ConnectTimeout:   new(2.0),
		ReadTimeout:      new(3.0),
		WriteTimeout:     new(4.0),
		ToolTimeout:      new(5.0),
		DisableTelemetry: new(true),
		SkipAuth:         new(true),
	}.config(lookupEnv(map[string]string{"AWS_REGION": "eu-west-1"}))

	if cfg.Service == nil || *cfg.Service != "service" {
		t.Fatalf("Service = %#v", cfg.Service)
	}
	if cfg.Region == nil || *cfg.Region != "eu-west-1" {
		t.Fatalf("Region = %#v", cfg.Region)
	}
	if cfg.Metadata == nil || (*cfg.Metadata)["team"] != "platform" {
		t.Fatalf("Metadata team = %#v", cfg.Metadata)
	}
	if (*cfg.Metadata)["AWS_REGION"] != "us-west-2" {
		t.Fatalf("Metadata AWS_REGION = %q", (*cfg.Metadata)["AWS_REGION"])
	}
	if cfg.Profiles == nil {
		t.Fatal("Profiles = nil")
	}
	if got := strings.Join(*cfg.Profiles, ","); got != "default,dev" {
		t.Fatalf("Profiles = %q", got)
	}
	if cfg.CaBundle == nil || *cfg.CaBundle != "/tmp/company-ca.pem" {
		t.Fatalf("CaBundle = %#v", cfg.CaBundle)
	}
	if cfg.ReadOnly == nil || !*cfg.ReadOnly || cfg.LogLevel == nil || *cfg.LogLevel != "DEBUG" || cfg.Retries == nil || *cfg.Retries != 3 {
		t.Fatalf("unexpected config: %+v", cfg)
	}
	if cfg.Timeout == nil || *cfg.Timeout != 10500*time.Millisecond {
		t.Fatalf("Timeout = %#v", cfg.Timeout)
	}
	if cfg.ConnectTimeout == nil || *cfg.ConnectTimeout != 2*time.Second || cfg.ReadTimeout == nil || *cfg.ReadTimeout != 3*time.Second ||
		cfg.WriteTimeout == nil || *cfg.WriteTimeout != 4*time.Second || cfg.ToolTimeout == nil || *cfg.ToolTimeout != 5*time.Second {
		t.Fatalf("unexpected timeouts: %+v", cfg)
	}
	if cfg.DisableTelemetry == nil || !*cfg.DisableTelemetry || cfg.SkipAuth == nil || !*cfg.SkipAuth {
		t.Fatalf("expected disable telemetry and skip auth: %+v", cfg)
	}
}

func TestAppConfigPreservesExplicitMetadata(t *testing.T) {
	cfg := app{
		Endpoint: new("https://service.us-east-1.api.aws/mcp"),
		Metadata: map[string]string{
			"AWS_REGION": "us-west-2",
		},
	}.config(lookupEnv(nil))

	if cfg.Region == nil || *cfg.Region != "us-east-1" {
		t.Fatalf("Region = %#v", cfg.Region)
	}
	if cfg.Metadata == nil || (*cfg.Metadata)["AWS_REGION"] != "us-west-2" {
		t.Fatalf("Metadata AWS_REGION = %#v", cfg.Metadata)
	}
}

func TestAppConfigDedupesProfiles(t *testing.T) {
	cfg := app{
		Endpoint: new("https://service.us-east-1.api.aws/mcp"),
		Profiles: []string{
			"default",
			"dev",
			"default",
			"",
		},
	}.config(lookupEnv(nil))

	if cfg.Profiles == nil {
		t.Fatal("Profiles = nil")
	}
	if got := strings.Join(*cfg.Profiles, ","); got != "default,dev" {
		t.Fatalf("Profiles = %q", got)
	}
}

func TestAppConfigLeavesOmittedOptionalValuesUnset(t *testing.T) {
	cfg := app{
		Endpoint: new("https://service.us-east-1.api.aws/mcp"),
	}.config(lookupEnv(nil))

	if cfg.CaBundle != nil {
		t.Fatalf("CaBundle = %#v, want nil", cfg.CaBundle)
	}
	if cfg.Metadata != nil {
		t.Fatalf("Metadata = %#v, want nil", cfg.Metadata)
	}
	if cfg.Profiles != nil {
		t.Fatalf("Profiles = %#v, want nil", cfg.Profiles)
	}
	if cfg.ReadOnly != nil || cfg.Retries != nil || cfg.Timeout != nil {
		t.Fatalf("optional defaults were unexpectedly set: %+v", cfg)
	}
}

func lookupEnv(values map[string]string) LookupEnv {
	return func(name string) (string, bool) {
		value, ok := values[name]
		return value, ok
	}
}
