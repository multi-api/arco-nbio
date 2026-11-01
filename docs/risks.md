# Risk investigation (2026-10-31, second pass)

Follow-up to the "remaining risks" in `docs/results.md`. Each risk: what was done, what was found, what is left.

## 1. Second review of `nonblock.go` and the GCM shim

Done by an independent reviewer (a separate agent given only the code, not our conclusions), then each finding
verified here.

| # | Finding | Severity | Status |
|---|---|---|---|
| 1 | **nbio passes the poller's buffer as both input and output of `AppendAndRead`.** `feed` only borrowed the input, so decrypted plaintext written to `out[0:]` could overwrite ciphertext not yet consumed. Large uploads failed with `bad record MAC` and the connection dropped (no confidentiality loss: the AEAD caught it). Our tests missed it because they used a separate output buffer. | **High** | **Fixed** (`AppendAndRead` copies the input when it overlaps the output). Reproduced first: `TestNonBlockAliasedBuffers` failed at all 4 buffer sizes / both versions; a new end-to-end test through the real nbhttp engine (`nbhttp/arco_tls_test.go`, POST bodies up to 2 MiB over TLS 1.2/1.3) failed without the fix ("broken pipe" at 300 KB) and passes with it |
| 2 | After a WebSocket upgrade in blocking mode, borrowed input could point into a pooled buffer that nbio frees | Medium | **Fixed**: `ResetRawInput` and `ResetConn` copy borrowed input first (and finding 1's fix removes most borrowing) |
| 3 | nbio callers checked `err` before using `nread`; Go's `Read` returns `(n>0, io.EOF)` when a close_notify follows data, so the last plaintext was dropped | Medium | **Fixed** in `TLSDataHandler`, the blocking TLS reader and the WebSocket upgrader (which also closed with the wrong error variable — an upstream bug) |
| 4 | `Handshake`/`Read`/`Write` called directly before the first `AppendAndRead` raced with the poller's input | Low (misuse) | **Fixed**: fails with `errHandshakeNotDriven` before touching poller state. `ResetConn` during a non-blocking handshake would wait for the handshake: documented as unsupported |
| 5 | A non-blocking *client* cannot start a handshake; TLS 1.2 client renegotiation would run on the poller | Low | Documented. nbio's client handshakes in blocking mode and then switches (supported); renegotiation is off by default |
| 6 | One goroutine per pending handshake; `GetCertificate` runs while the poller waits | Low | Quantified in §3 and §4 |
| 7–8 | Nits in upstream code and shims (godebug reads only `GODEBUG`) | Nit | Upstream nit fixed with #3; shim behaviour documented in `design.md` |
| — | **GCM shim**: matches Go 1.27.3 `gcm_nonces.go` line for line, checks always on; no way found for a nonce to repeat or a valid sequence to be rejected (TLS 1.2 explicit nonce, TLS 1.3 mask, new AEAD per KeyUpdate) | — | No change |

Still open: the reviewer was another model instance, not a human. A human review of `nonblock.go` (≈220 lines of
logic now) is still recommended before production.

## 2. p99 tail (the 2-second stall at 15k)

Observed before: both fork runs stalled at the 15,000 step (p99 1.6 s and 2.0 s; one run delivered 96.7 %),
while llib and fork-without-buffer-release did not.

What was measured (`bench/gc-matrix.sh`, `bench/results/gc-*`, `fixed-full-*`, all with `GODEBUG=gctrace=1`, after
the fixes in §1):

| Run | KiB/conn | p99 ms @10k / 15k / 20k / 25k / 30k | Delivered | GC cycles | max GC pause |
|---|---|---|---|---|---|
| fork, short ramp ×2 | 13.38 / 12.78 | 164–222 / 242–286 / 304–362 | 100 % | 41 | 1.2 ms |
| fork without release ×2 | 19.17 / 19.68 | 158–171 / 224–265 / 326–385 | 100 % | 28 | 0.06 ms |
| llib ×2 | 19.99 / 19.22 | 150–183 / 223–262 / 299–384 | 100 % | 19 | 0.16 ms |
| fork, `GOGC=200 GOMEMLIMIT=450MiB` ×2 | 15.85 / 15.71 | 151–186 / 200–330 / 342–385 | 100 % | 21 | — |
| **fork, full ramp ×2** | **12.80 / 12.66** | 146–159 / 208–214 / 293–369 / 547–678 / 455–619 | **100 % at every step** | 53 | 3.1 ms |

Findings:
- **No stall in 6 runs after the fixes.** p99 of the fork is now in the same range as llib and the no-release
  variant at every step up to 20k; 30,000 connections stay stable in 512 MiB (llib: 25,000).
- **GC is not the cause of second-long stalls.** The fork does run the GC about twice as often as llib (smaller
  live heap, buffers re-allocated on read events), but stop-the-world pauses stay ≤ 3.1 ms and GC CPU rounds to
  0 %. GC tuning brings nothing but more memory, so it is not recommended.
- The earlier stall could not be attributed with certainty: those runs had no gctrace and the server does not log
  TLS errors. The aliasing bug (§1 #1) is the most plausible cause — it corrupts the stream when a record spans
  two poller reads, which happens more under load — but this is an inference, not a proof.

## 3. Slowloris: cost of a stalled handshake

`TestMeasureStalledHandshakeCost` (`ARCO_MEASURE=1`): 2,000 connections that send a ClientHello and stall cost
**18.4 KiB each** (10.3 KiB heap of handshake state + 8.0 KiB goroutine stack), against ~12.7 KiB for an
established connection. They are released by nbio's read deadline or `NonBlockingHandshakeTimeout` (30 s).
llib kept the same handshake state on the heap without the stack, so this is ~8 KiB worse per *stalled* handshake.
At the benchmark's 500 new connections/s and normal RTTs only a few hundred are in flight at once. Mitigation if
needed: a 5–10 s handshake deadline, and `tls.HandshakesInFlight()` as a metric/alarm.

## 4. Poller blocked by the handshake / slow `GetCertificate`

`BenchmarkNonBlockHandshake` (TLS 1.3, ECDSA P-256, X25519MLKEM768, std client over `net.Pipe`, Apple M-series):
non-blocking 491–509 µs per full handshake (client + server) against 485–491 µs for Go's standard server: the
coroutine hand-offs cost **~1–2 %**. The server's share is what a poller spends per new connection, the same
work llib did inline. Not measured on Graviton (expect several times slower per core).

`GetCertificate` runs inside that window, so a slow callback (first-time ACME issuance) stalls the poller, as with
llib. This stays a usage rule rather than a code change: keep `GetCertificate` cache-only and issue certificates
in the background. Making the poller not wait would need a way to re-enter nbio's data handler when the handshake
finishes, which is a larger change to nbio.

## 5. Reliance on Go internals

New canary test `arco_assumptions_test.go`: compile-time checks that `rawInput`/`hand` are `bytes.Buffer`,
`input` a `bytes.Reader`, `in`/`out` lockable; and a runtime check that a temporary error in the middle of a
record leaves the connection usable. A Go release that changes either fails this file first, with a message that
names the assumption.

## 6. Unchanged

- FIPS 140-3 mode unsupported; go.mod `godebug` defaults not applied (only `GODEBUG`).
- Go's `BenchmarkHandshakeServer` cannot run here: it replays recorded handshakes and needs a deterministic
  `crypto/rand`, which code outside GOROOT cannot set.

## Fuzzing (longer run)

`tools/tlssync/check.sh 10m` passed end to end (`docs/results/check-full.txt`): diff check, build/vet, Go suite + ours with `-race`, nbio tests, both fuzzers 10 minutes each with no failure (`FuzzNonBlockRecordStream` 53,264 executions, `FuzzNonBlockGarbage` 274,822), govulncheck clean on the module and the copy.

# Third pass (2026-11-01): production readiness

## 7. Second independent review (fresh reviewer, most capable model)

| Finding | Severity | Status |
|---|---|---|
| **A panic in a handshake callback (`GetCertificate`, `GetConfigForClient`, `VerifyConnection`, …) crashed the whole process**: the handshake goroutine had no `recover` (llib ran the handshake under nbio's `recover` in `TLSDataHandler`) | **High** | **Fixed**: recovered in the goroutine, the connection fails with the panic and stack, the transport is closed. `TestNonBlockHandshakePanicRecovered` crashed the test binary before the fix |
| The poller waits while the handshake goroutine runs (slow callbacks stall the poller; anything on the poller taking `handshakeMutex` mid-handshake would wait) | Medium | By design, same as llib; documented (`GetCertificate` must be cache-only). No call site takes `handshakeMutex` on the poller |
| `extension/tls` `WrapData` dropped plaintext delivered with a close_notify | Low | **Fixed** |
| After a failed handshake, `AppendAndRead(nil, …)` returned `(0, 0, nil)` | Low | **Fixed**: the error stays sticky (`TestNonBlockFailedHandshakeErrorSticky`) |
| Misuse cases (write before first `AppendAndRead`, client renegotiation, `ResetConn` concurrent with the poller) | Low | Documented as unsupported |
| GCM shim, buffer hand-off, error stickiness, Close vs coroutine, KeyUpdate vs Write | — | Checked, fine |

`NonBlockingHandshakeTimeout` default lowered from 30 s to 10 s.

## 8. Root cause of the 15k stall

- Rebuilt the exact pre-fix code (`563bb44`) with diagnostics (`ARCO_DIAG=1`: close errors, handshakes in
  flight, scheduling delay of a 10 ms ticker) and ran it twice: **no stall** at 15k (p99 243 / 232 ms, 100 %).
  With the 6 runs of the second pass and 3 final runs, the stall has not reappeared in 11 runs. Run 1 of the
  original pair overlapped a host-side `go test -race`; the cause of run 2's stall is most likely the same kind of
  host interference. It is not reproducible with any version of the code.
- During every broadcast phase the server's ticker stays within ~30 ms and GC pauses ≤ 3 ms, including final run 3,
  whose 20k step had a p99 of 839 ms and 99.0 % delivery: **the server was not stalled**; the tail comes from the
  load generator (Node decrypting TLS for 20k sockets on the same host). llib shows the same variance.
- Real server stalls (1–17 s) appear only when the container is at 30–39k connections, at its 512 MiB limit
  (memory reclaim; these runs then end by OOM or ramp timeout). A mass disconnect of 20,000 TLS clients at 20k
  costs ≤ 50 ms with or without TLS. **Bound: keep ≥ 20 % memory headroom** (on a 512 MiB box, ~30k is the
  edge; plan for ≤ 25k), which is a capacity rule, not a TLS issue.

## 9. Final benchmark (3 runs, 512 MiB / 1 CPU, final code)

| Run | KiB/conn | Max stable | p99 @10k | p99 @20k | Delivery |
|---|---|---|---|---|---|
| final-1 | 13.00 | 30,000 | 247 | 378 | 100 % every step |
| final-2 | 12.21 | 30,000 | 263 | 334 | 100 % every step |
| final-3 | 12.50 | 30,000 | 172 | 839 | 100 %, except 99.0 % at 20k (load-generator tail, see §8) |

Mean 12.6 KiB/conn (all 7 full runs of the fork: 12.2–13.1). llib in the same harness: 19.2–20.0.

## 10. Long fuzzing (2 hours, final code)

`tools/tlssync/fuzz-long.sh 2h` on the final code (Apple M-series, both fuzzers at the same time, `-timeout 0`),
logs in `docs/results/fuzz-long-*.txt`: **no failure, no crasher written to `testdata/fuzz`**.

| Fuzzer | Executions | New interesting inputs |
|---|---|---|
| `FuzzNonBlockRecordStream` (real handshake + records, split at fuzzed points) | 228,971 | 64 |
| `FuzzNonBlockGarbage` (arbitrary bytes into a non-blocking server) | 5,827,654 | 361 |

`FuzzNonBlockRecordStream` is slow per execution because each one runs a full TLS handshake.

## 11. Third independent review (2026-11-01, fresh reviewer, most capable model)

Scope: everything in the TLS layer that is not Go's (`nonblock.go`, the shims, the nbio glue), against Go 1.27.3,
threat model "malicious internet clients against a multi-tenant WebSocket server". The reviewer saw only the code
and Go's source, not earlier reviews. **No memory-safety bug, no data race in the adapter, nothing that lets one
client crash the process or corrupt another connection.** Every finding has a test in `arco_review3_test.go`
(crypto/tls, nbhttp, extension/tls) that failed before its fix.

| id | Finding | Severity | Status |
|---|---|---|---|
| R3-2 | The poller waited for the handshake goroutine with no bound: a blocking `GetCertificate` (e.g. a lookup keyed on an attacker-chosen SNI) stalled every connection on that poller for as long as the callback ran, even after the connection closed | Low (Medium if a callback can block) | **Fixed**: the wait ends at `NonBlockingHandshakeTimeout` or when the connection closes; the connection then fails and its input is dropped. Callbacks must still be cache-only (design.md §5) |
| R3-1 | `NonBlockingHandshakeTimeout` bounded each gap between bytes, not the handshake: a client dripping one byte every 9.9 s kept a handshake (goroutine + ~18 KiB) alive for hours where nbio's read deadline is not set | Low | **Fixed**: 10 s for the whole handshake, from its first byte |
| R3-5 | `extension/tls` `WrapClose` did not close the TLS conn, so abandoned handshakes waited for the timeout | Low | **Fixed** (as nbhttp's `OnClose`) |
| R3-4 | After a failed handshake, `AppendAndRead` still buffered input (not reachable through nbio's glue, which closes on error) | Info | **Fixed**: nothing is buffered after a failure |
| R3-6 | `NonBlockingHandshakeTimeout` was read by handshake goroutines without synchronization | Info | **Fixed**: read once per handshake on the poller; documented as "set at startup" |
| R3-3 | `GODEBUG=fips140=only` makes every AES-GCM connection fail (the GCM shim uses the public API) | Info | Not fixed: FIPS mode is unsupported (design.md §3); the test runs with `ARCO_FIPS_ONLY_TEST=1` |

Tried and reverted: letting the poller stop waiting after 20 ms of a slow handshake step, so a slow callback would
not stall other connections at all. The 5-minute fuzz run found that, under CPU load, plaintext pipelined right
behind the client's Finished could then be left unprocessed until the connection's next input (a client waiting
for a response would hang). Doing it safely needs a way to wake nbio's poller from the handshake goroutine; not
worth the complexity while callbacks are cache-only.

After the fixes: `check.sh 5m` passed (`docs/results/check-review3.txt`): diff vs Go, build/vet, Go's suite + all
ours with `-race`, nbio tests, fuzzing 5 minutes each (`FuzzNonBlockRecordStream` 33,517 executions,
`FuzzNonBlockGarbage` 2,806,265), govulncheck.

Not covered by tests (reviewer's list; a Go change here would show as a rare race or leak, not a clean failure):
`handshakeContext` holding `c.in` for the whole handshake (`ReleaseBuffers` relies on it),
`HandshakeContext(context.Background())` not starting an extra goroutine, `isHandshakeComplete` set only at the
very end of the server handshake. `docs/porting.md` step 5 lists these among the things to re-read.

## 12. Per-write defer cost in `handshakeContext` (reported by arco-ws, 2026-11-01)

arco-ws's CPU profile of a broadcast to 20,000 connections shows ~2.7 % of CPU in
`runtime.(*_panic).nextDefer` / `nextFrame` under `tls.(*Conn).handshakeContext`, reached from every `Conn.Write`
(and `Read`). The source returns on `isHandshakeComplete` before any `defer` runs, but the compiler cannot open-code
that function's four defers (`-gcflags=-d=defer` reports "stack-allocated defer" at conn.go:1530, 1540, 1549,
1559), so each return still goes through `runtime.deferreturn`. **Go's standard `crypto/tls` compiles the same way**
(checked with the same flag on go1.27.3's GOROOT copy): this is Go's cost, not the adapter's, and every
`crypto/tls` user pays it.

Not fixed here: avoiding it means editing Go's `Write`/`Read` (or moving the slow path of `handshakeContext` into its
own function), which breaks the "Go's code, imports only" rule that keeps porting scripted. The right fix is in Go
(a small CL splitting `handshakeContext` into a fast check and a slow function), reported as
https://github.com/golang/go/issues/82754. Low priority; it arrives here with the Go release that fixes it.
