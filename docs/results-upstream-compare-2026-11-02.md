# Head-to-head: our fork vs upstream nbio + the llib security backport

Decides whether to migrate arco-ws off our fork now that lesismal has shipped his crypto/tls security
backport (llib#26). Same memory/latency bench as `docs/results.md §4`: Apple Silicon Mac, Docker Desktop
linux/arm64, 512 MiB / 1 CPU per server, go1.27.3, same loadgen/harness (`bench/run.sh`, slope by
`bench/slope.py`), each variant on a clean host.

## What upstream actually ships today (verified 2026-11-03 via the module proxy + GitHub API)

- **nbio v1.7.2** pins **llib v1.2.5**, and `go get github.com/lesismal/nbio@v1.7.2` installs that today.
- **llib v1.2.5 IS the backport.** Its tag resolves to commit `09451cad` — the merge commit of
  **llib#26 "tls-security-backport"** (2026-11-02). So nbio v1.7.2 already ships lesismal's backported
  crypto/tls; there is nothing "pre-backport" about what a consumer gets.
- llib also has a `v2.0.0` tag (commit `e70b385`), a **separate, mis-tagged** branch: its `go.mod` is
  still `module github.com/lesismal/llib` with no `/v2`, so Go rejects `require …/llib@v2.0.0`. It is not
  what nbio ships and not installable through a normal dependency. (Earlier notes that called v1.2.5
  "pre-backport" and expected a "v1.2.6" were wrong; recorded here to correct them.)

## Result — the consumer-relevant comparison (2026-11-03, clean 2-way)

Fork (this repo, `e05f103`) vs stock `go get nbio@v1.7.2` (→ llib v1.2.5, **with** the backport). Both
binaries rebuilt from scratch; the stock one from a plain `require nbio v1.7.2 + llib v1.2.5`, no `replace`.

| Variant | KiB/conn (slope) | Idle MiB | Max stable conns | Stop |
|---|---|---|---|---|
| **fork** (Go 1.27.3 crypto/tls + non-blocking adapter) | **12.90** | 2.4 | 25,000 | none (ramp end, no crash) |
| stock **nbio v1.7.2** (llib v1.2.5, backport) | **23.67** | 11.2 | 20,000 | OOM-killed at 25,000 |

**The fork uses ~45 % less memory per connection and sustains 25 % more connections (25k vs 20k before
OOM) than the nbio you can install today — the released backport included.**

## Corroborating 2026-11-02 run (3-way, same harness)

| Variant | KiB/conn | Idle MiB | Max stable | Stop | p50/p99 @10k | @20k |
|---|---|---|---|---|---|---|
| fork | 12.34 | 11.3 | 30,000 | ramp timeout 38.4k | 84/198 | 145/281 |
| nbio + llib v1.2.5 (backport, what v1.7.2 ships) | 22.90 | 7.4 | 20,000 | OOM → 25k | 79/361 | 162/335 |
| nbio + llib v2.0.0 (the mis-tagged branch, forced via replace) | 18.32 | 5.5 | 25,000 | OOM → 30k | 74/163 | 183/440 |

(KiB/conn = least-squares slope of container memory vs live connections over all completed steps. Note the
two upstream tags are different codebases — v1.2.5, the released #26 backport, measures ~23; the separate
mis-tagged v2.0.0 commit measured ~18 but is not what nbio ships and can't be `require`d.)

## Reading

- Our fork is clearly ahead of the **installable** upstream: **12.9 vs 23.7 KiB/conn (~45 % less)**, and
  it holds 25k vs 20k under the 512 MiB cap. The gap is our idle read-buffer release.
- lesismal's backport is real work and is now released (llib v1.2.5), but on this bench the version nbio
  actually ships still sits ~2× our memory per connection.
- Latency (p50/p99) is broadly comparable; our fork's p99 is the steadiest.

## Decision

**Stay on the fork for arco-ws.** Even with lesismal's security backport released and consumed by
nbio v1.7.2, upstream is ~2× our memory per connection and tops out ~25 % lower. Re-evaluate if a future
upstream release closes that memory gap and has production mileage — then re-run this bench.
