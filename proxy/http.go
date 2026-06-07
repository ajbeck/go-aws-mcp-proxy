package proxy

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"runtime"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	"github.com/aws/aws-sdk-go-v2/config"
)

type credentialsProvider interface {
	Retrieve(context.Context) (aws.Credentials, error)
}

type signer interface {
	SignHTTP(context.Context, aws.Credentials, *http.Request, string, string, string, time.Time, ...func(*v4.SignerOptions)) error
}

type clock interface {
	Now() time.Time
}

type systemClock struct{}

func (systemClock) Now() time.Time {
	return time.Now()
}

type transport struct {
	Base        http.RoundTripper
	Clock       clock
	Credentials credentialsProvider
	Metadata    map[string]string
	Logger      *slog.Logger
	Profile     string
	Region      string
	Retries     int
	RetryDelay  func(int) time.Duration
	Service     string
	Signer      signer
	SkipAuth    bool
	UserAgent   string
}

type httpConfig struct {
	Service          string
	Profile          string
	Region           string
	CaBundle         string
	Metadata         map[string]string
	Retries          int
	Timeout          time.Duration
	ConnectTimeout   time.Duration
	ReadTimeout      time.Duration
	WriteTimeout     time.Duration
	DisableTelemetry bool
	SkipAuth         bool
}

func newClient(ctx context.Context, cfg httpConfig, base *http.Client, loggers ...*slog.Logger) (*http.Client, error) {
	options := clientOptions{Logger: firstLogger(loggers)}
	transport, err := newTransportWithOptions(ctx, cfg, baseTransport(base), options)
	if err != nil {
		return nil, err
	}

	client := *http.DefaultClient
	if base != nil {
		client = *base
	}
	client.Transport = transport
	if cfg.Timeout > 0 {
		client.Timeout = cfg.Timeout
	}
	return &client, nil
}

func newTransport(ctx context.Context, cfg httpConfig, base http.RoundTripper, loggers ...*slog.Logger) (*transport, error) {
	return newTransportWithOptions(ctx, cfg, base, clientOptions{Logger: firstLogger(loggers)})
}

type clientOptions struct {
	ClientName    string
	ClientVersion string
	Logger        *slog.Logger
	Version       string
}

func newClientWithOptions(ctx context.Context, cfg httpConfig, base *http.Client, options clientOptions) (*http.Client, error) {
	transport, err := newTransportWithOptions(ctx, cfg, baseTransport(base), options)
	if err != nil {
		return nil, err
	}

	client := *http.DefaultClient
	if base != nil {
		client = *base
	}
	client.Transport = transport
	if cfg.Timeout > 0 {
		client.Timeout = cfg.Timeout
	}
	return &client, nil
}

func newTransportWithOptions(ctx context.Context, cfg httpConfig, base http.RoundTripper, options clientOptions) (*transport, error) {
	if base == nil {
		base = http.DefaultTransport
	}

	var caBundle []byte
	var err error
	if hasTransportTimeouts(cfg) {
		base, err = transportWithTimeouts(base, cfg)
		if err != nil {
			return nil, err
		}
	}
	if cfg.CaBundle != "" {
		caBundle, err = readCABundle(cfg.CaBundle)
		if err != nil {
			return nil, err
		}
		base, err = transportWithCABundle(base, cfg.CaBundle, caBundle)
		if err != nil {
			return nil, err
		}
	}

	var provider credentialsProvider
	if !cfg.SkipAuth {
		awsCfg, err := loadAWSConfig(ctx, cfg, caBundle)
		if err != nil {
			return nil, err
		}
		provider = awsCfg.Credentials
	}

	return &transport{
		Base:        base,
		Clock:       systemClock{},
		Credentials: provider,
		Logger:      options.Logger,
		Metadata:    cfg.Metadata,
		Profile:     cfg.Profile,
		Region:      cfg.Region,
		Retries:     cfg.Retries,
		Service:     cfg.Service,
		Signer:      v4.NewSigner(),
		SkipAuth:    cfg.SkipAuth,
		UserAgent:   userAgent(options, cfg.DisableTelemetry),
	}, nil
}

func firstLogger(loggers []*slog.Logger) *slog.Logger {
	if len(loggers) == 0 {
		return nil
	}
	return loggers[0]
}

func userAgent(options clientOptions, disableTelemetry bool) string {
	version := options.Version
	if version == "" {
		version = "dev"
	}
	agent := "go/" + runtime.Version() + " aws-mcp-proxy/" + sanitizeUserAgentToken(version)
	if disableTelemetry || options.ClientName == "" || options.ClientVersion == "" {
		return agent
	}
	return agent + " " + sanitizeClientName(options.ClientName) + "/" + sanitizeUserAgentToken(options.ClientVersion)
}

func sanitizeClientName(value string) string {
	value = strings.ToLower(value)
	value = strings.ReplaceAll(value, " ", "-")
	value = strings.ReplaceAll(value, "/", "-")
	return sanitizeUserAgentToken(value)
}

func sanitizeUserAgentToken(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return "unknown"
	}
	value = strings.ReplaceAll(value, " ", "-")
	value = strings.ReplaceAll(value, "/", "-")
	return value
}

func loadAWSConfig(ctx context.Context, cfg httpConfig, caBundle []byte) (aws.Config, error) {
	options := []func(*config.LoadOptions) error{
		config.WithRegion(cfg.Region),
	}
	if cfg.Profile != "" {
		options = append(options, config.WithSharedConfigProfile(cfg.Profile))
	}
	if len(caBundle) > 0 {
		options = append(options, config.WithCustomCABundle(bytes.NewReader(caBundle)))
	}
	return config.LoadDefaultConfig(ctx, options...)
}

func baseTransport(client *http.Client) http.RoundTripper {
	if client != nil && client.Transport != nil {
		return client.Transport
	}
	return http.DefaultTransport
}

func hasTransportTimeouts(cfg httpConfig) bool {
	return cfg.ConnectTimeout > 0 || cfg.ReadTimeout > 0 || cfg.WriteTimeout > 0
}

func transportWithTimeouts(base http.RoundTripper, cfg httpConfig) (http.RoundTripper, error) {
	transport, ok := base.(*http.Transport)
	if !ok {
		return nil, fmt.Errorf("timeouts require an *http.Transport base, got %T", base)
	}

	cloned := transport.Clone()
	if cfg.ConnectTimeout > 0 {
		cloned.TLSHandshakeTimeout = cfg.ConnectTimeout
	}
	if cfg.ReadTimeout > 0 {
		cloned.ResponseHeaderTimeout = cfg.ReadTimeout
	}
	if cfg.WriteTimeout > 0 {
		cloned.ExpectContinueTimeout = cfg.WriteTimeout
	}
	cloned.DialContext = timeoutDialContext(cloned.DialContext, cfg)
	if cloned.DialTLSContext != nil {
		cloned.DialTLSContext = timeoutDialContext(cloned.DialTLSContext, cfg)
	}

	return cloned, nil
}

func timeoutDialContext(dial func(context.Context, string, string) (net.Conn, error), cfg httpConfig) func(context.Context, string, string) (net.Conn, error) {
	if dial == nil {
		dialer := &net.Dialer{Timeout: cfg.ConnectTimeout}
		dial = dialer.DialContext
	}

	return func(ctx context.Context, network, address string) (net.Conn, error) {
		if cfg.ConnectTimeout > 0 {
			var cancel context.CancelFunc
			ctx, cancel = context.WithTimeout(ctx, cfg.ConnectTimeout)
			defer cancel()
		}

		conn, err := dial(ctx, network, address)
		if err != nil {
			return nil, err
		}
		if cfg.ReadTimeout <= 0 && cfg.WriteTimeout <= 0 {
			return conn, nil
		}
		return &deadlineConn{
			Conn:         conn,
			readTimeout:  cfg.ReadTimeout,
			writeTimeout: cfg.WriteTimeout,
		}, nil
	}
}

func readCABundle(path string) ([]byte, error) {
	bundle, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read CA bundle %q: %w", path, err)
	}
	return bundle, nil
}

func transportWithCABundle(base http.RoundTripper, path string, bundle []byte) (http.RoundTripper, error) {
	transport, ok := base.(*http.Transport)
	if !ok {
		return nil, fmt.Errorf("CA bundle %q requires an *http.Transport base, got %T", path, base)
	}

	pool, err := x509.SystemCertPool()
	if err != nil {
		return nil, fmt.Errorf("load system certificate pool: %w", err)
	}
	if pool == nil {
		pool = x509.NewCertPool()
	}
	if !pool.AppendCertsFromPEM(bundle) {
		return nil, fmt.Errorf("CA bundle %q does not contain PEM certificates", path)
	}

	cloned := transport.Clone()
	tlsConfig := cloned.TLSClientConfig.Clone()
	if tlsConfig == nil {
		tlsConfig = &tls.Config{}
	}
	tlsConfig.RootCAs = pool
	cloned.TLSClientConfig = tlsConfig

	return cloned, nil
}

type deadlineConn struct {
	net.Conn
	readTimeout  time.Duration
	writeTimeout time.Duration
}

func (c *deadlineConn) Read(b []byte) (int, error) {
	if c.readTimeout > 0 {
		_ = c.Conn.SetReadDeadline(time.Now().Add(c.readTimeout))
	}
	return c.Conn.Read(b)
}

func (c *deadlineConn) Write(b []byte) (int, error) {
	if c.writeTimeout > 0 {
		_ = c.Conn.SetWriteDeadline(time.Now().Add(c.writeTimeout))
	}
	return c.Conn.Write(b)
}

func (t *transport) RoundTrip(req *http.Request) (*http.Response, error) {
	base := t.Base
	if base == nil {
		base = http.DefaultTransport
	}

	body, err := readBody(req)
	if err != nil {
		return nil, err
	}
	body = injectMetadata(body, t.Metadata)

	maxRetries := t.Retries
	for attempt := 0; ; attempt++ {
		attemptReq := req.Clone(req.Context())
		setBody(attemptReq, body)
		t.applyHeaders(attemptReq)

		if !t.SkipAuth {
			if t.Credentials == nil {
				return nil, fmt.Errorf("AWS credentials provider is not configured")
			}
			credentials, err := t.Credentials.Retrieve(req.Context())
			if err != nil {
				return nil, fmt.Errorf("retrieve AWS credentials: %w", err)
			}
			if err := t.sign(attemptReq, body, credentials); err != nil {
				return nil, err
			}
		}

		start := time.Now()
		resp, err := base.RoundTrip(attemptReq)
		duration := time.Since(start)
		t.logRequest(attemptReq, resp, err, attempt, duration)
		if !t.shouldRetry(req.Context(), resp, err, attempt, maxRetries) {
			return resp, err
		}

		if resp != nil && resp.Body != nil {
			_ = resp.Body.Close()
		}
		t.logRetry(attemptReq, resp, err, attempt)
		if err := waitForRetry(req.Context(), t.retryDelay(attempt)); err != nil {
			return nil, err
		}
	}
}

func (t *transport) applyHeaders(req *http.Request) {
	if t.UserAgent != "" && req.Header.Get("User-Agent") == "" {
		req.Header.Set("User-Agent", t.UserAgent)
	}
}

func (t *transport) shouldRetry(ctx context.Context, resp *http.Response, err error, attempt, maxRetries int) bool {
	if maxRetries <= 0 || attempt >= maxRetries || ctx.Err() != nil {
		return false
	}
	if err != nil {
		return true
	}
	return resp != nil && retryableStatus(resp.StatusCode)
}

func retryableStatus(status int) bool {
	switch status {
	case http.StatusTooManyRequests, http.StatusInternalServerError, http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return true
	default:
		return false
	}
}

func (t *transport) retryDelay(attempt int) time.Duration {
	if t.RetryDelay != nil {
		return t.RetryDelay(attempt)
	}
	delay := 100 * time.Millisecond * (1 << attempt)
	if delay > 2*time.Second {
		return 2 * time.Second
	}
	return delay
}

func waitForRetry(ctx context.Context, delay time.Duration) error {
	if delay <= 0 {
		return nil
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func (t *transport) logRequest(req *http.Request, resp *http.Response, err error, attempt int, duration time.Duration) {
	if t.Logger == nil {
		return
	}

	attrs := []any{
		"method", req.Method,
		"host", req.URL.Host,
		"path", req.URL.Path,
		"duration_ms", duration.Milliseconds(),
		"attempt", attempt,
		"profile", t.Profile,
	}
	if resp != nil {
		attrs = append(attrs, "status", resp.StatusCode)
	}
	if err != nil {
		attrs = append(attrs, "error", err)
		t.Logger.Warn("upstream HTTP request failed", attrs...)
		return
	}
	if resp != nil && resp.StatusCode >= 400 {
		t.Logger.Warn("upstream HTTP request returned error status", attrs...)
		return
	}
	t.Logger.Debug("upstream HTTP request completed", append(attrs, "headers", sanitizedHeaders(req.Header))...)
}

func (t *transport) logRetry(req *http.Request, resp *http.Response, err error, attempt int) {
	if t.Logger == nil {
		return
	}
	attrs := []any{
		"method", req.Method,
		"host", req.URL.Host,
		"path", req.URL.Path,
		"attempt", attempt,
		"next_attempt", attempt + 1,
	}
	if resp != nil {
		attrs = append(attrs, "status", resp.StatusCode)
	}
	if err != nil {
		attrs = append(attrs, "error", err)
	}
	t.Logger.Warn("retrying upstream HTTP request", attrs...)
}

func (t *transport) sign(req *http.Request, body []byte, credentials aws.Credentials) error {
	signer := t.Signer
	if signer == nil {
		signer = v4.NewSigner()
	}
	clock := t.Clock
	if clock == nil {
		clock = systemClock{}
	}

	req.Header.Del("Connection")
	hash := sha256.Sum256(body)
	payloadHash := hex.EncodeToString(hash[:])
	if err := signer.SignHTTP(req.Context(), credentials, req, payloadHash, t.Service, t.Region, clock.Now()); err != nil {
		return fmt.Errorf("sign AWS request: %w", err)
	}
	return nil
}

func readBody(req *http.Request) ([]byte, error) {
	if req.Body == nil {
		return nil, nil
	}
	defer req.Body.Close()

	body, err := io.ReadAll(req.Body)
	if err != nil {
		return nil, err
	}
	return body, nil
}

func setBody(req *http.Request, body []byte) {
	req.Body = io.NopCloser(bytes.NewReader(body))
	req.ContentLength = int64(len(body))
	req.GetBody = func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(body)), nil
	}
}

func injectMetadata(body []byte, metadata map[string]string) []byte {
	if len(body) == 0 || len(metadata) == 0 {
		return body
	}

	var message map[string]any
	if err := json.Unmarshal(body, &message); err != nil {
		return body
	}
	if _, ok := message["jsonrpc"]; !ok {
		return body
	}

	params, _ := message["params"].(map[string]any)
	if params == nil {
		params = map[string]any{}
		message["params"] = params
	}

	existing, _ := params["_meta"].(map[string]any)
	if existing == nil {
		existing = map[string]any{}
	}
	meta := map[string]any{}
	for key, value := range metadata {
		meta[key] = value
	}
	for key, value := range existing {
		meta[key] = value
	}
	params["_meta"] = meta

	encoded, err := json.Marshal(message)
	if err != nil {
		return body
	}
	return encoded
}

func redactHeader(name, value string) string {
	switch strings.ToLower(name) {
	case "authorization", "x-amz-security-token", "x-amz-date":
		return "[REDACTED]"
	default:
		return value
	}
}

func sanitizedHeaders(headers http.Header) map[string][]string {
	out := make(map[string][]string, len(headers))
	for name, values := range headers {
		redacted := make([]string, len(values))
		for i, value := range values {
			redacted[i] = redactHeader(name, value)
		}
		out[name] = redacted
	}
	return out
}
