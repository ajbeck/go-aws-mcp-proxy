package awshttp

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
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	"github.com/aws/aws-sdk-go-v2/config"

	"github.com/ajbeck/go-aws-mcp-proxy/internal/proxyconfig"
)

type CredentialsProvider interface {
	Retrieve(context.Context) (aws.Credentials, error)
}

type Signer interface {
	SignHTTP(context.Context, aws.Credentials, *http.Request, string, string, string, time.Time, ...func(*v4.SignerOptions)) error
}

type Clock interface {
	Now() time.Time
}

type SystemClock struct{}

func (SystemClock) Now() time.Time {
	return time.Now()
}

type Transport struct {
	Base        http.RoundTripper
	Clock       Clock
	Credentials CredentialsProvider
	Metadata    map[string]string
	Region      string
	Service     string
	Signer      Signer
	SkipAuth    bool
}

func NewClient(ctx context.Context, cfg proxyconfig.Config, base *http.Client) (*http.Client, error) {
	transport, err := NewTransport(ctx, cfg, baseTransport(base))
	if err != nil {
		return nil, err
	}

	client := *http.DefaultClient
	if base != nil {
		client = *base
	}
	client.Transport = transport
	return &client, nil
}

func NewTransport(ctx context.Context, cfg proxyconfig.Config, base http.RoundTripper) (*Transport, error) {
	if base == nil {
		base = http.DefaultTransport
	}

	var caBundle []byte
	var err error
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

	var provider CredentialsProvider
	if !cfg.SkipAuth {
		awsCfg, err := loadAWSConfig(ctx, cfg, caBundle)
		if err != nil {
			return nil, err
		}
		provider = awsCfg.Credentials
	}

	return &Transport{
		Base:        base,
		Clock:       SystemClock{},
		Credentials: provider,
		Metadata:    cfg.Metadata,
		Region:      cfg.Region,
		Service:     cfg.Service,
		Signer:      v4.NewSigner(),
		SkipAuth:    cfg.SkipAuth,
	}, nil
}

func loadAWSConfig(ctx context.Context, cfg proxyconfig.Config, caBundle []byte) (aws.Config, error) {
	options := []func(*config.LoadOptions) error{
		config.WithRegion(cfg.Region),
	}
	if len(cfg.Profiles) > 0 {
		options = append(options, config.WithSharedConfigProfile(cfg.Profiles[0]))
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

func (t *Transport) RoundTrip(req *http.Request) (*http.Response, error) {
	base := t.Base
	if base == nil {
		base = http.DefaultTransport
	}

	body, err := readBody(req)
	if err != nil {
		return nil, err
	}
	body = injectMetadata(body, t.Metadata)
	setBody(req, body)

	if !t.SkipAuth {
		if t.Credentials == nil {
			return nil, fmt.Errorf("AWS credentials provider is not configured")
		}
		credentials, err := t.Credentials.Retrieve(req.Context())
		if err != nil {
			return nil, fmt.Errorf("retrieve AWS credentials: %w", err)
		}
		if err := t.sign(req, body, credentials); err != nil {
			return nil, err
		}
	}

	return base.RoundTrip(req)
}

func (t *Transport) sign(req *http.Request, body []byte, credentials aws.Credentials) error {
	signer := t.Signer
	if signer == nil {
		signer = v4.NewSigner()
	}
	clock := t.Clock
	if clock == nil {
		clock = SystemClock{}
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

func RedactHeader(name, value string) string {
	switch strings.ToLower(name) {
	case "authorization", "x-amz-security-token", "x-amz-date":
		return "[REDACTED]"
	default:
		return value
	}
}
