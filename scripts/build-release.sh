#!/usr/bin/env bash
# Build release artifacts for one or more platforms.
#
# Usage: scripts/build-release.sh <version> [os/arch ...]
#   scripts/build-release.sh 0.1.0                    # every default platform
#   scripts/build-release.sh 0.1.0 linux/amd64        # just one
#
# Writes dist/notify-<version>-<os>-<arch>.tar.gz plus dist/SHA256SUMS.
#
# This is the single definition of how a shipped notify binary is stamped: the
# release workflow calls it, and so can a human deploying by hand. A binary
# built any other way reports 0.0.0-dev and a running instance then cannot say
# what it is -- which is what made a silent production failure indistinguishable
# from a fixed one (#325, #327).
set -euo pipefail

VERSION="${1:-}"
if [ -z "$VERSION" ]; then
  echo "usage: $0 <version> [os/arch ...]" >&2
  exit 2
fi
shift
PLATFORMS=("$@")
if [ ${#PLATFORMS[@]} -eq 0 ]; then
  # linux/amd64 is what consumers deploy; darwin/arm64 is the mac-mini we run.
  PLATFORMS=(linux/amd64 darwin/arm64)
fi

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
DIST="$ROOT/dist"
rm -rf "$DIST"
mkdir -p "$DIST"

for platform in "${PLATFORMS[@]}"; do
  os="${platform%%/*}"
  arch="${platform##*/}"
  name="notify-$VERSION-$os-$arch"
  workdir="$DIST/$name"
  mkdir -p "$workdir"

  echo "building $platform"
  # CGO disabled so the binary is static and runs on a host with no toolchain;
  # -trimpath so the build is reproducible from any checkout path.
  CGO_ENABLED=0 GOOS="$os" GOARCH="$arch" go build -trimpath \
    -ldflags="-s -w -X github.com/danmestas/notify/internal/version.Version=$VERSION" \
    -o "$workdir/notify" "$ROOT/cmd/notify"

  tar -C "$DIST" -czf "$DIST/$name.tar.gz" "$name"
  rm -rf "$workdir"
done

# Checksums are relative to dist/ so `sha256sum -c SHA256SUMS` works after a
# plain download of the release assets into one directory.
cd "$DIST"
if command -v sha256sum >/dev/null 2>&1; then
  sha256sum ./*.tar.gz > SHA256SUMS
else
  shasum -a 256 ./*.tar.gz > SHA256SUMS
fi
cat SHA256SUMS
