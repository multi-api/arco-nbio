# Porting a Go security release into `crypto/tls`

The copy in `crypto/tls/` is Go's `src/crypto/tls` with import lines rewritten (`docs/design.md` §3). Porting a
new Go release is therefore "re-copy and re-test", not "merge a patch by hand". Measured on 2026-10-31
(`docs/results/port-drill.txt`): re-importing go1.27.0 over go1.27.3 took 6 s and passed the test suite with no
manual step; importing go1.26.10 (previous minor) compiled unchanged and needed one more test-only shim.

## When

- Every Go point release whose notes or vulndb entries mention `crypto/tls` (about 3 a year 2022–2026, 6 in 2026).
- Every new Go minor release (each February and August), together with the toolchain upgrade.
- `tools/tlssync/vulncheck.sh` (run it in CI, daily or weekly) fails as soon as vulndb lists a `crypto/tls` fix
  newer than `crypto/tls/UPSTREAM`. Plain `govulncheck` can't see our copy; this script can.
- Subscribe to golang-announce (security pre-announcements give a few days' notice of the release date).

## Checklist (point release, e.g. go1.27.3 → go1.27.4) — expected effort: 1–2 hours

1. `git fetch` the Go repo: `git -C .src/go fetch origin tag go1.27.4` (any clone of
   `https://go.googlesource.com/go` works; `--filter=blob:none` is enough).
2. Branch: `git checkout -b tls-go1.27.4`.
3. Re-import: `tools/tlssync/sync.sh go1.27.4`. Our files (`nonblock*.go`, `arco_*`, shims) are kept.
4. Toolchain: set `go 1.27.4` in `go.mod` (and `bench/server/go.mod`), `go mod tidy`.
5. Read what changed in Go: `git -C .src/go diff go1.27.3 go1.27.4 -- src/crypto/tls src/crypto/internal/fips140/tls12 src/crypto/internal/fips140/tls13`.
   For each change, answer two questions and write the answers in the commit message:
   - Does it touch the read path (`readRecordOrCCS`, `readFromUntil`, `Read`, `handlePostHandshakeMessage`,
     `handleKeyUpdate`), the error stickiness (`setErrorLocked`, `Temporary()`), the locks (`c.in`, `c.out`,
     `handshakeMutex`, `activeCall`), or the buffers `rawInput` / `hand` / `input`? These are what `nonblock.go`
     relies on. If yes, re-read `nonblock.go` against the new code. Also check, by reading (no test pins them):
     `handshakeContext` still holds `c.in` for the whole handshake; `HandshakeContext(context.Background())`
     starts no extra goroutine; the server sets `isHandshakeComplete` only at the very end of its handshake.
   - Does it add an import of a Go-internal package? Then `sync.sh` leaves it unresolved and the build fails:
     add a shim (same package name and API, under `crypto/tls/internal/`) and a line to `REWRITE` in `sync.sh`.
6. `tools/tlssync/check.sh` — must print "all checks passed". It runs, in order:
   - `diff-upstream.sh`: fails if anything other than import lines and the two `NewGCMForTLS1x` lines differs
     from Go. A failure means `sync.sh` missed a rewrite, or someone edited a Go file by hand.
   - build + vet;
   - Go's `crypto/tls` test suite + our non-blocking, audit-regression and fuzz-seed tests, **with `-race`**;
   - nbio's tests;
   - both fuzzers for 60 s each (`check.sh 10m` for a longer run);
   - `vulncheck.sh` (module + copy).
7. If Go added a regression test for the fix, it came with the copy and runs in step 6. If the fix is relevant to a
   non-blocking server and Go's test only uses a blocking `net.Pipe`, add a non-blocking variant to
   `crypto/tls/arco_regress_test.go` (pattern: `startNonBlockServer` + this package's client).
8. Benchmark only if step 5 found changes in the record layer or buffers: `bench/build.sh && bench/run.sh fork-tls <label>`.
9. Commit (`crypto/tls: sync to go1.27.4` + the answers from step 5), tag, bump the `replace` in consumers,
   rebuild, deploy.

## Checklist (minor release, e.g. go1.27 → go1.28) — expected effort: 1–3 days

Same steps, plus:
- Expect new Go-internal imports (shims) and new test helpers. The go1.26 → go1.27 jump would have needed one
  test-only shim (`internal/obscuretestdata`) on top of the current set.
- Expect API additions in `crypto/tls` that use new public std APIs: build with the new toolchain only.
- Re-read the design assumptions in `docs/design.md` §3 against the new `conn.go` (temporary errors not sticky,
  handshake under `c.in` and `handshakeMutex`, `outBufPool`, `rawInput` as a `bytes.Buffer`).
- Run the full benchmark and update `docs/results.md`.

## Running BoGo (optional, by hand)

Go's BoGo shim (`bogo_shim_test.go`) runs BoringSSL's TLS conformance suite against the client and server. It is
skipped here because it fetches BoringSSL over the network. To run it, use Go's own instructions in that file
with `-bogo-local-dir` pointing to a BoringSSL checkout, against this package.

## Upstream nbio updates

`git fetch upstream && git merge upstream/master`. Conflicts can only be in the 11 files of `docs/design.md` §4,
and they are one-line import changes except `extension/tls/tls.go` and the `OnClose` block in
`nbhttp/engine.go`. If upstream ever moves to a newer llib, keep our import.

## CI (GitHub Actions)

| Workflow | Runs | What |
|---|---|---|
| `.github/workflows/arco-tls.yml` | push / PR to `arco`; daily | `check.sh 5m` on linux amd64 + arm64 (diff vs Go, build/vet, Go suite + ours with `-race`, nbio tests, fuzzing, govulncheck); daily `vulncheck.sh` + `latest.sh` (red when Go ships a newer crypto/tls fix) |
| `.github/workflows/ci.yml` | push / PR to `arco`, tags `v*`; weekly CodeQL | nbio's original CI adapted: golangci-lint (`.golangci.yml`; Go's verbatim copy is excluded, our `nonblock*`/`arco_*` files and shims are linted), tests on macOS and Windows, Autobahn WebSocket conformance (ws and wss, so it exercises our TLS), CodeQL |
| `.github/workflows/auto-sync.yml` | weekly (Mon) + `workflow_dispatch` | opens the sync work as a PR (or an issue) so a release can't be missed — see Automation below |

Both `arco-tls` and `ci` must be green before tagging `v1.7.1-arco.N`. If Go adds a file to `crypto/tls` whose name
starts with `n` or `ar`, add it to the exclusion regex in `.golangci.yml` (the lint job will flag it).

## Automation (`auto-sync.yml`)

`arco-tls.yml`'s daily `latest.sh`/`vulncheck.sh` only turns a dashboard **red**; a human still has to notice and
do the port. `auto-sync.yml` does the mechanical part and hands back a reviewable PR. It runs weekly (and on demand
via *Run workflow*), with logic in `tools/tlssync/auto/*.sh` so each half can be run and tested by hand.

**What it automates**

- **Go crypto/tls** (`tools/tlssync/auto/go-sync.sh`): finds the newest patch of our minor (`crypto/tls/UPSTREAM`)
  from `go.dev/dl`. If `git diff <old> <new>` over `src/crypto/tls` + the two fips140 key-schedule packages shows a
  change, it runs `sync.sh` then `check.sh` and opens a PR (branch `auto/tls-sync-<goX>`) with the diffstat and the
  `check.sh` result. If crypto/tls did **not** change, it opens a PR that only bumps `crypto/tls/UPSTREAM` (so
  `latest.sh` goes quiet) — no re-import. A newer Go **minor** is only mentioned in the log, never acted on (it is
  the 1–3 day port above).
- **nbio** (`tools/tlssync/auto/nbio-sync.sh`): finds the newest `lesismal/nbio` release tag not yet an ancestor of
  `arco`, tries the merge on a branch and runs `check.sh`. Clean merge → PR (`auto/nbio-<tag>`); conflicts → an
  **issue** listing the conflicting files and the manual steps. It never force-resolves a conflict.
- **No duplicates**: it skips opening a second PR/issue when one for the same version is already open (it just
  refreshes the branch).

**What a human still does** (the PR body repeats this): review the diff/merge (read path, locks, buffers per
step 5); fix `check.sh` if the PR shows it failed; then **tag** the next `v1.7.x-arco.N` and **bump** the
`replace` in arco-ws. Tagging and the consumer bump are deliberately left manual.

**Permissions / safety**: only the built-in `GITHUB_TOKEN` with `contents: write`, `pull-requests: write`,
`issues: write` — no new secrets. PR/issue text is neutral (never names the post-handshake work). Assumes `arco`
is the repo's default branch (scheduled runs use the default branch's workflow).

**Run / test by hand**

```
# detection only, no changes committed (NO_COMMIT=1); override the "newest" version to simulate a release
FORCE_GO_VERSION=go1.27.4 NO_COMMIT=1 tools/tlssync/auto/go-sync.sh
FORCE_NBIO_TAG=v1.7.3   NO_COMMIT=1 tools/tlssync/auto/nbio-sync.sh
# or from GitHub: Actions → auto-sync → Run workflow, with the force_* inputs
```
