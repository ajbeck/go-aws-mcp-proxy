package proxy

import "time"

// Config describes a proxy run.
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

// LookupEnv looks up an environment variable by name.
type LookupEnv func(string) (string, bool)

func (c Config) httpConfig() httpConfig {
	return httpConfig{
		Service:          c.Service,
		Profile:          defaultProfile(c.Profiles),
		Region:           c.Region,
		CaBundle:         c.CaBundle,
		Metadata:         c.Metadata,
		Retries:          c.Retries,
		Timeout:          c.Timeout,
		ConnectTimeout:   c.ConnectTimeout,
		ReadTimeout:      c.ReadTimeout,
		WriteTimeout:     c.WriteTimeout,
		DisableTelemetry: c.DisableTelemetry,
		SkipAuth:         c.SkipAuth,
	}
}
