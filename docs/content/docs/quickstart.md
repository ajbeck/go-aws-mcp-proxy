---

title: Quickstart
description: Bridge an MCP client to an AWS MCP endpoint in a couple of commands.
weight: 20
kicker: Documentation
---
## Run against a public endpoint

The public AWS documentation MCP endpoint can be queried without SigV4 signing. This is the fastest way to confirm the proxy works end to end:

{{< command >}}aws-mcp-proxy https://aws-mcp.us-east-1.api.aws/mcp --skip-auth{{< /command >}}

The proxy speaks stdio to your MCP client and forwards traffic to the remote Streamable HTTP endpoint. `--skip-auth` makes every upstream request unsigned and does not load AWS credentials.

## Run against a signed endpoint

Drop `--skip-auth` and point the proxy at an IAM-authenticated endpoint. Credentials load from the normal AWS chain — environment variables, shared config, profiles, and SSO:

```bash
aws-mcp-proxy https://<your-endpoint>.api.aws/mcp --profile <profile> --region us-east-1
```

{{< note >}}The proxy follows the upstream CLI where practical. See [parity with upstream](/docs/parity/) for the deliberate authentication-mode differences.{{< /note >}}

Before adding the signed endpoint to an MCP client, verify its resolved identity:

{{< command >}}aws-mcp-proxy doctor https://<your-endpoint>.api.aws/mcp --profile <profile>{{< /command >}}

The doctor command reports the credential source, expiration, account, and ARN
without printing credential values. Add `--probe` to check MCP initialize and
tool discovery.

## Use best-effort optional authentication

`--optional-auth` signs when credentials are available and otherwise sends unsigned requests. Use it only when an endpoint accepts both forms; it cannot be combined with `--skip-auth`.

## Wire it into an MCP client

Register the proxy as an MCP server command in your client. For a client that reads a JSON config, the server entry runs the binary with the endpoint as its argument:

```json
{
  "mcpServers": {
    "aws-docs": {
      "command": "aws-mcp-proxy",
      "args": ["https://aws-mcp.us-east-1.api.aws/mcp", "--skip-auth"]
    }
  }
}
```

{{< note type="tip" >}}Start with the unsigned documentation endpoint to validate your client wiring before introducing AWS credentials and SigV4 signing.{{< /note >}}
