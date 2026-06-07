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

	"github.com/ajbeck/go-aws-mcp-proxy/internal/proxy"
)

const (
	exitOK    = 0
	exitError = 1
	exitUsage = 2
)

type RunProxy func(context.Context, proxy.Config, *slog.Logger) error

type Options struct {
	LookupEnv proxy.LookupEnv
	RunProxy  RunProxy
	Stderr    io.Writer
	Stdout    io.Writer
	Version   string
}

type app struct {
	Version kong.VersionFlag `help:"Print version information and exit."`

	Endpoint string `arg:"" help:"SigV4 MCP endpoint URL."`

	Service  string   `help:"AWS service name for SigV4 signing. Inferred from endpoint when omitted."`
	Profiles []string `name:"profile" env:"AWS_MCP_PROXY_PROFILES,AWS_PROFILE" help:"AWS profile(s) to use. First profile is the default." sep:" " placeholder:"PROFILE"`
	Region   string   `help:"AWS region to sign. Inferred from endpoint or AWS_REGION when omitted."`
	CaBundle string   `name:"ca-bundle" env:"AWS_CA_BUNDLE" help:"Path to a PEM certificate bundle to trust in addition to the system roots." placeholder:"PATH"`

	Metadata map[string]string `help:"Metadata to inject into MCP requests as key=value pairs." mapsep:"none" placeholder:"KEY=VALUE"`

	ReadOnly bool `name:"read-only" help:"Disable tools that do not advertise readOnlyHint=true."`

	LogLevel string `name:"log-level" default:"ERROR" enum:"DEBUG,INFO,WARNING,ERROR,CRITICAL" help:"Set the logging level."`
	Retries  int    `default:"0" help:"Number of retries when calling endpoint MCP. 0 disables retries."`

	Timeout        float64 `default:"180" help:"Total timeout in seconds when connecting to endpoint."`
	ConnectTimeout float64 `name:"connect-timeout" default:"60" help:"Connection timeout in seconds."`
	ReadTimeout    float64 `name:"read-timeout" default:"120" help:"Read timeout in seconds."`
	WriteTimeout   float64 `name:"write-timeout" default:"180" help:"Write timeout in seconds."`
	ToolTimeout    float64 `name:"tool-timeout" default:"300" help:"Maximum seconds a tool call may take before cancellation."`

	DisableTelemetry bool `name:"disable-telemetry" help:"Disable client telemetry in outbound user-agent data."`
	SkipAuth         bool `name:"skip-auth" help:"Send unsigned requests when AWS credentials are unavailable."`
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

	_, err = parser.Parse(args)
	if exited {
		return exitCode
	}
	if err != nil {
		fmt.Fprintln(options.Stderr, err)
		return exitUsage
	}
	cfg := application.config(options.LookupEnv)
	if err := options.RunProxy(ctx, cfg, newLogger(cfg.LogLevel, options.Stderr)); err != nil {
		fmt.Fprintln(options.Stderr, err)
		return exitCodeForError(err)
	}

	return exitOK
}

func exitCodeForError(err error) int {
	var parseErr *kong.ParseError
	if errors.As(err, &parseErr) {
		return exitUsage
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

func (a app) config(lookupEnv proxy.LookupEnv) proxy.Config {
	if lookupEnv == nil {
		lookupEnv = os.LookupEnv
	}
	endpointService, endpointRegion := serviceNameAndRegionFromEndpoint(a.Endpoint)
	service := a.Service
	if service == "" {
		service = endpointService
	}
	region := a.Region
	if region == "" {
		region = endpointRegion
	}
	if region == "" {
		region, _ = lookupEnv("AWS_REGION")
	}
	return proxy.Config{
		Endpoint:         a.Endpoint,
		Service:          service,
		Profiles:         dedupe(a.Profiles),
		Region:           region,
		CaBundle:         a.CaBundle,
		Metadata:         a.Metadata,
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

func seconds(value float64) time.Duration {
	return time.Duration(value * float64(time.Second))
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
