package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/alecthomas/kong"

	"github.com/ajbeck/go-aws-mcp-proxy/proxy"
)

const (
	exitOK    = 0
	exitError = 1
)

type RunProxy func(context.Context, proxy.Config, *slog.Logger) error

type LookupEnv func(string) (string, bool)

type Options struct {
	LookupEnv LookupEnv
	RunProxy  RunProxy
	Stderr    io.Writer
	Stdout    io.Writer
	Version   string
}

type app struct {
	Version kong.VersionFlag `help:"Print version information and exit."`

	Endpoint *string `arg:"" help:"SigV4 MCP endpoint URL."`

	Service  *string  `help:"AWS service name for SigV4 signing. Inferred from endpoint when omitted."`
	Profiles []string `name:"profile" env:"AWS_MCP_PROXY_PROFILES,AWS_PROFILE" help:"AWS profile(s) to use. First profile is the default." sep:" " placeholder:"PROFILE"`
	Region   *string  `help:"AWS region to sign. Inferred from endpoint or AWS_REGION when omitted."`
	CaBundle *string  `name:"ca-bundle" env:"AWS_CA_BUNDLE" help:"Path to a PEM certificate bundle to trust in addition to the system roots." placeholder:"PATH"`

	Metadata map[string]string `help:"Metadata to inject into MCP requests as key=value pairs." mapsep:"none" placeholder:"KEY=VALUE"`

	ReadOnly *bool `name:"read-only" help:"Disable tools that do not advertise readOnlyHint=true."`

	LogLevel *string `name:"log-level" enum:"DEBUG,INFO,WARNING,ERROR,CRITICAL" help:"Set the logging level."`
	Retries  *int    `help:"Number of retries when calling endpoint MCP. Defaults to 3; 0 disables retries."`

	Timeout        *float64 `help:"Total timeout in seconds when connecting to endpoint."`
	ConnectTimeout *float64 `name:"connect-timeout" help:"Connection timeout in seconds."`
	ReadTimeout    *float64 `name:"read-timeout" help:"Read timeout in seconds."`
	WriteTimeout   *float64 `name:"write-timeout" help:"Write timeout in seconds."`
	ToolTimeout    *float64 `name:"tool-timeout" help:"Maximum seconds a tool call may take before cancellation."`

	DisableTelemetry *bool `name:"disable-telemetry" help:"Disable client telemetry in outbound user-agent data."`
	SkipAuth         *bool `name:"skip-auth" help:"Send unsigned requests when AWS credentials are unavailable."`
}

func (a *app) Run(ctx context.Context, lookupEnv LookupEnv, runProxy RunProxy, stderr io.Writer) error {
	cfg := a.config(lookupEnv)
	return runProxy(ctx, cfg, newLogger(valueOr(a.LogLevel, "ERROR"), stderr))
}

func Run(ctx context.Context, args []string, options Options) int {
	options = options.withDefaults()

	var application app
	var exitCode int
	exited := false

	parser, err := kong.New(
		&application,
		kong.Name("aws-mcp-proxy"),
		kong.Description("MCP Proxy for AWS"),
		kong.UsageOnError(),
		kong.WithHyphenPrefixedParameters(true),
		kong.Vars{"version": options.Version},
		kong.Bind(options.LookupEnv, options.RunProxy),
		kong.BindTo(ctx, (*context.Context)(nil)),
		kong.BindTo(options.Stderr, (*io.Writer)(nil)),
		kong.Writers(options.Stdout, options.Stderr),
		kong.Exit(func(code int) {
			exitCode = code
			exited = true
		}),
	)
	if err != nil {
		fmt.Fprintln(options.Stderr, err)
		return exitError
	}

	kctx, err := parser.Parse(args)
	if exited {
		return exitCode
	}
	if err != nil {
		fmt.Fprintln(options.Stderr, err)
		return exitCodeForError(err)
	}
	if err := kctx.Run(); err != nil {
		fmt.Fprintln(options.Stderr, err)
		return exitCodeForError(err)
	}

	return exitOK
}

func exitCodeForError(err error) int {
	var parseErr *kong.ParseError
	if errors.As(err, &parseErr) {
		return parseErr.ExitCode()
	}
	return exitError
}

func (o Options) withDefaults() Options {
	if o.LookupEnv == nil {
		o.LookupEnv = os.LookupEnv
	}
	if o.Version == "" {
		o.Version = "dev"
	}
	if o.RunProxy == nil {
		o.RunProxy = func(ctx context.Context, cfg proxy.Config, logger *slog.Logger) error {
			return proxy.Run(ctx, cfg, proxy.RunOptions{
				Logger:  logger,
				Version: o.Version,
			})
		}
	}
	if o.Stderr == nil {
		o.Stderr = os.Stderr
	}
	if o.Stdout == nil {
		o.Stdout = os.Stdout
	}
	return o
}

func (a app) config(lookupEnv LookupEnv) proxy.Config {
	if lookupEnv == nil {
		lookupEnv = os.LookupEnv
	}

	endpoint := value(a.Endpoint)
	endpointService, endpointRegion := serviceNameAndRegionFromEndpoint(endpoint)
	service := a.Service
	if service == nil && endpointService != "" {
		service = new(endpointService)
	}
	region := a.Region
	if region == nil && endpointRegion != "" {
		region = new(endpointRegion)
	}
	if region == nil {
		if value, ok := lookupEnv("AWS_REGION"); ok {
			region = new(value)
		}
	}

	cfg := proxy.Config{
		Endpoint:         a.Endpoint,
		Service:          service,
		Region:           region,
		CaBundle:         a.CaBundle,
		ReadOnly:         a.ReadOnly,
		LogLevel:         a.LogLevel,
		Retries:          a.Retries,
		Timeout:          seconds(a.Timeout),
		ConnectTimeout:   seconds(a.ConnectTimeout),
		ReadTimeout:      seconds(a.ReadTimeout),
		WriteTimeout:     seconds(a.WriteTimeout),
		ToolTimeout:      seconds(a.ToolTimeout),
		DisableTelemetry: a.DisableTelemetry,
		SkipAuth:         a.SkipAuth,
	}
	profiles := dedupe(a.Profiles)
	if len(profiles) > 0 {
		cfg.Profiles = new(profiles)
	}
	if len(a.Metadata) > 0 {
		cfg.Metadata = new(a.Metadata)
	}
	return cfg
}

func value[T any](ptr *T) T {
	if ptr == nil {
		var zero T
		return zero
	}
	return *ptr
}

func valueOr[T any](ptr *T, fallback T) T {
	if ptr == nil {
		return fallback
	}
	return *ptr
}

func seconds(value *float64) *time.Duration {
	if value == nil {
		return nil
	}
	return new(time.Duration(*value * float64(time.Second)))
}

func dedupe(values []string) []string {
	var out []string
	seen := map[string]bool{}
	for _, value := range values {
		if value == "" || seen[value] {
			continue
		}
		seen[value] = true
		out = append(out, value)
	}
	return out
}

func serviceNameAndRegionFromEndpoint(endpoint string) (string, string) {
	parsed, err := url.Parse(endpoint)
	if err != nil {
		return "", ""
	}
	host := parsed.Hostname()
	if host == "" {
		return "", ""
	}

	parts := strings.Split(host, ".")
	if len(parts) >= 4 {
		tail := parts[len(parts)-4:]
		if tail[0] == "bedrock-agentcore" && tail[2] == "amazonaws" && tail[3] == "com" {
			return "bedrock-agentcore", tail[1]
		}
	}
	if len(parts) == 4 && parts[2] == "api" && parts[3] == "aws" {
		return parts[0], parts[1]
	}
	if parts[0] != "" {
		return parts[0], ""
	}
	return "", ""
}

func newLogger(levelName string, w io.Writer) *slog.Logger {
	level := slog.LevelError
	switch strings.ToUpper(levelName) {
	case "DEBUG":
		level = slog.LevelDebug
	case "INFO":
		level = slog.LevelInfo
	case "WARNING":
		level = slog.LevelWarn
	case "ERROR":
		level = slog.LevelError
	case "CRITICAL":
		level = slog.LevelError + 4
	}

	return slog.New(slog.NewTextHandler(w, &slog.HandlerOptions{Level: level}))
}
