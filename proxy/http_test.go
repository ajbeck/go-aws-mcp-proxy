package proxy

import (
	"bytes"
	"context"
	"encoding/pem"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/aws/signer/v4"
)

type staticCredentials struct {
	called bool
	creds  aws.Credentials
	err    error
}

func (p *staticCredentials) Retrieve(context.Context) (aws.Credentials, error) {
	p.called = true
	return p.creds, p.err
}

type recordingSigner struct {
	called      bool
	payloadHash string
	region      string
	service     string
}

func (s *recordingSigner) SignHTTP(_ context.Context, _ aws.Credentials, req *http.Request, payloadHash string, service string, region string, _ time.Time, _ ...func(*v4.SignerOptions)) error {
	s.called = true
	s.payloadHash = payloadHash
	s.service = service
	s.region = region
	req.Header.Set("Authorization", "signed")
	return nil
}

type fixedClock struct{}

func (fixedClock) Now() time.Time {
	return time.Unix(1000, 0).UTC()
}

type captureRoundTripper struct {
	request *http.Request
	body    []byte
}

func (rt *captureRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	rt.request = req
	if req.Body != nil {
		body, err := io.ReadAll(req.Body)
		if err != nil {
			return nil, err
		}
		if err := req.Body.Close(); err != nil {
			return nil, err
		}
		rt.body = body
	}
	return &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(bytes.NewReader([]byte("{}"))),
		Header:     make(http.Header),
		Request:    req,
	}, nil
}

func TestSigningRoundTripperSignsClonedRequest(t *testing.T) {
	base := &captureRoundTripper{}
	creds := &staticCredentials{creds: aws.Credentials{
		AccessKeyID:     "AKIA",
		SecretAccessKey: "secret",
		Source:          "test",
	}}
	signer := &recordingSigner{}
	transport := sigV4RoundTripper{
		base:        base,
		clock:       fixedClock{},
		credentials: creds,
		region:      "us-east-1",
		service:     "aws-mcp",
		signer:      signer,
	}

	req := newJSONRequest(t, `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}`)
	resp, err := transport.RoundTrip(req)
	if err != nil {
		t.Fatalf("RoundTrip() error = %v", err)
	}
	resp.Body.Close()

	if !creds.called {
		t.Fatal("credentials were not retrieved")
	}
	if !signer.called {
		t.Fatal("signer was not called")
	}
	if signer.service != "aws-mcp" || signer.region != "us-east-1" {
		t.Fatalf("signing service/region = %q/%q", signer.service, signer.region)
	}
	if base.request.Header.Get("Authorization") != "signed" {
		t.Fatalf("Authorization = %q", base.request.Header.Get("Authorization"))
	}
	if base.request == req {
		t.Fatal("signing RoundTripper passed the original request downstream")
	}
	if req.Header.Get("Authorization") != "" {
		t.Fatalf("original request Authorization = %q, want empty", req.Header.Get("Authorization"))
	}
	if string(base.body) != `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}` {
		t.Fatalf("downstream body = %s", base.body)
	}
}

func TestNewHTTPClientTrustsCABundle(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}))
	defer server.Close()

	bundlePath := filepath.Join(t.TempDir(), "ca.pem")
	writeCertificateBundle(t, bundlePath, server.Certificate().Raw)

	client, err := newClient(t.Context(), Config{
		CaBundle: new(bundlePath),
		SkipAuth: new(true),
	}, nil, clientOptions{})
	if err != nil {
		t.Fatalf("newClient() error = %v", err)
	}

	resp, err := client.Get(server.URL)
	if err != nil {
		t.Fatalf("client.Get() error = %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("StatusCode = %d", resp.StatusCode)
	}
}

func TestNewHTTPTransportRejectsInvalidCABundle(t *testing.T) {
	bundlePath := filepath.Join(t.TempDir(), "invalid.pem")
	if err := os.WriteFile(bundlePath, []byte("not a certificate"), 0o600); err != nil {
		t.Fatalf("write bundle: %v", err)
	}

	_, err := newRoundTripper(t.Context(), Config{
		CaBundle: new(bundlePath),
		SkipAuth: new(true),
	}, http.DefaultTransport, clientOptions{})
	if err == nil {
		t.Fatal("newRoundTripper() error = nil")
	}
}

func TestNewHTTPTransportRequiresSigningConfigWhenAuthEnabled(t *testing.T) {
	_, err := newRoundTripper(t.Context(), Config{
		Region: new("us-east-1"),
	}, http.DefaultTransport, clientOptions{})
	if err == nil || !strings.Contains(err.Error(), "service is required") {
		t.Fatalf("newRoundTripper() error = %v, want service required", err)
	}

	_, err = newRoundTripper(t.Context(), Config{
		Service: new("aws-mcp"),
	}, http.DefaultTransport, clientOptions{})
	if err == nil || !strings.Contains(err.Error(), "region is required") {
		t.Fatalf("newRoundTripper() error = %v, want region required", err)
	}
}

func TestNewHTTPClientAppliesTimeouts(t *testing.T) {
	client, err := newClient(t.Context(), Config{
		Timeout:        new(2 * time.Second),
		ConnectTimeout: new(3 * time.Second),
		ReadTimeout:    new(4 * time.Second),
		WriteTimeout:   new(5 * time.Second),
		SkipAuth:       new(true),
	}, nil, clientOptions{})
	if err != nil {
		t.Fatalf("newClient() error = %v", err)
	}

	if client.Timeout != 2*time.Second {
		t.Fatalf("client.Timeout = %s", client.Timeout)
	}
	base, ok := findHTTPTransport(client.Transport)
	if !ok {
		t.Fatalf("client.Transport type = %T, no *http.Transport found", client.Transport)
	}
	if base.TLSHandshakeTimeout != 3*time.Second {
		t.Fatalf("TLSHandshakeTimeout = %s", base.TLSHandshakeTimeout)
	}
	if base.ResponseHeaderTimeout != 4*time.Second {
		t.Fatalf("ResponseHeaderTimeout = %s", base.ResponseHeaderTimeout)
	}
	if base.DialContext == nil {
		t.Fatal("DialContext was not configured")
	}
}

func TestTransportUserAgentIncludesClientInfoWhenTelemetryEnabled(t *testing.T) {
	base := &captureRoundTripper{}
	transport, err := newRoundTripper(t.Context(), Config{
		SkipAuth: new(true),
	}, base, clientOptions{
		ClientName:    "My Client",
		ClientVersion: "2.0",
		Version:       "1.2.3",
	})
	if err != nil {
		t.Fatalf("newRoundTripper() error = %v", err)
	}

	resp, err := transport.RoundTrip(newJSONRequest(t, `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}`))
	if err != nil {
		t.Fatalf("RoundTrip() error = %v", err)
	}
	resp.Body.Close()

	userAgent := base.request.Header.Get("User-Agent")
	if !strings.Contains(userAgent, "aws-mcp-proxy/1.2.3") {
		t.Fatalf("User-Agent = %q, want proxy version", userAgent)
	}
	if !strings.Contains(userAgent, "my-client/2.0") {
		t.Fatalf("User-Agent = %q, want client info", userAgent)
	}
}

func TestTransportUserAgentOmitsClientInfoWhenTelemetryDisabled(t *testing.T) {
	base := &captureRoundTripper{}
	transport, err := newRoundTripper(t.Context(), Config{
		DisableTelemetry: new(true),
		SkipAuth:         new(true),
	}, base, clientOptions{
		ClientName:    "My Client",
		ClientVersion: "2.0",
		Version:       "1.2.3",
	})
	if err != nil {
		t.Fatalf("newRoundTripper() error = %v", err)
	}

	resp, err := transport.RoundTrip(newJSONRequest(t, `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}`))
	if err != nil {
		t.Fatalf("RoundTrip() error = %v", err)
	}
	resp.Body.Close()

	userAgent := base.request.Header.Get("User-Agent")
	if !strings.Contains(userAgent, "aws-mcp-proxy/1.2.3") {
		t.Fatalf("User-Agent = %q, want proxy version", userAgent)
	}
	if strings.Contains(userAgent, "my-client") {
		t.Fatalf("User-Agent = %q, leaked client info", userAgent)
	}
}

func TestDeadlineConnAppliesReadAndWriteDeadlines(t *testing.T) {
	inner := &recordingConn{}
	conn := &deadlineConn{
		Conn:         inner,
		readTimeout:  time.Second,
		writeTimeout: 2 * time.Second,
	}

	buf := make([]byte, 1)
	_, _ = conn.Read(buf)
	_, _ = conn.Write([]byte("x"))

	if inner.readDeadline.IsZero() {
		t.Fatal("read deadline was not set")
	}
	if inner.writeDeadline.IsZero() {
		t.Fatal("write deadline was not set")
	}
	if time.Until(inner.readDeadline) <= 0 {
		t.Fatalf("read deadline is not in the future: %s", inner.readDeadline)
	}
	if time.Until(inner.writeDeadline) <= 0 {
		t.Fatalf("write deadline is not in the future: %s", inner.writeDeadline)
	}
}

type recordingConn struct {
	readDeadline  time.Time
	writeDeadline time.Time
}

func findHTTPTransport(rt http.RoundTripper) (*http.Transport, bool) {
	switch rt := rt.(type) {
	case *http.Transport:
		return rt, true
	case userAgentRoundTripper:
		return findHTTPTransport(rt.base)
	case sigV4RoundTripper:
		return findHTTPTransport(rt.base)
	default:
		return nil, false
	}
}

func (*recordingConn) Read([]byte) (int, error) {
	return 0, io.EOF
}

func (*recordingConn) Write(b []byte) (int, error) {
	return len(b), nil
}

func (*recordingConn) Close() error {
	return nil
}

func (*recordingConn) LocalAddr() net.Addr {
	return testAddr("local")
}

func (*recordingConn) RemoteAddr() net.Addr {
	return testAddr("remote")
}

func (*recordingConn) SetDeadline(time.Time) error {
	return nil
}

func (c *recordingConn) SetReadDeadline(t time.Time) error {
	c.readDeadline = t
	return nil
}

func (c *recordingConn) SetWriteDeadline(t time.Time) error {
	c.writeDeadline = t
	return nil
}

type testAddr string

func (a testAddr) Network() string {
	return string(a)
}

func (a testAddr) String() string {
	return string(a)
}

func newJSONRequest(t *testing.T, body string) *http.Request {
	t.Helper()

	req, err := http.NewRequest(http.MethodPost, "https://aws-mcp.us-east-1.api.aws/mcp", bytes.NewReader([]byte(body)))
	if err != nil {
		t.Fatalf("NewRequest() error = %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	return req
}

func writeCertificateBundle(t *testing.T, path string, der []byte) {
	t.Helper()

	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	if pemBytes == nil {
		t.Fatal("EncodeToMemory() returned nil")
	}
	if err := os.WriteFile(path, pemBytes, 0o600); err != nil {
		t.Fatalf("write bundle: %v", err)
	}
}
