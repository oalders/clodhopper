# Roster Waiting-Retention Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Keep agents that are waiting on the user on the dashboard roster until their session truly ends (`SessionEnd`) or a configurable cap, and add a `clodhopper end` command so teardown scripts can evict hard-killed agents immediately.

**Architecture:** `agentRoster` becomes status-aware: it queries over a long "waiting cap" window, still ages out silent *working* agents at the 30-minute live window, but retains *waiting / needs-you / needs-approval* agents out to the cap. A new `endSessions` store function (and `clodhopper end` subcommand) writes a synthetic `SessionEnd` for live sessions matched by branch / cwd / session-id, so a hard kill that skips Claude's own `SessionEnd` hook still clears the agent.

**Tech Stack:** Go (stdlib `database/sql` + `github.com/mattn/go-sqlite3`, CGO required), `html/template`. Tests are standard `go test` with an injected `now time.Time`.

**Spec:** `docs/superpowers/specs/2026-06-01-roster-waiting-retention-design.md`

**Conventions to honor (from CLAUDE.md):**
- Time is injected into tested functions (`now time.Time`), never read from the clock inside them.
- No new persisted payload fields — `end` writes only existing allowlisted columns.
- `ingest` path is untouched; `end` is a separate user-invoked command.
- Keep code `gofmt`-clean; `go vet ./...` must pass. Tests need CGO (a C compiler present).

---

### Task 1: Add the `CLODHOPPER_WAITING_RETAIN_HOURS` config getter

**Files:**
- Modify: `main.go` (const block ~12-16, add getter after `retainDays` ~113, usage ENV block ~94-101)
- Test: `main_test.go`

- [ ] **Step 1: Write the failing test**

Add to `main_test.go`:

```go
func TestWaitingRetainHours_DefaultAndOverride(t *testing.T) {
	if got := waitingRetainHours(); got != 16 {
		t.Errorf("default waitingRetainHours() = %d, want 16", got)
	}
	t.Setenv("CLODHOPPER_WAITING_RETAIN_HOURS", "24")
	if got := waitingRetainHours(); got != 24 {
		t.Errorf("override waitingRetainHours() = %d, want 24", got)
	}
	t.Setenv("CLODHOPPER_WAITING_RETAIN_HOURS", "0") // non-positive is ignored
	if got := waitingRetainHours(); got != 16 {
		t.Errorf("zero override should fall back to 16, got %d", got)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test -run TestWaitingRetainHours_DefaultAndOverride ./...`
Expected: FAIL — `undefined: waitingRetainHours`.

- [ ] **Step 3: Add the const and getter**

In `main.go`, add to the const block (currently `defaultRetainDays = 14`, `fallbackPort = 4555`, `fallbackRefresh = 5`):

```go
	defaultWaitingRetainHours = 16
```

Add this getter immediately after `retainDays()`:

```go
// waitingRetainHours reads CLODHOPPER_WAITING_RETAIN_HOURS or returns the
// default. It bounds how long an agent that is waiting on you (Stop /
// Notification / PermissionRequest) stays on the roster when no SessionEnd ever
// arrives — long enough to survive a lunch or overnight gap, short enough that a
// hard-killed "zombie" session cannot linger indefinitely.
func waitingRetainHours() int {
	if v := os.Getenv("CLODHOPPER_WAITING_RETAIN_HOURS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return defaultWaitingRetainHours
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test -run TestWaitingRetainHours_DefaultAndOverride ./...`
Expected: PASS.

- [ ] **Step 5: Update the usage ENV block**

In `usage()`, add a line after the `CLODHOPPER_REFRESH_SECS` line:

```go
  CLODHOPPER_WAITING_RETAIN_HOURS  hours a waiting agent stays on the roster (default 16)
```

- [ ] **Step 6: Commit**

```bash
gofmt -w main.go main_test.go
git add main.go main_test.go
git commit -m "feat: add CLODHOPPER_WAITING_RETAIN_HOURS config getter"
```

---

### Task 2: Make `agentRoster` status-aware

**Files:**
- Modify: `store.go` (`agentRoster` signature ~264 and keep-loop ~309-327)
- Modify: `server.go` (call site ~305 in `buildDashboardData`)
- Modify: `roster_test.go` (existing call ~72)
- Test: `roster_test.go`

- [ ] **Step 1: Update the existing roster test call and add the new retention test**

In `roster_test.go`, change the existing call in `TestAgentRoster_DerivesStateAndSorts`:

```go
	agents, err := agentRoster(db, 30*time.Minute, 16*time.Hour, now)
```

Then add this new test:

```go
func TestAgentRoster_StatusAwareRetention(t *testing.T) {
	db, err := openDB(filepath.Join(t.TempDir(), "events.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	now := time.Now().UTC()
	at := func(mins int) string { return now.Add(time.Duration(-mins) * time.Minute).Format(time.RFC3339) }
	ins := func(ts, sess, branch, etype, tool, summary string) {
		insertEvent(db, Event{TS: ts, SourceApp: "myapp", Branch: branch, SessionID: sess, EventType: etype, ToolName: tool, Summary: summary, PayloadJSON: "{}"})
	}

	// Waiting 90m ago: past the 30m live window but within the 2h cap -> stays.
	ins(at(90), "s-wait", "b1", "Stop", "", "Stop")
	// Working 45m ago: a silent worker is stale -> drops at the live window.
	ins(at(45), "s-work", "b2", "PreToolUse", "Bash", "Bash: go test")
	// Waiting 200m ago: beyond the 2h cap -> not even fetched -> drops.
	ins(at(200), "s-old", "b3", "Stop", "", "Stop")

	agents, err := agentRoster(db, 30*time.Minute, 2*time.Hour, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(agents) != 1 {
		t.Fatalf("want only the in-cap waiting agent, got %d: %+v", len(agents), agents)
	}
	if agents[0].SessionID != "s-wait" || agents[0].Status != statusWaiting {
		t.Errorf("expected s-wait/waiting to survive, got %+v", agents[0])
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test -run TestAgentRoster_StatusAwareRetention ./...`
Expected: FAIL — `agentRoster` still takes one window arg (too many arguments in call), or wrong count returned.

- [ ] **Step 3: Change the `agentRoster` signature and keep-loop**

In `store.go`, change the signature line:

```go
func agentRoster(db *sql.DB, liveWindow, waitingCap time.Duration, now time.Time) ([]Agent, error) {
```

Change the `since` line just below it to query over the larger (waiting) bound:

```go
	since := now.Add(-waitingCap).UTC().Format(time.RFC3339)
```

Update the doc comment above the function to read:

```go
// agentRoster folds recent events into one row per session, reflecting each
// agent's current state. It applies two windows: a "working" session is live
// only within liveWindow (a silent worker is stale and drops off), while a
// session waiting on you (waiting / needs-you / needs-approval) is retained out
// to waitingCap — so it survives a lunch or overnight gap and only leaves on a
// SessionEnd. now is injected so the result is deterministic under test.
```

In the keep-loop, replace the block that currently reads:

```go
	out := make([]Agent, 0, len(byID))
	for _, s := range byID {
		label, rank, active := deriveStatus(s.a.LastEvent)
		if !active {
			continue
		}
		a := s.a
		a.Status, a.StatusRank = label, rank
```

with:

```go
	out := make([]Agent, 0, len(byID))
	for _, s := range byID {
		label, rank, active := deriveStatus(s.a.LastEvent)
		if !active {
			continue
		}
		idleSecs := idleSeconds(s.lastTS, now)
		// A "working" agent is live only within the short window; once it goes
		// quiet it is stale and drops off. Needs-me statuses persist out to
		// waitingCap (already bounded by the query window above), so an agent
		// genuinely waiting on you is not lost when you step away.
		if label == statusWorking && time.Duration(idleSecs)*time.Second > liveWindow {
			continue
		}
		a := s.a
		a.Status, a.StatusRank = label, rank
```

Then change the two lines further down that recompute idle so they reuse `idleSecs` instead of calling `idleSeconds` again:

```go
		a.IdleSecs = idleSecs
		a.Idle = humanizeSeconds(idleSecs)
		a.IdleSince = now.Unix() - int64(idleSecs)
```

- [ ] **Step 4: Update the server call site**

In `server.go`, in `buildDashboardData`, replace:

```go
	now := time.Now()
	agents, err := agentRoster(db, agentWindow, now)
```

with:

```go
	now := time.Now()
	waitingCap := time.Duration(waitingRetainHours()) * time.Hour
	agents, err := agentRoster(db, agentWindow, waitingCap, now)
```

(Leave the `activeCounts(db, agentWindow, now)` call unchanged — the activity tally keeps the 30-minute window.)

- [ ] **Step 5: Run the roster tests to verify they pass**

Run: `go test -run TestAgentRoster ./...`
Expected: PASS (both `TestAgentRoster_DerivesStateAndSorts` and `TestAgentRoster_StatusAwareRetention`).

- [ ] **Step 6: Commit**

```bash
gofmt -w store.go server.go roster_test.go
git add store.go server.go roster_test.go
git commit -m "feat: status-aware roster retention (keep waiting agents past the live window)"
```

---

### Task 3: Add `endSessions` and the `clodhopper end` subcommand

**Files:**
- Modify: `store.go` (add `EndSelector` type and `endSessions` after `insertEvent` ~118)
- Create: `end.go`
- Modify: `main.go` (dispatch ~47, usage ~84-92)
- Test: `store_test.go`, `main_test.go`

- [ ] **Step 1: Write the failing store test**

Add to `store_test.go`:

```go
func TestEndSessions_ByBranchSkipsAlreadyEnded(t *testing.T) {
	db, err := openDB(filepath.Join(t.TempDir(), "events.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	now := time.Now().UTC()
	at := func(mins int) string { return now.Add(time.Duration(-mins) * time.Minute).Format(time.RFC3339) }
	ins := func(ts, sess, branch, cwd, etype string) {
		insertEvent(db, Event{TS: ts, SourceApp: "myapp", Branch: branch, Cwd: cwd, SessionID: sess, EventType: etype, PayloadJSON: "{}"})
	}
	// Live session on b1 (latest = Stop) -> should be ended.
	ins(at(20), "s1", "b1", "/w/b1", "PreToolUse")
	ins(at(5), "s1", "b1", "/w/b1", "Stop")
	// Live session on b2 -> must be untouched by --branch b1.
	ins(at(4), "s2", "b2", "/w/b2", "Stop")
	// Already-ended session on b1 -> must be skipped (not double-counted).
	ins(at(30), "s3", "b1", "/w/b1", "PreToolUse")
	ins(at(10), "s3", "b1", "/w/b1", "SessionEnd")

	n, err := endSessions(db, EndSelector{Branch: "b1"}, now)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("want 1 session ended (s1; s3 already ended), got %d", n)
	}

	agents, err := agentRoster(db, 30*time.Minute, 16*time.Hour, now)
	if err != nil {
		t.Fatal(err)
	}
	var sawS1, sawS2 bool
	for _, a := range agents {
		switch a.SessionID {
		case "s1":
			sawS1 = true
		case "s2":
			sawS2 = true
		}
	}
	if sawS1 {
		t.Error("s1 should be off the roster after end")
	}
	if !sawS2 {
		t.Error("s2 (different branch) must be untouched")
	}
}

func TestEndSessions_ByCwd(t *testing.T) {
	db, err := openDB(filepath.Join(t.TempDir(), "events.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	now := time.Now().UTC()
	ts := now.Add(-3 * time.Minute).Format(time.RFC3339)
	insertEvent(db, Event{TS: ts, SourceApp: "myapp", Branch: "b1", Cwd: "/w/one", SessionID: "s1", EventType: "Stop", PayloadJSON: "{}"})
	insertEvent(db, Event{TS: ts, SourceApp: "myapp", Branch: "b1", Cwd: "/w/two", SessionID: "s2", EventType: "Stop", PayloadJSON: "{}"})

	n, err := endSessions(db, EndSelector{Cwd: "/w/one"}, now)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("want 1 session ended for cwd /w/one, got %d", n)
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test -run TestEndSessions ./...`
Expected: FAIL — `undefined: endSessions`, `undefined: EndSelector`.

- [ ] **Step 3: Implement `EndSelector` and `endSessions`**

In `store.go`, add immediately after `insertEvent`:

```go
// EndSelector identifies which live sessions clodhopper end should mark ended.
// Each non-empty field narrows the match; the caller must set at least one.
type EndSelector struct {
	SessionID string
	Branch    string
	Cwd       string
}

// endSessions writes a synthetic SessionEnd row for every currently-live session
// (one whose latest event is not already SessionEnd) matching sel, so an agent
// that was hard-killed — and therefore never emitted its own SessionEnd — drops
// off the roster at once instead of lingering until the waiting cap. It returns
// the number of sessions ended. now is injected for deterministic tests.
func endSessions(db *sql.DB, sel EndSelector, now time.Time) (int, error) {
	// Resolve the selector against the latest row of each session, so branch/cwd
	// match the session's current values.
	q := `SELECT session_id, source_app, COALESCE(branch,''), COALESCE(cwd,''), event_type
	      FROM events e
	      WHERE session_id IS NOT NULL AND session_id <> ''
	        AND id = (SELECT MAX(id) FROM events WHERE session_id = e.session_id)`
	var args []any
	if sel.SessionID != "" {
		q += " AND session_id = ?"
		args = append(args, sel.SessionID)
	}
	if sel.Branch != "" {
		q += " AND branch = ?"
		args = append(args, sel.Branch)
	}
	if sel.Cwd != "" {
		q += " AND cwd = ?"
		args = append(args, sel.Cwd)
	}
	rows, err := db.Query(q, args...)
	if err != nil {
		return 0, err
	}
	defer rows.Close()

	type live struct{ sess, app, branch, cwd string }
	var targets []live
	for rows.Next() {
		var l live
		var etype string
		if err := rows.Scan(&l.sess, &l.app, &l.branch, &l.cwd, &etype); err != nil {
			return 0, err
		}
		if etype == "SessionEnd" {
			continue // already ended
		}
		targets = append(targets, l)
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}

	ts := now.UTC().Format(time.RFC3339)
	for _, l := range targets {
		ev := Event{
			TS: ts, SourceApp: l.app, Branch: l.branch, Cwd: l.cwd,
			SessionID: l.sess, EventType: "SessionEnd", Summary: "ended via clodhopper end",
			PayloadJSON: "{}",
		}
		if err := insertEvent(db, ev); err != nil {
			return 0, err
		}
	}
	return len(targets), nil
}
```

- [ ] **Step 4: Run the store test to verify it passes**

Run: `go test -run TestEndSessions ./...`
Expected: PASS.

- [ ] **Step 5: Create the `end` subcommand**

Create `end.go`:

```go
package main

import (
	"flag"
	"fmt"
	"os"
	"time"
)

// runEnd implements `clodhopper end`: it marks the live sessions matching a
// selector as ended by writing a synthetic SessionEnd, so agents that were
// hard-killed (tmux kill-session, kill -9, crash, sleep) drop off the dashboard
// roster immediately instead of lingering until the waiting-retention cap. A
// teardown script knows the branch or worktree path it is tearing down, not
// Claude's session_id, so those are the natural selectors. At least one selector
// is required so the command can never end every session by accident.
func runEnd(args []string) int {
	fs := flag.NewFlagSet("end", flag.ContinueOnError)
	branch := fs.String("branch", "", "end live sessions on this git branch")
	cwd := fs.String("cwd", "", "end live sessions whose latest event has this cwd")
	session := fs.String("session", "", "end this exact session id")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *branch == "" && *cwd == "" && *session == "" {
		fmt.Fprintln(os.Stderr, "clodhopper end: need at least one of --branch, --cwd, --session")
		return 2
	}
	db, err := openDB(defaultDBPath())
	if err != nil {
		fmt.Fprintln(os.Stderr, "clodhopper end:", err)
		return 1
	}
	defer db.Close()

	n, err := endSessions(db, EndSelector{SessionID: *session, Branch: *branch, Cwd: *cwd}, time.Now())
	if err != nil {
		fmt.Fprintln(os.Stderr, "clodhopper end:", err)
		return 1
	}
	fmt.Printf("ended %d live session(s)\n", n)
	return 0
}
```

- [ ] **Step 6: Wire dispatch and usage in `main.go`**

In `run()`'s switch, add after the `prune` case:

```go
	case "end":
		return runEnd(rest)
```

In `usage()`, add after the `prune` line:

```go
  clodhopper end --branch B | --cwd D | --session S   mark matching live sessions ended (for teardown scripts)
```

- [ ] **Step 7: Write the command-level test for the required selector**

Add to `main_test.go`:

```go
func TestRun_EndRequiresSelector(t *testing.T) {
	// No selector must fail fast (exit 2) before touching the database.
	if code := run([]string{"end"}); code != 2 {
		t.Errorf("run(end) with no selector = %d, want 2", code)
	}
}
```

- [ ] **Step 8: Run the new tests to verify they pass**

Run: `go test -run 'TestEndSessions|TestRun_EndRequiresSelector' ./...`
Expected: PASS.

- [ ] **Step 9: Commit**

```bash
gofmt -w store.go end.go main.go store_test.go main_test.go
git add store.go end.go main.go store_test.go main_test.go
git commit -m "feat: add clodhopper end to evict hard-killed sessions from the roster"
```

---

### Task 4: Update the dashboard wording

**Files:**
- Modify: `templates/dashboard.html` (header ~120, empty state ~145)
- Test: `roster_test.go` (existing `TestHandleDashboard_RendersAgentBoard` already asserts the `"Agents (live"` prefix — keep it satisfied)

- [ ] **Step 1: Update the header**

In `templates/dashboard.html`, change:

```html
  <h2>Agents (live, last 30 min)</h2>
```

to:

```html
  <h2>Agents (live + waiting on you)</h2>
```

- [ ] **Step 2: Update the empty state**

Change:

```html
  <div class="empty">No live agents in the last 30 min. (Detecting "waiting for you" needs the <code>Stop</code> and <code>Notification</code> hooks wired in <code>.claude/settings.json</code>.)</div>
```

to:

```html
  <div class="empty">No agents working or waiting on you. (Detecting "waiting for you" needs the <code>Stop</code> and <code>Notification</code> hooks wired in <code>.claude/settings.json</code>; a waiting agent clears when its session ends — on <code>SessionEnd</code>, or via <code>clodhopper end</code> for hard-killed sessions.)</div>
```

- [ ] **Step 3: Run the dashboard render test to verify it still passes**

Run: `go test -run TestHandleDashboard_RendersAgentBoard ./...`
Expected: PASS (the body still contains `"Agents (live"`).

- [ ] **Step 4: Commit**

```bash
git add templates/dashboard.html
git commit -m "docs: reword roster header/empty-state for waiting-retention"
```

---

### Task 5: Update the README

**Files:**
- Modify: `README.md` (Usage command block ~38-42, config table ~55-62, plus a short teardown subsection)

- [ ] **Step 1: Add `end` to the Usage block**

In `README.md`, in the `## Usage` fenced block, after the `prune` line, add:

```text
clodhopper end --branch B                # mark matching live sessions ended (teardown)
```

- [ ] **Step 2: Add a teardown subsection**

After the paragraph that explains `ingest` never breaking a tool call (ends "...only when `CLODHOPPER_DEBUG` is set."), add:

```markdown
### Tearing down agents from scripts

The roster treats a session as gone when it sees a `SessionEnd` event. A hard
kill — `tmux kill-session`, `kill -9`, a crash, or laptop sleep — gives Claude
Code no chance to emit `SessionEnd`, so the agent would otherwise linger as a
"waiting for you" zombie until the retention cap expires. If a script tears down
sessions that way, have it tell clodhopper first:

```bash
clodhopper end --branch "$branch"
```

This writes a synthetic `SessionEnd` for every live session on that branch. The
script never needs Claude's `session_id` — clodhopper resolves the branch (or
`--cwd`, or an exact `--session`) to the live sessions itself. Guard the call so
it no-ops where the binary is absent:

```bash
command -v clodhopper >/dev/null && clodhopper end --branch "$branch"
```

Switching the kill to a gentler signal is not a reliable alternative — Claude
Code only sometimes fires `SessionEnd` on `SIGTERM`/`SIGINT`, and a closing
pane's `SIGHUP` usually kills it before cleanup runs.
```

- [ ] **Step 3: Add the config-table row**

In the `## Configuration (environment)` table, add a row after `CLODHOPPER_HOST`:

```markdown
| `CLODHOPPER_WAITING_RETAIN_HOURS` | `16` | How long an agent that is waiting on you stays on the roster when no `SessionEnd` arrives. Working agents still age out after ~30 min. |
```

- [ ] **Step 4: Commit**

```bash
git add README.md
git commit -m "docs: document clodhopper end and CLODHOPPER_WAITING_RETAIN_HOURS"
```

---

### Task 6: Full build, vet, and test sweep

**Files:** none (verification only)

- [ ] **Step 1: Format check**

Run: `gofmt -l .`
Expected: no output (all files formatted).

- [ ] **Step 2: Vet**

Run: `go vet ./...`
Expected: no output, exit 0.

- [ ] **Step 3: Build**

Run: `go build ./...`
Expected: success, no output.

- [ ] **Step 4: Full test suite**

Run: `go test ./...`
Expected: `ok` for the package (CGO must be enabled — a C compiler present).

- [ ] **Step 5: Manual smoke (optional but recommended)**

```bash
CLODHOPPER_DB=$(mktemp -u) sh -c '
  clodhopper end --branch nope; echo "exit=$?"   # ended 0 live session(s)
  clodhopper end;               echo "exit=$?"   # error: need a selector; exit 2
'
```

Expected: first prints `ended 0 live session(s)` exit 0; second prints the selector error, exit 2.

- [ ] **Step 6: Commit any formatting fixes**

```bash
git add -A
git commit -m "chore: gofmt/vet sweep for waiting-retention" --allow-empty
```

---

## Self-Review

**Spec coverage:**
- Status-aware retention (`agentRoster`) → Task 2. ✓
- Zombie-eviction cap config (`CLODHOPPER_WAITING_RETAIN_HOURS`, default 16) → Task 1 + wired in Task 2 Step 4. ✓
- `clodhopper end` with `--branch | --cwd | --session`, ≥1 required, synthetic `SessionEnd` via `insertEvent` → Task 3. ✓
- Presentation (header + empty state) → Task 4. ✓
- README (usage, teardown subsection, config row) → Task 5. ✓
- Tests for: waiting past live window stays, working past live window drops, waiting past cap drops, SessionEnd drops (covered by existing `TestAgentRoster_DerivesStateAndSorts` + new `TestAgentRoster_StatusAwareRetention`), and `end` by branch/cwd skipping already-ended → Tasks 2-3. ✓
- Invariants: no new payload fields (end writes existing columns), ingest untouched, time injected → honored across Tasks 2-3. ✓

**Placeholder scan:** none — every code/step is concrete.

**Type consistency:** `agentRoster(db, liveWindow, waitingCap, now)` used identically in `store.go`, `server.go`, and all three roster tests. `EndSelector{SessionID, Branch, Cwd}` and `endSessions(db, sel, now) (int, error)` used identically in `store.go`, `end.go`, and `store_test.go`. `waitingRetainHours() int` defined in `main.go`, called in `server.go`.
