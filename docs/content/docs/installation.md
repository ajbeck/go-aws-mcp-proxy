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
curl -fsSL https://raw.githubusercontent.com/ajbeck/go-aws-mcp-proxy/main/install.sh | BIN_DIR="$HOME/.local/bin" sh
```

For reproducible installation, pin both the installer source and the release:

```bash
curl -fsSL https://raw.githubusercontent.com/ajbeck/go-aws-mcp-proxy/v0.4.0/install.sh | VERSION=v0.4.0 sh
```

## Verify a release

Every release archive includes a SHA-256 file and an SPDX JSON SBOM. GitHub also
stores build-provenance and SBOM attestations for each archive. After downloading
an archive and its matching `.sha256` file, verify all three records:

```bash
sha256sum --check aws-mcp-proxy-linux-amd64.tar.gz.sha256
gh attestation verify aws-mcp-proxy-linux-amd64.tar.gz \
  --repo ajbeck/go-aws-mcp-proxy
gh attestation verify aws-mcp-proxy-linux-amd64.tar.gz \
  --repo ajbeck/go-aws-mcp-proxy \
  --predicate-type https://spdx.dev/Document/v2.3
```

On macOS, use `shasum -a 256 -c` for the checksum when `sha256sum` is not
installed.

## Build from source

The project builds with Go 1.26 and a small script runner:

```bash
go run ./cmd/scripts build
```

The binary is written to `./bin/aws-mcp-proxy`.

{{< note >}}The proxy loads AWS credentials from the standard AWS chain, including profiles and SSO. No credentials are required for unsigned endpoints such as the public AWS documentation MCP server.{{< /note >}}
