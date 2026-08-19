package proxy

import (
	"errors"
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

	ReadOnly *bool
	LogLevel *string
	Retries  *int

	Timeout        *time.Duration
	ConnectTimeout *time.Duration
	ReadTimeout    *time.Duration
	WriteTimeout   *time.Duration
	ToolTimeout    *time.Duration

	DisableTelemetry *bool
	SkipAuth         *bool
	OptionalAuth     *bool
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
