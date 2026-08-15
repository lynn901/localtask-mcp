#!/usr/bin/env bash
# Build localtask-mcp RPMs for RHEL-family Linux (x86_64 + aarch64).
#
# The binary has a FIXED embedKey baked in via -ldflags. Pass it in the
# EMBED_KEY env var (64 hex chars). All hosts installing these RPMs share
# that embedKey; each keeps its own keys.json.enc encrypted with it.
#
# Usage:
#   EMBED_KEY=<64hex> ./packaging/build-rpm.sh
#
# Requires: go (on PATH), nfpm (on PATH, or at ~/.local/bin/nfpm).
# Produces: dist/localtask-mcp-<ver>-1.<arch>.rpm
#
# Two nfpm configs (nfpm has no single-config multi-arch mode: `overrides:`
# keys are packager names, not arches; and ${VAR} substitution isn't applied
# to `src` globs in this nfpm version). arch: amd64|arm64 in each config;
# nfpm converts to x86_64|aarch64 in the RPM metadata + filename.
set -euo pipefail

cd "$(dirname "$0")/.."

export PATH="$HOME/.local/go/bin:$HOME/go/bin:$HOME/.local/bin:$PATH"
: ${EMBED_KEY:?EMBED_KEY is required (64 hex chars for AES-256).}
command -v go    >/dev/null || { echo "go not found on PATH" >&2; exit 1; }
command -v nfpm  >/dev/null || { echo "nfpm not found on PATH" >&2; exit 1; }

VERSION=$(awk '/^version:/{print $2}' packaging/nfpm-amd64.yaml)
RELEASE=$(awk '/^release:/{gsub(/"/,"",$2);print $2}' packaging/nfpm-amd64.yaml)

mkdir -p dist
EK="$EMBED_KEY"
chmod +x packaging/postinstall.sh packaging/preremove.sh

for ARCH in amd64 arm64; do
  echo "==> Building $ARCH binary (embedKey baked in)"
  GOOS=linux GOARCH=$ARCH CGO_ENABLED=0 go build -trimpath \
    -ldflags "-s -w -X main.embedKey=$EK" -o "dist/localtask-mcp-$ARCH" .

  echo "==> Packaging $ARCH RPM"
  # Point --target at the dir; nfpm names the file using the RPM arch
  # (amd64 -> x86_64, arm64 -> aarch64).
  TARGET_ARCH=$ARCH nfpm package --config "packaging/nfpm-$ARCH.yaml" \
    --packager rpm --target dist/
done

echo
echo "==> Done. Artifacts:"
ls -lh dist/localtask-mcp-*.rpm
echo
echo "Install on a target host (after placing keys.json.enc in /etc/localtask):"
echo "  sudo rpm -ivh dist/localtask-mcp-${VERSION}-${RELEASE}.<arch>.rpm"
echo "  sudo systemctl daemon-reload"
echo "  sudo systemctl enable --now localtask-mcp"
echo "(see the post-install message printed by rpm for the keys setup steps)"
