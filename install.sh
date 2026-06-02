#!/bin/sh
set -eu

repo="${REPO:-ajbeck/go-aws-mcp-proxy}"
version="${VERSION:-latest}"
install_dir="${BIN_DIR:-${INSTALL_DIR:-/usr/local/bin}}"
binary="aws-mcp-proxy"

usage() {
  cat <<EOF
Usage: install.sh

Environment:
  REPO         GitHub repository to install from. Default: ${repo}
  VERSION      Release version to install, such as 1.2.3 or v1.2.3. Default: latest
  INSTALL_DIR  Directory to install into. Default: /usr/local/bin
  BIN_DIR      Alias for INSTALL_DIR.
EOF
}

need() {
  if ! command -v "$1" >/dev/null 2>&1; then
    echo "install.sh: required command not found: $1" >&2
    exit 1
  fi
}

detect_os() {
  case "$(uname -s)" in
    Darwin) echo "darwin" ;;
    Linux) echo "linux" ;;
    *)
      echo "install.sh: unsupported OS: $(uname -s)" >&2
      exit 1
      ;;
  esac
}

detect_arch() {
  case "$(uname -m)" in
    x86_64 | amd64) echo "amd64" ;;
    arm64 | aarch64) echo "arm64" ;;
    *)
      echo "install.sh: unsupported architecture: $(uname -m)" >&2
      exit 1
      ;;
  esac
}

checksum() {
  file="$1"
  sums="$2"

  if command -v sha256sum >/dev/null 2>&1; then
    (cd "$(dirname "$file")" && sha256sum -c "$(basename "$sums")")
    return
  fi

  if command -v shasum >/dev/null 2>&1; then
    (cd "$(dirname "$file")" && shasum -a 256 -c "$(basename "$sums")")
    return
  fi

  echo "install.sh: sha256sum or shasum is required to verify downloads" >&2
  exit 1
}

case "${1:-}" in
  -h | --help)
    usage
    exit 0
    ;;
  "")
    ;;
  *)
    echo "install.sh: unexpected argument: $1" >&2
    usage >&2
    exit 2
    ;;
esac

need curl
need tar
need install
need mktemp

os="$(detect_os)"
arch="$(detect_arch)"
asset="${binary}-${os}-${arch}.tar.gz"

case "$version" in
  latest) base_url="https://github.com/${repo}/releases/latest/download" ;;
  v*) base_url="https://github.com/${repo}/releases/download/${version}" ;;
  *) base_url="https://github.com/${repo}/releases/download/v${version}" ;;
esac

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT INT TERM

curl -fsSL "${base_url}/${asset}" -o "${tmp}/${asset}"
curl -fsSL "${base_url}/${asset}.sha256" -o "${tmp}/${asset}.sha256"

checksum "${tmp}/${asset}" "${tmp}/${asset}.sha256"
tar -C "$tmp" -xzf "${tmp}/${asset}"

install -d "$install_dir"
install -m 0755 "${tmp}/${binary}-${os}-${arch}/${binary}" "${install_dir}/${binary}"

echo "Installed ${binary} to ${install_dir}/${binary}"
