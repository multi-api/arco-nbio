# Results and verdict

Date: 2026-10-31. Host: Apple Silicon Mac (darwin/arm64), Docker Desktop VM (linux/arm64, 8 CPUs, 4 GiB).
Toolchain go1.27.3. Raw outputs are in `docs/results/` and `bench/results/`.

## 1. Tests

| What | Result | Output |
|---|---|---|
| Go 1.27.3 `crypto/tls` test suite on the copy (206 top-level tests, 1,500+ subtests, including the byte-exact recorded handshakes in `testdata/`) | **pass**; 8 skipped: BoGo (needs BoringSSL fetched), 4 FIPS-140-only reruns + `TestGenerateCertificates` (FIPS-only mode unsupported here), 2 external-network tests | `results/go-tls-suite.txt` |
| Same with `-race` | **pass** | `results/go-tls-suite-race.txt` |
| Our non-blocking tests (9): echo against Go's **standard library** client (an independent copy) for TLS 1.2/1.3 × poller read sizes 1, 7, 100, 512, 4096, 65536 bytes; first request arriving with the client Finished; resumption with tickets; garbage ClientHello; 50 abandoned handshakes leave no goroutine; handshake timeout; direct `Handshake` fails instead of blocking; buffers released when idle; llib API surface | **pass**, also with `-race` | in the suite above |
| Audit regression tests (8), all against the non-blocking server | **pass**, also with `-race` | see §2 |
| nbio's own tests (fork) | **pass** — same as upstream | `results/nbio-tests.txt`, `results/nbio-tests-upstream-baseline.txt` |
| nbio's own tests with `-race` | `crypto/tls`, `nbhttp`, `nbhttp/websocket`, `logging`, `timer` pass. `nbio` (`TestUnix`) and `lmux` fail with **data races inside nbio's test code** — upstream nbio without any change fails the same two tests the same way (10 races). `mempool` hit the 10-minute timeout under `-race` (stress test; passes in 2 s without `-race`; package untouched) | `results/nbio-tests-race.txt` |
| Fuzzing, 2 targets × 3 min: `FuzzNonBlockRecordStream` (full TLS 1.2/1.3 connection, fuzz-chosen read sizes, one corrupted byte anywhere; invariants: no panic, no hang, delivered plaintext is always a prefix of what was sent, exact when uncorrupted) and `FuzzNonBlockGarbage` (arbitrary bytes before a handshake; the handshake goroutine must exit) | **no failure** (4,443 and 15,183 executions; slow targets — each runs real handshakes over pipes — and the machine was shared with other runs). Run longer before production: `tools/tlssync/check.sh 30m` | `results/fuzz.txt` |
| Pusher conformance (`bench/correctness.mjs`, Echo + pusher-js: public, private, presence, client events, HTTP API, forged signatures) over the fork's TLS | **10/10** | `bench/results/correctness-fork-tls.log` |

**Mutation check.** To show the race test is not vacuous: removing Go's `c.out.Lock()` from `handleKeyUpdate`
(llib's change) makes `TestAuditKeyUpdateWriteRace` fail under `-race` with 6–24 data races. Restored afterwards.

## 2. The audit findings, one by one

| Audit finding | Status in this fork | Proof |
|---|---|---|
| Go 1.16 TLS, 0 of 14 Go `crypto/tls` security fixes | Copy of go1.27.3: **all 14** are in (it postdates every one of them). `vulncheck.sh` reports no known `crypto/tls` vulnerability for go1.27.3 | `results/govulncheck.txt`; `UPSTREAM` |
| GO-2026-6090: unlimited KeyUpdates (llib accepted 5,000) | Fixed: server stops after 16 consecutive ones with "too many non-advancing records" | `TestAuditKeyUpdateFlood`; KeyUpdates separated by data still work: `TestAuditKeyUpdateInterleavedAccepted` |
| GO-2022-0531: `ticket_age_add` = 0 | Fixed: random per ticket | `TestAuditTicketAgeAdd` |
| GO-2026-4340 (+4870): handshake data spanning a key change | Fixed: rejected ("handshake buffer not empty…"), no deadlock with two KeyUpdates in one record | `TestAuditKeyUpdateWithTrailingHandshakeData` |
| GO-2023-2375: RSA key exchange offered by default | Off by default (Go ≥1.22 defaults) | `TestAuditDefaults/no_RSA_kex` |
| llib #1: unlocked writes from the read path (remote race → process-wide DoS; nonce reuse on close) | Gone by construction: Go's `c.out`/`c.in` locks are untouched; the poller and broadcasters both go through them | `TestAuditKeyUpdateWriteRace` (4 broadcasters + KeyUpdate(update_requested) storm, AES-GCM and ChaCha20, every record must decrypt) and `TestAuditCloseDuringBroadcast`, both clean under `-race`; mutation check above. The GCM shim also panics on any nonce reuse |
| llib #2: TLS tests do not build; 495 `//go:norace` | Go's full suite runs, with `-race`. Our TLS code has no `//go:norace` (nbio's own annotations are unchanged) | §1 |
| llib #3: `tls.Server` / `tls.Client` panic, llib client vs llib server fails | Standard Go behaviour; our client against our non-blocking server passes TLS 1.2/1.3 incl. resumption | `TestNonBlockOwnClient` |
| llib #4: CPU features never detected (ChaCha20 picked on AES hardware) | Detected via `x/sys/cpu`; AES-GCM preferred on arm64/amd64 with AES | `TestAuditDefaults/AES-GCM_preferred…` |
| llib #5: 2021 defaults (TLS 1.0/1.1, 3DES, no EMS, no X25519MLKEM768) | Go 1.27 defaults: TLS 1.0/1.1 refused by default on the server, no 3DES/RSA-kex, Extended Master Secret, X25519MLKEM768 negotiated with current clients | `TestAuditDefaults`, `TestAuditEMSServerSide` |
| govulncheck blind to the copy | Still true for plain govulncheck, so `tools/tlssync/vulncheck.sh` checks the copy's Go version against vulndb (verified: pointed at go1.27.0 it reports GO-2026-6607) | `results/govulncheck.txt` |

## 3. Diff size and porting cost (measured)

- Against Go 1.27.3 `crypto/tls`: **87 changed lines, all import lines except 2**; no hand patches
  (`results/diff-vs-go1.27.3.txt`). Our code: `nonblock.go` 390 lines (about half comments), shims 324 lines
  (+164 test-only).
- Against upstream nbio: 11 non-test files, +50/−42 (after the second-pass fixes), plus one test file.
- Port drill (`results/port-drill.txt`): re-import of go1.27.0 → passes in 6 s with no manual work; go1.26.10
  (previous minor) → library compiles unchanged, one extra test-only shim needed.
- `sync.sh` is idempotent: re-running it on go1.27.3 changes nothing.

## 4. Memory and latency (512 MiB, 1 CPU)

Same harness and method as arco-ws `docs/engine-spike.md` (ramp of pusher-js clients, 15 publishes per step,
latency on a 10 % sample; KiB/conn = least-squares slope of container memory over all completed steps; computed by
`bench/slope.py`, which reproduces the spike's 19.38 and 7.37 from its JSON). All runs on the same evening.

| Run | KiB/conn | Idle MiB | Max stable conns | Stop | p50/p99 ms @10k | p50/p99 ms @20k | CPU % bc avg/max @10k | @20k |
|---|---|---|---|---|---|---|---|---|
| llib (arco-ws spike-b binary, unchanged) | 19.28 | 11.1 | 25,000 | OOM-killed going to 30k | 72 / 177 | 177 / 507 | 10 / 22 | 20 / 33 |
| **fork** (run 2, quiet host) | **12.34** | 10.8 | **30,000** | ramp timeout at 38,885/40,000 (memory) | 74 / 247 | 174 / 434 | 10 / 26 | 22 / 34 |
| fork (run 1, host busy with a `-race` test run in its first steps: latency not comparable) | 13.07 | 7.5 | 30,000 | ramp timeout at 36,030/40,000 | (175 / 1,481) | 145 / 323 | (26 / 56) | 19 / 29 |
| fork with buffer release off (≈ this adapter over **stock** `crypto/tls`) | 19.50 | 11.2 | 25,000 | OOM-killed going to 30k | 76 / 193 | 154 / 310 | 11 / 20 | 21 / 33 |
| Reference from the spike: B plain (no TLS) | 7.37 | 5.4 | 50,000 | ceiling | 73 / 127 | 132 / 248 | 9 / 19 | 17 / 27 |

Every completed step delivered 100 % of broadcasts, except fork run 2 at 15,000 (96.7 %, p99 1,977 ms) — see
below. A third fork run was not done: another session started its own benchmark in the same Docker VM right after
run 2 (00:31 UTC; `run.sh` refuses to share the VM). None of the four runs above overlapped another benchmark
(checked with `docker events`); run 1 overlapped a host-side `go test -race` run.

What the numbers say:

- **The ~19 KiB/conn does not just hold, it improves: ~12.3–13.1 KiB/conn**, about 5.5 KiB/conn of TLS on top of
  plain nbio (7.37). On a t4g.nano budget (400 MiB, method of the spike) that is ≈ 31,000–32,000 connections
  instead of ≈ 20,800 with llib; on a micro (900 MiB) ≈ 70,000 (CPU limits first at those counts, as the spike
  noted).
- **Where the saving comes from: releasing idle read buffers.** With release off, the same code costs 19.5
  KiB/conn — the same as llib. So "stock Go `crypto/tls` behind a non-blocking adapter" (no fork, the audit's
  path 2) would land at ~19.5 KiB/conn; the fork is what buys the other ~7 KiB/conn.
- **Latency/CPU:** broadcast CPU and p50 are the same as llib. Runs 1–2 above had a 1.6–2 s stall at 15k; **after
  the fixes of the second pass (`docs/risks.md`) 6 more runs show no stall and 100 % delivery at every step**:
  full ramp ×2 → 12.80 / 12.66 KiB/conn, 30,000 stable, p99 146–159 ms @10k and 293–369 ms @20k (llib 177 /
  507 here, 150–183 / 299–384 in the second-pass runs). GC pauses ≤ 3.1 ms.

Projection (`(budget − idle) × 1024 / KiB-per-conn`): nano (400 MiB): fork 12.34 → **32,300**; llib 19.28 →
20,700; no-release 19.50 → 20,400. Micro (900 MiB): fork → **73,800**; llib → 47,200.

## 5. Verdict

**Is this fork safe?** Much safer than llib, and close to "stock Go" for everything except one new file:

- The TLS protocol code *is* Go 1.27.3's, verified by Go's own test suite including byte-exact recorded
  handshakes, with all Go TLS security fixes to date, and checked against vulndb by `vulncheck.sh`.
- The audit's race is removed by construction (Go's locks untouched) and pinned by a test that fails if it comes
  back.
- What remains ours and therefore unreviewed by the Go team: `nonblock.go` (≈200 lines of logic: the would-block
  wrapper, the handshake coroutine, buffer release) and the shims (≈320 lines, mostly one-liners; the only
  non-trivial one is the GCM nonce wrapper, which is stricter than std). This is small enough for a careful
  review by a second person, which has not happened yet.

**Is it cheap enough to maintain?** Yes, much cheaper than the audit's estimate for option (b) (3–6 weeks + 1–3
days per release), because the design avoided touching Go's code:
- Effort spent here: about one working day for the whole thing (inventory, port, tests, fuzzing, benchmark,
  docs). The audit's 3–6 weeks assumed re-deriving llib's handshake checkpoints, which this design does not need.
- Per Go point release: run `sync.sh` + `check.sh`, read the diff for the six things `docs/porting.md` lists —
  **1–2 hours**. Per minor release (twice a year): a few shims at most — **1–3 days**.
- It still is a permanent obligation: someone must run it within days of every Go security release that touches
  `crypto/tls` (≈3–6 a year), forever. `vulncheck.sh` in CI makes forgetting visible.

**Versus the current plan (std Go TLS now, kTLS later):**

| | Std Go TLS (goroutine per conn) | This fork | kTLS (estimate, not built) |
|---|---|---|---|
| KiB/conn | ~62 (measured in the spike) | **~12.3–13** (measured) | ~9–14 (estimate) |
| nano capacity | ~6,500 | **~32,000** | ~29,000–44,000 |
| Security maintenance | none (Go does it) | 1–2 h per Go TLS release, by us | kernel updates + our key handling |
| Code we own on the TLS path | none | ~200 lines + shims, plus nbio | handshake hand-off, key export, rekey, recvmsg in engine (2–4 weeks) |
| Engine | arco-ws's own epoll engine | nbio (a large third-party dependency) | arco-ws's own epoll engine |
| Unknowns | none | p99 tail under GC (above); needs a second reviewer; long fuzz runs | everything (not built) |

**Recommendation:** keep the current plan as the default (std TLS now), but **treat this fork as the leading
candidate to replace "kTLS later"**: it already delivers the memory kTLS hopes for (~5× better than std TLS,
~1.6× better than llib), with a maintenance cost of hours per Go release instead of weeks, and without kernel
dependencies. Before adopting it in arco-ws:
1. A human engineer reviews `crypto/tls/nonblock.go` and the GCM shim (half a day). An independent automated
   review was done and its findings fixed (`docs/risks.md`); one of them was a real bug.
2. ~~Investigate the p99 tail~~ done (`docs/risks.md` §2): no stall in 6 runs after the fixes; GC tuning not needed.
3. Run the fuzzers for hours, not minutes, and run BoGo once by hand.
4. Wire `tools/tlssync/vulncheck.sh` into CI and name an owner for Go security releases.
5. Accept nbio as the engine (arco-ws currently keeps its own epoll engine behind an interface); or port the same
   `nonblock.go` idea onto arco-ws's engine — it only needs a transport that returns would-block and a place to
   run the handshake coroutine, so it is not nbio-specific.

## 6. Remaining risks

Investigated in a second pass: see **`docs/risks.md`** (an independent review found and we fixed a buffer-aliasing
bug that dropped connections on large uploads; the p99 stall did not reappear; stalled-handshake and handshake-CPU
costs measured). The list below is the original one.

- `nonblock.go` relies on Go behaviour that is not a documented API: temporary errors not being sticky in
  `readRecordOrCCS`, and the buffers being `bytes.Buffer` fields. A Go release could change either; the tests
  would fail loudly (not silently), and `docs/porting.md` step 5 tells the porter to look.
- The GCM shim panics on nonce reuse. That is the right failure, but a bug there would crash the process.
- A slow `GetCertificate` (first-time ACME) blocks a poller during the handshake, as with llib.
- FIPS 140-3 mode is unsupported. `go.mod` `godebug` defaults are not honoured by the copy (only `GODEBUG`).
- Handshake goroutines: one per connection mid-handshake, ≈1 RTT each; a slowloris holds one up to the read
  deadline or 30 s.
- Benchmark caveats as in the spike: one Docker Desktop VM, load generator on the same host, latencies relative.
