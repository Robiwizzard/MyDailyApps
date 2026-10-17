#!/bin/sh
# Builds the UI, then the single MyDailyApps executable (macOS / Linux).
# Usage: ./scripts/build.sh            (VERSION=v1.0.0 GOOGLE_CLIENT_ID=... optional)
set -eu
cd "$(dirname "$0")/.."
# go.mod needs Go 1.27.2. Go 1.21+ downloads it by itself; older Go cannot read go.mod.
command -v go >/dev/null || { echo "Go is not installed. Get Go 1.27.2 from https://go.dev/dl/"; exit 1; }
minor=$(go version | sed -E 's/.*go1\.([0-9]+).*/\1/')
if [ "$minor" -lt 21 ] 2>/dev/null; then echo "Found $(go version), but this project needs Go 1.27.2: https://go.dev/dl/"; exit 1; fi
(cd ui && npm install && npm run build && : > dist/.keep)
LDFLAGS="-s -w -X main.Version=${VERSION:-dev}"
if [ -n "${GOOGLE_CLIENT_ID:-}" ]; then
  LDFLAGS="$LDFLAGS -X github.com/robiwizzard/mydailyapps/internal/config.DefaultGoogleClientID=$GOOGLE_CLIENT_ID"
  LDFLAGS="$LDFLAGS -X github.com/robiwizzard/mydailyapps/internal/config.DefaultGoogleClientSecret=${GOOGLE_CLIENT_SECRET:-}"
fi
OUT=${OUT:-mydailyapps}
CGO_ENABLED=0 go build -trimpath -ldflags "$LDFLAGS" -o "$OUT" ./cmd/mydailyapps
echo "Built $OUT"
