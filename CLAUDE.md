# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What this is

`clodhopper` is a project-agnostic observability tool for Claude Code. A single Go
binary with three subcommands captures lifecycle events from any project's hooks
into a local SQLite database and serves a read-only dashboard. See `README.md`
for the user-facing design and configuration table.

## Commands

```bash
go build ./...        # build
go test ./...         # run all tests (requires CGO — uses github.com/mattn/go-sqlite3)
go test -run TestName ./...   # run a single test
go vet ./...
go install ./...      # install `clodhopper` to GOBIN / $GOPATH/bin
precious lint --all   # gofmt + go vet + golangci-lint (what CI runs)
precious tidy --all   # auto-fix formatting
```

CGO must be enabled (a C compiler must be present) because of the SQLite driver.
Linting is orchestrated by [precious](https://github.com/houseabsolute/precious)
(`precious.toml`): `gofmt`, `go vet`, and `golangci-lint`. The same checks run
in the pre-commit hook (`scripts/pre-commit --init` to enable) and in CI
(`.github/workflows/lint.yml`), so keep code `precious lint --all`-clean. See
README.md "Linting and formatting".

## Architecture

The defining constraint is **capture and viewing are decoupled through the shared
SQLite file**. `ingest` (write) never depends on `serve` (read); the dashboard
being down never loses events.

Three subcommands, dispatched in `main.go`:

- **`ingest`** (`ingest.go`) — reads ONE hook event as JSON on stdin, scrubs it,
  writes one row, exits. This is what project hooks call on every tool use.
- **`serve`** (`server.go`) — HTTP dashboard, renders `templates/dashboard.html`; read-only
  by default, plus an **opt-in** write path (`--enable-merge`) that runs allowlisted
  `merge-pr`/`gh pr ready` actions per roster row, plus an `end` action that runs
  no subprocess at all and just calls `endSessions` to drop the row. `ingest` never gains write-to-repo
  capability — the capture path stays read-only.
- **`prune`** (`main.go`) — deletes events older than the retention window.

`store.go` owns the schema, the `Event`/`Agent`/`SourceCount` types, and all
queries. The dashboard's two main views are both derived here: `agentRoster`
folds recent events into one live-status row per session (the "which agent needs
me" board), and `activeCounts` tallies activity per (source_app, branch).

### Two invariants that shape everything

1. **`ingest` must NEVER fail a tool call.** Every error path in `runIngest`
   returns exit 0; diagnostics go to stderr only when `CLODHOPPER_DEBUG` is set
   (`debugf`). Missing/malformed JSON, an unwritable DB, a `git` timeout — all
   no-op silently. Preserve this when editing the ingest path. Side effects like
   `gitBranch` use tight timeouts for the same reason.

2. **No chat/transcript content is ever persisted.** `scrub.go` enforces this
   with a strict **allowlist**, not a denylist: only keys in `payloadAllow`
   (and within `tool_input`, only `toolInputAllow`) survive; everything else
   (messages, `content`, `old_string`/`new_string`, `tool_response`,
   `transcript_path`, …) is dropped before storage. On top of the allowlist,
   `scrubPatterns` redacts secret-shaped substrings (tokens, keys, `Authorization`
   headers, PEM blocks) in any retained free text. Prompts are truncated to an
   80-char preview. When adding a new captured field, add its key to the
   appropriate allowlist — fields are dropped by default. The scrub layer fails
   closed (prefers over-redaction to leaks); preserve that bias.

   (The `--enable-merge` write path added to `serve` doesn't touch either
   invariant or the ingest/capture path. It's off by default, and when on is
   POST-only, CSRF-protected (custom `X-Clodhopper-Token` header, constant-time
   compare), Host-header-allowlisted against DNS rebinding, and restricted to a
   closed argv allowlist — no user-supplied string ever reaches a command line.
  The `end` action never reaches `actionArgv`: it execs nothing, and its
  `session_id` travels only as a bound SQL parameter.)

### Other conventions

- **Time is injected, not read from the clock**, in any function whose output is
  tested (`agentRoster`, `idleSeconds`, `ciCache.status`, …) — they take a `now
  time.Time`. Follow this for new time-dependent logic so tests stay deterministic.
- **Schema migrations** append to the `migrations` slice in `store.go` and run
  best-effort (an already-applied `ALTER` errors harmlessly and is ignored).
- **CI status** (`server.go`) shells out to `gh pr checks` per cwd, memoised in
  `ciCache` (60s TTL) and fetched concurrently across worktrees. Like `gitBranch`,
  it is strictly best-effort: missing `gh`, no PR, or a timeout yields "".
- The `branch` dimension exists so each concurrent **git worktree** shows up as
  its own roster row / activity tally — that grouping is the point, not incidental.
- Config is read from env vars with `CLODHOPPER_` prefixes (see README table);
  the getters live in `main.go`.

## Wiring into a project

Projects opt in by adding hooks to their own `.claude/settings.json` that call
`clodhopper ingest --source-app NAME`, guarded on the binary existing so
environments without it no-op. `source_app` is the per-project label; finer
grouping comes from `cwd` / `session_id` already in each hook payload.
