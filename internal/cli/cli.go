package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"os"
	"reflect"
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
	Profiles []string `name:"profile" type:"grouped-strings" help:"AWS profile(s) to use. First profile is the default." placeholder:"PROFILE"`
	Region   *string  `help:"AWS region to sign. Inferred from endpoint or AWS_REGION when omitted."`
	CaBundle *string  `name:"ca-bundle" env:"AWS_CA_BUNDLE" help:"Path to a PEM certificate bundle to trust in addition to the system roots." placeholder:"PATH"`

	Metadata map[string]string `type:"grouped-map" help:"Metadata to inject into MCP requests as key=value pairs." placeholder:"KEY=VALUE"`

	AllowEmptyTools *bool `name:"allow-empty-tools" help:"Allow an upstream endpoint to initialize with no tools."`
	ReadOnly        *bool `name:"read-only" help:"Disable tools that do not advertise readOnlyHint=true."`

	LogLevel *string `name:"log-level" enum:"DEBUG,INFO,WARNING,ERROR,CRITICAL" help:"Set the logging level."`
	Retries  *int    `help:"Number of retries for connection, discovery, and stream recovery. Defaults to 3; 0 disables retries."`

	Timeout        *float64 `help:"Total timeout in seconds when connecting to endpoint."`
	ConnectTimeout *float64 `name:"connect-timeout" help:"Connection timeout in seconds."`
	ReadTimeout    *float64 `name:"read-timeout" help:"Read timeout in seconds."`
	WriteTimeout   *float64 `name:"write-timeout" help:"Write timeout in seconds."`
	ToolTimeout    *float64 `name:"tool-timeout" help:"Maximum seconds a tool call may take before cancellation."`

	DisableTelemetry *bool `name:"disable-telemetry" help:"Disable client telemetry in outbound user-agent data."`
	SkipAuth         *bool `name:"skip-auth" help:"Always send unsigned requests without loading AWS credentials."`
	OptionalAuth     *bool `name:"optional-auth" help:"Sign requests when AWS credentials are available; otherwise send unsigned requests."`
}

func (a *app) Run(ctx context.Context, lookupEnv LookupEnv, runProxy RunProxy, stderr io.Writer) error {
	if enabled(a.SkipAuth) && enabled(a.OptionalAuth) {
		return errors.New("--skip-auth and --optional-auth cannot be used together")
	}
	if a.Retries != nil && (*a.Retries < 0 || *a.Retries > 10) {
		return fmt.Errorf("--retries must be between 0 and 10, got %d", *a.Retries)
	}
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
		kong.NamedMapper("grouped-strings", kong.MapperFunc(decodeGroupedStrings)),
		kong.NamedMapper("grouped-map", kong.MapperFunc(decodeGroupedMap)),
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
		AllowEmptyTools:  a.AllowEmptyTools,
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
		OptionalAuth:     a.OptionalAuth,
	}
	profiles := a.Profiles
	if value, ok := lookupEnv("AWS_MCP_PROXY_PROFILES"); ok {
		profiles = strings.Fields(value)
	} else if len(profiles) == 0 {
		if value, ok := lookupEnv("AWS_PROFILE"); ok {
			profiles = []string{value}
		}
	}
	profiles = dedupe(profiles)
	if len(profiles) > 0 {
		cfg.Profiles = new(profiles)
	}
	if len(a.Metadata) > 0 {
		cfg.Metadata = new(a.Metadata)
	}
	return cfg
}

func decodeGroupedStrings(ctx *kong.DecodeContext, target reflect.Value) error {
	count := 0
	for ctx.Scan.Peek().IsValue() {
		token, err := ctx.Scan.PopValue("string")
		if err != nil {
			return err
		}
		value, ok := token.Value.(string)
		if !ok {
			return fmt.Errorf("expected string but got %T", token.Value)
		}
		target.Set(reflect.Append(target, reflect.ValueOf(value)))
		count++
	}
	if count == 0 {
		return errors.New("missing value, expecting \"<string>...\"")
	}
	return nil
}

func decodeGroupedMap(ctx *kong.DecodeContext, target reflect.Value) error {
	count := 0
	for ctx.Scan.Peek().IsValue() {
		token, err := ctx.Scan.PopValue("key=value")
		if err != nil {
			return err
		}
		value, ok := token.Value.(string)
		if !ok {
			return fmt.Errorf("expected key=value string but got %T", token.Value)
		}
		parts := strings.SplitN(value, "=", 2)
		if len(parts) != 2 {
			return fmt.Errorf("expected \"<key>=<value>\" but got %q", value)
		}
		target.SetMapIndex(reflect.ValueOf(parts[0]), reflect.ValueOf(parts[1]))
		count++
	}
	if count == 0 {
		return errors.New("missing value, expecting \"<key>=<value>...\"")
	}
	return nil
}

func value[T any](ptr *T) T {
	if ptr == nil {
		var zero T
		return zero
	}
	return *ptr
}

func enabled(value *bool) bool {
	return value != nil && *value
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
