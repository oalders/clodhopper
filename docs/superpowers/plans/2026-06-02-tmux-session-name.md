# tmux Session Name Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Disambiguate look-alike branch names (`fix-1710` vs `fix-1711`) on the dashboard by capturing each session's tmux session name at ingest time and surfacing it in the roster and activity table.

**Architecture:** Capture the tmux session name per event in `ingest` (best-effort, exactly like `gitBranch`), store it in a new `tmux_session` column, and thread it through the two derived views (`agentRoster`, `activeCounts`), the view-change fingerprint (`viewSignature`), and the dashboard template. No view-time querying or caching — the name rides with each row.

**Tech Stack:** Go, SQLite (`github.com/mattn/go-sqlite3`, CGO), `html/template`. Tests run with `go test ./...` (CGO required).

**Spec:** `docs/superpowers/specs/2026-06-02-tmux-session-name-design.md`

---

## File Structure

| File | Change | Responsibility |
| --- | --- | --- |
| `store.go` | Modify | `Event`/`Agent`/`SourceCount` structs, base `schema`, `migrations`, `insertEvent`, `agentRoster`, `activeCounts` |
| `ingest.go` | Modify | new `tmuxSession()` capture helper; `buildEvent` wiring |
| `server.go` | Modify | `viewSignature` (roster + activity fingerprint loops, activity sort) |
| `templates/dashboard.html` | Modify | roster first column (name + dimmed branch), chip header rename, activity table column, colgroups, CSS |
| `tmux_test.go` | Create | all tmux-capture/storage/roster/activity tests (mirrors `branch_test.go`) |
| `server_test.go` | Modify | signature + render tests |

Column ordering convention: the new `tmux_session` column goes **immediately after `cwd`** everywhere (struct fields, schema, INSERT, roster SELECT/scan) so the lists stay easy to eyeball.

---

## Task 1: Storage — `tmux_session` column

**Files:**
- Modify: `store.go` (`Event` struct ~L17-28, `schema` ~L30-45, `migrations` ~L50-52, `insertEvent` ~L111-118)
- Create: `tmux_test.go`

- [ ] **Step 1: Write the failing test**

Create `tmux_test.go`:

```go
package main

import (
	"path/filepath"
	"testing"
	"time"
)

// A captured tmux session name survives a write/read round-trip.
func TestTmuxSessionPersists(t *testing.T) {
	db, err := openDB(filepath.Join(t.TempDir(), "events.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	now := time.Now().UTC().Format(time.RFC3339)
	insertEvent(db, Event{TS: now, SourceApp: "myapp", TmuxSession: "roster-colors", EventType: "PreToolUse", PayloadJSON: "{}"})

	var got string
	if err := db.QueryRow(`SELECT tmux_session FROM events LIMIT 1`).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != "roster-colors" {
		t.Errorf("tmux_session = %q, want roster-colors", got)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test -run TestTmuxSessionPersists ./...`
Expected: FAIL — compile error `unknown field TmuxSession in struct literal` (the field and column do not exist yet).

- [ ] **Step 3: Add the `TmuxSession` field to `Event`**

In `store.go`, in the `Event` struct, add the field right after `Cwd`:

```go
	Cwd         string
	TmuxSession string // tmux session name at capture time, "" if not in tmux
	SessionID   string
```

- [ ] **Step 4: Add the column to the base schema**

In `store.go`, in the `schema` const, add `tmux_session` right after the `cwd` line (matching how `branch` lives in the base schema):

```sql
  cwd          TEXT,
  tmux_session TEXT,
  session_id   TEXT,
```

- [ ] **Step 5: Add the migration for existing databases**

In `store.go`, append to the `migrations` slice (best-effort `ALTER`; a duplicate-column error on a fresh DB that already has it from `schema` is ignored, exactly as with `branch`):

```go
var migrations = []string{
	`ALTER TABLE events ADD COLUMN branch TEXT`,
	`ALTER TABLE events ADD COLUMN tmux_session TEXT`,
}
```

- [ ] **Step 6: Write the column in `insertEvent`**

In `store.go`, update `insertEvent` — keep the column list, the `?` placeholders, and the args in lock-step:

```go
func insertEvent(db *sql.DB, ev Event) error {
	_, err := db.Exec(
		`INSERT INTO events (ts, source_app, branch, cwd, tmux_session, session_id, event_type, tool_name, summary, payload_json)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		ev.TS, ev.SourceApp, ev.Branch, ev.Cwd, ev.TmuxSession, ev.SessionID, ev.EventType, ev.ToolName, ev.Summary, ev.PayloadJSON,
	)
	return err
}
```

- [ ] **Step 7: Run the test to verify it passes**

Run: `go test -run TestTmuxSessionPersists ./...`
Expected: PASS.

- [ ] **Step 8: Run the full suite (nothing else regressed)**

Run: `go test ./...`
Expected: PASS (existing `TestBranchRoundTripAndFilter` etc. still green — the new column defaults to `""`).

- [ ] **Step 9: Commit**

```bash
git add store.go tmux_test.go
git commit -m "$(printf 'feat(store): add tmux_session column\n\nCo-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>')"
```

---

## Task 2: Capture — `tmuxSession()` + `buildEvent` wiring

**Files:**
- Modify: `ingest.go` (`buildEvent` ~L59-79; new helper near `gitBranch` ~L121-138)
- Modify: `tmux_test.go`

- [ ] **Step 1: Write the failing tests**

In `tmux_test.go`, first widen the import block to:

```go
import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)
```

Then append these tests:

```go
// Outside tmux ($TMUX unset), capture is empty and never errors.
func TestTmuxSession_NotInTmux(t *testing.T) {
	t.Setenv("TMUX", "")
	if got := tmuxSession(); got != "" {
		t.Errorf("tmuxSession() outside tmux = %q, want \"\"", got)
	}
}

// Inside a real tmux session, capture matches `tmux display-message -p '#S'`.
// Skipped unless the suite itself runs inside tmux with the binary present —
// tmuxSession() reads ambient $TMUX, so we cannot fake it hermetically (unlike
// gitBranch, which takes a dir argument).
func TestTmuxSession_InTmux(t *testing.T) {
	if os.Getenv("TMUX") == "" {
		t.Skip("not running inside tmux")
	}
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not available")
	}
	out, err := exec.Command("tmux", "display-message", "-p", "#S").Output()
	if err != nil {
		t.Skipf("tmux display-message failed: %v", err)
	}
	want := strings.TrimSpace(string(out))
	if got := tmuxSession(); got != want {
		t.Errorf("tmuxSession() = %q, want %q", got, want)
	}
}

// buildEvent wires the captured name onto the Event. With $TMUX forced empty the
// value is deterministically "".
func TestBuildEvent_PopulatesTmuxSession(t *testing.T) {
	t.Setenv("TMUX", "")
	raw := []byte(`{"hook_event_name":"PreToolUse","tool_name":"Bash","tool_input":{"command":"echo hi"}}`)
	ev := buildEvent(raw, "myapp")
	if ev.TmuxSession != "" {
		t.Errorf("tmux session with TMUX unset = %q, want \"\"", ev.TmuxSession)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test -run 'TestTmuxSession_|TestBuildEvent_PopulatesTmuxSession' ./...`
Expected: FAIL — compile error `undefined: tmuxSession`.

- [ ] **Step 3: Add the `tmuxSession()` helper**

In `ingest.go`, add below `gitBranch` (the `os`, `os/exec`, `context`, `strings`, `time` imports are already present):

```go
// tmuxSession returns the name of the tmux session the current process is in, or
// "" when not inside tmux, on any error, or if it times out. Like gitBranch it is
// deliberately best-effort: capture must never block or fail a tool call. The
// $TMUX guard avoids spawning tmux (and its stderr noise) outside a session;
// `display-message -p '#S'` resolves the current pane's session via $TMUX, so no
// `-t` target is needed. The name is user-chosen free text, so it is scrubbed and
// truncated to honour the scrub layer's fail-closed bias.
func tmuxSession() string {
	if os.Getenv("TMUX") == "" {
		return ""
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

- [ ] **Step 4: Wire it into `buildEvent`**

In `ingest.go`, in the `Event{...}` literal inside `buildEvent`, add the field right after `Cwd`:

```go
	ev := Event{
		TS:          time.Now().UTC().Format(time.RFC3339),
		SourceApp:   sourceApp,
		Branch:      gitBranch(cwd),
		Cwd:         cwd,
		TmuxSession: tmuxSession(),
		SessionID:   str(p, "session_id"),
		EventType:   str(p, "hook_event_name"),
		ToolName:    str(p, "tool_name"),
		PayloadJSON: scrubPayload(raw),
	}
```

- [ ] **Step 5: Run the tests to verify they pass**

Run: `go test -run 'TestTmuxSession_|TestBuildEvent_PopulatesTmuxSession' ./...`
Expected: PASS (`TestTmuxSession_InTmux` PASSes inside tmux, otherwise SKIPs).

- [ ] **Step 6: Commit**

```bash
git add ingest.go tmux_test.go
git commit -m "$(printf 'feat(ingest): capture tmux session name (best-effort)\n\nCo-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>')"
```

---

## Task 3: Roster — carry the name onto each `Agent`

**Files:**
- Modify: `store.go` (`Agent` struct ~L289-302, `agentRoster` SELECT ~L364-367, scan ~L380-390)
- Modify: `tmux_test.go`

- [ ] **Step 1: Write the failing test**

In `tmux_test.go`, append (imports already cover `time`/`testing`/`filepath`):

```go
// The roster folds in the latest tmux session name per session (last write wins),
// just like branch/cwd.
func TestAgentRoster_CarriesTmuxSession(t *testing.T) {
	db, err := openDB(filepath.Join(t.TempDir(), "events.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	now := time.Now().UTC()
	at := func(mins int) string { return now.Add(time.Duration(-mins) * time.Minute).Format(time.RFC3339) }

	// Same session, two events; the later one renamed the tmux session.
	insertEvent(db, Event{TS: at(5), SourceApp: "myapp", Branch: "fix-1710", TmuxSession: "old-name", SessionID: "s1", EventType: "PreToolUse", ToolName: "Bash", PayloadJSON: "{}"})
	insertEvent(db, Event{TS: at(1), SourceApp: "myapp", Branch: "fix-1710", TmuxSession: "roster-colors", SessionID: "s1", EventType: "Stop", Summary: "Stop", PayloadJSON: "{}"})

	agents, err := agentRoster(db, 30*time.Minute, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(agents) != 1 {
		t.Fatalf("want 1 agent, got %d", len(agents))
	}
	if agents[0].TmuxSession != "roster-colors" {
		t.Errorf("TmuxSession = %q, want roster-colors (last write wins)", agents[0].TmuxSession)
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test -run TestAgentRoster_CarriesTmuxSession ./...`
Expected: FAIL — compile error `agents[0].TmuxSession undefined (type Agent has no field TmuxSession)`.

- [ ] **Step 3: Add `TmuxSession` to the `Agent` struct**

In `store.go`, in the `Agent` struct, add right after `Cwd`:

```go
	Cwd        string
	TmuxSession string // tmux session name, the disambiguating label
	Status     string // human label (see status* constants)
```

- [ ] **Step 4: Select and scan the column in `agentRoster`**

In `store.go`, update the query in `agentRoster` to add `COALESCE(tmux_session,'')` after `cwd`:

```go
	rows, err := db.Query(
		`SELECT ts, source_app, COALESCE(branch,''), COALESCE(cwd,''), COALESCE(tmux_session,''), COALESCE(session_id,''),
		        event_type, COALESCE(tool_name,''), COALESCE(summary,'')
		 FROM events WHERE ts >= ? AND session_id IS NOT NULL AND session_id <> ''
		 ORDER BY id ASC`, since)
```

Then in the scan loop, add a `tmuxSess` variable and assign it last-write-wins:

```go
		var ts, app, branch, cwd, tmuxSess, sess, etype, tool, summary string
		if err := rows.Scan(&ts, &app, &branch, &cwd, &tmuxSess, &sess, &etype, &tool, &summary); err != nil {
			return nil, err
		}
		s := byID[sess]
		if s == nil {
			s = &state{a: Agent{SessionID: sess}}
			byID[sess] = s
		}
		// Ascending scan: the last write wins, so these hold the latest values.
		s.a.SourceApp, s.a.Branch, s.a.Cwd, s.a.TmuxSession = app, branch, cwd, tmuxSess
```

- [ ] **Step 5: Run the test to verify it passes**

Run: `go test -run TestAgentRoster_CarriesTmuxSession ./...`
Expected: PASS.

- [ ] **Step 6: Run the full suite**

Run: `go test ./...`
Expected: PASS (existing roster tests unaffected — column defaults to `""`).

- [ ] **Step 7: Commit**

```bash
git add store.go tmux_test.go
git commit -m "$(printf 'feat(roster): carry tmux session name per agent\n\nCo-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>')"
```

---

## Task 4: Activity — group counts by tmux session

**Files:**
- Modify: `store.go` (`SourceCount` struct ~L463-467, `activeCounts` ~L474-494)
- Modify: `tmux_test.go`

- [ ] **Step 1: Write the failing test**

In `tmux_test.go`, append:

```go
// activeCounts groups by tmux session, so two sessions on the same branch are
// two rows, each carrying its own name.
func TestActiveCounts_GroupsByTmuxSession(t *testing.T) {
	db, err := openDB(filepath.Join(t.TempDir(), "events.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	now := time.Now().UTC().Format(time.RFC3339)
	insertEvent(db, Event{TS: now, SourceApp: "myapp", Branch: "fix-1710", TmuxSession: "alpha", EventType: "PreToolUse", PayloadJSON: "{}"})
	insertEvent(db, Event{TS: now, SourceApp: "myapp", Branch: "fix-1710", TmuxSession: "beta", EventType: "PreToolUse", PayloadJSON: "{}"})

	counts, err := activeCounts(db, time.Hour, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(counts) != 2 {
		t.Fatalf("want 2 groups (one per tmux session), got %d: %+v", len(counts), counts)
	}
	names := map[string]bool{}
	for _, c := range counts {
		names[c.TmuxSession] = true
	}
	if !names["alpha"] || !names["beta"] {
		t.Errorf("missing a tmux-session group: %+v", counts)
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test -run TestActiveCounts_GroupsByTmuxSession ./...`
Expected: FAIL — compile error `c.TmuxSession undefined (type SourceCount has no field TmuxSession)`.

- [ ] **Step 3: Add `TmuxSession` to `SourceCount`**

In `store.go`:

```go
// SourceCount is a per-(tmux_session, source_app, branch) activity tally for a
// recent window.
type SourceCount struct {
	TmuxSession string
	SourceApp   string
	Branch      string
	Count       int
}
```

- [ ] **Step 4: Group and scan the new dimension in `activeCounts`**

In `store.go`, update the query and scan (tmux_session leads the SELECT and the GROUP BY):

```go
	rows, err := db.Query(
		`SELECT COALESCE(tmux_session,''), source_app, COALESCE(branch, ''), COUNT(*) FROM events WHERE ts >= ?
		 GROUP BY tmux_session, source_app, branch ORDER BY COUNT(*) DESC`,
		since,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SourceCount
	for rows.Next() {
		var sc SourceCount
		if err := rows.Scan(&sc.TmuxSession, &sc.SourceApp, &sc.Branch, &sc.Count); err != nil {
			return nil, err
		}
		out = append(out, sc)
	}
```

- [ ] **Step 5: Run the test to verify it passes**

Run: `go test -run TestActiveCounts_GroupsByTmuxSession ./...`
Expected: PASS.

- [ ] **Step 6: Run the full suite**

Run: `go test ./...`
Expected: PASS (`TestBranchRoundTripAndFilter` still sees 2 groups — those events share an empty tmux session, so the extra GROUP BY key does not split them).

- [ ] **Step 7: Commit**

```bash
git add store.go tmux_test.go
git commit -m "$(printf 'feat(activity): group counts by tmux session\n\nCo-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>')"
```

---

## Task 5: Signature — fingerprint the new column in both views

**Files:**
- Modify: `server.go` (`viewSignature` ~L379-407)
- Modify: `server_test.go`

- [ ] **Step 1: Write the failing test**

In `server_test.go`, add:

```go
func TestViewSignature_TracksTmuxSession(t *testing.T) {
	base := dashboardData{
		Agents:   []Agent{{SessionID: "s1", TmuxSession: "alpha", Status: statusWorking}},
		Activity: []SourceCount{{SourceApp: "myapp", TmuxSession: "alpha", Count: 2}},
	}

	// Roster row differs only by tmux session name.
	rosterDiff := base
	rosterDiff.Agents = []Agent{{SessionID: "s1", TmuxSession: "beta", Status: statusWorking}}
	if viewSignature(base) == viewSignature(rosterDiff) {
		t.Error("signature unchanged after a roster tmux-session change")
	}

	// Activity row differs only by tmux session name.
	activityDiff := base
	activityDiff.Activity = []SourceCount{{SourceApp: "myapp", TmuxSession: "beta", Count: 2}}
	if viewSignature(base) == viewSignature(activityDiff) {
		t.Error("signature unchanged after an activity tmux-session change")
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test -run TestViewSignature_TracksTmuxSession ./...`
Expected: FAIL — both differences currently hash identically ("signature unchanged ..." errors).

- [ ] **Step 3: Add the name to the roster fingerprint loop**

In `server.go`, update the agent line (server.go:392):

```go
	for _, a := range agents {
		fmt.Fprintf(h, "|a:%s:%s:%s:%s:%s:%s:%s", a.SessionID, a.TmuxSession, a.SourceApp, a.Branch, a.Status, a.Doing, a.CI)
	}
```

- [ ] **Step 4: Add the name to the activity sort and fingerprint loop**

In `server.go`, extend the activity sort with a tmux-session tie-break (so rows that differ only by session order deterministically) and add it to the count line:

```go
	activity := append([]SourceCount(nil), d.Activity...)
	sort.Slice(activity, func(i, j int) bool {
		if activity[i].SourceApp != activity[j].SourceApp {
			return activity[i].SourceApp < activity[j].SourceApp
		}
		if activity[i].Branch != activity[j].Branch {
			return activity[i].Branch < activity[j].Branch
		}
		return activity[i].TmuxSession < activity[j].TmuxSession
	})
	for _, c := range activity {
		fmt.Fprintf(h, "|c:%s:%s:%s:%d", c.TmuxSession, c.SourceApp, c.Branch, c.Count)
	}
```

- [ ] **Step 5: Run the test to verify it passes**

Run: `go test -run TestViewSignature_TracksTmuxSession ./...`
Expected: PASS.

- [ ] **Step 6: Run the full suite**

Run: `go test ./...`
Expected: PASS (`TestViewSignature_IgnoresIdleButTracksEvents` still green — its data has empty tmux sessions, so the formula change is consistent on both sides of each comparison).

- [ ] **Step 7: Commit**

```bash
git add server.go server_test.go
git commit -m "$(printf 'feat(server): fingerprint tmux session in view signature\n\nCo-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>')"
```

---

## Task 6: Dashboard — render the name, rename the chip column

**Files:**
- Modify: `templates/dashboard.html` (CSS ~L25 + ~L38-40, roster table ~L130-151, activity table ~L156-175)
- Modify: `server_test.go`

- [ ] **Step 1: Write the failing test**

In `server_test.go`, add a render test (uses `openDB`, `insertEvent`, `getBody`, all already in this file):

```go
func TestHandleDashboard_RendersTmuxSession(t *testing.T) {
	db, _ := openDB(filepath.Join(t.TempDir(), "events.db"))
	defer db.Close()
	now := time.Now().UTC().Format(time.RFC3339)
	insertEvent(db, Event{TS: now, SourceApp: "myapp", Branch: "fix-1710", TmuxSession: "roster-colors",
		SessionID: "sess-a", EventType: "Stop", Summary: "Stop", PayloadJSON: "{}"})

	body := getBody(t, db, "/")
	for _, want := range []string{
		"roster-colors",          // the disambiguating name appears
		"fix-1710",               // branch still shown (dimmed sub-label)
		"<th>session name</th>",  // activity table's new first column
		"<th>id</th>",            // the renamed session-id chip column
	} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q in dashboard:\n%s", want, body)
		}
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test -run TestHandleDashboard_RendersTmuxSession ./...`
Expected: FAIL — `roster-colors` is not rendered and the new headers do not exist.

- [ ] **Step 3: Add the stacked-cell CSS**

In `templates/dashboard.html`, add `td.namecell` to the existing nowrap rule (the line currently reads `td.time, td.branch, td.tool, td.idle, td.sess { white-space: nowrap; }`):

```css
    td.time, td.branch, td.tool, td.idle, td.sess, td.namecell { white-space: nowrap; }
```

Then add two rules just below the `.chip` rule (near the end of the `<style>` block):

```css
    /* Roster first column: tmux session name on top, branch dimmed beneath it. */
    td.namecell .name, td.namecell .sub { display: block; }
    td.namecell .sub { color: #888; font-size: .85em; }
```

- [ ] **Step 4: Update the roster table (colgroup, headers, first cell)**

In `templates/dashboard.html`, widen the first roster column to `20ch` and rename the headers — `branch` → `session`, trailing `session` → `id`:

```html
  <table>
    <colgroup>
      <col style="width: 20ch"><col style="width: 8ch"><col style="width: 17ch">
      <col style="width: 20ch"><col style="width: 9ch"><col style="width: 6ch"><col style="width: 10ch">
    </colgroup>
    <thead>
      <tr><th>session</th><th>app</th><th>status</th><th>doing</th><th>CI</th><th>idle</th><th>id</th></tr>
    </thead>
```

Then replace the first `<td>` of the roster row (the `td.branch` cell) with the stacked name/branch cell:

```html
      <tr {{ if le .StatusRank 1 }}class="alert"{{ end }}>
        <td class="namecell">{{ if .TmuxSession }}<span class="name">{{ .TmuxSession }}</span>{{ if .Branch }}<span class="sub">{{ .Branch }}</span>{{ end }}{{ else if .Branch }}<span class="name">{{ .Branch }}</span>{{ else }}—{{ end }}</td>
```

(Leave the rest of the row — app, status, doing, CI, idle, and the `td.sess` chip cell — unchanged.)

- [ ] **Step 5: Update the activity table (colgroup, headers, cells)**

In `templates/dashboard.html`, give the activity table a four-column colgroup, add the `session name` header first, and render the tmux name in a new leading cell:

```html
  <table class="counts">
    <colgroup>
      <col style="width: 24ch"><col style="width: 16ch"><col style="width: 12ch"><col style="width: 9ch">
    </colgroup>
    <thead>
      <tr><th>session name</th><th>branch</th><th>app</th><th>events</th></tr>
    </thead>
    <tbody>
      {{ range .Activity }}
      <tr>
        <td class="branch">{{ if .TmuxSession }}{{ .TmuxSession }}{{ else }}—{{ end }}</td>
        <td class="branch">{{ if .Branch }}{{ .Branch }}{{ else }}—{{ end }}</td>
        <td>{{ .SourceApp }}</td>
        <td>{{ .Count }}</td>
      </tr>
      {{ end }}
    </tbody>
  </table>
```

- [ ] **Step 6: Run the render test to verify it passes**

Run: `go test -run TestHandleDashboard_RendersTmuxSession ./...`
Expected: PASS.

- [ ] **Step 7: Run the full suite**

Run: `go test ./...`
Expected: PASS. Note `TestHandleDashboard_SessionColors` still passes: it asserts `<th>session</th>` exists, which is now the roster's first-column header (still present), and the colored chip cell (`td.sess`) is unchanged.

- [ ] **Step 8: Vet and build**

Run: `go vet ./... && go build ./...`
Expected: clean (no output), build succeeds.

- [ ] **Step 9: Commit**

```bash
git add templates/dashboard.html server_test.go
git commit -m "$(printf 'feat(dashboard): show tmux session name in roster and activity\n\nCo-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>')"
```

---

## Manual verification (after Task 6)

In a tmux session, with hooks wired to `clodhopper ingest`, trigger a couple of tool uses across two worktrees on look-alike branches, then open the dashboard:

- The roster's first column shows each session's tmux name with the branch dimmed beneath it.
- The Activity table leads with **session name**, then branch, app, events.
- The session-id colour chip is unchanged under the **id** header.
- A session with no tmux context (run outside tmux) falls back to the branch alone, `—` if neither is known.

---

## Self-Review Notes (author)

- **Spec coverage:** capture (Task 2), storage/migration (Task 1), roster (Task 3), activity grouping + column (Tasks 4, 6), `viewSignature` both loops + sort (Task 5), header rename + dimmed-branch cell + colgroup/CSS (Task 6), tests incl. the `t.Skip` tmux path (Task 2). Invariants: `tmuxSession()` returns `""` on every error path with a 2s timeout (ingest never fails); the name is a side-effect capture scrubbed + truncated, no allowlist change (no chat content). All spec sections map to a task.
- **No placeholders:** every code step shows complete code; every run step shows an exact command and expected result.
- **Type consistency:** field is `TmuxSession string` on `Event`, `Agent`, and `SourceCount`; column is `tmux_session`; helper is `tmuxSession()` throughout; signature line uses `a.TmuxSession` / `c.TmuxSession`.
