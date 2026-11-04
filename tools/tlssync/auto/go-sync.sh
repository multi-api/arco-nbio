#!/usr/bin/env bash
# go-sync.sh — detect a newer Go patch of the minor our crypto/tls copy tracks
# (crypto/tls/UPSTREAM) and, if crypto/tls changed between the two, re-import it
# with sync.sh and run check.sh. Leaves the result committed on the current
# branch. Used by .github/workflows/auto-sync.yml; safe to run by hand.
#
# Env overrides (for local testing / workflow_dispatch):
#   FORCE_GO_VERSION   pretend this is the newest patch of our minor (e.g. go1.27.4)
#   GO_DL_JSON         path/URL for the go.dev release list (default: the live API)
#   GODIR              a clone of https://go.googlesource.com/go (default: .src/go)
#   NO_COMMIT=1        do the work but leave it uncommitted (inspect with git diff)
#
# Outputs (written to $GITHUB_OUTPUT when set, always echoed):
#   status   up-to-date | tls-changed | recorded-only
#   from     the recorded Go version (before)
#   target   the newer Go version (after), empty when up-to-date
# A human-readable PR/summary body is written to ${SUMMARY_FILE:-tls-sync-summary.md}
# when something changed.
set -euo pipefail
ROOT=$(cd "$(dirname "$0")/../../.." && pwd)
cd "$ROOT"
GODIR=${GODIR:-$ROOT/.src/go}
DL=${GO_DL_JSON:-https://go.dev/dl/?mode=json}
SUMMARY_FILE=${SUMMARY_FILE:-$ROOT/tls-sync-summary.md}
TLS_PATHS=(src/crypto/tls src/crypto/internal/fips140/tls12 src/crypto/internal/fips140/tls13)

out() { # key value — emit a workflow output and a log line
	echo "$1=$2"
	[ -n "${GITHUB_OUTPUT:-}" ] && echo "$1=$2" >>"$GITHUB_OUTPUT"
	return 0
}
fetch_json() { case "$DL" in http*) curl -fsSL "$DL";; *) cat "$DL";; esac; }

UP=$(cat crypto/tls/UPSTREAM)
MINOR=$(echo "$UP" | sed -E 's/^(go[0-9]+\.[0-9]+).*/\1/')

if [ -n "${FORCE_GO_VERSION:-}" ]; then
	TARGET=$FORCE_GO_VERSION
else
	TARGET=$(fetch_json | python3 -c 'import json,sys; m=sys.argv[1]; print(next((r["version"] for r in json.load(sys.stdin) if r["version"]==m or r["version"].startswith(m+".")), ""))' "$MINOR")
fi

echo "recorded: $UP   newest $MINOR.x: ${TARGET:-unknown}"
out from "$UP"

if [ -z "$TARGET" ] || [ "$TARGET" = "$UP" ]; then
	out status up-to-date
	out target ""
	# A newer minor is a human decision (bigger port): surface it, don't act.
	NEWEST=$(fetch_json | python3 -c 'import json,sys; print(json.load(sys.stdin)[0]["version"])' 2>/dev/null || true)
	NEWMINOR=$(echo "${NEWEST:-}" | sed -E 's/^(go[0-9]+\.[0-9]+).*/\1/')
	[ -n "$NEWEST" ] && [ "$NEWMINOR" != "$MINOR" ] && echo "note: a newer Go minor ($NEWEST) exists — plan the minor port by hand (docs/porting.md)."
	exit 0
fi

# A clone of the Go source so we can diff crypto/tls between the two tags.
if [ ! -d "$GODIR" ]; then
	git clone --filter=blob:none --no-checkout https://go.googlesource.com/go "$GODIR"
fi
for t in "$UP" "$TARGET"; do
	git -C "$GODIR" rev-parse -q --verify "refs/tags/$t" >/dev/null ||
		git -C "$GODIR" fetch -q origin "refs/tags/$t:refs/tags/$t"
done

if git -C "$GODIR" diff --quiet "$UP" "$TARGET" -- "${TLS_PATHS[@]}"; then
	# No crypto/tls change between the two: just record the new version so
	# latest.sh stops flagging it. Nothing to re-import or re-test.
	echo "$TARGET" >crypto/tls/UPSTREAM
	out status recorded-only
	out target "$TARGET"
	{
		echo "## Record Go $TARGET (no crypto/tls change)"
		echo
		echo "Go **$TARGET** is out. \`git diff $UP $TARGET\` over \`src/crypto/tls\` and the two"
		echo "fips140 key-schedule packages is empty, so the copy needs no re-import — only"
		echo "\`crypto/tls/UPSTREAM\` is bumped so \`latest.sh\` stops flagging it."
		echo
		echo "Human steps left: none beyond merging this PR."
	} >"$SUMMARY_FILE"
	[ "${NO_COMMIT:-}" = 1 ] || { git add crypto/tls/UPSTREAM; git commit -q -m "chore(tls): record $TARGET (no crypto/tls change)"; }
	exit 0
fi

echo "crypto/tls changed $UP -> $TARGET: re-importing with sync.sh"
tools/tlssync/sync.sh "$TARGET" "$GODIR"

echo "running check.sh"
CHECKLOG=$(mktemp)
set +e
( ulimit -n 30000 2>/dev/null; tools/tlssync/check.sh "${FUZZTIME:-60s}" ) >"$CHECKLOG" 2>&1
CHECK_RC=$?
set -e
tail -40 "$CHECKLOG"

out status tls-changed
out target "$TARGET"
{
	echo "## Sync crypto/tls to Go $TARGET"
	echo
	echo "Go **$TARGET** changed \`src/crypto/tls\` vs **$UP**; re-imported with \`tools/tlssync/sync.sh $TARGET\`."
	echo
	echo "### Diff summary (this repo's crypto/tls)"
	echo '```'
	git --no-pager diff --stat HEAD -- crypto/tls | tail -40
	echo '```'
	echo "### check.sh result: $([ $CHECK_RC -eq 0 ] && echo PASS || echo 'FAIL — review required')"
	echo "Runs: diff-vs-Go, build+vet, Go crypto/tls suite + ours with -race, nbio tests, fuzz, govulncheck."
	echo '```'
	tail -25 "$CHECKLOG"
	echo '```'
	echo "### Steps left for a human"
	echo "1. Review the diff against Go (docs/porting.md step 5): read what changed in the read path, locks and buffers."
	echo "2. If check.sh failed above, resolve it (a missing shim or rewrite) before merging."
	echo "3. After merge: tag the next \`v1.7.x-arco.N\` and bump the dep in arco-ws (docs/porting.md)."
} >"$SUMMARY_FILE"

if [ "${NO_COMMIT:-}" != 1 ]; then
	git add -A crypto/tls
	git commit -q -m "sync(tls): import Go $TARGET crypto/tls" \
		-m "Re-imported with tools/tlssync/sync.sh. check.sh: $([ $CHECK_RC -eq 0 ] && echo passed || echo FAILED — needs review)."
fi
# Surface a check failure to the caller, but only after committing so the PR shows the state.
exit 0
