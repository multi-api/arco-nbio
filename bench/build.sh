#!/usr/bin/env bash
# Build the benchmark server with this repo's nbio + crypto/tls (Go from go.mod, CGO off,
# linux/<docker arch>). bin/spike-b-llib is the unchanged arco-ws spike-b binary (upstream nbio
# v1.7.1 + llib v1.2.4, go1.27.0), copied from ~/Herd/arco-ws/spike/b/bin for a same-day baseline.
set -euo pipefail
cd "$(dirname "$0")/server"
arch=$(docker info --format '{{.Architecture}}' | sed 's/aarch64/arm64/; s/x86_64/amd64/')
CGO_ENABLED=0 GOOS=linux GOARCH="$arch" go build -trimpath -ldflags="-s -w" -o bin/server .
[ -f bin/spike-b-llib ] || cp ~/Herd/arco-ws/spike/b/bin/spike-b bin/spike-b-llib
echo "built bench/server/bin/server ($(go version | awk '{print $3}'), linux/$arch)"
