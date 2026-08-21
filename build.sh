#!/usr/bin/env bash
# ============================================================
#  mmx build script
#  usage: ./build.sh [version]
#    version - optional, e.g. v1.19.1 (default: read from VERSION file, fallback v0.0.0)
# ============================================================

set -euo pipefail

cd "$(dirname "${BASH_SOURCE[0]}")"

# --- determine version ---
MMX_VERSION="${1:-}"
if [[ -z "$MMX_VERSION" ]]; then
  if [[ -f "internal/core/VERSION" ]]; then
    MMX_VERSION="$(cat internal/core/VERSION)"
  fi
  if [[ -z "$MMX_VERSION" ]]; then
    MMX_VERSION="v0.0.0"
  fi
fi

echo "[1/3] Version: $MMX_VERSION"

# --- write VERSION embed file (no trailing newline) ---
printf '%s' "$MMX_VERSION" > internal/core/VERSION

# --- ensure bin directory ---
mkdir -p bin

# --- build ---
echo "[2/3] Building mmx ..."
export CGO_ENABLED=0
go build -trimpath -ldflags="-s -w" -o "bin/mmx" .

# --- done ---
echo "[3/3] Done: bin/mmx"
size=$(stat -c%s "bin/mmx" 2>/dev/null || stat -f%z "bin/mmx")
echo "       size: ${size} bytes, version: $MMX_VERSION"
