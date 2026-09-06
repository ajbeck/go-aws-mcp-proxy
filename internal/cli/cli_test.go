package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/ajbeck/go-aws-mcp-proxy/proxy"
	"github.com/aws/aws-sdk-go-v2/credentials/processcreds"
	"github.com/modelcontextprotocol/go-sdk/mcp"
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
	assertDuration(t, "Timeout", run.config.Timeout, 180*time.Second)
	assertDuration(t, "ConnectTimeout", run.config.ConnectTimeout, 60*time.Second)
	assertDuration(t, "ReadTimeout", run.config.ReadTimeout, 120*time.Second)
	assertDuration(t, "WriteTimeout", run.config.WriteTimeout, 180*time.Second)
	assertDuration(t, "ToolTimeout", run.config.ToolTimeout, 300*time.Second)
}

func TestCredentialProcessStdinIsolation(t *testing.T) {
	if os.Getenv("AWS_MCP_PROXY_CREDENTIAL_HELPER") == "1" {
		credentialProcessHelper()
		return
	}

	originalStdin := os.Stdin
	pipeReader, pipeWriter, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe() error = %v", err)
	}
	os.Stdin = pipeReader
	t.Cleanup(func() {
		os.Stdin = originalStdin
		_ = pipeReader.Close()
		_ = pipeWriter.Close()
	})

	transport, restore, err := isolatedStdioTransport()
	if err != nil {
		t.Fatalf("isolatedStdioTransport() error = %v", err)
	}
	t.Cleanup(restore)
	if transport.Reader != pipeReader {
		t.Fatal("MCP transport did not retain the original stdin pipe")
	}
	if _, ok := transport.Writer.(nopWriteCloser); !ok {
		t.Fatalf("MCP transport writer = %T, want nopWriteCloser", transport.Writer)
	}

	executable, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable() error = %v", err)
	}
	t.Setenv("AWS_MCP_PROXY_CREDENTIAL_HELPER", "1")
	t.Setenv("AWS_MCP_PROXY_TEST_BINARY", executable)
	command := `"$AWS_MCP_PROXY_TEST_BINARY" -test.run=^TestCredentialProcessStdinIsolation$`
	if runtime.GOOS == "windows" {
		command = `"%AWS_MCP_PROXY_TEST_BINARY%" -test.run=^TestCredentialProcessStdinIsolation$`
	}
	provider := processcreds.NewProvider(command)
	credentials, err := provider.Retrieve(t.Context())
	if err != nil {
		t.Fatalf("credential process Retrieve() error = %v", err)
	}
	if credentials.AccessKeyID != "test-access-key" || credentials.SecretAccessKey != "test-secret-key" {
		t.Fatalf("credentials = %#v", credentials)
	}
}

func credentialProcessHelper() {
	buffer := make([]byte, 1)
	if _, err := os.Stdin.Read(buffer); !errors.Is(err, io.EOF) {
		fmt.Fprintf(os.Stderr, "credential helper stdin error = %v, want EOF\n", err)
		os.Exit(2)
	}
	fmt.Print(`{"Version":1,"AccessKeyId":"test-access-key","SecretAccessKey":"test-secret-key"}`)
	os.Exit(0)
}

func TestIsolatedStdioTransportRestoresStdin(t *testing.T) {
	original := os.Stdin
	_, restore, err := isolatedStdioTransport()
	if err != nil {
		t.Fatalf("isolatedStdioTransport() error = %v", err)
	}
	if os.Stdin == original {
		t.Fatal("os.Stdin was not isolated")
	}
	restore()
	if os.Stdin != original {
		t.Fatal("os.Stdin was not restored")
	}
}

func TestIsolatedStdioTransportUsesSDKIOTransport(t *testing.T) {
	transport, restore, err := isolatedStdioTransport()
	if err != nil {
		t.Fatalf("isolatedStdioTransport() error = %v", err)
	}
	defer restore()
	var _ mcp.Transport = transport
}

func TestRunAcceptsGroupedProfilesAndMetadata(t *testing.T) {
	run := &fakeProxyRun{}
	var stderr bytes.Buffer

	code := Run(t.Context(), []string{
		"https://service.us-east-1.api.aws/mcp",
		"--profile", "prod", "dev", "staging",
		"--metadata", "A=1", "B=two=parts",
		"--retries", "10",
	}, Options{
		LookupEnv: lookupEnv(nil),
		RunProxy:  run.call,
		Stderr:    &stderr,
	})

	if code != exitOK {
		t.Fatalf("Run() code = %d, stderr = %q", code, stderr.String())
	}
	if run.config.Profiles == nil || strings.Join(*run.config.Profiles, ",") != "prod,dev,staging" {
		t.Fatalf("Profiles = %#v", run.config.Profiles)
	}
	if run.config.Metadata == nil || (*run.config.Metadata)["A"] != "1" || (*run.config.Metadata)["B"] != "two=parts" {
		t.Fatalf("Metadata = %#v", run.config.Metadata)
	}
	if run.config.Retries == nil || *run.config.Retries != 10 {
		t.Fatalf("Retries = %#v", run.config.Retries)
	}
}

func TestRunValidatesRetryRange(t *testing.T) {
	for _, retries := range []string{"-1", "11"} {
		t.Run(retries, func(t *testing.T) {
			run := &fakeProxyRun{}
			var stderr bytes.Buffer
			code := Run(t.Context(), []string{
				"https://service.us-east-1.api.aws/mcp",
				"--retries", retries,
			}, Options{
				LookupEnv: lookupEnv(nil),
				RunProxy:  run.call,
				Stderr:    &stderr,
			})

			if code == exitOK || run.called {
				t.Fatalf("Run() code = %d, called = %v", code, run.called)
			}
			if !strings.Contains(stderr.String(), "between 0 and 10") {
				t.Fatalf("stderr = %q", stderr.String())
			}
		})
	}
}

func TestRunRejectsNegativeTimeouts(t *testing.T) {
	flags := []string{"--timeout", "--connect-timeout", "--read-timeout", "--write-timeout", "--tool-timeout"}
	for _, flag := range flags {
		t.Run(flag, func(t *testing.T) {
			run := &fakeProxyRun{}
			var stderr bytes.Buffer
			code := Run(t.Context(), []string{
				"https://service.us-east-1.api.aws/mcp",
				flag, "-1",
			}, Options{
				LookupEnv: lookupEnv(nil),
				RunProxy:  run.call,
				Stderr:    &stderr,
			})

			if code == exitOK || run.called {
				t.Fatalf("Run() code = %d, called = %v", code, run.called)
			}
			if !strings.Contains(stderr.String(), "greater than or equal to 0") {
				t.Fatalf("stderr = %q", stderr.String())
			}
		})
	}
}

func TestRunAcceptsZeroTimeouts(t *testing.T) {
	run := &fakeProxyRun{}
	var stderr bytes.Buffer
	code := Run(t.Context(), []string{
		"https://service.us-east-1.api.aws/mcp",
		"--timeout", "0",
		"--connect-timeout", "0",
		"--read-timeout", "0",
		"--write-timeout", "0",
		"--tool-timeout", "0",
	}, Options{
		LookupEnv: lookupEnv(nil),
		RunProxy:  run.call,
		Stderr:    &stderr,
	})

	if code != exitOK {
		t.Fatalf("Run() code = %d, stderr = %q", code, stderr.String())
	}
	for name, value := range map[string]*time.Duration{
		"Timeout":        run.config.Timeout,
		"ConnectTimeout": run.config.ConnectTimeout,
		"ReadTimeout":    run.config.ReadTimeout,
		"WriteTimeout":   run.config.WriteTimeout,
		"ToolTimeout":    run.config.ToolTimeout,
	} {
		assertDuration(t, name, value, 0)
	}
}

func TestRunRejectsConflictingAuthModes(t *testing.T) {
	var stderr bytes.Buffer

	code := Run(t.Context(), []string{
		"https://service.us-east-1.api.aws/mcp",
		"--skip-auth",
		"--optional-auth",
	}, Options{
		LookupEnv: lookupEnv(nil),
		RunProxy:  (&fakeProxyRun{}).call,
		Stderr:    &stderr,
	})

	if code == exitOK {
		t.Fatalf("Run() code = %d, want non-zero for conflicting auth modes", code)
	}
	if !strings.Contains(stderr.String(), "cannot be used together") {
		t.Fatalf("Run() stderr = %q, want conflicting auth modes error", stderr.String())
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
		AllowEmptyTools:  new(true),
		LazyConnect:      new(true),
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
		OptionalAuth:     new(true),
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
	if cfg.AllowEmptyTools == nil || !*cfg.AllowEmptyTools {
		t.Fatalf("AllowEmptyTools = %#v, want true", cfg.AllowEmptyTools)
	}
	if cfg.LazyConnect == nil || !*cfg.LazyConnect {
		t.Fatalf("LazyConnect = %#v, want true", cfg.LazyConnect)
	}
	if cfg.Timeout == nil || *cfg.Timeout != 10500*time.Millisecond {
		t.Fatalf("Timeout = %#v", cfg.Timeout)
	}
	if cfg.ConnectTimeout == nil || *cfg.ConnectTimeout != 2*time.Second || cfg.ReadTimeout == nil || *cfg.ReadTimeout != 3*time.Second ||
		cfg.WriteTimeout == nil || *cfg.WriteTimeout != 4*time.Second || cfg.ToolTimeout == nil || *cfg.ToolTimeout != 5*time.Second {
		t.Fatalf("unexpected timeouts: %+v", cfg)
	}
	if cfg.DisableTelemetry == nil || !*cfg.DisableTelemetry || cfg.SkipAuth == nil || !*cfg.SkipAuth || cfg.OptionalAuth == nil || !*cfg.OptionalAuth {
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

func TestAppConfigUsesDocumentedProfilePrecedence(t *testing.T) {
	tests := []struct {
		name     string
		cli      []string
		env      map[string]string
		want     string
		wantNone bool
	}{
		{
			name: "proxy_environment_overrides_cli_and_aws_profile",
			cli:  []string{"cli"},
			env: map[string]string{
				"AWS_MCP_PROXY_PROFILES": "prod dev prod",
				"AWS_PROFILE":            "legacy",
			},
			want: "prod,dev",
		},
		{
			name: "cli_overrides_aws_profile",
			cli:  []string{"cli"},
			env:  map[string]string{"AWS_PROFILE": "legacy"},
			want: "cli",
		},
		{
			name: "aws_profile_is_fallback",
			env:  map[string]string{"AWS_PROFILE": "legacy"},
			want: "legacy",
		},
		{
			name:     "empty_proxy_environment_still_overrides_cli",
			cli:      []string{"cli"},
			env:      map[string]string{"AWS_MCP_PROXY_PROFILES": ""},
			wantNone: true,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cfg := app{
				Endpoint: new("https://service.us-east-1.api.aws/mcp"),
				Profiles: test.cli,
			}.config(lookupEnv(test.env))
			if test.wantNone {
				if cfg.Profiles != nil {
					t.Fatalf("Profiles = %#v, want nil", cfg.Profiles)
				}
				return
			}
			if cfg.Profiles == nil || strings.Join(*cfg.Profiles, ",") != test.want {
				t.Fatalf("Profiles = %#v, want %q", cfg.Profiles, test.want)
			}
		})
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
	if cfg.AllowEmptyTools != nil || cfg.LazyConnect != nil || cfg.ReadOnly != nil || cfg.Retries != nil || cfg.Timeout != nil {
		t.Fatalf("optional defaults were unexpectedly set: %+v", cfg)
	}
}

func lookupEnv(values map[string]string) LookupEnv {
	return func(name string) (string, bool) {
		value, ok := values[name]
		return value, ok
	}
}

func assertDuration(t *testing.T, name string, got *time.Duration, want time.Duration) {
	t.Helper()
	if got == nil || *got != want {
		t.Fatalf("%s = %#v, want %s", name, got, want)
	}
}
