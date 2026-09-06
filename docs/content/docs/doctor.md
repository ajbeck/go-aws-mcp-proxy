---

title: Diagnose a connection
description: Validate configuration, credentials, identity, and MCP connectivity before client startup.
weight: 25
kicker: Documentation
---
`aws-mcp-proxy doctor` runs non-mutating preflight checks without starting the
stdio MCP server:

{{< command >}}aws-mcp-proxy doctor https://aws-mcp.us-east-1.api.aws/mcp --profile production{{< /command >}}

The command validates the endpoint, signing service, region, authentication
mode, and optional CA bundle. For each selected profile it then uses the AWS SDK
credential chain and STS `GetCallerIdentity` to report:

- credential provider source and expiration;
- effective AWS account, ARN, and user ID;
- an actionable remediation when a check fails.

It never prints access keys, secret keys, or session tokens. It also never
changes AWS configuration or invokes `aws login` or `aws sso login`.

## Probe the MCP endpoint

The configured MCP endpoint is not contacted by default. Add `--probe` to run
MCP initialize and `tools/list` using the same authentication and transport
configuration as the proxy:

{{< command >}}aws-mcp-proxy doctor https://aws-mcp.us-east-1.api.aws/mcp --profile production --probe{{< /command >}}

The probe does not call any upstream tool. It reports the discovered tool count
and closes the diagnostic session when finished.

For a public endpoint, disable credential and identity checks explicitly:

{{< command >}}aws-mcp-proxy doctor https://aws-mcp.us-east-1.api.aws/mcp --skip-auth --probe{{< /command >}}

With `--optional-auth`, unavailable credentials produce a warning and the
command remains healthy because the proxy can continue unsigned. Identity or
network failures after credentials resolve remain failures.

## JSON output

Use `--json` for scripts and support bundles:

```bash
aws-mcp-proxy doctor https://<endpoint>.api.aws/mcp \
  --profile production \
  --json
```

JSON field names, diagnostic statuses, categories, and exit codes are stable.
Credential values are never included.

## Exit codes

| Code | Meaning                                                                 |
| ---: | ----------------------------------------------------------------------- |
|  `0` | All required checks passed; warnings and skipped checks are non-fatal.  |
|  `1` | An internal or output error prevented diagnostics from completing.      |
|  `2` | CLI or proxy configuration is invalid.                                  |
|  `3` | Credentials could not be resolved or STS could not verify the identity. |
|  `4` | DNS, network, timeout, or TLS validation failed.                        |
|  `5` | The upstream MCP initialize or tool-discovery check failed.             |

The command writes its report to stdout. Transport logs are written to stderr
only when `--log-level` is supplied explicitly.
