# Changelog

All notable changes to `go-aws-mcp-proxy` are documented in this file.

Release Please manages this file from merged pull requests. The project follows
[Semantic Versioning](https://semver.org/spec/v2.0.0.html), and links are
absolute so they resolve identically on GitHub and on the
[documentation site](https://aws-mcp-proxy.ajbeck.dev/). Pending changes live
in the release pull request rather than in an `Unreleased` section on `main`.

## [1.0.1](https://github.com/ajbeck/go-aws-mcp-proxy/compare/v1.0.0...v1.0.1) (2026-09-07)


### Bug Fixes

* **release:** specify repository for GitHub CLI ([#42](https://github.com/ajbeck/go-aws-mcp-proxy/issues/42)) ([246aedb](https://github.com/ajbeck/go-aws-mcp-proxy/commit/246aedb2ebf0ba8a4565d492cee475b414672796))

## [1.0.0](https://github.com/ajbeck/go-aws-mcp-proxy/compare/v0.4.0...v1.0.0) (2026-09-06)


### Features

* add lazy connection and empty tool controls ([1ad9978](https://github.com/ajbeck/go-aws-mcp-proxy/commit/1ad9978f0711f3ea6e3c9729563e4f146e5dfc00))
* expose supported embedding and doctor diagnostic APIs ([1ad9978](https://github.com/ajbeck/go-aws-mcp-proxy/commit/1ad9978f0711f3ea6e3c9729563e4f146e5dfc00))
* publish release SBOMs and provenance attestations ([1ad9978](https://github.com/ajbeck/go-aws-mcp-proxy/commit/1ad9978f0711f3ea6e3c9729563e4f146e5dfc00))
* route authenticated tools across selected AWS profiles ([1ad9978](https://github.com/ajbeck/go-aws-mcp-proxy/commit/1ad9978f0711f3ea6e3c9729563e4f146e5dfc00))


### Bug Fixes

* align CLI timeouts, grouped arguments, and profile precedence ([1ad9978](https://github.com/ajbeck/go-aws-mcp-proxy/commit/1ad9978f0711f3ea6e3c9729563e4f146e5dfc00))
* harden upstream HTTP redirects and transport failure classification ([1ad9978](https://github.com/ajbeck/go-aws-mcp-proxy/commit/1ad9978f0711f3ea6e3c9729563e4f146e5dfc00))
* isolate credential process input and preserve upstream profile fields ([1ad9978](https://github.com/ajbeck/go-aws-mcp-proxy/commit/1ad9978f0711f3ea6e3c9729563e4f146e5dfc00))
* recover deferred sessions and credentials without replaying tool calls ([1ad9978](https://github.com/ajbeck/go-aws-mcp-proxy/commit/1ad9978f0711f3ea6e3c9729563e4f146e5dfc00))
* support Windows credential process execution ([1ad9978](https://github.com/ajbeck/go-aws-mcp-proxy/commit/1ad9978f0711f3ea6e3c9729563e4f146e5dfc00))
* synchronize live upstream tool catalogs and capabilities ([1ad9978](https://github.com/ajbeck/go-aws-mcp-proxy/commit/1ad9978f0711f3ea6e3c9729563e4f146e5dfc00))

## [0.4.0](https://github.com/ajbeck/go-aws-mcp-proxy/releases/tag/v0.4.0) - 2026-08-19

### Added

- `--optional-auth` for endpoints that accept either signed or unsigned requests.

### Changed

- Make `--skip-auth` strictly unsigned: credentials are not loaded and no upstream request is signed.
- Update Go and GitHub Actions dependencies.

## [0.3.0](https://github.com/ajbeck/go-aws-mcp-proxy/releases/tag/v0.3.0) - 2026-07-22

### Fixed

- Support MCP endpoints that reject the optional standalone SSE `GET` stream and operate over POST-only Streamable HTTP.

### Changed

- Default manual releases to the current `main` commit while retaining explicit commit selection.

## [0.2.0](https://github.com/ajbeck/go-aws-mcp-proxy/releases/tag/v0.2.0) - 2026-07-15

### Added

- Hugo documentation site with GitHub Pages deployment.
- MCP-visible proxy status reporting for credential and connection failures.

### Fixed

- Restore upstream HTTP request defaults and reject unsafe endpoint schemes.
- Inject the resolved AWS region into request metadata.
- Classify and log upstream HTTP failures with actionable MCP-visible errors.
- Defer upstream connection for Kiro and Amazon Q compatibility.

### Changed

- Update Go and GitHub Actions dependencies.

## [0.1.0](https://github.com/ajbeck/go-aws-mcp-proxy/releases/tag/v0.1.0) - 2026-06-12

First functional release: a Go rewrite of [`aws/mcp-proxy-for-aws`](https://github.com/aws/mcp-proxy-for-aws) that ships the upstream proxy's core behavior as a single native binary — with no Python or `uv` runtime to bootstrap — and reaches full parity on the upstream CLI flags. Start with the [installation guide](https://aws-mcp-proxy.ajbeck.dev/docs/installation/) and the [quickstart](https://aws-mcp-proxy.ajbeck.dev/docs/quickstart/).

### Added

- Bridge a stdio MCP client to a remote Streamable HTTP AWS MCP endpoint.
- AWS SigV4 request signing, with credentials loaded from the standard AWS chain: environment variables, shared config, named profiles, and SSO.
- Full parity with the upstream CLI flags — `--service`, `--profile`, `--region`, `--metadata`, `--read-only`, `--retries`, `--log-level`, the `--connect-timeout` / `--read-timeout` / `--write-timeout` / `--tool-timeout` timeouts, `--skip-auth`, and `--disable-telemetry`. See the [parity matrix](https://aws-mcp-proxy.ajbeck.dev/docs/parity/).
- `--ca-bundle` (and the `AWS_CA_BUNDLE` environment variable) to trust an additional PEM certificate bundle for corporate TLS interception, without modifying the system trust store.
- Automatic SigV4 service and region inference from the endpoint host, including `*.api.aws` and `bedrock-agentcore` endpoints.
- Resilient retry defaults: transient upstream failures are retried up to three times by default (`--retries 0` disables retries).
- `--skip-auth` for unsigned endpoints, such as the public AWS documentation MCP server.
- Install script for Linux and macOS, and `go run ./cmd/scripts build` to build from source.
