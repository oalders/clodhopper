# clodhopper

A small, project-agnostic observability tool for Claude Code. It captures
lifecycle events from any project's hooks into a local SQLite database and
serves a read-only, per-host dashboard.

Built as the "lightweight, inspiration-only" outcome of an evaluation of
[disler/claude-code-hooks-multi-agent-observability](https://github.com/disler/claude-code-hooks-multi-agent-observability):
same idea (see what your agents are doing across concurrent sessions), but on
the Go + SQLite stack, with secret scrubbing and bounded retention built in, and
no dependency on a running server for capture.

## Design in one breath

```
Claude Code hook (event JSON on stdin)
   -> clodhopper ingest --source-app NAME    # fast, scrubs secrets, writes one row
        -> ~/.claude/clodhopper/var/events.db (SQLite, WAL)

clodhopper serve    # run only when you want to look
   -> read-only dashboard on http://127.0.0.1:4555
```

Capture (write) and viewing (read) are decoupled through the shared SQLite file.
The dashboard being down never loses events; `ingest` never depends on a server.

## Install

```bash
cd ~/.claude/clodhopper
go install ./...        # puts `clodhopper` on your PATH (GOBIN / $GOPATH/bin)
```

Requires CGO (a C compiler) because it uses `github.com/mattn/go-sqlite3`.

## Usage

```bash
clodhopper ingest --source-app myapp       # reads one hook event as JSON on stdin
clodhopper serve [--port 4555] [--host H] # read-only dashboard (default 127.0.0.1)
clodhopper prune [--days 14]              # delete events older than N days
```

By default the dashboard binds `127.0.0.1` (loopback only). If your browser runs
on a different machine or namespace than the server (e.g. inside a container or
VM), bind all interfaces with `clodhopper serve --host 0.0.0.0` (or `CLODHOPPER_HOST=0.0.0.0`)
and reach it via that host's IP — note this exposes the dashboard to your LAN.

To reach the dashboard from your other devices without exposing it to the LAN,
bind it to your [Tailscale](https://tailscale.com) IP instead of `0.0.0.0`:

```bash
clodhopper serve --host "$(tailscale ip -4)"
```

This listens only on the tailnet interface, so the dashboard is reachable from
any device on your tailnet (subject to your ACLs) but not from the local
network. Reach it at `http://<this-host's-tailscale-ip>:4555`, or by the host's
MagicDNS name. `tailscale ip -4` prints the host's IPv4 tailnet address; drop
`-4` for IPv6.

`ingest` is what hooks call. It is designed to **never** break a tool call: any
error (bad JSON, unwritable DB, …) results in exit 0, and diagnostics are
written to stderr only when `CLODHOPPER_DEBUG` is set.

## Configuration (environment)

| Var | Default | Meaning |
|---|---|---|
| `CLODHOPPER_DB` | `~/.claude/clodhopper/var/events.db` | SQLite path. |
| `CLODHOPPER_RETAIN_DAYS` | `14` | Events older than this are pruned (opportunistically on ~1% of ingests, and on demand via `prune`). |
| `CLODHOPPER_DISABLED` | unset | `1` makes `ingest` a no-op. |
| `CLODHOPPER_PORT` | `4555` | Dashboard port. |
| `CLODHOPPER_HOST` | `127.0.0.1` | Dashboard bind address. Set `0.0.0.0` for container/LAN access. |
| `CLODHOPPER_DEBUG` | unset | If set, `ingest` writes errors to stderr. |

## What gets captured (and what does not)

Each event row holds: timestamp, `source_app`, `cwd`, `session_id`,
`event_type`, `tool_name`, a short human `summary`, and the full **scrubbed**
event JSON.

Privacy guarantees:

- **No transcript / chat capture.** The hook's `transcript_path` file is never
  read. `prompt` fields are truncated to an 80-character preview.
- **Secret scrubbing before persistence**, in three layers:
  1. JSON string values under credential-shaped keys (`*token*`, `*secret*`,
     `*password*`, `*api_key*`, `*_key`, …) are redacted wholesale.
  2. Secret-shaped substrings anywhere are redacted: `NAME=value` /
     `NAME: value` credential assignments, `Authorization: Bearer/Basic …`,
     AWS access key IDs, GitHub tokens (`ghp_…`, `github_pat_…`), and PEM
     private-key blocks.
  3. The `summary` line is built from a per-tool allowlist (Bash → command,
     Read/Edit/Write → file path only) and is itself scrubbed.

Redactions appear as `«redacted»`.

## Wiring a project

The easiest way is `clodhopper init`, run from the project's root. It writes the
ingest hooks for every Claude Code lifecycle event into the project's settings,
idempotently (safe to re-run):

```bash
clodhopper init --project                 # -> .claude/settings.json (committed)
clodhopper init --local                   # -> .claude/settings.local.json (gitignored)
clodhopper init --project --dry-run       # preview without writing
```

With neither `--project` nor `--local` it prompts you to choose. `--source-app`
defaults to the git repo name; pass `--source-app NAME` to override (required
outside a git repo). The generated command is guarded so environments without the
binary simply no-op; `--guard command` (default) uses the portable
`command -v clodhopper` check, `--guard is` uses the `is there clodhopper` helper.

`init` writes 2-space-indented JSON; the first run on an existing committed
settings file will re-sort its keys (a one-time noisy diff) — preview with
`--dry-run`.

Or, by hand: add hooks to the project's `.claude/settings.json` (or
`settings.local.json`). Guard on the binary existing so environments without
`clodhopper` simply no-op:

```json
{
  "hooks": {
    "PreToolUse": [
      { "hooks": [ { "type": "command",
        "command": "command -v clodhopper >/dev/null 2>&1 && clodhopper ingest --source-app myproject || true" } ] }
    ]
  }
}
```

`source_app` is a static label per project; finer grouping (worktree, session)
comes from the `cwd` / `session_id` already in each hook payload.

## Development

```bash
go test ./...   # requires CGO
go vet ./...
```

Runtime data lives under `var/` (gitignored).
