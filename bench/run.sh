#!/usr/bin/env bash
# One ramp + broadcast run against one server (adapted from arco-ws bench/run.sh).
# Usage: bench/run.sh <service> <label>
#   service: fork-tls | fork-tls-norelease | fork-plain | llib-tls
# Writes bench/results/<label>.json and <label>.log. Env: MEM, CPUS, STEPS, RATE, HOLD, ... (see bench.mjs).
set -uo pipefail
cd "$(dirname "$0")"

# Never run next to another benchmark project: its containers would skew memory and CPU.
if running=$(docker ps --format '{{.Names}}' | grep -E '^(ws-bench-|arco-ws-bench-)') ; then
  echo "refusing to start: ws-bench containers are running, stop them first:" >&2
  echo "$running" >&2
  exit 1
fi

service=${1:-}; label=${2:-}
if [[ -z "$service" || -z "$label" ]]; then
  echo "usage: $0 <service> <label>" >&2
  exit 2
fi

PROJECT=arco-nbio-bench
STEPS=${STEPS:-1000,2500,5000,7500,10000,15000,20000,25000,30000,40000,50000}
EXTRA_CONTAINERS=${EXTRA_CONTAINERS:-}
TLS=0
case "$service" in
  fork-plain) ;;
  fork-tls|fork-tls-norelease|llib-tls) TLS=1 ;;
  *) echo "unknown service: $service" >&2; exit 2 ;;
esac

mkdir -p results
cleanup() { docker compose rm -sf fork-tls fork-tls-norelease fork-plain llib-tls >/dev/null 2>&1; }
trap cleanup EXIT
trap 'exit 130' INT TERM
cleanup
docker compose up -d "$service" || exit 1
sleep 8
docker compose run --rm \
  -e SERVER="$service" -e LABEL="$label" -e STEPS="$STEPS" -e PORT=6001 -e TLS="$TLS" \
  -e PROJECT="$PROJECT" -e EXTRA_CONTAINERS="$EXTRA_CONTAINERS" \
  loadgen node bench.mjs 2>&1 | tee "results/$label.log"
docker compose logs --no-log-prefix "$service" > "results/$label.server.log" 2>&1
docker inspect "$PROJECT-$service-1" --format '{{json .State}}' > "results/$label.state.json" 2>/dev/null
for c in ${EXTRA_CONTAINERS//,/ }; do
  docker logs "$c" 2>&1 | tail -200 > "results/$label.$c.log"
done
