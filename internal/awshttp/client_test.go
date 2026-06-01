package awshttp

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/pem"
	"io"
	"log/slog"
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

	"github.com/ajbeck/go-aws-mcp-proxy/internal/proxyconfig"
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
	body, err := io.ReadAll(req.Body)
	if err != nil {
		return nil, err
	}
	rt.body = body
	return &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(bytes.NewReader([]byte("{}"))),
		Header:     make(http.Header),
		Request:    req,
	}, nil
}

func TestTransportInjectsMetadataBeforeSigning(t *testing.T) {
	base := &captureRoundTripper{}
	creds := &staticCredentials{creds: aws.Credentials{
		AccessKeyID:     "AKIA",
		SecretAccessKey: "secret",
		Source:          "test",
	}}
	signer := &recordingSigner{}
	transport := &Transport{
		Base:        base,
		Clock:       fixedClock{},
		Credentials: creds,
		Metadata: map[string]string{
			"AWS_REGION": "us-west-2",
			"team":       "platform",
		},
		Region:  "us-east-1",
		Service: "aws-mcp",
		Signer:  signer,
	}

	req := newJSONRequest(t, `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{"_meta":{"team":"client"}}}`)
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

	var body map[string]any
	if err := json.Unmarshal(base.body, &body); err != nil {
		t.Fatalf("unmarshal body: %v", err)
	}
	params := body["params"].(map[string]any)
	meta := params["_meta"].(map[string]any)
	if meta["AWS_REGION"] != "us-west-2" {
		t.Fatalf("AWS_REGION metadata = %#v", meta["AWS_REGION"])
	}
	if meta["team"] != "client" {
		t.Fatalf("existing metadata was not preserved: %#v", meta)
	}
}

func TestTransportSkipAuthStillInjectsMetadata(t *testing.T) {
	base := &captureRoundTripper{}
	creds := &staticCredentials{}
	signer := &recordingSigner{}
	transport := &Transport{
		Base:        base,
		Credentials: creds,
		Metadata:    map[string]string{"AWS_REGION": "us-east-1"},
		Signer:      signer,
		SkipAuth:    true,
	}

	req := newJSONRequest(t, `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}`)
	resp, err := transport.RoundTrip(req)
	if err != nil {
		t.Fatalf("RoundTrip() error = %v", err)
	}
	resp.Body.Close()

	if creds.called {
		t.Fatal("credentials were retrieved with skip auth")
	}
	if signer.called {
		t.Fatal("signer was called with skip auth")
	}
	var body map[string]any
	if err := json.Unmarshal(base.body, &body); err != nil {
		t.Fatalf("unmarshal body: %v", err)
	}
	params := body["params"].(map[string]any)
	meta := params["_meta"].(map[string]any)
	if meta["AWS_REGION"] != "us-east-1" {
		t.Fatalf("metadata = %#v", meta)
	}
}

func TestInjectMetadataLeavesNonJSONRPCBodyUnchanged(t *testing.T) {
	body := []byte(`{"hello":"world"}`)

	got := injectMetadata(body, map[string]string{"AWS_REGION": "us-east-1"})

	if string(got) != string(body) {
		t.Fatalf("body = %s", got)
	}
}

func TestNewClientTrustsCABundle(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}))
	defer server.Close()

	bundlePath := filepath.Join(t.TempDir(), "ca.pem")
	writeCertificateBundle(t, bundlePath, server.Certificate().Raw)

	client, err := NewClient(context.Background(), proxyconfig.Config{
		CaBundle: bundlePath,
		SkipAuth: true,
	}, nil)
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
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

func TestNewTransportRejectsInvalidCABundle(t *testing.T) {
	bundlePath := filepath.Join(t.TempDir(), "invalid.pem")
	if err := os.WriteFile(bundlePath, []byte("not a certificate"), 0o600); err != nil {
		t.Fatalf("write bundle: %v", err)
	}

	_, err := NewTransport(context.Background(), proxyconfig.Config{
		CaBundle: bundlePath,
		SkipAuth: true,
	}, http.DefaultTransport)
	if err == nil {
		t.Fatal("NewTransport() error = nil")
	}
}

func TestNewClientAppliesTimeouts(t *testing.T) {
	client, err := NewClient(context.Background(), proxyconfig.Config{
		Timeout:        2 * time.Second,
		ConnectTimeout: 3 * time.Second,
		ReadTimeout:    4 * time.Second,
		WriteTimeout:   5 * time.Second,
		SkipAuth:       true,
	}, nil)
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}

	if client.Timeout != 2*time.Second {
		t.Fatalf("client.Timeout = %s", client.Timeout)
	}
	proxyTransport, ok := client.Transport.(*Transport)
	if !ok {
		t.Fatalf("client.Transport type = %T", client.Transport)
	}
	base, ok := proxyTransport.Base.(*http.Transport)
	if !ok {
		t.Fatalf("proxy base transport type = %T", proxyTransport.Base)
	}
	if base.TLSHandshakeTimeout != 3*time.Second {
		t.Fatalf("TLSHandshakeTimeout = %s", base.TLSHandshakeTimeout)
	}
	if base.ResponseHeaderTimeout != 4*time.Second {
		t.Fatalf("ResponseHeaderTimeout = %s", base.ResponseHeaderTimeout)
	}
	if base.ExpectContinueTimeout != 5*time.Second {
		t.Fatalf("ExpectContinueTimeout = %s", base.ExpectContinueTimeout)
	}
	if base.DialContext == nil {
		t.Fatal("DialContext was not configured")
	}
}

func TestTransportRetriesTransientHTTPStatus(t *testing.T) {
	base := &sequenceRoundTripper{
		responses: []*http.Response{
			responseWithStatus(http.StatusServiceUnavailable),
			responseWithStatus(http.StatusOK),
		},
	}
	var logs bytes.Buffer
	transport := &Transport{
		Base:       base,
		Logger:     slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
		Retries:    1,
		RetryDelay: func(int) time.Duration { return 0 },
		SkipAuth:   true,
	}

	resp, err := transport.RoundTrip(newJSONRequest(t, `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}`))
	if err != nil {
		t.Fatalf("RoundTrip() error = %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("StatusCode = %d", resp.StatusCode)
	}
	if base.calls != 2 {
		t.Fatalf("calls = %d, want 2", base.calls)
	}
	if !strings.Contains(logs.String(), "retrying upstream HTTP request") {
		t.Fatalf("logs = %q, want retry log", logs.String())
	}
}

func TestTransportLogsRedactedHeaders(t *testing.T) {
	base := &sequenceRoundTripper{responses: []*http.Response{responseWithStatus(http.StatusOK)}}
	var logs bytes.Buffer
	transport := &Transport{
		Base:     base,
		Logger:   slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
		SkipAuth: true,
	}

	req := newJSONRequest(t, `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}`)
	req.Header.Set("Authorization", "secret")
	resp, err := transport.RoundTrip(req)
	if err != nil {
		t.Fatalf("RoundTrip() error = %v", err)
	}
	resp.Body.Close()

	logText := logs.String()
	if !strings.Contains(logText, "[REDACTED]") {
		t.Fatalf("logs = %q, want redacted header", logText)
	}
	if strings.Contains(logText, "secret") {
		t.Fatalf("logs = %q, leaked secret", logText)
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

func TestRedactHeader(t *testing.T) {
	if got := RedactHeader("Authorization", "secret"); got != "[REDACTED]" {
		t.Fatalf("Authorization redaction = %q", got)
	}
	if got := RedactHeader("Accept", "application/json"); got != "application/json" {
		t.Fatalf("Accept redaction = %q", got)
	}
}

type recordingConn struct {
	readDeadline  time.Time
	writeDeadline time.Time
}

type sequenceRoundTripper struct {
	calls     int
	responses []*http.Response
	errs      []error
}

func (rt *sequenceRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	rt.calls++
	index := rt.calls - 1
	if index < len(rt.errs) && rt.errs[index] != nil {
		return nil, rt.errs[index]
	}
	if index < len(rt.responses) {
		resp := rt.responses[index]
		resp.Request = req
		return resp, nil
	}
	return responseWithStatus(http.StatusOK), nil
}

func responseWithStatus(status int) *http.Response {
	return &http.Response{
		StatusCode: status,
		Body:       io.NopCloser(bytes.NewReader([]byte("{}"))),
		Header:     make(http.Header),
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
