# arco-nbio: design and what changed

Date: 2026-10-31. Upstream: `github.com/lesismal/nbio` master at `2247df8` (v1.7.1 + 2 commits). TLS: Go
**1.27.3** `src/crypto/tls` (latest stable Go on that date). Toolchain: go1.27.3.

This fork answers one question from `~/Herd/arco-ws/docs/tls-audit.md`: can nbio keep in-process,
no-goroutine-per-connection TLS (~19 KiB/conn) on top of a **current, patched** `crypto/tls` instead of llib's
2021 copy, at a maintenance cost we can live with?

## 1. Repository layout

| Path | What | Origin |
|---|---|---|
| `*.go`, `nbhttp/`, `extension/`, … | nbio | upstream, 11 non-test files touched (+50/−42 lines, section 4) |
| `crypto/tls/*.go` except below | Go's `crypto/tls` | **go1.27.3, unmodified except import lines** (section 3) |
| `crypto/tls/nonblock.go` | the non-blocking layer (llib-compatible API) | ours, 390 lines with comments |
| `crypto/tls/nonblock_test.go`, `arco_regress_test.go`, `arco_fuzz_test.go` | our tests | ours |
| `crypto/tls/internal/fips140/tls12`, `tls13`, `internal/fips140tls`, `internal/byteorder` | key schedule, PRF, small helpers | Go 1.27.3, copied verbatim (import lines rewritten) |
| `crypto/tls/internal/*/shim.go` | stand-ins for Go-internal packages | ours, 324 lines (+164 test-only) |
| `crypto/tls/UPSTREAM` | the Go tag the copy comes from | written by `sync.sh` |
| `tools/tlssync/` | `sync.sh`, `check.sh`, `diff-upstream.sh`, `vulncheck.sh` | ours (porting procedure, `docs/porting.md`) |
| `bench/` | memory/latency harness | adapted from arco-ws `bench/` + `spike/b` |

Module path stays `github.com/lesismal/nbio`, so the diff against upstream stays small. Consumers use
`replace github.com/lesismal/nbio => <this repo>`. llib is no longer a dependency (`go.mod`: only
`golang.org/x/crypto` v0.58.0 and `golang.org/x/sys` v0.49.0).

## 2. Step 1 — how llib made `crypto/tls` non-blocking

llib v1.2.4 `std/crypto/tls` is Go 1.16 `crypto/tls` (Feb 2021) plus +1,500/−701 lines (audit, `divergence.txt`):

| llib mechanism | Where | Purpose |
|---|---|---|
| `errDataNotEnough` returned from `readRecordOrCCS` / `readHandshake` when the input buffer ends mid-record, and swallowed by `Read`/`AppendAndRead` | `conn.go` | the poller hands in whatever bytes arrived; TLS says "need more" instead of blocking |
| Handshake checkpoints: each step of the TLS 1.2/1.3 server handshake records `handshakeStatusAsync` and is skipped on re-entry | `handshake_server.go` (+385/−120), `handshake_server_tls13.go` (+148/−49) | resume the handshake on the poller without a goroutine. Each step that can stop halfway must be idempotent |
| `rawInput`, `hand`, `input`, `sendBuf` as `*[]byte` from an `Allocator` (nbio's mempool), freed when empty | `conn.go`, `tls.go` (+430/−387) | no idle buffers per connection |
| Removed `c.out.Lock()` in `handleKeyUpdate`, `sendAlert`, `writeRecord`, `closeNotify`; removed parts of `c.in` locking | `conn.go` | avoid lock cost / deadlocks on the poller. **This is the audit's remote race** |
| New API: `NewConn(conn, cfg, isClient, isNonBlock, allocator)`, `Append`, `AppendAndRead`, `ResetOrFreeBuffer`, `ResetRawInput`, `ResetConn`, `IsNonBlock`, `Conn()`, `Session`/`SetSession` | `conn.go`, `tls.go` | what nbio calls |
| Small accessors (`ClientHello`/`ServerHello` getters) | `handshake_messages.go` (+20) | API |

The handshake checkpoints are the expensive part to re-derive: Go's handshake code changed by +6,379/−2,092
lines since 1.16 (ECH, ML-KEM, QUIC, error-returning marshalling), and every checkpoint would have to be
re-checked by hand on every Go release.

## 3. What this fork does instead (and the diff against Go)

The goal "same non-blocking behaviour, smallest possible diff against Go" led to **not** re-applying llib's
checkpoints. The same result is reached from outside Go's code, in one new file, `nonblock.go`:

1. **Would-block reads.** A non-blocking `Conn`'s transport is wrapped in `nbConn`. The poller hands input to
   `AppendAndRead`; `nbConn.Read` serves it to Go's record layer and, when it runs out, returns `errWouldBlock`,
   a `net.Error` with `Temporary() == true`. Go 1.27's `readRecordOrCCS` does not store temporary errors
   (`conn.go:633`, `678`), so the next `AppendAndRead` resumes where Go stopped; a partial record stays in Go's
   own `c.rawInput`. No change to Go's code. (Pinned by the tests: if Go ever makes temporary errors sticky, every
   `NonBlock*` test fails.)
2. **Handshake on a coroutine.** The handshake cannot be resumed that way (Go stores the handshake error for
   good), so it runs on its own goroutine, used strictly as a coroutine: the poller passes it the new bytes and
   **waits** until it either needs more input or finishes. Only one of the two runs at a time, so the handshake
   has no concurrency with the poller, like llib's inline handshake. When it finishes the goroutine exits and the
   poller reads leftover input itself (in TLS 1.3 the client usually sends its first request together with its
   Finished). Cost: one parked goroutine per connection *in the middle of a handshake* (≈1 RTT), none afterwards.
3. **Go's locks untouched.** After the handshake, reads run on the poller and writes on any goroutine, both
   through Go's own `c.in` / `c.out` locked paths. KeyUpdate replies, alerts and `closeNotify` take `c.out` as in
   upstream Go. This removes the audit's race by construction.
4. **Idle buffers released.** `ReleaseBuffers` (called by nbio through `ResetOrFreeBuffer` after each read
   event) drops `c.rawInput`, `c.hand` and the `c.input` reference when they hold no unread bytes, under `c.in`.
   They are ordinary GC memory (no pool, no manual free), so a use-after-free like llib #23 cannot happen.
   Go already pools its write buffer (`outBufPool`) and clears `sendBuf` after each flight.
5. **Same API as llib**, so nbio changes only import paths (section 4). `Allocator` is accepted and ignored.

### Diff against Go 1.27.3 `src/crypto/tls`

`tools/tlssync/diff-upstream.sh` (output: `docs/results/diff-vs-go1.27.3.txt`): **87 changed lines in 20 files,
all import lines except 2**:

- Import paths of Go-internal packages → `github.com/lesismal/nbio/crypto/tls/internal/...` (mechanical `sed` in
  `sync.sh`).
- `cipher_suites.go`: `gcm.NewGCMForTLS12(aes.(*fipsaes.Block))` → `gcm.NewGCMForTLS12(aes)` (same for TLS13),
  and the `fipsaes` import line removed. Code outside GOROOT cannot name the internal AES block type.
- Not imported: `fipsonly/` (a FIPS-only switch package) and `defaults_boring.go` (BoringCrypto builds only).

There are **no hand-made patches** (`tools/tlssync/patches/` is empty).

### The shims (`crypto/tls/internal/`)

Each shim keeps the package name and the exported API that `crypto/tls` uses, so only import lines change.

| Go-internal package | Shim | Notes |
|---|---|---|
| `crypto/internal/fips140/tls12`, `tls13` | Go's file, verbatim | TLS 1.2 PRF/EMS and the TLS 1.3 key schedule |
| `crypto/internal/fips140/hkdf`, `hmac` | public `crypto/hkdf`, `crypto/hmac` | panics on the errors the public API adds (cannot happen with TLS sizes) |
| `crypto/internal/fips140/aes/gcm` | public `crypto/cipher` GCM + Go's TLS nonce checks | **stricter than std**: Go enforces "record nonce strictly increasing" only in FIPS mode; the shim always does, so a nonce reuse panics instead of leaking |
| `crypto/internal/fips140`, `sha256`, `sha512` | no-op service indicator, dummy `Digest` | only feed FIPS accounting |
| `crypto/internal/boring` | `Enabled = false` | no BoringCrypto |
| `internal/cpu` | `golang.org/x/sys/cpu` | AES-GCM hardware detection works (llib's never initialised, audit finding 4) |
| `internal/godebug` | reads `GODEBUG` from the environment | `go.mod` `godebug` lines are **not** applied: an unset setting means the Go 1.27 default |
| `internal/byteorder`, `crypto/tls/internal/fips140tls` | Go's files, verbatim | |
| test-only: `internal/testenv`, `crypto/internal/cryptotest`, `crypto/internal/rand` | reduced copies | FIPS-only reruns and BoGo skip (see results) |

Consequences, stated plainly:
- **FIPS 140-3 mode is not supported.** `GODEBUG=fips140=only` makes the public GCM refuse TLS nonces, and only
  Go's own module can be validated anyway. We don't need it.
- The copy must be built with a toolchain of the **same minor version** as `UPSTREAM` (it uses 1.27 APIs such as
  `crypto/mldsa`, `crypto/hpke`, `crypto/hkdf`). `go.mod` pins `go 1.27.3`.
- Everything that `crypto/tls` imports publicly (x509, ecdsa, rsa, ecdh, mlkem, aes, sha*) is the toolchain's, so
  fixes there arrive with a Go upgrade, as with any Go program.

## 4. Diff against upstream nbio

`git diff upstream/master` outside `crypto/`, `bench/`, `docs/`, `tools/` (and test files): 11 files, +50/−42, plus one new test file `nbhttp/arco_tls_test.go`.

- 10 files: `github.com/lesismal/llib/std/crypto/tls` → `github.com/lesismal/nbio/crypto/tls` (one line each).
- `extension/tls/tls.go`: `tls.Dial` loses the allocator argument (Go's `Dial` signature); `WrapData` reads with
  `AppendAndRead` instead of `Append` + `Read` (in non-blocking mode Go's `Read` reports would-block as an error).
- `nbhttp/engine.go` and `nbhttp/websocket/upgrader.go`: use `nread` before `err` (plaintext delivered together
  with a close_notify was dropped; the upgrader also closed with the wrong error variable, an upstream bug).
- `nbhttp/engine.go` (`OnClose`): if a connection drops before its TLS handshake finished, close the TLS conn so
  its handshake goroutine exits immediately (otherwise it waits for `NonBlockingHandshakeTimeout`, 30 s).
- `go.mod`: `go 1.27.3`; llib removed; `x/crypto` v0.58.0, `x/sys` v0.49.0.

## 5. Limits and sharp edges

- **A slow `GetCertificate` blocks the poller**, as in llib: the poller waits for the handshake coroutine. With
  autocert, issue certificates outside the handshake path (cache, pre-issue) — first-time ACME issuance inside a
  handshake would stall that poller for seconds.
- **Parked handshakes cost a goroutine each** (stack ~8–16 KiB) for ≈1 RTT, plus up to
  `NonBlockingHandshakeTimeout` (30 s) for clients that stall mid-handshake (slowloris). nbio's read deadline
  closes those sooner. `tls.HandshakesInFlight()` exposes the count for metrics.
- Calling `Handshake`, `Read` or `Write` on a non-blocking conn **before** the first `AppendAndRead` fails the
  handshake (`errHandshakeNotDriven`) instead of blocking. A `Write` during a handshake waits for it to finish.
- `ResetRawInput` never discards buffered input (llib's did).
- `AppendAndRead(in, out)` copies `in` when it overlaps `out` (nbio passes the same buffer): one allocation per read
  event in exchange for never overwriting unread ciphertext.
- A non-blocking **client** cannot start a handshake (nbio's client handshakes in blocking mode and then calls
  `ResetConn`, which is supported). `ResetConn` during a non-blocking handshake is unsupported.
- A stalled handshake costs ~18.4 KiB (state + goroutine stack) until its deadline (`docs/risks.md` §3).
- `ReleaseBuffers` frees memory to the GC; a burst of reads allocates again (≈ record size + 512 B per read event
  that ends on a record boundary). Pusher clients send little, so this is cheap.
- Out of scope, unchanged from upstream nbio: `nbhttp/convert.go` stores connection pointers as `uintptr` map keys
  (works because the connections stay referenced elsewhere, but it hides them from the GC); nbio marks many
  functions `//go:norace`; nbio's own tests have 3 data races in test code (`docs/results.md`).
