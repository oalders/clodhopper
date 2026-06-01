# Roster waiting-retention design

**Date:** 2026-06-01
**Status:** Approved (pending spec review)

## Problem

The dashboard roster ("which of my agents is waiting on me") folds events from a
single flat 30-minute window (`agentRoster(db, window, now)` in `store.go`). Any
session with no event in the last 30 minutes drops off the board entirely —
including agents whose latest event is a `Stop` (waiting for you),
`Notification` (needs you), or `PermissionRequest` (needs approval).

Consequence: step away for a lunch or overnight and the agents that are
*literally waiting for your input* silently vanish, replaced by the unhelpful
empty state "No live agents in the last 30 min." The 30-minute cutoff is being
used as a liveness proxy, but it discards exactly the agents that matter most.

## Key insight: SessionEnd is the real liveness signal

Claude Code emits a session lifecycle: `SessionStart` → … → `Stop` /
`Notification` / `PermissionRequest` → `SessionEnd`. clodhopper already captures
the event type, and `deriveStatus` already treats `SessionEnd` as the
authoritative "agent gone" signal (`active=false`, drops off the board).

So a session whose latest event is a needs-me status and that has **not** since
emitted `SessionEnd` is genuinely still attached and waiting. The time window
should not be what removes it — `SessionEnd` should.

### Why a fallback cap is still required

`SessionEnd` is not guaranteed to arrive:

1. **Not always wired** — a project must opt its hooks in.
2. **Hard kills skip it** — `tmux kill-session` / `kill -9`, crashes, and laptop
   sleep give Claude Code no chance to run shutdown. A closing tmux pane sends
   SIGHUP, which frequently kills the process before the hook runs; even SIGTERM
   / SIGINT only *sometimes* fire `SessionEnd` (confirmed against Claude Code
   hook behavior — signal-driven shutdown is unreliable and has no documented
   `reason` value). SIGKILL never fires it.

Without a cap these produce "zombie" sessions that look like they are waiting
forever. So retention is bounded by `min(SessionEnd, cap)`.

### Why not process-liveness (PID) detection

Rejected. clodhopper does not capture a PID (not in the scrub allowlist), and a
local PID check breaks the moment capture (`ingest`) and viewing (`serve`) run
on different hosts — which is a supported, encouraged setup (e.g. serving over
Tailscale). The decoupled, possibly-cross-host design makes PID liveness the
wrong tool.

## Design

### 1. Status-aware retention (`store.go: agentRoster`)

Change the signature from `agentRoster(db, window, now)` to
`agentRoster(db, liveWindow, waitingCap, now)`:

- Query over the larger bound (`waitingCap`); fold to latest-event-per-session
  as today.
- `SessionEnd` latest → drop (unchanged).
- `working` latest (PreToolUse/etc.) → keep only if idle ≤ `liveWindow`
  (30 min). Stale workers still age out, exactly as today.
- `waiting` / `needs you` / `needs approval` latest → keep until `SessionEnd`
  or `waitingCap`, whichever comes first.

The `idle` column already renders growing age ("1h", "3h"), so a long-waiting
agent is visually obvious rather than silently dropped.

`activeCounts` is unchanged — it keeps the 30-minute window because it is an
activity tally, not a needs-me board.

### 2. Zombie-eviction cap (config)

New env knob `CLODHOPPER_WAITING_RETAIN_HOURS`, default **16** (covers an
overnight gap: step away in the evening, clear a waiting agent over morning
coffee; still well inside the 14-day prune window so zombies cannot accumulate).
The 30-minute `liveWindow` stays a const (`agentWindow` in `server.go`).

### 3. New `clodhopper end` subcommand (`main.go` dispatch + `store.go`)

`clodhopper end [--branch B | --cwd DIR | --session ID]` — at least one selector
required (it refuses to mark everything). It finds each currently-live session
(latest event ≠ `SessionEnd`) matching the selector and inserts a synthetic
`SessionEnd` row via the existing `insertEvent` path: `ts=now`, copying
`source_app`/`branch`/`cwd` from the session's latest row, `event_type` =
`SessionEnd`, summary `ended via clodhopper end`. Prints the count of sessions
ended.

This is an explicit user-invoked command, not the hot `ingest` path, so it may
return real errors — but it stays tolerant enough not to break a teardown script
(no matching sessions → exit 0).

**Selector rationale:** the caller's teardown script does not know Claude's
`session_id`, and should not need to. It knows the **branch** it is merging (the
natural handle for a merge-pr script) or the **worktree path**. clodhopper
resolves either to the live session_ids internally:

```bash
clodhopper end --branch "$branch"                                   # merge-pr
clodhopper end --cwd "$(tmux display-message -p -t "$pane" '#{pane_current_path}')"
```

### 4. Presentation (`templates/dashboard.html`)

- Header `Agents (live, last 30 min)` → `Agents (live + waiting on you)`.
- Empty-state reworded — drop "last 30 min"; keep the hint that detection needs
  the `Stop` / `Notification` hooks wired, and note that `SessionEnd` (or
  `clodhopper end`) is what clears a waiting agent.

### 5. Docs (`README.md`)

- Add `end` to the Usage command block.
- Short subsection on tearing down agents from scripts (the `--branch`
  one-liner, and why it beats relying on `SessionEnd` for hard kills).
- Add the `CLODHOPPER_WAITING_RETAIN_HOURS` row (default 16) to the config table.

### 6. Tests (`store_test.go`), following the injected-`now` convention

- Waiting agent older than `liveWindow` but within `waitingCap` → stays.
- Working agent past `liveWindow` → drops.
- Waiting agent past `waitingCap` → drops.
- `SessionEnd` latest → drops.
- `end` marks matching live sessions ended — by branch and by cwd — while
  leaving non-matching and already-ended sessions untouched.

## Invariants preserved

- **No chat/transcript content persisted.** `end` writes only the existing
  allowlisted columns (synthesized `SessionEnd`), no new payload fields.
- **`ingest` never fails a tool call.** Untouched; `end` is a separate,
  user-invoked path.
- **Time is injected.** `agentRoster` keeps taking `now`; the new `waitingCap`
  is a parameter, not read from the clock inside the tested function.

## Out of scope (YAGNI)

- A dashboard "dismiss" button / write endpoint from the served UI.
- Making the 30-minute live window configurable.
- Capturing PID or adding process-liveness checks.
