---

title: Parity with upstream
description: How the aws-mcp-proxy CLI maps to aws/mcp-proxy-for-aws, flag by flag.
weight: 30
kicker: Documentation
---
This project targets **parity+** with [aws/mcp-proxy-for-aws](https://github.com/aws/mcp-proxy-for-aws): match the upstream CLI where practical, then add features on top. The table below maps every upstream flag to its status here.

<p class="badge-legend"><span class="badge-legend__item"><span class="badge badge--ok">Supported</span> matches upstream</span><span class="badge-legend__item"><span class="badge badge--add">Added here</span> beyond upstream</span><span class="badge-legend__item"><span class="badge badge--plan">Planned</span> not yet implemented</span></p>

## CLI flags

| Upstream flag         | Value     | Status                                           | Notes                                                                                                                                       |
| --------------------- | --------- | ------------------------------------------------ | ------------------------------------------------------------------------------------------------------------------------------------------- |
| `endpoint`            | URL       | <span class="badge badge--ok">Supported</span>   | Required positional SigV4 MCP endpoint URL.                                                                                                 |
| `--service`           | string    | <span class="badge badge--ok">Supported</span>   | Inferred from the endpoint host when omitted.                                                                                               |
| `--profile`           | string    | <span class="badge badge--ok">Supported</span>   | Repeatable or grouped; first is the default. `AWS_MCP_PROXY_PROFILES` takes precedence, then CLI values, then `AWS_PROFILE`.                |
| `--region`            | string    | <span class="badge badge--ok">Supported</span>   | Inferred from the endpoint or `AWS_REGION` when omitted.                                                                                    |
| `--metadata`          | key=value | <span class="badge badge--ok">Supported</span>   | Repeatable or grouped; injected into MCP requests.                                                                                          |
| `--read-only`         | flag      | <span class="badge badge--ok">Supported</span>   | Disables tools that don't advertise `readOnlyHint=true`.                                                                                    |
| `--retries`           | int       | <span class="badge badge--ok">Supported</span>   | Default differs — see [behavior differences](#behavior-differences).                                                                      |
| `--log-level`         | enum      | <span class="badge badge--ok">Supported</span>   | `DEBUG` / `INFO` / `WARNING` / `ERROR` / `CRITICAL`.                                                                                        |
| `--timeout`           | seconds   | <span class="badge badge--ok">Supported</span>   | Total operation timeout; defaults to 180 seconds.                                                                                           |
| `--connect-timeout`   | seconds   | <span class="badge badge--ok">Supported</span>   | Connection timeout; defaults to 60 seconds.                                                                                                 |
| `--read-timeout`      | seconds   | <span class="badge badge--ok">Supported</span>   | Read timeout; defaults to 120 seconds.                                                                                                      |
| `--write-timeout`     | seconds   | <span class="badge badge--ok">Supported</span>   | Write timeout; defaults to 180 seconds.                                                                                                     |
| `--tool-timeout`      | seconds   | <span class="badge badge--ok">Supported</span>   | Tool-call deadline; defaults to 300 seconds.                                                                                                |
| `--skip-auth`         | flag      | <span class="badge badge--add">Added here</span> | Always send unsigned requests and do not load AWS credentials.                                                                              |
| `--optional-auth`     | flag      | <span class="badge badge--add">Added here</span> | Sign when credentials resolve; otherwise send unsigned requests. Cannot be combined with `--skip-auth`.                                     |
| `--disable-telemetry` | flag      | <span class="badge badge--ok">Supported</span>   | Disable telemetry in outbound user-agent data.                                                                                              |
| `--ca-bundle`         | path      | <span class="badge badge--add">Added here</span> | Not in upstream. Trust an extra PEM bundle for TLS-intercepting corporate proxies without installing roots globally. Reads `AWS_CA_BUNDLE`. |
| `--lazy-connect`      | flag      | <span class="badge badge--add">Added here</span> | Defer the upstream connection until the first upstream request; Kiro and Q clients receive this compatibility behavior automatically.       |
| `--allow-empty-tools` | flag      | <span class="badge badge--add">Added here</span> | Accept an intentionally empty initial upstream tool catalog instead of treating it as a retryable startup failure.                          |

`--help` and `--version` are available on both.

## Behavior differences

Where this proxy diverges from upstream, it leans toward resilience and convenience — the "+" in parity+:

- **Retries default to 3, not 0.** Upstream disables retries by default; this proxy retries transient connection and discovery failures out of the box (pass `--retries 0` to disable). It never automatically replays a tool call.
- **Service and region are inferred from the endpoint.** The host is parsed to derive the SigV4 service and region — including `*.api.aws` and `bedrock-agentcore` forms — so `--service` and `--region` are usually optional.
- **A managed CA bundle option.** `--ca-bundle` (or `AWS_CA_BUNDLE`) trusts an extra PEM bundle on top of the system roots, for corporate TLS interception, without modifying the machine's global trust store.
- **Authentication modes are explicit.** Upstream `--skip-auth` still signs when it can resolve credentials. Here, `--skip-auth` is strictly unsigned; `--optional-auth` provides a best-effort signing fallback for mixed endpoints.
- **Profile routing follows the signed endpoint boundary.** Public AWS MCP knowledge tools remain profile-free; future authenticated AWS tools and tools on other SigV4 endpoints, including EKS, can switch among configured profiles. An upstream-owned `aws_profile` field is preserved rather than shadowed.
- **Tool discovery remains live.** The proxy reconciles additions, removals, schema changes, pagination, and upstream list-change notifications instead of freezing the initial catalog.

{{< note >}}This table uses [aws/mcp-proxy-for-aws](https://github.com/aws/mcp-proxy-for-aws) as its comparison point. The entries marked “Added here” intentionally define this proxy’s different authentication behavior.{{< /note >}}
