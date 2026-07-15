---

title: Documentation
description: Install, configure, and run the AWS MCP proxy binary.
---

Bridge a stdio MCP client to a remote Streamable HTTP AWS MCP endpoint, signed with AWS SigV4 — shipped as a single native binary, with no Python or `uv` runtime required at agent startup.

{{< note >}}This project targets **parity+** with the upstream [aws/mcp-proxy-for-aws](https://github.com/aws/mcp-proxy-for-aws): it matches the upstream proxy's core behavior and flags where practical, then adds native-binary features on top. When a flag or behavior isn't documented here yet, treat the [upstream project](https://github.com/aws/mcp-proxy-for-aws) as canonical.{{< /note >}}
