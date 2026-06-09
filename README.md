# Go AWS MCP Proxy

`go-aws-mcp-proxy` is an early Go rewrite of
[`aws/mcp-proxy-for-aws`](https://github.com/aws/mcp-proxy-for-aws).

The upstream project solves an important problem: it lets MCP clients connect to
IAM-authenticated AWS MCP endpoints by signing MCP traffic with AWS SigV4. This
project keeps that value proposition, but targets enterprise-friendly
distribution and operation by shipping a native binary instead of requiring a
Python and `uv` runtime at agent startup.

> Project status: early functional proxy. The current implementation can bridge
> stdio MCP clients to remote Streamable HTTP AWS MCP endpoints, including a
> live unsigned smoke test against the AWS documentation MCP endpoint.

## Why Rewrite It In Go?

The upstream proxy is distributed and documented as a Python package. The common
install path is:

```bash
uvx mcp-proxy-for-aws@latest <SigV4 MCP endpoint URL>
```

Local development and release workflows also rely on `uv`, Python packaging, and
Python runtime dependencies such as FastMCP, boto3, and botocore. That is a good
fit for Python-first users, but it creates friction in locked-down corporate
environments:

- `uvx` may download Python and dependencies on first launch, which is hard to
  approve, cache, audit, or make deterministic for every MCP client workstation.
- Corporate TLS interception often requires a managed CA bundle. Public issues
  in the AWS MCP ecosystem show users hitting `CERTIFICATE_VERIFY_FAILED` behind
  products such as Zscaler and Cloudflare WARP.
- Every proxied local AWS MCP server that depends on `uvx` inherits the same
  runtime, network, and certificate-management problem.
- A binary can be signed, checksummed, SBOM-scanned, mirrored internally, and
  installed without bootstrapping Python tooling inside every AI client.

Go gives us a smaller operational surface:

- one binary per platform for the proxy;
- standard AWS credential loading through the AWS SDK for Go v2;
- system trust store support through Go's TLS stack, plus an explicit CA bundle
  option when a company does not want to install roots globally;
- straightforward cross-platform CI and release builds.

## What This Proxy Should Do

The first functional version should match the upstream proxy's core behavior
before adding new surface area:

- bridge a stdio MCP client to a remote Streamable HTTP MCP endpoint;
- sign outgoing requests with AWS SigV4;
- load AWS credentials from the normal AWS chain, including profiles and SSO;
- support region, service, profile, timeout, retry, metadata, and read-only
  options compatible with the upstream CLI where practical;
- provide clear logging for startup, credential, TLS, throttling, and upstream
  MCP errors;
- fail loudly when upstream tool discovery is suspicious instead of silently
  returning an empty tool list after transient throttling.

Non-goals for the first version:

- replacing every AWS MCP server immediately;
- building a Go library API equivalent to the upstream Python library mode;
- adding client-specific plugins before the binary CLI and flags stabilize.

## Current Local Development

Build the application:

```bash
go run ./cmd/scripts build
```

The binary is written to `./bin/aws-mcp-proxy`.

Run tests:

```bash
go run ./cmd/scripts test
```

Run the full local CI path:

```bash
go run ./cmd/scripts ci
```

Run the proxy:

```bash
go run ./cmd/aws-mcp-proxy <SigV4 MCP endpoint URL> [flags]
```

For example, the public AWS documentation MCP endpoint can be queried without
SigV4 signing:

```bash
go run ./cmd/aws-mcp-proxy https://aws-mcp.us-east-1.api.aws/mcp --skip-auth
```

Run the live AWS documentation smoke test:

```bash
go run ./cmd/scripts/main.go smoke:aws-mcp --skip-auth
```

That smoke test lists upstream tools and calls
`aws___search_documentation` with an Amazon S3 documentation query. It is the
default live validation path because it does not require AWS credentials.

Run a signed smoke manually by passing a configured AWS profile:

```bash
go run ./cmd/scripts/main.go smoke:aws-mcp --skip-auth=false --profile <profile>
```

Install from a GitHub release on Linux or macOS:

```bash
curl -fsSL https://raw.githubusercontent.com/ajbeck/go-aws-mcp-proxy/main/install.sh | sh
```

## Planning

The first implementation scope, workflow plan, AWS MCP server rewrite sequence,
and plugin packaging recommendation are tracked in
[`getting started`](https://github.com/ajbeck/go-aws-mcp-proxy/issues/1).

## Upstream Research Notes

The README update is based on these upstream observations:

- [`aws/mcp-proxy-for-aws`](https://github.com/aws/mcp-proxy-for-aws) documents
  `uvx mcp-proxy-for-aws@latest` for normal use and `uv run` / `uv sync` for
  local development.
- The upstream `pyproject.toml` declares Python `>=3.10,<3.15` and dependencies
  on FastMCP, boto3, and botocore.
- The upstream workflows install `uv`, run Python checks and tests, build Python
  distributions, publish to PyPI, and publish a container image to public ECR.
- [`awslabs/mcp`](https://github.com/awslabs/mcp) documents many local AWS MCP
  servers using `uvx awslabs.<server>@latest`.
- Public issues document corporate TLS and certificate failures, including
  [`awslabs/mcp#773`](https://github.com/awslabs/mcp/issues/773) and
  [`awslabs/mcp#2498`](https://github.com/awslabs/mcp/issues/2498).
- Public proxy issues also point to reliability improvements worth designing in
  early, including
  [`aws/mcp-proxy-for-aws#287`](https://github.com/aws/mcp-proxy-for-aws/issues/287)
  and
  [`aws/mcp-proxy-for-aws#295`](https://github.com/aws/mcp-proxy-for-aws/issues/295).
