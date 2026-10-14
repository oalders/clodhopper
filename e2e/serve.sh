#!/bin/sh
# Playwright's webServer: build clodhopper from this working tree, seed a
# throwaway DB with the worst-case fixtures, and serve it locked down — no real
# DB, no tmux, no host CLODHOPPER_* config, and no write path unless the second
# argument is --enable-merge (restart.spec.ts, which stubs every /api/action).
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
# socket; pointing the socket dir at $tmp does, should an action ever slip past
# the tests' stubs under --enable-merge.
export TMUX_TMPDIR="$tmp"

cd "$repo"
go build -o "$tmp/clodhopper" .
go test -count=1 -tags e2eseed -run '^TestE2ESeed$' .
# Not exec'd, so the EXIT trap outlives serve and removes $tmp.
# shellcheck disable=SC2086 # ${2-} is empty or exactly --enable-merge (checked above)
"$tmp/clodhopper" serve --host 127.0.0.1 --port "$1" ${2-}
