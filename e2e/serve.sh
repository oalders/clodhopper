#!/bin/sh
# Playwright's webServer: build clodhopper from this working tree, seed a
# throwaway DB with the worst-case fixtures, and serve it locked down — no real
# DB, no tmux, no write path, no host CLODHOPPER_* config.
set -eu

repo=$(cd "$(dirname "$0")/.." && pwd)
tmp=$(mktemp -d "${TMPDIR:-/tmp}/clodhopper-e2e.XXXXXX")
trap 'rm -rf "$tmp"' EXIT
# A signal must exit via the EXIT trap too (Playwright stops us with SIGTERM).
trap 'exit 143' TERM INT

# shellcheck disable=SC2046 # word-splitting the variable names is the point
unset TMUX TMUX_PANE $(env | sed -n 's/^\(CLODHOPPER_[A-Z_]*\)=.*/\1/p')
export CLODHOPPER_DB="$tmp/events.db"

cd "$repo"
go build -o "$tmp/clodhopper" .
go test -count=1 -tags e2eseed -run '^TestE2ESeed$' .
# Not exec'd, so the EXIT trap outlives serve and removes $tmp.
"$tmp/clodhopper" serve --host 127.0.0.1 --port "$1"
