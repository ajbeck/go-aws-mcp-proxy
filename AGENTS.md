# Repository Guidelines

## Project Structure & Modular Organization

- `bin/aws-mcp-proxy` compiled application binary, output from `go run ./cmd/scripts build`
- `cmd/aws-mcp-proxy` Go 1.26 application
- `internal/` Core application packages, not for export

## Build, Test, Run

- `go run ./cmd/scripts build` builds `./bin/aws-mcp-proxy`
- `go run ./cmd/scripts test` runs `go test ./...`
- `go run ./cmd/scripts build:all` cleans build output path, verifies formatting, runs `go vet ./...`, compiles application, and runs tests

## Coding Style & Naming

## Testing Guidelines

## Commit and PR Guidelines

## Agent Notes

@RTK.md
