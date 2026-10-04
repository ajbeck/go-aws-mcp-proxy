package proxy

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
)

// Config describes a proxy run.
type Config struct {
	Endpoint *string
	Service  *string
	Profiles *[]string
	Region   *string
	CaBundle *string
	Metadata *map[string]string
	Headers  *map[string]string

	AllowEmptyTools *bool
	LazyConnect     *bool
	ReadOnly        *bool
	LogLevel        *string
	Retries         *int

	Timeout        *time.Duration
	ConnectTimeout *time.Duration
	ReadTimeout    *time.Duration
	WriteTimeout   *time.Duration
	ToolTimeout    *time.Duration

	DisableTelemetry *bool
	SkipAuth         *bool
	OptionalAuth     *bool
}

// reservedHeaders are set by SigV4 signing and cannot be supplied as extra
// headers.
var reservedHeaders = []string{"authorization", "date", "x-amz-date", "x-amz-security-token"}

func validateHeaders(headers map[string]string) error {
	for name := range headers {
		if slices.Contains(reservedHeaders, strings.ToLower(name)) {
			return fmt.Errorf("--header %s is not allowed: it is set by SigV4 signing. Reserved headers: %s", name, strings.Join(reservedHeaders, ", "))
		}
	}
	return nil
}

type authMode int

const (
	authModeRequired authMode = iota
	authModeOptional
	authModeSkipped
)

func authModeFor(cfg Config) (authMode, error) {
	if enabled(cfg.SkipAuth) && enabled(cfg.OptionalAuth) {
		return authModeRequired, errors.New("--skip-auth and --optional-auth cannot be used together")
	}
	if enabled(cfg.SkipAuth) {
		return authModeSkipped, nil
	}
	if enabled(cfg.OptionalAuth) {
		return authModeOptional, nil
	}
	return authModeRequired, nil
}
