package proxy

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
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

type userAgentRoundTripper struct {
	base      http.RoundTripper
	userAgent string
}

type acceptRoundTripper struct {
	base http.RoundTripper
}

type upstreamErrorRoundTripper struct {
	base   http.RoundTripper
	logger *slog.Logger
}

type sigV4RoundTripper struct {
	base        http.RoundTripper
	clock       clock
	credentials credentialsProvider
	region      string
	service     string
	signer      signer
}

type clientOptions struct {
	ClientName    string
	ClientVersion string
	Credentials   credentialsProvider
	Logger        *slog.Logger
	Version       string
}

const maxHTTPErrorBodyBytes = 4096

func newClient(ctx context.Context, cfg Config, base *http.Client, options clientOptions) (*http.Client, error) {
	transport, err := newRoundTripper(ctx, cfg, baseRoundTripper(base), options)
	if err != nil {
		return nil, err
	}

	client := *http.DefaultClient
	if base != nil {
		client = *base
	}
	client.Transport = transport
	if positiveDuration(cfg.Timeout) {
		client.Timeout = *cfg.Timeout
	}
	return &client, nil
}

func newRoundTripper(ctx context.Context, cfg Config, base http.RoundTripper, options clientOptions) (http.RoundTripper, error) {
	if base == nil {
		base = http.DefaultTransport
	}

	base = roundTripperWithConnectionLimits(base)

	var caBundle []byte
	var err error
	if hasTransportTimeouts(cfg) {
		base, err = roundTripperWithTimeouts(base, cfg)
		if err != nil {
			return nil, err
		}
	}
	if cfg.CaBundle != nil {
		caBundle, err = readCABundle(*cfg.CaBundle)
		if err != nil {
			return nil, err
		}
		base, err = roundTripperWithCABundle(base, *cfg.CaBundle, caBundle)
		if err != nil {
			return nil, err
		}
	}

	rt := base
	if !enabled(cfg.SkipAuth) {
		if cfg.Service == nil {
			return nil, missingConfigError("service", reasonMissingService)
		}
		if cfg.Region == nil {
			return nil, missingConfigError("region", reasonMissingRegion)
		}
		credentials, err := signingCredentialsProvider(ctx, cfg, caBundle, options)
		if err != nil {
			return nil, err
		}
		rt = sigV4RoundTripper{
			base:        rt,
			clock:       systemClock{},
			credentials: credentials,
			region:      *cfg.Region,
			service:     *cfg.Service,
			signer:      v4.NewSigner(),
		}
	}
	if agent := userAgent(options, cfg.DisableTelemetry); agent != "" {
		rt = userAgentRoundTripper{base: rt, userAgent: agent}
	}
	rt = upstreamErrorRoundTripper{base: rt, logger: options.Logger}
	rt = acceptRoundTripper{base: rt}
	return rt, nil
}

func signingCredentialsProvider(ctx context.Context, cfg Config, caBundle []byte, options clientOptions) (credentialsProvider, error) {
	if options.Credentials != nil {
		return options.Credentials, nil
	}
	awsCfg, err := loadAWSConfig(ctx, cfg, caBundle)
	if err != nil {
		return nil, err
	}
	return awsCfg.Credentials, nil
}

func preflightSigningCredentials(ctx context.Context, provider credentialsProvider) error {
	if provider == nil {
		return credentialUnavailableError(errors.New("AWS credentials provider is not configured"))
	}
	credentials, err := provider.Retrieve(ctx)
	if err != nil {
		return credentialUnavailableError(err)
	}
	if !credentials.HasKeys() {
		return credentialUnavailableError(errors.New("AWS credentials are empty"))
	}
	return nil
}

func userAgent(options clientOptions, disableTelemetry *bool) string {
	version := options.Version
	if version == "" {
		version = "dev"
	}
	agent := "go/" + runtime.Version() + " aws-mcp-proxy/" + sanitizeUserAgentToken(version)
	if enabled(disableTelemetry) || options.ClientName == "" || options.ClientVersion == "" {
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

func loadAWSConfig(ctx context.Context, cfg Config, caBundle []byte) (aws.Config, error) {
	options := []func(*config.LoadOptions) error{
		config.WithRegion(value(cfg.Region)),
	}
	if profile := defaultProfile(cfg.Profiles); profile != nil {
		options = append(options, config.WithSharedConfigProfile(*profile))
	}
	if len(caBundle) > 0 {
		options = append(options, config.WithCustomCABundle(bytes.NewReader(caBundle)))
	}
	return config.LoadDefaultConfig(ctx, options...)
}

func baseRoundTripper(client *http.Client) http.RoundTripper {
	if client != nil && client.Transport != nil {
		return client.Transport
	}
	return http.DefaultTransport
}

func hasTransportTimeouts(cfg Config) bool {
	return positiveDuration(cfg.ConnectTimeout) || positiveDuration(cfg.ReadTimeout) || positiveDuration(cfg.WriteTimeout)
}

func roundTripperWithConnectionLimits(base http.RoundTripper) http.RoundTripper {
	transport, ok := base.(*http.Transport)
	if !ok {
		return base
	}

	cloned := transport.Clone()
	cloned.MaxIdleConnsPerHost = 1
	cloned.MaxConnsPerHost = 5
	return cloned
}

func roundTripperWithTimeouts(base http.RoundTripper, cfg Config) (http.RoundTripper, error) {
	transport, ok := base.(*http.Transport)
	if !ok {
		return nil, fmt.Errorf("timeouts require an *http.Transport base, got %T", base)
	}

	cloned := transport.Clone()
	applyHTTPTransportTimeouts(cloned, cfg)
	if hasConnectionDeadlines(cfg) {
		applyConnectionDeadlines(cloned, cfg)
	}

	return cloned, nil
}

func applyHTTPTransportTimeouts(transport *http.Transport, cfg Config) {
	if positiveDuration(cfg.ConnectTimeout) {
		transport.TLSHandshakeTimeout = *cfg.ConnectTimeout
	}
	if positiveDuration(cfg.ReadTimeout) {
		transport.ResponseHeaderTimeout = *cfg.ReadTimeout
	}
}

func hasConnectionDeadlines(cfg Config) bool {
	return positiveDuration(cfg.ConnectTimeout) || positiveDuration(cfg.ReadTimeout) || positiveDuration(cfg.WriteTimeout)
}

func applyConnectionDeadlines(transport *http.Transport, cfg Config) {
	transport.DialContext = deadlineDialContext(transport.DialContext, cfg)
	if transport.DialTLSContext != nil {
		transport.DialTLSContext = deadlineDialContext(transport.DialTLSContext, cfg)
	}
	transport.ForceAttemptHTTP2 = true
}

func deadlineDialContext(dial func(context.Context, string, string) (net.Conn, error), cfg Config) func(context.Context, string, string) (net.Conn, error) {
	if dial == nil {
		dialer := &net.Dialer{Timeout: value(cfg.ConnectTimeout)}
		dial = dialer.DialContext
	}

	return func(ctx context.Context, network, address string) (net.Conn, error) {
		if positiveDuration(cfg.ConnectTimeout) {
			var cancel context.CancelFunc
			ctx, cancel = context.WithTimeout(ctx, *cfg.ConnectTimeout)
			defer cancel()
		}

		conn, err := dial(ctx, network, address)
		if err != nil {
			return nil, err
		}
		if !positiveDuration(cfg.ReadTimeout) && !positiveDuration(cfg.WriteTimeout) {
			return conn, nil
		}
		return &deadlineConn{
			Conn:         conn,
			readTimeout:  value(cfg.ReadTimeout),
			writeTimeout: value(cfg.WriteTimeout),
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

func roundTripperWithCABundle(base http.RoundTripper, path string, bundle []byte) (http.RoundTripper, error) {
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

func required[T any](ptr *T, name string) (T, error) {
	if ptr == nil {
		var zero T
		return zero, fmt.Errorf("%s is required", name)
	}
	return *ptr, nil
}

func value[T any](ptr *T) T {
	if ptr == nil {
		var zero T
		return zero
	}
	return *ptr
}

func enabled(ptr *bool) bool {
	return ptr != nil && *ptr
}

func positiveDuration(ptr *time.Duration) bool {
	return ptr != nil && *ptr > 0
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

func (t userAgentRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	if t.userAgent == "" || req.Header.Get("User-Agent") != "" {
		return t.base.RoundTrip(req)
	}
	clone := req.Clone(req.Context())
	clone.Header.Set("User-Agent", t.userAgent)
	return t.base.RoundTrip(clone)
}

func (t acceptRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Header.Get("Accept") != "" {
		return t.base.RoundTrip(req)
	}
	clone := req.Clone(req.Context())
	clone.Header.Set("Accept", "application/json, text/event-stream")
	return t.base.RoundTrip(clone)
}

func (t upstreamErrorRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := t.base.RoundTrip(req)
	if err != nil || resp == nil || resp.StatusCode < 300 {
		return resp, err
	}

	upstreamErr := newUpstreamHTTPError(resp)
	if t.logger != nil {
		t.logger.LogAttrs(req.Context(), httpErrorLogLevel(upstreamErr.statusCode), "upstream HTTP request failed", upstreamErr.logAttrs(req)...)
	}
	resp.Body.Close()
	return nil, upstreamErr
}

func (t sigV4RoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	if t.credentials == nil {
		return nil, fmt.Errorf("AWS credentials provider is not configured")
	}
	body, err := readBody(req)
	if err != nil {
		return nil, err
	}
	credentials, err := t.credentials.Retrieve(req.Context())
	if err != nil {
		return nil, fmt.Errorf("retrieve AWS credentials: %w", err)
	}

	clone := req.Clone(req.Context())
	setBody(clone, body)
	if err := t.sign(clone, body, credentials); err != nil {
		return nil, err
	}
	return t.base.RoundTrip(clone)
}

type upstreamHTTPError struct {
	statusCode     int
	status         string
	contentType    string
	bodyExcerpt    string
	bodyTruncated  bool
	jsonrpcCode    *int64
	jsonrpcMessage string
	jsonrpcData    string
}

func (e *upstreamHTTPError) Error() string {
	if e.jsonrpcMessage != "" {
		return fmt.Sprintf("upstream HTTP %d: JSON-RPC error %d: %s", e.statusCode, value(e.jsonrpcCode), e.jsonrpcMessage)
	}
	if e.bodyExcerpt != "" {
		return fmt.Sprintf("upstream HTTP %d: %s", e.statusCode, e.bodyExcerpt)
	}
	return fmt.Sprintf("upstream HTTP %d: %s", e.statusCode, http.StatusText(e.statusCode))
}

func (e *upstreamHTTPError) logAttrs(req *http.Request) []slog.Attr {
	attrs := []slog.Attr{
		slog.String("method", req.Method),
		slog.String("url_scheme", req.URL.Scheme),
		slog.String("url_host", req.URL.Host),
		slog.String("url_path", req.URL.Path),
		slog.Int("status", e.statusCode),
		slog.String("content_type", e.contentType),
		slog.Bool("response_body_truncated", e.bodyTruncated),
	}
	if e.bodyExcerpt != "" {
		attrs = append(attrs, slog.String("response_body_excerpt", e.bodyExcerpt))
	}
	if e.jsonrpcCode != nil {
		attrs = append(attrs, slog.Int64("jsonrpc_code", *e.jsonrpcCode))
	}
	if e.jsonrpcMessage != "" {
		attrs = append(attrs, slog.String("jsonrpc_message", e.jsonrpcMessage))
	}
	if e.jsonrpcData != "" {
		attrs = append(attrs, slog.String("jsonrpc_data_excerpt", e.jsonrpcData))
	}
	return attrs
}

func newUpstreamHTTPError(resp *http.Response) *upstreamHTTPError {
	body, truncated := readHTTPErrorBody(resp)
	bodyExcerpt := strings.TrimSpace(string(body))
	err := &upstreamHTTPError{
		statusCode:    resp.StatusCode,
		status:        resp.Status,
		contentType:   resp.Header.Get("Content-Type"),
		bodyExcerpt:   bodyExcerpt,
		bodyTruncated: truncated,
	}
	err.parseJSONRPCError(body)
	return err
}

func readHTTPErrorBody(resp *http.Response) ([]byte, bool) {
	if resp.Body == nil {
		return nil, false
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxHTTPErrorBodyBytes+1))
	if err != nil {
		return []byte("failed to read upstream error response body: " + err.Error()), false
	}
	if len(body) > maxHTTPErrorBodyBytes {
		return body[:maxHTTPErrorBodyBytes], true
	}
	return body, false
}

func (e *upstreamHTTPError) parseJSONRPCError(body []byte) {
	var response struct {
		Error *struct {
			Code    int64           `json:"code"`
			Message string          `json:"message"`
			Data    json.RawMessage `json:"data,omitempty"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &response); err != nil || response.Error == nil {
		return
	}
	e.jsonrpcCode = new(response.Error.Code)
	e.jsonrpcMessage = response.Error.Message
	if len(response.Error.Data) > 0 {
		e.jsonrpcData = string(response.Error.Data)
	}
}

func httpErrorLogLevel(statusCode int) slog.Level {
	switch statusCode {
	case http.StatusTooManyRequests, http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return slog.LevelWarn
	default:
		return slog.LevelError
	}
}

func (t sigV4RoundTripper) sign(req *http.Request, body []byte, credentials aws.Credentials) error {
	signer := t.signer
	if signer == nil {
		signer = v4.NewSigner()
	}
	clock := t.clock
	if clock == nil {
		clock = systemClock{}
	}

	req.Header.Del("Connection")
	hash := sha256.Sum256(body)
	payloadHash := hex.EncodeToString(hash[:])
	if err := signer.SignHTTP(req.Context(), credentials, req, payloadHash, t.service, t.region, clock.Now()); err != nil {
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
