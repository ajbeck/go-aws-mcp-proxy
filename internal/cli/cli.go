package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/alecthomas/kong"

	"github.com/ajbeck/go-aws-mcp-proxy/internal/proxyconfig"
)

const (
	exitOK    = 0
	exitError = 1
	exitUsage = 2
)

type ProxyRunner interface {
	RunProxy(context.Context, proxyconfig.Config) error
}

type Options struct {
	Env     proxyconfig.Env
	Runner  ProxyRunner
	Stderr  io.Writer
	Stdout  io.Writer
	Version string
}

type app struct {
	Version kong.VersionFlag `help:"Print version information and exit."`

	Endpoint string `arg:"" help:"SigV4 MCP endpoint URL."`

	Service  string   `help:"AWS service name for SigV4 signing. Inferred from endpoint when omitted."`
	Profiles []string `name:"profile" help:"AWS profile(s) to use. First profile is the default." sep:"none" placeholder:"PROFILE"`
	Region   string   `help:"AWS region to sign. Inferred from endpoint or AWS_REGION when omitted."`

	Metadata []string `help:"Metadata to inject into MCP requests as key=value pairs." sep:"none" placeholder:"KEY=VALUE"`

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
		kong.BindTo(ctx, (*context.Context)(nil)),
		kong.BindTo(options.Env, (*proxyconfig.Env)(nil)),
		kong.BindTo(options.Runner, (*ProxyRunner)(nil)),
	)
	if err != nil {
		fmt.Fprintln(options.Stderr, err)
		return exitError
	}

	kctx, err := parser.Parse(normalizeMultiValueFlags(args))
	if exited {
		return exitCode
	}
	if err != nil {
		fmt.Fprintln(options.Stderr, err)
		return exitUsage
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
		return exitUsage
	}
	var validationErr *proxyconfig.ValidationError
	if errors.As(err, &validationErr) {
		return exitUsage
	}
	return exitError
}

func (o Options) withDefaults() Options {
	if o.Env == nil {
		o.Env = proxyconfig.OSEnv{}
	}
	if o.Runner == nil {
		o.Runner = notImplementedRunner{}
	}
	if o.Stderr == nil {
		o.Stderr = os.Stderr
	}
	if o.Stdout == nil {
		o.Stdout = os.Stdout
	}
	if o.Version == "" {
		o.Version = "dev"
	}
	return o
}

func (a *app) Run(ctx context.Context, runner ProxyRunner, env proxyconfig.Env) error {
	cfg, err := proxyconfig.Resolve(proxyconfig.Input{
		Endpoint:         a.Endpoint,
		Service:          a.Service,
		Profiles:         a.Profiles,
		Region:           a.Region,
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
	}, env)
	if err != nil {
		return err
	}

	return runner.RunProxy(ctx, cfg)
}

func seconds(value float64) time.Duration {
	return time.Duration(value * float64(time.Second))
}

func normalizeMultiValueFlags(args []string) []string {
	var out []string
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--profile" || arg == "--metadata" {
			name := arg
			start := len(out)
			for i++; i < len(args) && !strings.HasPrefix(args[i], "-"); i++ {
				out = append(out, name+"="+args[i])
			}
			i--
			if len(out) == start && name == "--profile" {
				out = append(out, name)
			}
			continue
		}
		out = append(out, arg)
	}
	return out
}

type notImplementedRunner struct{}

func (notImplementedRunner) RunProxy(context.Context, proxyconfig.Config) error {
	return errors.New("proxy runtime is not implemented yet")
}
