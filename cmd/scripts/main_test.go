package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestDefaultBuildOutput(t *testing.T) {
	tests := []struct {
		name string
		goos string
		want string
	}{
		{name: "unix", goos: "darwin", want: "bin/aws-mcp-proxy"},
		{name: "linux", goos: "linux", want: "bin/aws-mcp-proxy"},
		{name: "windows", goos: "windows", want: "bin/aws-mcp-proxy.exe"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := defaultBuildOutput(tt.goos); got != tt.want {
				t.Fatalf("defaultBuildOutput(%q) = %q, want %q", tt.goos, got, tt.want)
			}
		})
	}
}

func TestFlagProvided(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want bool
	}{
		{name: "long separate", args: []string{"--output", "bin/custom"}, want: true},
		{name: "long equals", args: []string{"--output=bin/custom"}, want: true},
		{name: "short separate", args: []string{"-output", "bin/custom"}, want: true},
		{name: "short equals", args: []string{"-output=bin/custom"}, want: true},
		{name: "absent", args: []string{"--goos", "windows"}, want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := flagProvided(tt.args, "output"); got != tt.want {
				t.Fatalf("flagProvided(%v, output) = %v, want %v", tt.args, got, tt.want)
			}
		})
	}
}

func TestRunWritesCommandErrorsToProvidedStderr(t *testing.T) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer

	code := run([]string{"build", "--unknown"}, &stdout, &stderr)

	if code == 0 {
		t.Fatal("run returned success for invalid build flag")
	}
	if stdout.Len() != 0 {
		t.Fatalf("stdout = %q, want empty", stdout.String())
	}
	if !strings.Contains(stderr.String(), "flag provided but not defined: -unknown") {
		t.Fatalf("stderr = %q, want invalid flag error", stderr.String())
	}
}
