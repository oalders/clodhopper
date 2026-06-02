# Disambiguate branches with the tmux session name

## Problem

A roster/activity row labelled `fix-1710` says little, and `fix-1710` next to
`fix-1711` is worse — the branch name alone does not tell you which task an agent
is working on. We want a human-meaningful disambiguator.

Two routes were considered:

1. **tmux session name** — each session runs inside a tmux session whose name is
   already set by the time the agent launches. Capture that name.
2. **gh issue title** — resolve `fix-1710` → issue #1710's title via `gh`, cached
   to avoid re-querying GitHub.

We choose **(1)**. It covers branches that have no backing issue, needs no cache,
and fits the existing capture model exactly: the name is captured per event at
ingest time (like `gitBranch`), so the view layer never re-queries anything.

## Approach

Capture the tmux session name at **ingest time** as a new event column, then
surface it on the dashboard:

- **Activity table** gains it as the first column → `session name · branch · app · events`.
- **Roster** shows it as the primary, disambiguating label with the branch as a
  dimmed secondary tag.

Capturing per row (rather than resolving at serve time) means the name is held
with the event forever and the dashboard does no extra work. This is the same
shape as `branch`, which is also a best-effort side-effect capture.

## Components

### 1. Capture — `ingest.go`

New best-effort helper, mirroring `gitBranch`:

```go
func tmuxSession() string {
    if os.Getenv("TMUX") == "" {
        return "" // not inside tmux
    }
    ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
    defer cancel()
    out, err := exec.CommandContext(ctx, "tmux", "display-message", "-p", "#S").Output()
    if err != nil {
        return ""
    }
    return truncate(scrubString(strings.TrimSpace(string(out))), maxFieldLen)
}
```

- `buildEvent` sets `TmuxSession: tmuxSession()`.
- `tmux display-message -p '#S'` resolves the current pane's session via `$TMUX`
  (no `-t` needed). Guarding on `$TMUX` first avoids spawning tmux — and its
  stderr noise — outside a session.
- Every error path returns `""` with a 2s timeout: **ingest must never fail a
  tool call**.
- The result is run through `scrubString` + `truncate(…, maxFieldLen)` to honour
  the scrub layer's fail-closed bias (the name is user-chosen free text).

### 2. Storage — `store.go`

- Add `TmuxSession string` to the `Event` struct.
- Add a `tmux_session TEXT` column via a new entry appended to the `migrations`
  slice (a best-effort `ALTER TABLE events ADD COLUMN tmux_session TEXT`; an
  already-applied `ALTER` errors harmlessly and is ignored).
- `insertEvent` writes the new column (extend the column list, the placeholder
  list, and the argument list).

### 3. Roster — `store.go` / `agentRoster`

- Add `TmuxSession` to the `Agent` struct.
- `SELECT COALESCE(tmux_session,'')` in the roster query and assign it
  last-write-wins in the ascending scan, exactly like `branch`/`cwd`.

### 4. Activity — `store.go` / `activeCounts` + `SourceCount`

- Add `TmuxSession` to `SourceCount`.
- Group by the new dimension as well:

  ```sql
  SELECT COALESCE(tmux_session,''), source_app, COALESCE(branch,''), COUNT(*)
  FROM events WHERE ts >= ?
  GROUP BY tmux_session, source_app, branch
  ORDER BY COUNT(*) DESC
  ```

### 5. Dashboard — `templates/dashboard.html`

- **Activity table**: header row becomes `session name | branch | app | events`;
  the first cell renders the tmux name, or `—` when empty.
- **Roster**: the first column shows the **tmux session name as the primary
  label** with the **branch as a dimmed tag** beneath it. Falls back to the
  branch alone when there is no tmux name, and `—` when neither is present.
- **Naming collision (resolved):** the roster already has a trailing column
  headed **"session"** — the Claude `session_id` colour-chip. To avoid two
  "session" columns, **rename that chip column's header to "id"** and head the
  new first column **"session"**.

### 6. Server — `server.go`

- Add `TmuxSession` to the roster etag hash (server.go:392, the `fmt.Fprintf(h, …)`
  line) so a session-name change busts the dashboard cache.

### 7. Tests

- `store_test` / `roster_test`: insert events carrying `TmuxSession`; assert the
  roster propagates it (last-write-wins) and `activeCounts` groups by and returns
  it.
- `ingest`: assert `tmuxSession()` returns `""` when `$TMUX` is unset. Add a
  tmux-backed happy-path test that `t.Skip`s when the `tmux` binary is absent
  (mirroring how `branch_test.go` exercises `gitBranch` against a real git repo).

## Invariants preserved

- **ingest never fails** — `tmuxSession` has a 2s timeout and returns `""` on
  every error path.
- **no chat/transcript content is persisted** — the name is a side-effect capture
  (like `branch`), not a payload field, so no allowlist change is needed; it is
  scrubbed and truncated regardless.
- **time/determinism** — no new clock reads; tests stay deterministic by passing
  `TmuxSession` in on inserted events.

## Out of scope (YAGNI)

- A tmux-session filter dropdown (the branch filter already exists; add later if
  wanted).
- The `gh` issue-title route and any caching for it.
