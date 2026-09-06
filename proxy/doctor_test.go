package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sts"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestDiagnoseVerifiesEveryConfiguredProfile(t *testing.T) {
	profiles := []string{"production", "development"}
	loadedProfiles := make([]string, 0, len(profiles))
	identityCalls := 0
	expires := time.Date(2030, 9, 6, 12, 0, 0, 0, time.UTC)

	report := Diagnose(t.Context(), Config{
		Endpoint: new("https://aws-mcp.us-east-1.api.aws/mcp"),
		Service:  new("aws-mcp"),
		Region:   new("us-east-1"),
		Profiles: &profiles,
	}, DiagnoseOptions{
		loadConfig: func(_ context.Context, cfg Config, _ []byte) (aws.Config, error) {
			profile := value(defaultProfile(cfg.Profiles))
			loadedProfiles = append(loadedProfiles, profile)
			return aws.Config{
				Credentials: &staticCredentials{creds: aws.Credentials{
					AccessKeyID:     "access-key",
					SecretAccessKey: "secret-key",
					Source:          "SharedConfigCredentials: " + profile,
					CanExpire:       true,
					Expires:         expires,
				}},
				Region: "us-east-1",
			}, nil
		},
		callIdentity: func(_ context.Context, cfg aws.Config) (*sts.GetCallerIdentityOutput, error) {
			identityCalls++
			credentials, err := cfg.Credentials.Retrieve(t.Context())
			if err != nil {
				return nil, err
			}
			return &sts.GetCallerIdentityOutput{
				Account: new("123456789012"),
				Arn:     new("arn:aws:sts::123456789012:assumed-role/Doctor/" + credentials.Source),
				UserId:  new("doctor-user"),
			}, nil
		},
	})

	if err := report.Validate(); err != nil {
		t.Fatalf("DiagnosticReport.Validate() error = %v", err)
	}
	if report.Healthy == nil || !*report.Healthy {
		t.Fatalf("Diagnose().Healthy = %#v, want true", report.Healthy)
	}
	if got := len(loadedProfiles); got != 2 || loadedProfiles[0] != "production" || loadedProfiles[1] != "development" {
		t.Fatalf("loaded profiles = %q, want %q", loadedProfiles, profiles)
	}
	if identityCalls != 2 {
		t.Fatalf("identity calls = %d, want 2", identityCalls)
	}
	credentialChecks := checksNamed(report, "credentials")
	if len(credentialChecks) != 2 {
		t.Fatalf("credential checks = %d, want 2", len(credentialChecks))
	}
	for _, check := range credentialChecks {
		if check.Source == nil || check.ExpiresAt == nil || !check.ExpiresAt.Equal(expires) {
			t.Errorf("credential check = %#v, want source and expiration", check)
		}
	}
	identityChecks := checksNamed(report, "identity")
	if len(identityChecks) != 2 || identityChecks[0].Account == nil || identityChecks[0].ARN == nil || identityChecks[0].UserID == nil {
		t.Fatalf("identity checks = %#v, want two populated identities", identityChecks)
	}
}

func TestDiagnosticReportDoesNotExposeCredentialValues(t *testing.T) {
	report := Diagnose(t.Context(), Config{
		Endpoint: new("https://aws-mcp.us-east-1.api.aws/mcp"),
		Service:  new("aws-mcp"),
		Region:   new("us-east-1"),
	}, DiagnoseOptions{
		loadConfig: func(context.Context, Config, []byte) (aws.Config, error) {
			return aws.Config{
				Credentials: &staticCredentials{creds: aws.Credentials{
					AccessKeyID:     "diagnostic-access-key",
					SecretAccessKey: "diagnostic-secret-key",
					SessionToken:    "diagnostic-session-token",
					Source:          "test provider",
				}},
				Region: "us-east-1",
			}, nil
		},
		callIdentity: func(context.Context, aws.Config) (*sts.GetCallerIdentityOutput, error) {
			return &sts.GetCallerIdentityOutput{
				Account: new("123456789012"),
				Arn:     new("arn:aws:iam::123456789012:user/doctor"),
				UserId:  new("doctor-user"),
			}, nil
		},
	})

	encoded, err := json.Marshal(report)
	if err != nil {
		t.Fatalf("json.Marshal(DiagnosticReport) error = %v", err)
	}
	for _, secret := range []string{"diagnostic-access-key", "diagnostic-secret-key", "diagnostic-session-token"} {
		if strings.Contains(string(encoded), secret) {
			t.Errorf("DiagnosticReport JSON contains credential value %q", secret)
		}
	}
}

func TestDiagnoseOptionalAuthWarnsWhenCredentialsAreUnavailable(t *testing.T) {
	report := Diagnose(t.Context(), Config{
		Endpoint:     new("https://aws-mcp.us-east-1.api.aws/mcp"),
		Service:      new("aws-mcp"),
		Region:       new("us-east-1"),
		OptionalAuth: new(true),
	}, DiagnoseOptions{
		loadConfig: func(context.Context, Config, []byte) (aws.Config, error) {
			return aws.Config{Credentials: &staticCredentials{err: errors.New("SSO session expired")}}, nil
		},
	})

	if report.Healthy == nil || !*report.Healthy {
		t.Fatalf("Diagnose().Healthy = %#v, want true for optional authentication", report.Healthy)
	}
	credentials := checksNamed(report, "credentials")
	if len(credentials) != 1 || credentials[0].Status == nil || *credentials[0].Status != DiagnosticWarning {
		t.Fatalf("credential checks = %#v, want one warning", credentials)
	}
	identities := checksNamed(report, "identity")
	if len(identities) != 1 || identities[0].Status == nil || *identities[0].Status != DiagnosticSkip {
		t.Fatalf("identity checks = %#v, want one skip", identities)
	}
}

func TestDiagnoseUsesSignedSTSGetCallerIdentity(t *testing.T) {
	var authorization string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		authorization = req.Header.Get("Authorization")
		body, err := io.ReadAll(req.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if !strings.Contains(string(body), "Action=GetCallerIdentity") {
			http.Error(w, "unexpected STS action", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "text/xml")
		_, _ = io.WriteString(w, `<GetCallerIdentityResponse xmlns="https://sts.amazonaws.com/doc/2011-06-15/"><GetCallerIdentityResult><Account>123456789012</Account><Arn>arn:aws:sts::123456789012:assumed-role/Doctor/session</Arn><UserId>doctor-user</UserId></GetCallerIdentityResult><ResponseMetadata><RequestId>request-id</RequestId></ResponseMetadata></GetCallerIdentityResponse>`)
	}))
	defer server.Close()

	report := Diagnose(t.Context(), Config{
		Endpoint: new("https://aws-mcp.us-east-1.api.aws/mcp"),
		Service:  new("aws-mcp"),
		Region:   new("us-east-1"),
	}, DiagnoseOptions{
		loadConfig: func(context.Context, Config, []byte) (aws.Config, error) {
			return aws.Config{
				BaseEndpoint: new(server.URL),
				Credentials: &staticCredentials{creds: aws.Credentials{
					AccessKeyID:     "access-key",
					SecretAccessKey: "secret-key",
					Source:          "test credentials",
				}},
				HTTPClient: server.Client(),
				Region:     "us-east-1",
			}, nil
		},
	})

	if report.Healthy == nil || !*report.Healthy {
		t.Fatalf("Diagnose().Healthy = %#v, checks = %#v", report.Healthy, report.Checks)
	}
	if authorization == "" || !strings.Contains(authorization, "Credential=access-key/") {
		t.Fatalf("STS Authorization = %q, want SigV4 credential", authorization)
	}
	identities := checksNamed(report, "identity")
	if len(identities) != 1 || identities[0].Account == nil || *identities[0].Account != "123456789012" {
		t.Fatalf("identity checks = %#v, want parsed STS identity", identities)
	}
}

func TestDiagnoseSkipAuthDoesNotLoadAWSConfig(t *testing.T) {
	loaded := false
	report := Diagnose(t.Context(), Config{
		Endpoint: new("https://aws-mcp.us-east-1.api.aws/mcp"),
		SkipAuth: new(true),
	}, DiagnoseOptions{
		loadConfig: func(context.Context, Config, []byte) (aws.Config, error) {
			loaded = true
			return aws.Config{}, nil
		},
	})

	if loaded {
		t.Fatal("Diagnose() loaded AWS configuration with --skip-auth")
	}
	if report.Healthy == nil || !*report.Healthy {
		t.Fatalf("Diagnose().Healthy = %#v, want true", report.Healthy)
	}
}

func TestDiagnoseRejectsUnsafeEndpointBeforeCredentials(t *testing.T) {
	loaded := false
	report := Diagnose(t.Context(), Config{
		Endpoint: new("http://example.com/mcp"),
		Service:  new("aws-mcp"),
		Region:   new("us-east-1"),
	}, DiagnoseOptions{
		loadConfig: func(context.Context, Config, []byte) (aws.Config, error) {
			loaded = true
			return aws.Config{}, nil
		},
	})

	if loaded {
		t.Fatal("Diagnose() loaded AWS configuration after configuration failure")
	}
	if report.Healthy == nil || *report.Healthy {
		t.Fatalf("Diagnose().Healthy = %#v, want false", report.Healthy)
	}
	if len(report.Checks) != 1 || report.Checks[0].Category == nil || *report.Checks[0].Category != DiagnosticConfiguration {
		t.Fatalf("Diagnose().Checks = %#v, want configuration failure", report.Checks)
	}
}

func TestDiagnoseProbeUsesUpstreamConnector(t *testing.T) {
	session := &fakeSession{tools: []*mcp.Tool{{Name: "one"}, {Name: "two"}}}
	connector := &fakeConnector{sess: session}
	report := Diagnose(t.Context(), Config{
		Endpoint: new("https://aws-mcp.us-east-1.api.aws/mcp"),
		SkipAuth: new(true),
	}, DiagnoseOptions{
		Probe:          true,
		ProbeConnector: connector,
		Version:        "v0.5.0-test",
	})

	if report.Healthy == nil || !*report.Healthy {
		t.Fatalf("Diagnose().Healthy = %#v, want true", report.Healthy)
	}
	if !connector.called || connector.params == nil || connector.params.ClientInfo == nil {
		t.Fatalf("probe connector = %#v, want initialized call", connector)
	}
	if connector.params.ClientInfo.Name != "aws-mcp-proxy-doctor" || connector.params.ClientInfo.Version != "v0.5.0-test" {
		t.Fatalf("probe ClientInfo = %#v", connector.params.ClientInfo)
	}
	checks := checksNamed(report, "mcp_probe")
	if len(checks) != 1 || checks[0].ToolCount == nil || *checks[0].ToolCount != 2 {
		t.Fatalf("probe checks = %#v, want tool count 2", checks)
	}
	if !session.closed {
		t.Fatal("Diagnose() did not close probe session")
	}
}

func TestDiagnosticCategoryForErrorRecognizesNetworkFailure(t *testing.T) {
	err := &net.DNSError{Err: "no such host", Name: "aws-mcp.example"}
	if got := diagnosticCategoryForError(err, DiagnosticIdentity); got != DiagnosticNetwork {
		t.Fatalf("diagnosticCategoryForError(%v) = %q, want %q", err, got, DiagnosticNetwork)
	}
}

func checksNamed(report DiagnosticReport, name string) []DiagnosticCheck {
	var matches []DiagnosticCheck
	for _, check := range report.Checks {
		if check.Name != nil && *check.Name == name {
			matches = append(matches, check)
		}
	}
	return matches
}
