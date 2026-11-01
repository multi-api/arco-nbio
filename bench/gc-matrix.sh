#!/usr/bin/env bash
# GC investigation: short ramps (5k..20k) with gctrace, several configs, repeated.
set -uo pipefail
cd "$(dirname "$0")"
export STEPS=5000,10000,15000,20000 MEM=512m CPUS=1 SERVER_GODEBUG=gctrace=1
run() { local svc=$1 label=$2; shift 2; env "$@" ./run.sh "$svc" "$label" >/dev/null 2>&1; echo "$label rc=$?"; }
for i in 1 2; do
	run fork-tls gc-fork-$i
	run fork-tls-norelease gc-norel-$i
	run fork-tls gc-fork-tuned-$i GOMEMLIMIT=450MiB GOGC=200
	run llib-tls gc-llib-$i
done
