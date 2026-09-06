package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
)

func main() {
	if os.Getenv("AWS_MCP_PROXY_CREDENTIAL_REQUIRE_EOF") == "1" {
		buffer := make([]byte, 1)
		if _, err := os.Stdin.Read(buffer); !errors.Is(err, io.EOF) {
			fmt.Fprintf(os.Stderr, "credential helper stdin error = %v, want EOF\n", err)
			os.Exit(2)
		}
	}

	accessKey := os.Getenv("AWS_MCP_PROXY_CREDENTIAL_ACCESS_KEY")
	if accessKey == "" {
		accessKey = "test-access-key"
	}
	if err := json.NewEncoder(os.Stdout).Encode(map[string]any{
		"Version":         1,
		"AccessKeyId":     accessKey,
		"SecretAccessKey": "test-secret-key",
		"SessionToken":    "test-session-token",
	}); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
