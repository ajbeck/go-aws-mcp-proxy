package proxy

import "time"

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
}
