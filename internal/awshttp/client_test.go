package awshttp

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/pem"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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

func TestRedactHeader(t *testing.T) {
	if got := RedactHeader("Authorization", "secret"); got != "[REDACTED]" {
		t.Fatalf("Authorization redaction = %q", got)
	}
	if got := RedactHeader("Accept", "application/json"); got != "application/json" {
		t.Fatalf("Accept redaction = %q", got)
	}
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
