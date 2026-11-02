# Head-to-head: our fork vs upstream after the llib backport (2026-11-02)

Context: lesismal merged his crypto/tls security backport (llib#26) and released it as **llib v2.0.0**.
This re-runs the same memory/latency bench as `docs/results.md §4`, three ways, to decide whether to
migrate arco-ws off our fork. Host: Apple Silicon Mac, Docker Desktop linux/arm64, 512 MiB / 1 CPU per
server, go1.27.3, same loadgen/harness (`bench/run.sh`, slope by `bench/slope.py`). All three ran
back-to-back on the same host, VM free of other benchmarks.

## Packaging finding (matters for the migration decision)
**llib v2.0.0 is mis-tagged for Go modules.** Its `go.mod` still declares `module github.com/lesismal/llib`
with no `/v2` suffix, which Go requires for major version ≥ 2, so `go get github.com/lesismal/llib@v2.0.0`
is rejected ("version v2.0.0 invalid: should be v0 or v1, not v2"). Consequently **nbio v1.7.2 (released
the same day) still pins llib v1.2.5** — upstream cannot consume the backport through a normal `require`
yet either. We benched v2.0.0 by forcing it with a local `replace` to the v2.0.0 commit (e70b385).

## Variants
| Label | Server |
|---|---|
| `cmp-fork` | our arco-nbio fork — Go 1.27.3 crypto/tls copy + non-blocking adapter (idle-buffer release on) |
| `cmp-nbio-llib200` | nbio v1.7.2 + **llib v2.0.0** (the backport), forced via replace |
| `cmp-nbio-llib125` | nbio v1.7.2 **as shipped** (llib v1.2.5, pre-backport TLS) |

## Result
| Variant | KiB/conn (slope) | Idle MiB | Max stable conns | Stop | p50/p99 @10k | @20k |
|---|---|---|---|---|---|---|
| **cmp-fork** | **12.34** | 11.3 | **30,000** | ramp timeout 38.4k (memory) | 84/198 | 145/281 |
| cmp-nbio-llib200 | 18.32 | 5.5 | 25,000 | OOM-killed → 30k | 74/163 | 183/440 |
| cmp-nbio-llib125 | 22.90 | 7.4 | 20,000 | OOM-killed → 25k | 79/361 | 162/335 |

(KiB/conn = least-squares slope of container memory vs live connections over all completed steps.)

## Reading
- **Our fork is still clearly ahead: 12.34 KiB/conn vs 18.32 (backport) vs 22.90 (stock)** — ~33 % less
  memory per connection than even the improved upstream, ~46 % less than current shipping nbio. It also
  sustains the most connections (30k vs 25k vs 20k) under the 512 MiB cap.
- **The backport genuinely helps upstream** (22.9 → 18.3 KiB/conn): lesismal's non-blocking rework
  recovers real memory. But it does not reach our fork's figure — the gap is our idle read-buffer
  release (with release off, our adapter sat at ~19.5 in `docs/results.md §4`, right where v2.0.0 lands).
- Latency (p50/p99) is broadly comparable across all three; our fork's p99 is the steadiest.
- Caveats: `cmp-nbio-llib125` OOM'd earliest (7 points) so its slope is the noisiest; the headline
  comparison is fork vs v2.0.0, both of which completed more steps on the same host.

## Decision input
Supports staying on our fork for arco-ws for now: it is materially more memory-efficient than both the
current upstream release and the (not-yet-consumable) backport, and the backport is not yet available
through a normal nbio dependency. Re-evaluate once nbio ships a release that depends on a correctly
tagged llib (v2.x with `/v2`, or a v1.2.6) and that combination has production mileage.

Raw: `bench/results/cmp-*.{json,log}`. Reproduce: `scratchpad` variant sources + `bench/docker-compose.override.yml`.
