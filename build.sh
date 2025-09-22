#!/usr/bin/env bash
set -euo pipefail

# Build Operetta server for multiple platforms.
# Usage: ./build.sh [platforms]
# Default platforms: windows/amd64 linux/amd64 windows/arm64 linux/arm64

cd "$(dirname "$0")"
mkdir -p dist

APP="operetta-server"
PKG="."
PLATFORMS=("windows/amd64" "linux/amd64" "windows/arm64" "linux/arm64")
if [ "$#" -gt 0 ]; then
  PLATFORMS=("$@")
fi

for p in "${PLATFORMS[@]}"; do
  os="${p%%/*}"
  arch="${p##*/}"
  ext=""
  [ "$os" = "windows" ] && ext=".exe"
  out="dist/${APP}-${os}-${arch}${ext}"
  echo "Building $out"
  CGO_ENABLED=0 GOOS="$os" GOARCH="$arch" go build -trimpath -ldflags "-s -w" -o "$out" "$PKG"
done

echo "Binaries are in ./dist"

