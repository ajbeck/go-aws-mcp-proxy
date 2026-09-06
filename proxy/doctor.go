package proxy

import (
	"context"
	"crypto/x509"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sts"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// DiagnosticStatus describes the outcome of one doctor check.
type DiagnosticStatus string

// Diagnostic statuses are stable values in the JSON doctor report.
const (
	DiagnosticPass    DiagnosticStatus = "pass"
	DiagnosticWarning DiagnosticStatus = "warning"
	DiagnosticFail    DiagnosticStatus = "fail"
	DiagnosticSkip    DiagnosticStatus = "skip"
)

// DiagnosticCategory identifies the subsystem checked by doctor.
type DiagnosticCategory string

// Diagnostic categories are stable values used to select the CLI exit code.
const (
	DiagnosticConfiguration DiagnosticCategory = "configuration"
	DiagnosticCredentials   DiagnosticCategory = "credentials"
	DiagnosticIdentity      DiagnosticCategory = "identity"
	DiagnosticNetwork       DiagnosticCategory = "network"
	DiagnosticUpstream      DiagnosticCategory = "upstream_mcp"
)

// DiagnosticCheck represents one redacted doctor check result.
type DiagnosticCheck struct {
	Name        *string             `json:"name,omitempty"`
	Status      *DiagnosticStatus   `json:"status,omitempty"`
	Category    *DiagnosticCategory `json:"category,omitempty"`
	Profile     *string             `json:"profile,omitempty"`
	Message     *string             `json:"message,omitempty"`
	Remediation *string             `json:"remediation,omitempty"`
	Detail      *string             `json:"technical_detail,omitempty"`
	Source      *string             `json:"credential_source,omitempty"`
	ExpiresAt   *time.Time          `json:"expires_at,omitempty"`
	Account     *string             `json:"account,omitempty"`
	ARN         *string             `json:"arn,omitempty"`
	UserID      *string             `json:"user_id,omitempty"`
	ToolCount   *int                `json:"tool_count,omitempty"`
}

// DiagnosticReport contains the ordered results of a doctor run.
type DiagnosticReport struct {
	Healthy        *bool             `json:"healthy,omitempty"`
	Endpoint       *string           `json:"endpoint,omitempty"`
	ProbeRequested *bool             `json:"probe_requested,omitempty"`
	Checks         []DiagnosticCheck `json:"checks"`
}

// DiagnoseOptions configures the optional online MCP probe and test seams.
type DiagnoseOptions struct {
	// HTTPClient is used for STS identity checks and the optional MCP probe.
	HTTPClient *http.Client
	// Logger receives diagnostic transport logs.
	Logger *slog.Logger
	// Probe requests an MCP initialize and tools/list check against the endpoint.
	Probe bool
	// ProbeConnector replaces the default Streamable HTTP connector.
	ProbeConnector UpstreamConnector
	// Version is reported to the upstream MCP server during a probe.
	Version string

	loadConfig   func(context.Context, Config, []byte) (aws.Config, error)
	callIdentity func(context.Context, aws.Config) (*sts.GetCallerIdentityOutput, error)
}

// Diagnose validates proxy configuration and AWS identity without modifying
// credentials or AWS configuration. It contacts the configured MCP endpoint
// only when DiagnoseOptions.Probe is true.
func Diagnose(ctx context.Context, cfg Config, options DiagnoseOptions) DiagnosticReport {
	diagnosticCtx := ctx
	if positiveDuration(cfg.Timeout) {
		var cancel context.CancelFunc
		diagnosticCtx, cancel = context.WithTimeout(ctx, *cfg.Timeout)
		defer cancel()
	}
	report := DiagnosticReport{
		Endpoint:       new(value(cfg.Endpoint)),
		ProbeRequested: new(options.Probe),
	}

	caBundle, err := diagnoseConfiguration(cfg)
	if err != nil {
		report.Checks = append(report.Checks, failedDiagnostic("configuration", DiagnosticConfiguration, nil, err, remediationFor(err)))
		return finishDiagnostics(report)
	}
	report.Checks = append(report.Checks, passedDiagnostic("configuration", DiagnosticConfiguration, nil, "Endpoint, signing settings, region, service, and CA bundle are valid."))

	mode, _ := authModeFor(cfg)
	if mode == authModeSkipped {
		report.Checks = append(report.Checks,
			skippedDiagnostic("credentials", DiagnosticCredentials, nil, "Credential loading is disabled by --skip-auth."),
			skippedDiagnostic("identity", DiagnosticIdentity, nil, "Identity verification is disabled by --skip-auth."),
		)
	} else {
		diagnoseIdentities(diagnosticCtx, cfg, caBundle, mode, options, &report)
	}

	if options.Probe {
		report.Checks = append(report.Checks, diagnoseMCP(diagnosticCtx, cfg, options))
	} else {
		report.Checks = append(report.Checks, skippedDiagnostic("mcp_probe", DiagnosticUpstream, nil, "MCP endpoint probe was not requested."))
	}
	return finishDiagnostics(report)
}

func diagnoseConfiguration(cfg Config) ([]byte, error) {
	if cfg.Endpoint == nil {
		return nil, missingConfigError("endpoint", reasonMissingEndpoint)
	}
	if err := validateEndpoint(*cfg.Endpoint); err != nil {
		return nil, err
	}
	mode, err := authModeFor(cfg)
	if err != nil {
		return nil, err
	}
	if mode != authModeSkipped && cfg.Service == nil {
		return nil, missingConfigError("service", reasonMissingService)
	}
	if mode != authModeSkipped && cfg.Region == nil {
		return nil, missingConfigError("region", reasonMissingRegion)
	}
	if cfg.CaBundle == nil {
		return nil, nil
	}
	bundle, err := readCABundle(*cfg.CaBundle)
	if err != nil {
		return nil, err
	}
	if _, err := roundTripperWithCABundle(http.DefaultTransport, *cfg.CaBundle, bundle); err != nil {
		return nil, err
	}
	return bundle, nil
}

func diagnoseIdentities(ctx context.Context, cfg Config, caBundle []byte, mode authMode, options DiagnoseOptions, report *DiagnosticReport) {
	loadConfig := options.loadConfig
	if loadConfig == nil {
		loadConfig = loadAWSConfig
	}
	callIdentity := options.callIdentity
	if callIdentity == nil {
		callIdentity = func(ctx context.Context, awsCfg aws.Config) (*sts.GetCallerIdentityOutput, error) {
			return sts.NewFromConfig(awsCfg).GetCallerIdentity(ctx, &sts.GetCallerIdentityInput{})
		}
	}

	for _, profile := range diagnosticProfiles(cfg.Profiles) {
		profileCfg := cfg
		if profile != "" {
			profileCfg.Profiles = new([]string{profile})
		} else {
			profileCfg.Profiles = nil
		}
		awsCfg, err := loadConfig(ctx, profileCfg, caBundle)
		if err != nil {
			appendCredentialFailure(report, mode, profile, err)
			continue
		}
		if options.HTTPClient != nil {
			awsCfg.HTTPClient = options.HTTPClient
		}
		credentials, err := awsCfg.Credentials.Retrieve(ctx)
		if err != nil || !credentials.HasKeys() || credentials.Expired() {
			if err == nil {
				if credentials.Expired() {
					err = errors.New("AWS credentials are expired")
				} else {
					err = errors.New("AWS credentials are empty")
				}
			}
			appendCredentialFailure(report, mode, profile, err)
			continue
		}

		credentialCheck := passedDiagnostic("credentials", DiagnosticCredentials, profilePointer(profile), "AWS credentials resolved through the SDK provider chain.")
		if credentials.Source != "" {
			credentialCheck.Source = new(credentials.Source)
		}
		if credentials.CanExpire {
			credentialCheck.ExpiresAt = new(credentials.Expires)
		}
		report.Checks = append(report.Checks, credentialCheck)

		identity, err := callIdentity(ctx, awsCfg)
		if err != nil {
			category := diagnosticCategoryForError(err, DiagnosticIdentity)
			report.Checks = append(report.Checks, failedDiagnostic("identity", category, profilePointer(profile), err, identityRemediation(err)))
			continue
		}
		check := passedDiagnostic("identity", DiagnosticIdentity, profilePointer(profile), "STS GetCallerIdentity verified the effective AWS identity.")
		check.Account = identity.Account
		check.ARN = identity.Arn
		check.UserID = identity.UserId
		report.Checks = append(report.Checks, check)
	}
}

func appendCredentialFailure(report *DiagnosticReport, mode authMode, profile string, err error) {
	if mode == authModeOptional {
		check := warningDiagnostic("credentials", DiagnosticCredentials, profilePointer(profile), "AWS credentials are unavailable; optional authentication will use unsigned requests.", "Configure or refresh AWS credentials if this endpoint requires signed requests.")
		report.Checks = append(report.Checks, check, skippedDiagnostic("identity", DiagnosticIdentity, profilePointer(profile), "Identity verification requires AWS credentials."))
		return
	}
	report.Checks = append(report.Checks,
		failedDiagnostic("credentials", DiagnosticCredentials, profilePointer(profile), err, credentialRemediation()),
		skippedDiagnostic("identity", DiagnosticIdentity, profilePointer(profile), "Identity verification requires valid AWS credentials."),
	)
}

func diagnoseMCP(ctx context.Context, cfg Config, options DiagnoseOptions) DiagnosticCheck {
	connector := options.ProbeConnector
	if connector == nil {
		connector = mcpUpstreamConnector{
			DisableStandaloneSSE: true,
			HTTPClient:           options.HTTPClient,
			Logger:               options.Logger,
			Version:              options.Version,
		}
	}
	version := options.Version
	if version == "" {
		version = "dev"
	}
	params := &mcp.InitializeParams{
		Capabilities: &mcp.ClientCapabilities{},
		ClientInfo: &mcp.Implementation{
			Name:    "aws-mcp-proxy-doctor",
			Version: version,
		},
	}
	session, err := connector.Connect(ctx, cfg, params)
	if err != nil {
		category := diagnosticCategoryForError(err, DiagnosticUpstream)
		return failedDiagnostic("mcp_probe", category, nil, err, probeRemediation(err))
	}
	degradedErr := error(nil)
	if degraded, ok := session.(interface{ degradedError() error }); ok {
		degradedErr = degraded.degradedError()
	}
	if degradedErr != nil {
		_ = session.Close()
		return failedDiagnostic("mcp_probe", DiagnosticCredentials, nil, degradedErr, credentialRemediation())
	}
	defer session.Close()

	run := proxyRun{config: cfg, logger: options.Logger}
	tools, err := run.discoverUpstreamTools(ctx, session)
	if err != nil {
		category := diagnosticCategoryForError(err, DiagnosticUpstream)
		return failedDiagnostic("mcp_probe", category, nil, err, probeRemediation(err))
	}
	count := 0
	if tools != nil {
		count = len(tools.Tools)
	}
	check := passedDiagnostic("mcp_probe", DiagnosticUpstream, nil, "MCP initialize and tools/list completed successfully.")
	check.ToolCount = new(count)
	return check
}

func diagnosticProfiles(profiles *[]string) []string {
	if profiles == nil || len(*profiles) == 0 {
		return []string{""}
	}
	return *profiles
}

func profilePointer(profile string) *string {
	if profile == "" {
		return new("default")
	}
	return new(profile)
}

func passedDiagnostic(name string, category DiagnosticCategory, profile *string, message string) DiagnosticCheck {
	return DiagnosticCheck{Name: new(name), Status: new(DiagnosticPass), Category: new(category), Profile: profile, Message: new(message)}
}

func warningDiagnostic(name string, category DiagnosticCategory, profile *string, message, remediation string) DiagnosticCheck {
	return DiagnosticCheck{Name: new(name), Status: new(DiagnosticWarning), Category: new(category), Profile: profile, Message: new(message), Remediation: new(remediation)}
}

func failedDiagnostic(name string, category DiagnosticCategory, profile *string, err error, remediation string) DiagnosticCheck {
	message := err.Error()
	check := DiagnosticCheck{Name: new(name), Status: new(DiagnosticFail), Category: new(category), Profile: profile, Message: new(message), Remediation: new(remediation)}
	if proxyErr, ok := errors.AsType[*proxyError](err); ok {
		check.Message = new(proxyErr.problem)
		if proxyErr.detail != "" {
			check.Detail = new(proxyErr.detail)
		}
		if remediation == "" {
			check.Remediation = new(proxyErr.next)
		}
	}
	return check
}

func skippedDiagnostic(name string, category DiagnosticCategory, profile *string, message string) DiagnosticCheck {
	return DiagnosticCheck{Name: new(name), Status: new(DiagnosticSkip), Category: new(category), Profile: profile, Message: new(message)}
}

func finishDiagnostics(report DiagnosticReport) DiagnosticReport {
	healthy := true
	for _, check := range report.Checks {
		if check.Status != nil && *check.Status == DiagnosticFail {
			healthy = false
			break
		}
	}
	report.Healthy = new(healthy)
	return report
}

func diagnosticCategoryForError(err error, fallback DiagnosticCategory) DiagnosticCategory {
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return DiagnosticNetwork
	}
	if _, ok := errors.AsType[net.Error](err); ok {
		return DiagnosticNetwork
	}
	if _, ok := errors.AsType[*url.Error](err); ok {
		return DiagnosticNetwork
	}
	if _, ok := errors.AsType[x509.UnknownAuthorityError](err); ok {
		return DiagnosticNetwork
	}
	if proxyErr, ok := errors.AsType[*proxyError](err); ok {
		if proxyErr.category == categoryConfiguration {
			return DiagnosticConfiguration
		}
		switch proxyErr.reason {
		case reasonCredentialUnavailable, reasonCredentialUnauthorized, reasonCredentialForbidden:
			return DiagnosticCredentials
		}
	}
	return fallback
}

func remediationFor(err error) string {
	if proxyErr, ok := errors.AsType[*proxyError](err); ok {
		return proxyErr.next
	}
	return "Correct the proxy configuration and run doctor again."
}

func credentialRemediation() string {
	return "Configure or refresh AWS credentials for this profile, then run doctor again."
}

func identityRemediation(err error) string {
	if diagnosticCategoryForError(err, DiagnosticIdentity) == DiagnosticNetwork {
		return "Check DNS, network access, proxy settings, and the configured CA bundle, then run doctor again."
	}
	return "Refresh the selected AWS credentials and verify the profile can call STS, then run doctor again."
}

func probeRemediation(err error) string {
	if proxyErr, ok := errors.AsType[*proxyError](err); ok {
		return proxyErr.next
	}
	if diagnosticCategoryForError(err, DiagnosticUpstream) == DiagnosticNetwork {
		return "Check DNS, network access, proxy settings, and the configured CA bundle, then run doctor --probe again."
	}
	return "Check the MCP endpoint and its policy, then run doctor --probe again."
}

// Validate reports whether all required report fields are populated.
func (r DiagnosticReport) Validate() error {
	if r.Healthy == nil {
		return errors.New("diagnostic report Healthy is unset")
	}
	if r.Endpoint == nil {
		return errors.New("diagnostic report Endpoint is unset")
	}
	if r.ProbeRequested == nil {
		return errors.New("diagnostic report ProbeRequested is unset")
	}
	for i, check := range r.Checks {
		if check.Name == nil || check.Status == nil || check.Category == nil || check.Message == nil {
			return fmt.Errorf("diagnostic check %d is incomplete", i)
		}
	}
	return nil
}
