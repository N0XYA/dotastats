#!/usr/bin/env bash
# Cross-compiles dotastats for every supported target into dist/.
# Usage: scripts/build.sh [version]   (default: git describe, else "dev")
set -euo pipefail

cd "$(dirname "$0")/.."

version="${1:-$(git describe --tags --always --dirty 2>/dev/null || echo dev)}"
targets=(
  windows/amd64
  linux/amd64
  linux/arm64
  darwin/arm64
  darwin/amd64
)

rm -rf dist
mkdir -p dist

for target in "${targets[@]}"; do
  goos="${target%/*}"
  goarch="${target#*/}"
  out="dist/dotastats-${goos}-${goarch}"
  if [[ "$goos" == windows ]]; then
    out+=".exe"
  fi
  echo "building $out ($version)"
  CGO_ENABLED=0 GOOS="$goos" GOARCH="$goarch" \
    go build -trimpath -ldflags "-s -w -X main.version=${version}" -o "$out" .
done

ls -lh dist
