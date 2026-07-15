---

title: Installation
description: Install the aws-mcp-proxy binary on Linux or macOS.
weight: 10
kicker: Documentation
---

## Install script

Install the latest release on Linux or macOS:

{{< command >}}curl -fsSL https://raw.githubusercontent.com/ajbeck/go-aws-mcp-proxy/main/install.sh | sh{{< /command >}}

The script downloads the correct binary for your platform and installs it to `/usr/local/bin` by default. Override the destination with `BIN_DIR`:

```bash
BIN_DIR="$HOME/.local/bin" curl -fsSL https://raw.githubusercontent.com/ajbeck/go-aws-mcp-proxy/main/install.sh | sh
```

## Build from source

The project builds with Go 1.26 and a small script runner:

```bash
go run ./cmd/scripts build
```

The binary is written to `./bin/aws-mcp-proxy`.

{{< note >}}The proxy loads AWS credentials from the standard AWS chain, including profiles and SSO. No credentials are required for unsigned endpoints such as the public AWS documentation MCP server.{{< /note >}}
