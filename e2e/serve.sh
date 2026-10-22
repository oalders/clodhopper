#!/bin/sh
# Playwright's webServer: build clodhopper from this working tree, seed a
# throwaway DB with the worst-case fixtures, and serve it locked down — no real
# DB, no real tmux, no host CLODHOPPER_* config, and no write path unless the
# second argument is --enable-merge, which also turns on --pane-peek (the
# restart, swap, merge, session and block specs, which stub every /api/action
# and /api/pane).
#
# usage: serve.sh PORT [--enable-merge]
set -eu

case "${2-}" in
'' | --enable-merge) ;;
*)
	echo "usage: serve.sh PORT [--enable-merge]" >&2
	exit 2
	;;
esac

repo=$(cd "$(dirname "$0")/.." && pwd)
tmp=$(mktemp -d "${TMPDIR:-/tmp}/clodhopper-e2e.XXXXXX")
trap 'rm -rf "$tmp"' EXIT
# A signal must exit via the EXIT trap too (Playwright stops us with SIGTERM).
trap 'exit 143' TERM INT

# shellcheck disable=SC2046 # word-splitting the variable names is the point
unset TMUX TMUX_PANE $(env | sed -n 's/^\(CLODHOPPER_[A-Z_]*\)=.*/\1/p')
export CLODHOPPER_DB="$tmp/events.db"
# Unsetting TMUX doesn't stop a tmux call reaching the host's default server
# socket, so a stub tmux shadows the real one: it lists the seeded panes (so
# their peek controls render) and fails everything else, should an action ever
# slip past the tests' stubs. TMUX_TMPDIR backs that up if the stub is bypassed.
export TMUX_TMPDIR="$tmp"
mkdir "$tmp/bin"
cat >"$tmp/bin/tmux" <<'STUB'
#!/bin/sh
# Every seeded pane id (e2e_seed_test.go) must be listed, or dropDeadPanes hides its row.
[ "${1-}" = list-panes ] || exit 1
printf '%s\tseeded\tclaude\t/nonexistent\n' %9001 %9002
# Longer than the merge-queue session column's old 18ch, so merge.spec.ts can catch it clipping.
printf '%%9003\tseeded-merge-queue-session-name\tclaude\t/nonexistent\n'
STUB
chmod +x "$tmp/bin/tmux"
# A stub gh puts two seeded merge-queue rows (e2e_seed_test.go) in the queue,
# ejects one, reports the merged row's PR merged and the draft-conflict row's a
# conflicted draft, keyed on the row's cwd; every other cwd has no PR. The exec
# server also gets a "landing" row, queued on its first lookup (serve's first
# render) and merged after, for ledger.spec.ts.
# Single-shot: landing.seen never resets and ledger.spec.ts dismisses the row, so that spec cannot retry.
export CLODHOPPER_E2E_REPOS="$tmp/repos"
mkdir -p "$CLODHOPPER_E2E_REPOS/queued" "$CLODHOPPER_E2E_REPOS/ejected" "$CLODHOPPER_E2E_REPOS/merged" "$CLODHOPPER_E2E_REPOS/queued-approval" "$CLODHOPPER_E2E_REPOS/draft-conflict"
[ -z "${2-}" ] || mkdir "$CLODHOPPER_E2E_REPOS/landing"
cat >"$tmp/bin/gh" <<'STUB'
#!/bin/sh
dir=$(basename "$(pwd)")
case "$dir" in queued | queued-approval | ejected | merged | landing | draft-conflict) ;; *) exit 1 ;; esac
case "${1-}" in
pr)
	if [ "$dir" = landing ]; then
		if [ -e ../landing.seen ]; then
			echo '{"id":"PR_e2e","number":42,"url":"https://github.com/e2e/landing/pull/42","state":"MERGED","mergedAt":"2026-01-01T00:00:00Z","statusCheckRollup":[]}'
		else
			touch ../landing.seen
			echo '{"id":"PR_e2e","number":42,"url":"https://github.com/e2e/landing/pull/42","state":"OPEN","mergeStateStatus":"CLEAN","statusCheckRollup":[]}'
		fi
		exit 0
	fi
	# merged: the stale failing rollup and DIRTY must not render for a finished PR (#204).
	if [ "$dir" = merged ]; then
		echo '{"id":"PR_e2e","state":"MERGED","mergeStateStatus":"DIRTY","statusCheckRollup":[{"__typename":"CheckRun","status":"COMPLETED","conclusion":"FAILURE"}]}'
	elif [ "$dir" = draft-conflict ]; then
		echo '{"id":"PR_e2e","state":"OPEN","isDraft":true,"mergeStateStatus":"DIRTY","statusCheckRollup":[{"__typename":"CheckRun","status":"COMPLETED","conclusion":"SUCCESS"}]}'
	else
		echo '{"id":"PR_e2e","state":"OPEN","mergeStateStatus":"CLEAN","statusCheckRollup":[{"__typename":"CheckRun","status":"COMPLETED","conclusion":"SUCCESS"}]}'
	fi
	;;
api)
	if [ "$dir" = queued ] || [ "$dir" = landing ]; then
		echo '{"data":{"node":{"mergeQueueEntry":{"state":"QUEUED","position":1,"estimatedTimeToMerge":3600,"enqueuedAt":null},"timelineItems":{"nodes":[]}}}}'
	elif [ "$dir" = queued-approval ]; then
		echo '{"data":{"node":{"mergeQueueEntry":{"state":"QUEUED","position":2,"estimatedTimeToMerge":3600,"enqueuedAt":null},"timelineItems":{"nodes":[]}}}}'
	elif [ "$dir" = draft-conflict ]; then
		echo '{"data":{"node":{"mergeQueueEntry":null,"timelineItems":{"nodes":[]}}}}'
	else
		echo '{"data":{"node":{"mergeQueueEntry":null,"timelineItems":{"nodes":[{"__typename":"RemovedFromMergeQueueEvent","reason":"merge conflict"}]}}}}'
	fi
	;;
*) exit 1 ;;
esac
STUB
chmod +x "$tmp/bin/gh"
export PATH="$tmp/bin:$PATH"

cd "$repo"
go build -o "$tmp/clodhopper" .
go test -count=1 -tags e2eseed -run '^TestE2ESeed$' .
# Not exec'd, so the EXIT trap outlives serve and removes $tmp.
# shellcheck disable=SC2086 # ${2-} is empty or exactly --enable-merge (checked above)
"$tmp/clodhopper" serve --host 127.0.0.1 --port "$1" ${2-} ${2:+--pane-peek}
