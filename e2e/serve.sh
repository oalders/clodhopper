#!/bin/sh
# Playwright's webServer: build clodhopper from this working tree, seed a
# throwaway DB with the worst-case fixtures, and serve it locked down — no real
# DB, no real tmux, no host CLODHOPPER_* config, and no write path unless the
# second argument is --enable-merge, which also turns on --pane-peek (the
# restart, swap and merge specs, which stub every /api/action and /api/pane).
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
STUB
chmod +x "$tmp/bin/tmux"
export PATH="$tmp/bin:$PATH"

cd "$repo"
go build -o "$tmp/clodhopper" .
go test -count=1 -tags e2eseed -run '^TestE2ESeed$' .
# Not exec'd, so the EXIT trap outlives serve and removes $tmp.
# shellcheck disable=SC2086 # ${2-} is empty or exactly --enable-merge (checked above)
"$tmp/clodhopper" serve --host 127.0.0.1 --port "$1" ${2-} ${2:+--pane-peek}
