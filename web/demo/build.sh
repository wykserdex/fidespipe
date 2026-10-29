#!/usr/bin/env bash
# Builds the browser demo: the engine as a js/wasm module, plus the Go wasm
# loader that ships with the toolchain. Output lands next to this script and is
# served as-is — no bundler, no framework.
set -euo pipefail

cd "$(dirname "$0")/../.."
ROOT=$(pwd)
OUT="$ROOT/web/demo"
VERSION=$(cat "$ROOT/VERSION" 2>/dev/null || echo dev)

echo "==> building js/wasm engine (version $VERSION)"
GOOS=js GOARCH=wasm go build \
  -trimpath \
  -ldflags "-s -w -X github.com/wykserdex/fidespipe/internal/buildinfo.Version=$VERSION" \
  -o "$OUT/fidespipe.wasm" \
  ./cmd/fidespipewasm

GOROOT=$(go env GOROOT)
LOADER=""
for candidate in "$GOROOT/lib/wasm/wasm_exec.js" "$GOROOT/misc/wasm/wasm_exec.js"; do
  [ -f "$candidate" ] && LOADER="$candidate" && break
done
if [ -z "$LOADER" ]; then
  echo "wasm_exec.js not found under $GOROOT" >&2
  exit 1
fi
echo "==> copying wasm_exec.js from $(go env GOROOT)"
cp "$LOADER" "$OUT/wasm_exec.js"

SIZE=$(wc -c < "$OUT/fidespipe.wasm" | tr -d ' ')
echo "==> fidespipe.wasm: $SIZE bytes"
echo "    serve with:  python3 -m http.server -d web/demo 8080"
