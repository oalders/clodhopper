# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What this is

`clodhopper` is a project-agnostic observability tool for Claude Code. A single Go
binary with a handful of subcommands captures lifecycle events from any project's hooks
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
cd e2e && npm ci && npx playwright test   # rendered-page tests; see README "Testing"
```

CGO must be enabled (a C compiler must be present) because of the SQLite driver.
Linting is orchestrated by [precious](https://github.com/houseabsolute/precious)
(`precious.toml`): `gofmt`, `go vet`, and `golangci-lint`. The same checks run
in the pre-commit hook (`scripts/pre-commit --init` to enable) and in CI
(`.github/workflows/lint.yml`), so keep code `precious lint --all`-clean. See
README.md "Linting and formatting".

**Never touch the user's real tmux server.** This code drives tmux, and the
default socket is the user's live session: one `tmux kill-server` there kills
every agent and terminal they have open. Tests use fake `tmux` stubs on `PATH`
(the e2e harness's `serve.sh` does the same); prefer them. If you must probe
real tmux behaviour, give every call a scratch socket — `tmux -L <name> …` or
`tmux -S "$TMPDIR/<sock>" …` — and only ever kill that socket's server. Unsetting
`TMUX` does not isolate a call: it still reaches the default socket.

## Architecture

The defining constraint is **capture and viewing are decoupled through the shared
SQLite file**. `ingest` (write) never depends on `serve` (read); the dashboard
being down never loses events.

Subcommands, dispatched in `main.go`:

- **`ingest`** (`ingest.go`) — reads ONE hook event as JSON on stdin, scrubs it,
  writes one row, exits. This is what project hooks call on every tool use.
- **`serve`** (`server.go`) — HTTP dashboard, renders `templates/dashboard.html`; read-only
  by default, plus an **opt-in** write path (`--enable-merge`) that runs allowlisted
  `merge-pr`/`gh pr ready` actions per roster row, plus tmux session actions
  (`monitor-ci`, `new-monitor`, `reload-plugins`, `rebase`; see
  `runSessionAction`/`handleAction` in `action.go`), plus an `end` action that
  runs no subprocess at all and just calls `endSessions` to drop the row, and a
  `dismiss` action that execs nothing and drops a merge-queue ledger row. Before any
  of that, middleware around the whole mux (`newServeMux`) applies the security
  headers and the Host allowlist (`hostAllowed`, 421 on a miss) to every route,
  so a DNS-rebinding page cannot even read the read-only board (this is in
  addition to, not instead of, `execPeerAllowed`'s own Host check). Every action
  that DOES exec is additionally gated on the peer's address (`remoteAllowed`):
  loopback or Tailscale only, whatever `--host` binds to, with no override — and
  the request is refused outright when a forwarding header (`X-Forwarded-For`,
  `X-Real-IP`, `Forwarded`, `X-Forwarded-Host`) is *present* — for EVERY action,
  including the exec-free `end`/`block`/`unblock`/`dismiss`, because behind a proxy every peer looks like
  loopback and no peer can be attributed at all. All three checks (proxy
  refusal, Host allowlist, peer network) live in ONE helper, `execPeerAllowed`,
  shared by handleAction and handlePane so they cannot drift. **Never run
  `serve --enable-merge` behind a reverse proxy, `tailscale serve`, or a port
  forward**: the peer gate assumes a direct socket, and `ssh -L` or a
  header-stripping proxy defeats it silently. `/api/pane` (`--pane-peek`) runs the SAME gate — it execs `tmux` and
  streams live transcript text, and being a GET it carries no CSRF token, so the
  Host allowlist is what stops a DNS-rebinding page from walking pane ids.
  The CSRF token is emitted into the page only for a peer that passes the gate,
  so a peer that fails it gets a genuinely read-only board (no `end`/`block`/`unblock`/`dismiss` either).
  `ingest` never gains write-to-repo
  capability — the capture path stays read-only.
- **`prune`** (`main.go`) — deletes events older than the retention window,
  plus blocks whose sessions have no events left.
- **`end`** (`end.go`) — writes a synthetic `SessionEnd` for live sessions matching a branch, cwd or session-id prefix, so hard-killed agents leave the roster.
- **`init`** (`init.go`) — wires the `clodhopper ingest` hooks into a project's `.claude/settings.json` (or `settings.local.json`), skipping ones already present.
- **`block` / `unblock`** (`block.go`) — record or drop "this session waits on
  that one" in the `blocks` table, or (`--on-pr N`) "waits on that open PR of its
  own GitHub repo" in `pr_blocks`. `applyBlocks` (server layer, after
  `dropDeadPanes`) relaxes a waiting/needs-you row to `statusBlocked` while its
  blocker session is still on the roster; `applyPRBlocks` does the same until the
  ledger's `finals` lookup reports the PR merged or closed (a failed lookup keeps
  the block). `agentRoster` itself never sees blocks.
  They never touch `ingest` or the capture path.

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
   compare), Host-header-allowlisted against DNS rebinding, peer-address-gated to
   loopback/Tailscale for anything that execs (`remoteAllowed`, which never reads
   a forwarding header — the Host allowlist alone cannot stop a LAN peer that
   sends `Host: 127.0.0.1`), and restricted to a
   closed argv allowlist — no user-supplied string ever reaches a command line.
  serve's read-only ledger lookup (`lookupPRFinal` in `ledger.go`: `gh pr view
  N --repo=OWNER/REPO` via `prFinalArgs`, then the same `mergeQueueQuery` call
  with the node id gh returned) is likewise built only from validated
  gh-derived values, never client input.
  The `end` action never reaches `actionArgv`: it execs nothing, and its
  `session_id` travels only as a bound SQL parameter. `block`/`unblock` are
  exec-free like `end` (`actionExecs`; they never reach `actionArgv`); `block` checks both ids against a
  fresh `agentRoster()` before writing the `blocks` row. `block-pr` is NOT
  exec-free (a cold open-PR cache runs `gh pr list`), so it gets the full peer
  gate; it never reaches `actionArgv` either: the client sends only a positive
  integer PR number, the repo is resolved server-side from the row's cwd, and
  the PR must be in that repo's open-PR list (`prListArgs` builds gh's argv from
  a `validRepo`-checked repo; the list is cached 5 minutes, outside `expire`). `dismiss` is exec-free too and only
  drops an in-memory `queueLedger` entry (404 for an unknown id). `rebase`, like
  `reload-plugins`, sends fixed `send-keys` text into a server-resolved pane,
  never through `actionArgv`; it execs tmux, so it gets `execPeerAllowed`'s full
  peer gate. The merge-pr actions (`squash`, `squash-admin`, `close`)
  never run as serve's subprocess: `startMergeWindow` opens a new tmux
  window in the row's own session running merge-pr (it may wait minutes on a
  merge queue) and returns at once; its varying argv elements are the
  `display-message` session id (validated), the stored absolute cwd (refused if
  it holds a tmux format `#`) and the `LookPath`'d merge-pr. The row drops
  only once `dropDeadPanes` sees the pane gone (a lone row may need `end`).
  `broadcast-restart` is a separate path that never reaches `actionArgv`: it
  sends `/exit`, then — only if nono's denied-paths review is the last thing on
  screen (`nonoReviewOnScreen`) — a fixed Escape key to cancel it, then types
  the fixed prefix `nn --resume ` plus the session id, all via tmux send-keys;
  the id comes from a fresh `agentRoster()` call inside the request (never
  client input) and must match `sessionIDRe` (a UUID) before `/exit` is even
  sent.)

### Other conventions

- **No screen-reader work.** The dashboard has a single user, who doesn't use a
  screen reader. Don't add ARIA live regions, screen-reader-only text or similar,
  and don't raise their absence as a review finding.
- **Time is injected, not read from the clock**, in any function whose output is
  tested (`agentRoster`, `idleSeconds`, `cwdCache.get`, …) — they take a `now
  time.Time`. Follow this for new time-dependent logic so tests stay deterministic.
- **Schema migrations** append to the `migrations` slice in `store.go` and run
  best-effort (an already-applied `ALTER` errors harmlessly and is ignored).
- **CI status** (`server.go`) shells out to one `gh pr view --json
  id,state,isDraft,mergeStateStatus,statusCheckRollup` per cwd, memoised in `cwdCache` (60s TTL)
  and fetched concurrently across worktrees. The "refresh CI" button sends
  `/api/state?force=1`, which calls `expire` (throttled to once per
  `ciForceMinInterval`); concurrent `get`s for one cwd share one lookup. The rollup drives the state and fail
  count (`classifyRollup`); `mergeStateStatus` only refines it (UNSTABLE →
  "soft-failing", DIRTY → merge conflict); `parsePRView`'s doc comment says why
  BLOCKED is deliberately not mapped. A MERGED or CLOSED PR skips the stale
  rollup for a muted chip and relaxes a needs-you/waiting row to background
  (`demotePendingCI`). For an OPEN PR, a second call (`gh api graphql`,
  `mergeQueueQuery`, sharing the same 4s deadline) reads the merge
  queue, because queue checks run on a temporary commit the rollup never sees;
  `applyMergeQueue`'s doc comment says when that yields a queue-position pill
  or the "ejected" state (⏏), which lapses once a rollup check starts after
  the removal (`RollupSince`). Like `gitBranch`, it is strictly best-effort: missing `gh`, no PR, or a
  timeout yields "". Any row with a queue entry leaves the roster for the
  merge-queue section and never alerts, stuck or not (`splitQueued`,
  `demotePendingCI`).
  Rows `splitQueued` folds into the queue are kept in memory by `queueLedger`
  (`ledger.go`; its doc comment gives the lifecycle and bounds) until dismissed
  or expired.
  The same generic `cwdCache` type also backs the pane peek's GitHub owner/repo for the
  footer's PR #N link (`newRepoCache`/`lookupRepo`, via `git remote get-url origin`),
  resolved per live-pane row (not concurrently) and likewise best-effort: a
  non-GitHub remote or any error yields "".
- The `branch` dimension exists so each concurrent **git worktree** shows up as
  its own roster row / activity tally — that grouping is the point, not incidental.
- Config is read from env vars with `CLODHOPPER_` prefixes (see README table);
  the getters live in `main.go`.

## Wiring into a project

Projects opt in by adding hooks to their own `.claude/settings.json` that call
`clodhopper ingest --source-app NAME`, guarded on the binary existing so
environments without it no-op. `source_app` is the per-project label; finer
grouping comes from `cwd` / `session_id` already in each hook payload.
