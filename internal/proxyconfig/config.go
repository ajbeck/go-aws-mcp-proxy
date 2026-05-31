package proxyconfig

import (
	"fmt"
	"net/url"
	"os"
	"strings"
	"time"
)

type Env interface {
	LookupEnv(string) (string, bool)
}

type OSEnv struct{}

func (OSEnv) LookupEnv(name string) (string, bool) {
	return os.LookupEnv(name)
}

type MapEnv map[string]string

func (e MapEnv) LookupEnv(name string) (string, bool) {
	value, ok := e[name]
	return value, ok
}

type Input struct {
	Endpoint string
	Service  string
	Profiles []string
	Region   string
	CaBundle string
	Metadata []string

	ReadOnly bool
	LogLevel string
	Retries  int

	Timeout        time.Duration
	ConnectTimeout time.Duration
	ReadTimeout    time.Duration
	WriteTimeout   time.Duration
	ToolTimeout    time.Duration

	DisableTelemetry bool
	SkipAuth         bool
}

type Config struct {
	Endpoint string
	Service  string
	Profiles []string
	Region   string
	CaBundle string
	Metadata map[string]string

	ReadOnly bool
	LogLevel string
	Retries  int

	Timeout        time.Duration
	ConnectTimeout time.Duration
	ReadTimeout    time.Duration
	WriteTimeout   time.Duration
	ToolTimeout    time.Duration

	DisableTelemetry bool
	SkipAuth         bool
}

type ValidationError struct {
	Err error
}

func (e *ValidationError) Error() string {
	return e.Err.Error()
}

func (e *ValidationError) Unwrap() error {
	return e.Err
}

func Resolve(input Input, env Env) (Config, error) {
	if env == nil {
		env = OSEnv{}
	}
	if err := validateEndpointURL(input.Endpoint); err != nil {
		return Config{}, err
	}
	if input.Retries < 0 || input.Retries > 10 {
		return Config{}, invalid("retries must be between 0 and 10")
	}
	for name, value := range map[string]time.Duration{
		"timeout":         input.Timeout,
		"connect-timeout": input.ConnectTimeout,
		"read-timeout":    input.ReadTimeout,
		"write-timeout":   input.WriteTimeout,
		"tool-timeout":    input.ToolTimeout,
	} {
		if value < 0 {
			return Config{}, invalid("%s must be >= 0", name)
		}
	}

	endpointService, endpointRegion := serviceNameAndRegionFromEndpoint(input.Endpoint)
	service := input.Service
	if service == "" {
		service = endpointService
	}
	if service == "" {
		return Config{}, invalid("could not determine AWS service name from endpoint %q; pass --service", input.Endpoint)
	}

	region := input.Region
	if region == "" {
		region = endpointRegion
	}
	if region == "" {
		region, _ = env.LookupEnv("AWS_REGION")
	}
	if region == "" {
		return Config{}, invalid("could not determine AWS region from endpoint %q or AWS_REGION; pass --region", input.Endpoint)
	}

	metadata, err := resolveMetadata(region, input.Metadata)
	if err != nil {
		return Config{}, err
	}

	caBundle := input.CaBundle
	if caBundle == "" {
		caBundle, _ = env.LookupEnv("AWS_CA_BUNDLE")
	}

	return Config{
		Endpoint:         input.Endpoint,
		Service:          service,
		Profiles:         resolveProfiles(input.Profiles, env),
		Region:           region,
		CaBundle:         caBundle,
		Metadata:         metadata,
		ReadOnly:         input.ReadOnly,
		LogLevel:         input.LogLevel,
		Retries:          input.Retries,
		Timeout:          input.Timeout,
		ConnectTimeout:   input.ConnectTimeout,
		ReadTimeout:      input.ReadTimeout,
		WriteTimeout:     input.WriteTimeout,
		ToolTimeout:      input.ToolTimeout,
		DisableTelemetry: input.DisableTelemetry,
		SkipAuth:         input.SkipAuth,
	}, nil
}

func resolveProfiles(profiles []string, env Env) []string {
	if value, ok := env.LookupEnv("AWS_MCP_PROXY_PROFILES"); ok && strings.TrimSpace(value) != "" {
		return dedupe(strings.Fields(value))
	}
	if len(profiles) > 0 {
		return dedupe(profiles)
	}
	if value, ok := env.LookupEnv("AWS_PROFILE"); ok && value != "" {
		return []string{value}
	}
	return nil
}

func resolveMetadata(region string, items []string) (map[string]string, error) {
	metadata := map[string]string{"AWS_REGION": region}
	for _, item := range items {
		key, value, ok := strings.Cut(item, "=")
		if !ok || key == "" {
			return nil, invalid("metadata must be in key=value format, got %q", item)
		}
		metadata[key] = value
	}
	return metadata, nil
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

func validateEndpointURL(endpoint string) error {
	parsed, err := url.Parse(endpoint)
	if err != nil {
		return invalid("invalid endpoint URL %q: %w", endpoint, err)
	}
	if parsed.Scheme == "" {
		return invalid("invalid endpoint URL %q: missing URL scheme; use https://", endpoint)
	}
	if parsed.Scheme == "https" {
		return nil
	}
	if parsed.Scheme == "http" && isLocalhost(parsed.Hostname()) {
		return nil
	}
	if parsed.Scheme == "http" {
		return invalid("invalid endpoint URL %q: HTTP is not allowed for remote endpoints; use https://", endpoint)
	}
	return invalid("invalid endpoint URL %q: unsupported scheme %q", endpoint, parsed.Scheme)
}

func invalid(format string, args ...any) error {
	return &ValidationError{Err: fmt.Errorf(format, args...)}
}

func isLocalhost(host string) bool {
	switch host {
	case "localhost", "127.0.0.1", "::1":
		return true
	default:
		return false
	}
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
