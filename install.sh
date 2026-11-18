#!/bin/sh
# Install kubeweave on macOS or Linux from GitHub releases.
#
#   curl -fsSL https://raw.githubusercontent.com/gauravthakur02/kubeweave/main/install.sh | sh
#
# Options (environment variables):
#   KUBEWEAVE_VERSION=0.2.0          a specific version (default: the latest release)
#   KUBEWEAVE_INSTALL_DIR=~/bin      where to put the binary (default: /usr/local/bin if writable, else ~/.local/bin)
#   KUBEWEAVE_DOWNLOAD_URL=https://… a mirror holding the release files (default: the GitHub release)
set -eu

REPO="gauravthakur02/kubeweave"
BIN="kubeweave"

say() { printf '%s\n' "$*" >&2; }
fail() { say "kubeweave install: $*"; exit 1; }
need() { command -v "$1" >/dev/null 2>&1 || fail "$1 is required"; }

fetch() { # fetch URL [output]
  if command -v curl >/dev/null 2>&1; then
    if [ $# -eq 2 ]; then curl -fsSL "$1" -o "$2"; else curl -fsSL "$1"; fi
  elif command -v wget >/dev/null 2>&1; then
    if [ $# -eq 2 ]; then wget -qO "$2" "$1"; else wget -qO- "$1"; fi
  else
    fail "curl or wget is required"
  fi
}

need tar
need uname

os=$(uname -s | tr '[:upper:]' '[:lower:]')
case "$os" in
  darwin | linux) ;;
  *) fail "unsupported OS $os (on Windows use install.ps1)" ;;
esac

arch=$(uname -m)
case "$arch" in
  x86_64 | amd64) arch=amd64 ;;
  arm64 | aarch64) arch=arm64 ;;
  armv7* | armv8l) arch=armv7 ;;
  *) fail "unsupported CPU $arch" ;;
esac
[ "$os" = darwin ] && [ "$arch" = armv7 ] && fail "unsupported CPU $arch on macOS"

version="${KUBEWEAVE_VERSION:-}"
if [ -z "$version" ]; then
  version=$(fetch "https://api.github.com/repos/$REPO/releases/latest" |
    sed -n 's/.*"tag_name": *"v\{0,1\}\([^"]*\)".*/\1/p' | head -n 1)
  [ -n "$version" ] || fail "could not find the latest release (set KUBEWEAVE_VERSION)"
fi
version="${version#v}"

archive="${BIN}_${version}_${os}_${arch}.tar.gz"
base="${KUBEWEAVE_DOWNLOAD_URL:-https://github.com/$REPO/releases/download/v$version}"
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT INT TERM

say "Downloading kubeweave $version for $os/$arch…"
fetch "$base/$archive" "$tmp/$archive" || fail "download failed: $base/$archive"
fetch "$base/checksums.txt" "$tmp/checksums.txt" || fail "download failed: checksums.txt"

want=$(grep " $archive\$" "$tmp/checksums.txt" | awk '{print $1}')
[ -n "$want" ] || fail "$archive is not listed in checksums.txt"
if command -v sha256sum >/dev/null 2>&1; then
  got=$(sha256sum "$tmp/$archive" | awk '{print $1}')
elif command -v shasum >/dev/null 2>&1; then
  got=$(shasum -a 256 "$tmp/$archive" | awk '{print $1}')
else
  fail "sha256sum or shasum is required to verify the download"
fi
[ "$want" = "$got" ] || fail "checksum mismatch for $archive"

tar -xzf "$tmp/$archive" -C "$tmp" "$BIN"

dir="${KUBEWEAVE_INSTALL_DIR:-}"
if [ -z "$dir" ]; then
  if [ -w /usr/local/bin ]; then dir=/usr/local/bin; else dir="$HOME/.local/bin"; fi
fi
mkdir -p "$dir"
install -m 0755 "$tmp/$BIN" "$dir/$BIN" 2>/dev/null || { cp "$tmp/$BIN" "$dir/$BIN" && chmod 0755 "$dir/$BIN"; }

say "✓ installed $dir/$BIN"
case ":$PATH:" in
  *":$dir:"*) ;;
  *) say "  add it to your PATH:  export PATH=\"$dir:\$PATH\"" ;;
esac
"$dir/$BIN" version >/dev/null 2>&1 || say "  (could not run $dir/$BIN version)"
