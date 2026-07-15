---

title: Quickstart
description: Bridge an MCP client to an AWS MCP endpoint in a couple of commands.
weight: 20
kicker: Documentation
---

## Run against a public endpoint

The public AWS documentation MCP endpoint can be queried without SigV4 signing. This is the fastest way to confirm the proxy works end to end:

{{< command >}}aws-mcp-proxy https://aws-mcp.us-east-1.api.aws/mcp --skip-auth{{< /command >}}

The proxy speaks stdio to your MCP client and forwards signed (or, with `--skip-auth`, unsigned) traffic to the remote Streamable HTTP endpoint.

## Run against a signed endpoint

Drop `--skip-auth` and point the proxy at an IAM-authenticated endpoint. Credentials load from the normal AWS chain — environment variables, shared config, profiles, and SSO:

```bash
aws-mcp-proxy https://<your-endpoint>.api.aws/mcp --profile <profile> --region us-east-1
```

{{< note >}}These flags mirror the upstream CLI. For the complete, canonical list of flags and signing behavior, see [aws/mcp-proxy-for-aws](https://github.com/aws/mcp-proxy-for-aws).{{< /note >}}

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
