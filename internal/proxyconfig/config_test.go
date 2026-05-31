package proxyconfig

import (
	"strings"
	"testing"
	"time"
)

func TestResolveInfersServiceAndRegionFromBedrockAgentCoreEndpoint(t *testing.T) {
	cfg, err := Resolve(Input{
		Endpoint:       "https://abc.bedrock-agentcore.us-west-2.amazonaws.com/mcp",
		LogLevel:       "ERROR",
		Timeout:        time.Second,
		ConnectTimeout: time.Second,
		ReadTimeout:    time.Second,
		WriteTimeout:   time.Second,
		ToolTimeout:    time.Second,
	}, MapEnv{})
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	if cfg.Service != "bedrock-agentcore" {
		t.Fatalf("Service = %q", cfg.Service)
	}
	if cfg.Region != "us-west-2" {
		t.Fatalf("Region = %q", cfg.Region)
	}
	if cfg.Metadata["AWS_REGION"] != "us-west-2" {
		t.Fatalf("Metadata AWS_REGION = %q", cfg.Metadata["AWS_REGION"])
	}
}

func TestResolveInfersServiceAndRegionFromAPIAWSEndpoint(t *testing.T) {
	cfg, err := Resolve(Input{
		Endpoint:       "https://example.us-east-1.api.aws/mcp",
		LogLevel:       "ERROR",
		Timeout:        time.Second,
		ConnectTimeout: time.Second,
		ReadTimeout:    time.Second,
		WriteTimeout:   time.Second,
		ToolTimeout:    time.Second,
	}, MapEnv{})
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	if cfg.Service != "example" || cfg.Region != "us-east-1" {
		t.Fatalf("Service/Region = %q/%q", cfg.Service, cfg.Region)
	}
}

func TestResolveUsesAWSRegionEnvironmentFallback(t *testing.T) {
	cfg, err := Resolve(Input{
		Endpoint:       "https://service.example.com/mcp",
		LogLevel:       "ERROR",
		Timeout:        time.Second,
		ConnectTimeout: time.Second,
		ReadTimeout:    time.Second,
		WriteTimeout:   time.Second,
		ToolTimeout:    time.Second,
	}, MapEnv{"AWS_REGION": "eu-west-1"})
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	if cfg.Service != "service" || cfg.Region != "eu-west-1" {
		t.Fatalf("Service/Region = %q/%q", cfg.Service, cfg.Region)
	}
}

func TestResolveProfiles(t *testing.T) {
	tests := []struct {
		name     string
		input    []string
		env      MapEnv
		profiles string
	}{
		{name: "cli profiles", input: []string{"default", "dev", "default"}, profiles: "default,dev"},
		{name: "aws profile env", env: MapEnv{"AWS_PROFILE": "sandbox"}, profiles: "sandbox"},
		{name: "proxy profiles env wins", input: []string{"default"}, env: MapEnv{"AWS_MCP_PROXY_PROFILES": "prod dev prod"}, profiles: "prod,dev"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg, err := Resolve(Input{
				Endpoint:       "https://service.us-east-1.api.aws/mcp",
				Profiles:       tt.input,
				LogLevel:       "ERROR",
				Timeout:        time.Second,
				ConnectTimeout: time.Second,
				ReadTimeout:    time.Second,
				WriteTimeout:   time.Second,
				ToolTimeout:    time.Second,
			}, tt.env)
			if err != nil {
				t.Fatalf("Resolve() error = %v", err)
			}
			if got := strings.Join(cfg.Profiles, ","); got != tt.profiles {
				t.Fatalf("Profiles = %q, want %q", got, tt.profiles)
			}
		})
	}
}

func TestResolveRejectsInvalidEndpointSchemes(t *testing.T) {
	tests := []string{
		"service.us-east-1.api.aws/mcp",
		"http://service.us-east-1.api.aws/mcp",
		"ftp://service.us-east-1.api.aws/mcp",
	}

	for _, endpoint := range tests {
		t.Run(endpoint, func(t *testing.T) {
			_, err := Resolve(Input{Endpoint: endpoint, Region: "us-east-1"}, MapEnv{})
			if err == nil {
				t.Fatal("Resolve() error = nil")
			}
		})
	}
}

func TestResolveAllowsLocalhostHTTP(t *testing.T) {
	_, err := Resolve(Input{
		Endpoint:       "http://localhost:8080/mcp",
		Service:        "execute-api",
		Region:         "us-east-1",
		LogLevel:       "ERROR",
		Timeout:        time.Second,
		ConnectTimeout: time.Second,
		ReadTimeout:    time.Second,
		WriteTimeout:   time.Second,
		ToolTimeout:    time.Second,
	}, MapEnv{})
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
}

func TestResolveRejectsInvalidMetadata(t *testing.T) {
	_, err := Resolve(Input{
		Endpoint:       "https://service.us-east-1.api.aws/mcp",
		Metadata:       []string{"not-key-value"},
		LogLevel:       "ERROR",
		Timeout:        time.Second,
		ConnectTimeout: time.Second,
		ReadTimeout:    time.Second,
		WriteTimeout:   time.Second,
		ToolTimeout:    time.Second,
	}, MapEnv{})
	if err == nil {
		t.Fatal("Resolve() error = nil")
	}
}

func TestResolveRejectsOutOfRangeValues(t *testing.T) {
	tests := []Input{
		{Retries: -1},
		{Retries: 11},
		{Timeout: -time.Second},
	}

	for _, input := range tests {
		t.Run("", func(t *testing.T) {
			input.Endpoint = "https://service.us-east-1.api.aws/mcp"
			_, err := Resolve(input, MapEnv{})
			if err == nil {
				t.Fatal("Resolve() error = nil")
			}
		})
	}
}
