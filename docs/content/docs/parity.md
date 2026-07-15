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
| `--profile`           | string    | <span class="badge badge--ok">Supported</span>   | Repeatable; first is the default. Also reads `AWS_MCP_PROXY_PROFILES` / `AWS_PROFILE`.                                                      |
| `--region`            | string    | <span class="badge badge--ok">Supported</span>   | Inferred from the endpoint or `AWS_REGION` when omitted.                                                                                    |
| `--metadata`          | key=value | <span class="badge badge--ok">Supported</span>   | Repeatable; injected into MCP requests.                                                                                                     |
| `--read-only`         | flag      | <span class="badge badge--ok">Supported</span>   | Disables tools that don't advertise `readOnlyHint=true`.                                                                                    |
| `--retries`           | int       | <span class="badge badge--ok">Supported</span>   | Default differs — see [behavior differences](#behavior-differences).                                                                      |
| `--log-level`         | enum      | <span class="badge badge--ok">Supported</span>   | `DEBUG` / `INFO` / `WARNING` / `ERROR` / `CRITICAL`.                                                                                        |
| `--timeout`           | seconds   | <span class="badge badge--ok">Supported</span>   | Total operation timeout.                                                                                                                    |
| `--connect-timeout`   | seconds   | <span class="badge badge--ok">Supported</span>   | Connection timeout.                                                                                                                         |
| `--read-timeout`      | seconds   | <span class="badge badge--ok">Supported</span>   | Read timeout.                                                                                                                               |
| `--write-timeout`     | seconds   | <span class="badge badge--ok">Supported</span>   | Write timeout.                                                                                                                              |
| `--tool-timeout`      | seconds   | <span class="badge badge--ok">Supported</span>   | Max seconds a tool call may run before cancellation.                                                                                        |
| `--skip-auth`         | flag      | <span class="badge badge--ok">Supported</span>   | Send unsigned requests when credentials are unavailable.                                                                                    |
| `--disable-telemetry` | flag      | <span class="badge badge--ok">Supported</span>   | Disable telemetry in outbound user-agent data.                                                                                              |
| `--ca-bundle`         | path      | <span class="badge badge--add">Added here</span> | Not in upstream. Trust an extra PEM bundle for TLS-intercepting corporate proxies without installing roots globally. Reads `AWS_CA_BUNDLE`. |

`--help` and `--version` are available on both.

## Behavior differences

Where this proxy diverges from upstream, it leans toward resilience and convenience — the "+" in parity+:

- **Retries default to 3, not 0.** Upstream disables retries by default; this proxy retries transient upstream failures out of the box (pass `--retries 0` to disable).
- **Service and region are inferred from the endpoint.** The host is parsed to derive the SigV4 service and region — including `*.api.aws` and `bedrock-agentcore` forms — so `--service` and `--region` are usually optional.
- **A managed CA bundle option.** `--ca-bundle` (or `AWS_CA_BUNDLE`) trusts an extra PEM bundle on top of the system roots, for corporate TLS interception, without modifying the machine's global trust store.

{{< note >}}This table reflects the upstream CLI as documented in [aws/mcp-proxy-for-aws](https://github.com/aws/mcp-proxy-for-aws). Upstream remains canonical for flag semantics and signing behavior — when in doubt, defer to it.{{< /note >}}
