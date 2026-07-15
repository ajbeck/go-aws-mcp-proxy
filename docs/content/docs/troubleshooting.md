---

title: Troubleshooting
description: TLS interception, credentials, and endpoint issues — and how to resolve them.
weight: 40
kicker: Documentation
---

## TLS: certificate signed by unknown authority

On a corporate network, a TLS-intercepting proxy — such as Zscaler or Cloudflare WARP — terminates HTTPS and re-signs it with an internal certificate authority. If that CA isn't in the trust store, connecting to the endpoint fails with:

```
x509: certificate signed by unknown authority
```

The proxy trusts the operating system's root store by default. To trust your organization's CA _without_ modifying the system store, point `--ca-bundle` (or the `AWS_CA_BUNDLE` environment variable) at a PEM file containing the corporate root:

{{< command >}}aws-mcp-proxy https://<endpoint>.api.aws/mcp --ca-bundle /etc/ssl/corp-root.pem{{< /command >}}

```bash
export AWS_CA_BUNDLE=/etc/ssl/corp-root.pem
aws-mcp-proxy https://<endpoint>.api.aws/mcp
```

The bundle is trusted _in addition to_ the system roots, so first-party AWS endpoints keep working.

{{< note >}}Ask IT for the organization's root CA in PEM format, or export it from the intercepting product. A `.crt` / `.cer` file converts with `openssl x509 -in corp-root.crt -out corp-root.pem -outform PEM`.{{< /note >}}

## Credentials: unable to sign the request

Signed endpoints need AWS credentials from the standard chain — environment variables, shared config, a named profile, or SSO. If none are found, the request can't be signed. Supply a profile:

{{< command >}}aws-mcp-proxy https://<endpoint>.api.aws/mcp --profile my-profile{{< /command >}}

For an unsigned endpoint, such as the public AWS documentation MCP server, skip signing entirely:

{{< command >}}aws-mcp-proxy https://aws-mcp.us-east-1.api.aws/mcp --skip-auth{{< /command >}}

## Region or service not detected

The SigV4 service and region are inferred from the endpoint host, including `*.api.aws` and `bedrock-agentcore` endpoints. For a host that doesn't follow those patterns, set them explicitly:

{{< command >}}aws-mcp-proxy https://<host>/mcp --service <service> --region us-east-1{{< /command >}}

## Turn on debug logging

When the cause isn't obvious, raise the log level to surface credential resolution, TLS setup, and upstream request detail on stderr:

{{< command >}}aws-mcp-proxy https://<endpoint>.api.aws/mcp --log-level DEBUG{{< /command >}}

{{< note >}}For flag semantics and signing behavior beyond what's covered here, the upstream [aws/mcp-proxy-for-aws](https://github.com/aws/mcp-proxy-for-aws) remains canonical.{{< /note >}}
