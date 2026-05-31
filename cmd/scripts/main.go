package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	appPackage = "./cmd/aws-mcp-proxy"
	binDir     = "bin"
	binaryName = "aws-mcp-proxy"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		printUsage(stdout)
		return 0
	}

	switch args[0] {
	case "-h", "--help", "help":
		printUsage(stdout)
	case "build":
		return exitCode(stderr, build(args[1:], stdout, stderr))
	case "test":
		return exitCode(stderr, runCommand(stdout, stderr, "go", "test", "./..."))
	case "vet":
		return exitCode(stderr, runCommand(stdout, stderr, "go", "vet", "./..."))
	case "fmt":
		return exitCode(stderr, formatGoFiles(stdout, stderr, true))
	case "fmt:check":
		return exitCode(stderr, formatGoFiles(stdout, stderr, false))
	case "clean":
		return exitCode(stderr, clean(stderr))
	case "ci", "build:all":
		return exitCode(stderr, ci(stdout, stderr))
	case "smoke:aws-mcp":
		return exitCode(stderr, smokeAWSMCP(args[1:], stdout, stderr))
	default:
		fmt.Fprintf(stderr, "unknown command %q\n\n", args[0])
		printUsage(stderr)
		return 2
	}

	return 0
}

func printUsage(w io.Writer) {
	fmt.Fprintln(w, "Usage: go run ./cmd/scripts <command> [options]")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Commands:")
	fmt.Fprintln(w, "  build       build ./bin/aws-mcp-proxy")
	fmt.Fprintln(w, "  test        run go test ./...")
	fmt.Fprintln(w, "  vet         run go vet ./...")
	fmt.Fprintln(w, "  fmt         format Go files")
	fmt.Fprintln(w, "  fmt:check   verify Go files are formatted")
	fmt.Fprintln(w, "  clean       remove build output")
	fmt.Fprintln(w, "  ci          clean, format-check, vet, build, and test")
	fmt.Fprintln(w, "  smoke:aws-mcp")
	fmt.Fprintln(w, "              live smoke test against the AWS MCP endpoint")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Aliases:")
	fmt.Fprintln(w, "  build:all   same as ci")
}

func smokeAWSMCP(args []string, stdout, stderr io.Writer) error {
	flags := flag.NewFlagSet("smoke:aws-mcp", flag.ContinueOnError)
	flags.SetOutput(stderr)

	var metadata stringList
	endpoint := flags.String("endpoint", "https://aws-mcp.us-east-1.api.aws/mcp", "AWS MCP endpoint URL")
	profile := flags.String("profile", "", "AWS profile to use")
	region := flags.String("region", "us-east-1", "AWS signing region")
	service := flags.String("service", "aws-mcp", "AWS signing service")
	skipAuth := flags.Bool("skip-auth", true, "skip SigV4 signing")
	timeout := flags.Duration("timeout", 90*time.Second, "smoke test timeout")
	flags.Var(&metadata, "metadata", "metadata key=value to inject; repeatable")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("smoke:aws-mcp does not accept positional arguments: %s", strings.Join(flags.Args(), " "))
	}

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()

	proxyArgs := []string{
		"run",
		appPackage,
		*endpoint,
		"--region", *region,
		"--service", *service,
	}
	for _, item := range metadata.values {
		proxyArgs = append(proxyArgs, "--metadata", item)
	}
	if *profile != "" {
		proxyArgs = append(proxyArgs, "--profile", *profile)
	}
	if *skipAuth {
		proxyArgs = append(proxyArgs, "--skip-auth")
	}

	cmd := exec.CommandContext(ctx, "go", proxyArgs...)
	cmd.Stderr = stderr
	transport := &mcp.CommandTransport{
		Command:           cmd,
		TerminateDuration: 5 * time.Second,
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "aws-mcp-smoke", Version: "dev"}, nil)
	session, err := client.Connect(ctx, transport, nil)
	if err != nil {
		return fmt.Errorf("connect through proxy: %w", err)
	}
	defer session.Close()

	tools, err := session.ListTools(ctx, &mcp.ListToolsParams{})
	if err != nil {
		return fmt.Errorf("list tools: %w", err)
	}
	if !hasTool(tools.Tools, "aws___search_documentation") {
		return fmt.Errorf("expected tool aws___search_documentation, got %s", toolNames(tools.Tools))
	}

	result, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name: "aws___search_documentation",
		Arguments: map[string]any{
			"search_phrase": "Amazon S3 bucket naming rules",
			"limit":         1,
		},
	})
	if err != nil {
		return fmt.Errorf("call aws___search_documentation: %w", err)
	}
	if result == nil {
		return fmt.Errorf("call aws___search_documentation returned nil result")
	}
	if result.IsError {
		return fmt.Errorf("call aws___search_documentation returned tool error")
	}

	fmt.Fprintf(stdout, "AWS MCP smoke passed: listed %d tools and called aws___search_documentation\n", len(tools.Tools))
	return nil
}

func hasTool(tools []*mcp.Tool, name string) bool {
	for _, tool := range tools {
		if tool != nil && tool.Name == name {
			return true
		}
	}
	return false
}

func toolNames(tools []*mcp.Tool) string {
	names := make([]string, 0, len(tools))
	for _, tool := range tools {
		if tool != nil {
			names = append(names, tool.Name)
		}
	}
	return strings.Join(names, ", ")
}

type stringList struct {
	values []string
}

func (l *stringList) String() string {
	return strings.Join(l.values, ",")
}

func (l *stringList) Set(value string) error {
	l.values = append(l.values, value)
	return nil
}

func build(args []string, stdout, stderr io.Writer) error {
	flags := flag.NewFlagSet("build", flag.ContinueOnError)
	flags.SetOutput(stderr)

	goos := flags.String("goos", runtime.GOOS, "target GOOS")
	goarch := flags.String("goarch", runtime.GOARCH, "target GOARCH")
	output := flags.String("output", defaultBuildOutput(runtime.GOOS), "binary output path")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if !flagProvided(args, "output") {
		*output = defaultBuildOutput(*goos)
	}

	if flags.NArg() != 0 {
		return fmt.Errorf("build does not accept positional arguments: %s", strings.Join(flags.Args(), " "))
	}

	if err := os.MkdirAll(filepath.Dir(*output), 0o755); err != nil {
		return err
	}

	env := append(os.Environ(), "GOOS="+*goos, "GOARCH="+*goarch)
	return runCommandEnv(stdout, stderr, env, "go", "build", "-o", *output, appPackage)
}

func ci(stdout, stderr io.Writer) error {
	steps := []struct {
		name string
		run  func() error
	}{
		{name: "clean", run: func() error { return clean(stderr) }},
		{name: "fmt:check", run: func() error { return formatGoFiles(stdout, stderr, false) }},
		{name: "vet", run: func() error { return runCommand(stdout, stderr, "go", "vet", "./...") }},
		{name: "build", run: func() error { return build(nil, stdout, stderr) }},
		{name: "test", run: func() error { return runCommand(stdout, stderr, "go", "test", "./...") }},
	}

	for _, step := range steps {
		fmt.Fprintf(stderr, "==> %s\n", step.name)
		if err := step.run(); err != nil {
			return err
		}
	}

	return nil
}

func clean(stderr io.Writer) error {
	fmt.Fprintf(stderr, "removing %s\n", binDir)
	return os.RemoveAll(binDir)
}

func formatGoFiles(stdout, stderr io.Writer, write bool) error {
	files, err := goFiles(".")
	if err != nil {
		return err
	}
	if len(files) == 0 {
		return nil
	}

	if write {
		args := append([]string{"-w"}, files...)
		return runCommand(stdout, stderr, "gofmt", args...)
	}

	args := append([]string{"-l"}, files...)
	cmd := exec.Command("gofmt", args...)
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = stderr

	if err := cmd.Run(); err != nil {
		return err
	}

	if out.Len() > 0 {
		fmt.Fprint(stderr, out.String())
		return errors.New("Go files are not formatted; run go run ./cmd/scripts fmt")
	}

	return nil
}

func goFiles(root string) ([]string, error) {
	var files []string
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		if entry.IsDir() {
			switch entry.Name() {
			case ".git", binDir:
				return filepath.SkipDir
			}
			return nil
		}

		if strings.HasSuffix(path, ".go") {
			files = append(files, path)
		}

		return nil
	})
	return files, err
}

func runCommand(stdout, stderr io.Writer, name string, args ...string) error {
	return runCommandEnv(stdout, stderr, os.Environ(), name, args...)
}

func runCommandEnv(stdout, stderr io.Writer, env []string, name string, args ...string) error {
	fmt.Fprintf(stderr, "+ %s %s\n", name, strings.Join(args, " "))

	cmd := exec.Command(name, args...)
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	cmd.Stdin = os.Stdin
	cmd.Env = env

	return cmd.Run()
}

func exitCode(stderr io.Writer, err error) int {
	if err == nil {
		return 0
	}

	fmt.Fprintln(stderr, err)
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode()
	}

	return 1
}

func defaultBuildOutput(goos string) string {
	name := binaryName
	if goos == "windows" {
		name += ".exe"
	}
	return filepath.Join(binDir, name)
}

func flagProvided(args []string, name string) bool {
	long := "--" + name
	short := "-" + name
	for _, arg := range args {
		if arg == long || arg == short || strings.HasPrefix(arg, long+"=") || strings.HasPrefix(arg, short+"=") {
			return true
		}
	}
	return false
}
