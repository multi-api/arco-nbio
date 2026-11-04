#!/usr/bin/env bash
# nbio-sync.sh — detect the newest lesismal/nbio release tag that is not yet in
# `arco`, try to merge it on the current branch and run check.sh. Leaves either a
# merge commit (success) or a clean tree plus a conflict report (failure). Used by
# .github/workflows/auto-sync.yml; safe to run by hand.
#
# Env overrides (for local testing / workflow_dispatch):
#   FORCE_NBIO_TAG     try this tag instead of auto-detecting the newest
#   NBIO_REMOTE_URL    upstream nbio URL (default https://github.com/lesismal/nbio)
#   NO_COMMIT=1        attempt the merge but abort it afterwards (detection/dry run)
#
# Outputs (written to $GITHUB_OUTPUT when set, always echoed):
#   status   up-to-date | merged | conflict
#   tag      the nbio tag considered, empty when up-to-date
# Writes ${SUMMARY_FILE:-nbio-sync-summary.md} (PR or issue body) when acting.
set -euo pipefail
ROOT=$(cd "$(dirname "$0")/../../.." && pwd)
cd "$ROOT"
NBIO_REMOTE_URL=${NBIO_REMOTE_URL:-https://github.com/lesismal/nbio}
SUMMARY_FILE=${SUMMARY_FILE:-$ROOT/nbio-sync-summary.md}

out() { echo "$1=$2"; [ -n "${GITHUB_OUTPUT:-}" ] && echo "$1=$2" >>"$GITHUB_OUTPUT"; return 0; }

# A read-only upstream remote just for fetching tags.
if ! git remote get-url nbio-upstream >/dev/null 2>&1; then
	git remote add nbio-upstream "$NBIO_REMOTE_URL"
fi
git fetch -q --tags nbio-upstream

if [ -n "${FORCE_NBIO_TAG:-}" ]; then
	TAG=$FORCE_NBIO_TAG
else
	# Newest upstream release tag by semver (plain vMAJ.MIN.PATCH, no -arco/pre-release).
	TAG=$(git tag -l 'v*' --sort=-v:refname | grep -E '^v[0-9]+\.[0-9]+\.[0-9]+$' | head -1)
fi
echo "newest upstream nbio tag: ${TAG:-none}"
out tag "${TAG:-}"

if [ -z "$TAG" ]; then out status up-to-date; exit 0; fi
if ! git rev-parse -q --verify "refs/tags/$TAG" >/dev/null; then
	echo "tag $TAG not found upstream"; out status up-to-date; exit 0
fi
if git merge-base --is-ancestor "refs/tags/$TAG" HEAD; then
	echo "$TAG is already merged into $(git rev-parse --abbrev-ref HEAD)"
	out status up-to-date
	exit 0
fi

echo "attempting merge of $TAG"
if ! git merge --no-ff --no-edit -m "Merge upstream nbio $TAG into arco" "refs/tags/$TAG"; then
	CONFLICTS=$(git diff --name-only --diff-filter=U || true)
	git merge --abort
	out status conflict
	{
		echo "## nbio $TAG does not merge cleanly into \`arco\`"
		echo
		echo "Auto-merge of upstream \`$TAG\` hit conflicts and was aborted. Resolve by hand"
		echo "(the fork owns \`crypto/tls/\`, \`extension/tls/\`, \`bench/\` and the \`docs/\`):"
		echo
		echo "### Conflicting files"
		echo '```'
		echo "$CONFLICTS"
		echo '```'
		echo "### How to do it"
		echo '```'
		echo "git checkout arco && git checkout -b auto/nbio-$TAG"
		echo "git merge refs/tags/$TAG   # resolve, keeping our crypto/tls + extension/tls"
		echo "ulimit -n 30000 && tools/tlssync/check.sh"
		echo '```'
	} >"$SUMMARY_FILE"
	exit 0
fi

echo "merge clean; running check.sh"
CHECKLOG=$(mktemp)
set +e
( ulimit -n 30000 2>/dev/null; tools/tlssync/check.sh "${FUZZTIME:-60s}" ) >"$CHECKLOG" 2>&1
CHECK_RC=$?
set -e
tail -40 "$CHECKLOG"

out status merged
{
	echo "## Merge upstream nbio $TAG into \`arco\`"
	echo
	echo "Upstream \`$TAG\` merged cleanly. The fork keeps its own \`crypto/tls\` (Go copy), so an"
	echo "upstream llib bump does not change our TLS."
	echo
	echo "### Merged commits"
	echo '```'
	git --no-pager log --oneline HEAD~1..HEAD^2 2>/dev/null | head -30
	echo '```'
	echo "### check.sh result: $([ $CHECK_RC -eq 0 ] && echo PASS || echo 'FAIL — review required')"
	echo '```'
	tail -25 "$CHECKLOG"
	echo '```'
	echo "### Steps left for a human"
	echo "1. Review the merge (especially anything under the read path / nbhttp)."
	echo "2. If check.sh failed above, resolve it before merging."
	echo "3. After merge: tag the next \`v1.7.x-arco.N\` and bump the dep in arco-ws (docs/porting.md)."
} >"$SUMMARY_FILE"

if [ "${NO_COMMIT:-}" = 1 ]; then git reset -q --hard HEAD~1; fi
exit 0
