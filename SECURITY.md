# Security Policy

## Reporting a Vulnerability

Please do not report security vulnerabilities through public GitHub issues.

Instead, report them privately through GitHub's [private vulnerability reporting](https://github.com/ajbeck/go-aws-mcp-proxy/security/advisories/new). This sends the report directly to the maintainers and keeps the details confidential until a fix is available.

When reporting, please include as much of the following as you can:

- A description of the vulnerability and its impact.
- Steps to reproduce, or a proof of concept.
- The affected version, commit, or platform.
- Any suggested mitigation, if you have one.

You can expect an initial response within a few days. We will keep you informed as we investigate and work toward a fix, and we will credit you in the advisory unless you ask us not to.

## Scope

This proxy loads AWS credentials through the AWS SDK for Go v2 and signs outgoing MCP traffic with AWS SigV4. Reports that are especially relevant include:

- Credential exposure, leakage, or unintended logging.
- Incorrect or bypassed SigV4 signing.
- TLS verification or CA bundle handling defects.
- Requests sent to unintended endpoints.

Findings in the third-party dependencies listed in `go.mod` are best reported upstream, though we are happy to help coordinate.
