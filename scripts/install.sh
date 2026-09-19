#!/bin/sh
# cometduty installer — downloads the latest (or a given) release, verifies
# the sha256 checksum, and installs the binary.
#
#   curl -fsSL https://raw.githubusercontent.com/abhijitkrm/cometduty/main/scripts/install.sh | sh
#   ... | sh -s -- --version v0.1.0 --dir /usr/local/bin
#
set -eu

REPO="abhijitkrm/cometduty"
VERSION=""
INSTALL_DIR=""

while [ $# -gt 0 ]; do
    case "$1" in
        --version|-v) VERSION="$2"; shift 2 ;;
        --dir|-d)     INSTALL_DIR="$2"; shift 2 ;;
        *) echo "unknown arg: $1" >&2; exit 1 ;;
    esac
done

# --- os/arch → release artifact naming ---
OS=$(uname -s | tr '[:upper:]' '[:lower:]')
ARCH=$(uname -m)
case "$ARCH" in
    x86_64|amd64) ARCH="amd64" ;;
    arm64|aarch64) ARCH="arm64" ;;
    *) echo "unsupported arch: $ARCH" >&2; exit 1 ;;
esac
case "$OS" in
    linux|darwin) ;;
    *) echo "unsupported os: $OS (download manually or use docker)" >&2; exit 1 ;;
esac

# --- resolve version ---
if [ -z "$VERSION" ]; then
    VERSION=$(curl -fsSL "https://api.github.com/repos/$REPO/releases/latest" \
        | sed -n 's/.*"tag_name":[[:space:]]*"\([^"]*\)".*/\1/p' | head -1)
    [ -n "$VERSION" ] || { echo "could not resolve latest release" >&2; exit 1; }
fi
VER="${VERSION#v}"

# --- pick an install dir ---
if [ -z "$INSTALL_DIR" ]; then
    if [ -w /usr/local/bin ]; then
        INSTALL_DIR=/usr/local/bin
    else
        INSTALL_DIR="$HOME/.local/bin"
    fi
fi
mkdir -p "$INSTALL_DIR"

# --- download + verify ---
TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT
BASE="https://github.com/$REPO/releases/download/$VERSION"
TARBALL="cometduty_${VER}_${OS}_${ARCH}.tar.gz"

echo "downloading $TARBALL ($VERSION)"
curl -fsSL "$BASE/$TARBALL" -o "$TMP/$TARBALL"
curl -fsSL "$BASE/checksums.txt" -o "$TMP/checksums.txt"

cd "$TMP"
if command -v sha256sum >/dev/null 2>&1; then
    grep " $TARBALL$" checksums.txt | sha256sum -c -
else
    grep " $TARBALL$" checksums.txt | shasum -a 256 -c -
fi

tar -xzf "$TARBALL" cometduty
install -m 0755 cometduty "$INSTALL_DIR/cometduty"

echo "installed cometduty $VERSION -> $INSTALL_DIR/cometduty"
case ":$PATH:" in
    *":$INSTALL_DIR:"*) ;;
    *) echo "note: $INSTALL_DIR is not on your PATH" ;;
esac
"$INSTALL_DIR/cometduty" version 2>/dev/null || true
