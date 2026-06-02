package main

import (
	"database/sql"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

// Event is one captured Claude Code lifecycle event, after scrubbing.
type Event struct {
	ID          int64
	TS          string // RFC3339 UTC, ingest time
	SourceApp   string
	Branch      string // git branch of Cwd at capture time, "" if unknown
	Cwd         string
	TmuxSession string // tmux session name at capture time, "" if not in tmux
	SessionID   string
	EventType   string
	ToolName    string
	Summary     string
	ToolUseID   string        // hook tool_use_id; pairs a Pre with its Post/Failure
	DurationMs  sql.NullInt64 // tool-call duration (Post* only); NULL otherwise
	PayloadJSON string
}

const schema = `
CREATE TABLE IF NOT EXISTS events (
  id           INTEGER PRIMARY KEY AUTOINCREMENT,
  ts           TEXT NOT NULL,
  source_app   TEXT NOT NULL,
  branch       TEXT,
  cwd          TEXT,
  tmux_session TEXT,
  session_id   TEXT,
  event_type   TEXT NOT NULL,
  tool_name    TEXT,
  summary      TEXT,
  tool_use_id  TEXT,
  duration_ms  INTEGER,
  payload_json TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_events_source_ts ON events(source_app, ts);
CREATE INDEX IF NOT EXISTS idx_events_ts ON events(ts);
`

// migrations bring an already-created database up to the current schema. Each
// runs best-effort: a "duplicate column" error just means it was applied on an
// earlier run, so we ignore errors rather than fail capture.
var migrations = []string{
	`ALTER TABLE events ADD COLUMN branch TEXT`,
	`ALTER TABLE events ADD COLUMN tmux_session TEXT`,
	`ALTER TABLE events ADD COLUMN tool_use_id TEXT`,
	`ALTER TABLE events ADD COLUMN duration_ms INTEGER`,
}

// defaultDBPath returns CLODHOPPER_DB if set, else ~/.claude/clodhopper/var/events.db.
func defaultDBPath() string {
	if p := os.Getenv("CLODHOPPER_DB"); p != "" {
		return p
	}
	home, err := os.UserHomeDir()
	if err != nil {
		// Fall back to a relative path rather than failing capture.
		return filepath.Join("var", "events.db")
	}
	return filepath.Join(home, ".claude", "clodhopper", "var", "events.db")
}

// openDB opens (creating parent dirs and schema as needed) the SQLite database
// in WAL mode with a busy timeout so concurrent ingest writers do not collide.
func openDB(path string) (*sql.DB, error) {
	if dir := filepath.Dir(path); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("create db dir: %w", err)
		}
	}
	dsn := path + "?_journal_mode=WAL&_busy_timeout=5000&_synchronous=NORMAL"
	db, err := sql.Open("sqlite3", dsn)
	if err != nil {
		return nil, fmt.Errorf("open db: %w", err)
	}
	// On a brand-new file, several processes opening at once race to initialise
	// the WAL: the journal-mode switch needs a brief exclusive lock that
	// _busy_timeout does not reliably cover, so the schema DDL can come back
	// "database is locked". Retry it (bounded to the same ~5s window as the busy
	// timeout) rather than failing — a lost openDB means a silently dropped event.
	if err := retryOnLock(func() error { _, e := db.Exec(schema); return e }); err != nil {
		db.Close()
		return nil, fmt.Errorf("apply schema: %w", err)
	}
	for _, m := range migrations {
		_, _ = db.Exec(m) // best-effort; already-applied migrations error harmlessly
	}
	return db, nil
}

// retryOnLock re-runs fn while SQLite reports the database (or a table) is
// locked, backing off briefly between attempts up to roughly the busy-timeout
// window. Any other error — or success — returns immediately.
func retryOnLock(fn func() error) error {
	const attempts = 50
	var err error
	for range attempts {
		err = fn()
		if err == nil || !strings.Contains(err.Error(), "is locked") {
			return err
		}
		time.Sleep(100 * time.Millisecond)
	}
	return err
}

func insertEvent(db *sql.DB, ev Event) error {
	_, err := db.Exec(
		`INSERT INTO events (ts, source_app, branch, cwd, tmux_session, session_id, event_type, tool_name, summary, tool_use_id, duration_ms, payload_json)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		ev.TS, ev.SourceApp, ev.Branch, ev.Cwd, ev.TmuxSession, ev.SessionID, ev.EventType, ev.ToolName, ev.Summary, ev.ToolUseID, ev.DurationMs, ev.PayloadJSON,
	)
	return err
}

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

// pruneOld deletes events older than the given number of days and returns the
// number of rows removed.
func pruneOld(db *sql.DB, days int) (int64, error) {
	if days <= 0 {
		return 0, nil
	}
	cutoff := time.Now().UTC().AddDate(0, 0, -days).Format(time.RFC3339)
	res, err := db.Exec(`DELETE FROM events WHERE ts < ?`, cutoff)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// maybePrune runs pruneOld on roughly 1% of calls so the database stays bounded
// without a background daemon. Errors are returned but callers may ignore them.
func maybePrune(db *sql.DB, days int) {
	if rand.Intn(100) == 0 {
		_, _ = pruneOld(db, days)
	}
}

// EventFilter narrows a dashboard query. Empty fields match everything.
type EventFilter struct {
	SourceApp string
	Branch    string
	EventType string
	Limit     int
}

func queryEvents(db *sql.DB, f EventFilter) ([]Event, error) {
	q := `SELECT id, ts, source_app, branch, cwd, session_id, event_type, tool_name, summary, COALESCE(tool_use_id,''), duration_ms, payload_json
	      FROM events WHERE 1=1`
	var args []any
	if f.SourceApp != "" {
		q += " AND source_app = ?"
		args = append(args, f.SourceApp)
	}
	if f.Branch != "" {
		q += " AND branch = ?"
		args = append(args, f.Branch)
	}
	if f.EventType != "" {
		q += " AND event_type = ?"
		args = append(args, f.EventType)
	}
	q += " ORDER BY id DESC"
	if f.Limit > 0 {
		q += " LIMIT ?"
		args = append(args, f.Limit)
	}
	rows, err := db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Event
	for rows.Next() {
		var e Event
		var branch, cwd, sess, tool, summary sql.NullString
		if err := rows.Scan(&e.ID, &e.TS, &e.SourceApp, &branch, &cwd, &sess, &e.EventType, &tool, &summary, &e.ToolUseID, &e.DurationMs, &e.PayloadJSON); err != nil {
			return nil, err
		}
		e.Branch, e.Cwd, e.SessionID, e.ToolName, e.Summary = branch.String, cwd.String, sess.String, tool.String, summary.String
		out = append(out, e)
	}
	return out, rows.Err()
}

// distinctColumn returns the set of non-empty values for a known column, used to
// build the filter dropdowns. The column name is from a fixed internal allowlist,
// never user input, so interpolating it into the query is safe.
func distinctColumn(db *sql.DB, col string) ([]string, error) {
	rows, err := db.Query(`SELECT DISTINCT ` + col + ` FROM events WHERE ` + col + ` <> '' AND ` + col + ` IS NOT NULL ORDER BY ` + col)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// distinctSourceApps returns the set of source_app values, for the filter dropdown.
func distinctSourceApps(db *sql.DB) ([]string, error) { return distinctColumn(db, "source_app") }

// distinctBranches returns the set of branch values, for the filter dropdown.
func distinctBranches(db *sql.DB) ([]string, error) { return distinctColumn(db, "branch") }

// Agent is the current state of one live Claude Code session, derived from its
// most recent events. It powers the roster at the top of the dashboard — the
// "which of my agents is waiting on me" view.
type Agent struct {
	SessionID   string
	SourceApp   string
	Branch      string
	Cwd         string
	TmuxSession string // tmux session name, the disambiguating label
	Status      string // human label (see status* constants)
	StatusRank  int    // sort key; lower = more urgent
	Doing       string // most recent skill/command, else latest tool/event
	DoingActive bool   // true while the agent is still working the phase; false once it has stopped/gone idle, when Doing is the last *completed* thing
	LastEvent   string
	Idle        string // humanised time since last event ("4m", "1h")
	IdleSecs    int
	IdleSince   int64  // unix seconds of the last event, so the client can tick idle in place
	CI          string // merge-readiness; filled by the server layer via gh
	firstSeq    int    // arrival order (0-based) within the roster window; drives stable color assignment, not displayed
}

// Status labels. Kept as constants so tests and the sort agree on the wording.
const (
	statusWaiting  = "waiting for you"
	statusNeedsYou = "needs you"
	statusApproval = "needs approval"
	statusWorking  = "working"
	statusIdle     = "idle"
	statusEnded    = "ended"
)

// staleWorkingSecs is how long a "working" session may go silent before the
// roster stops calling it working. An active agent emits a hook on every tool
// use, so a mid-flight session quiet this long was killed, crashed, or never
// fired its Stop hook — it is idle, not working. (Sessions that ended cleanly
// carry a Stop/SessionEnd event and never reach this branch.)
//
// This is a hook-driven heuristic: a session running a single long tool call
// (e.g. a 6-minute build) also goes silent and will flip to idle, since without
// a PostToolUse/heartbeat there is no way to tell "in a long call" from
// "crashed". That trade-off is intentional — "idle" is the honest label for "no
// signal", and beats the old contradictory "working" + climbing-idle. Don't
// lower this to chase long-call false positives; if PostToolUse is ever
// captured, that would be the real disambiguator. An idle session is kept on the
// board (it does not drop) until it leaves the waitingCap query window or its
// session ends — see agentRoster.
const staleWorkingSecs = 5 * 60

// deriveStatus maps a session's most recent event_type (and how long it has been
// silent) to a status label, a sort rank (lower = more urgent), and whether the
// agent is still active (an ended session drops off the board). idleSecs only
// matters for the working case: a mid-flight session quiet past
// staleWorkingSecs is reported as idle rather than working, since a genuinely
// busy agent keeps emitting hooks.
func deriveStatus(lastEvent string, idleSecs int) (label string, rank int, active bool) {
	switch lastEvent {
	case "Stop":
		return statusWaiting, 0, true
	case "Notification":
		return statusNeedsYou, 1, true
	case "PermissionRequest":
		return statusApproval, 1, true
	case "SessionEnd":
		return statusEnded, 9, false
	default:
		if idleSecs >= staleWorkingSecs {
			return statusIdle, 6, true
		}
		return statusWorking, 5, true
	}
}

// agentRoster folds recent events into one row per session, reflecting each
// agent's current state. Every live session within waitingCap is kept — a
// working agent that goes quiet is relabeled idle (see deriveStatus) rather than
// dropped, so it survives a lunch or overnight gap and only leaves the board on
// a SessionEnd or once it falls out of the waitingCap query window. now is
// injected so the result is deterministic under test.
func agentRoster(db *sql.DB, waitingCap time.Duration, now time.Time) ([]Agent, error) {
	since := now.Add(-waitingCap).UTC().Format(time.RFC3339)
	rows, err := db.Query(
		`SELECT ts, source_app, COALESCE(branch,''), COALESCE(cwd,''), COALESCE(tmux_session,''), COALESCE(session_id,''),
		        event_type, COALESCE(tool_name,''), COALESCE(summary,'')
		 FROM events WHERE ts >= ? AND session_id IS NOT NULL AND session_id <> ''
		 ORDER BY id ASC`, since)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	type state struct {
		a        Agent
		lastTS   string
		lastTool string
	}
	byID := map[string]*state{}
	nextSeq := 0 // rows are id-ascending, so first sighting order == arrival order
	for rows.Next() {
		var ts, app, branch, cwd, tmuxSess, sess, etype, tool, summary string
		if err := rows.Scan(&ts, &app, &branch, &cwd, &tmuxSess, &sess, &etype, &tool, &summary); err != nil {
			return nil, err
		}
		s := byID[sess]
		if s == nil {
			s = &state{a: Agent{SessionID: sess, firstSeq: nextSeq}}
			byID[sess] = s
			nextSeq++
		}
		// Ascending scan: the last write wins, so these hold the latest values.
		s.a.SourceApp, s.a.Branch, s.a.Cwd, s.a.TmuxSession = app, branch, cwd, tmuxSess
		s.a.LastEvent = etype
		s.lastTS = ts
		if tool != "" {
			s.lastTool = tool
		}
		// "Doing" tracks the most recent skill/command — the phase the agent is in
		// (fix-gh-issue, code-review, …). Only Skill events update it.
		if tool == "Skill" && summary != "" {
			s.a.Doing = strings.TrimPrefix(summary, "Skill: ")
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	out := make([]Agent, 0, len(byID))
	for _, s := range byID {
		idleSecs := idleSeconds(s.lastTS, now)
		label, rank, active := deriveStatus(s.a.LastEvent, idleSecs)
		if !active {
			continue
		}
		a := s.a
		a.Status, a.StatusRank = label, rank
		if a.Doing == "" {
			a.Doing = s.lastTool // fall back to the latest tool when no skill seen
		}
		// Only a working agent is actively in its phase; once it has stopped or
		// gone idle, Doing is the last *completed* thing — the dashboard shows
		// that in italics rather than as something in progress.
		a.DoingActive = label == statusWorking
		a.IdleSecs = idleSecs
		a.Idle = humanizeSeconds(idleSecs)
		// Absolute last-event time (derived from now − idle so it stays consistent
		// with IdleSecs); the dashboard's JS reads it from data-since to advance the
		// idle column locally, without a server round-trip.
		a.IdleSince = now.Unix() - int64(idleSecs)
		out = append(out, a)
	}
	// Sort by idle time alone: most recently active at the top, idle the
	// longest at the bottom. StatusRank still drives styling, not ordering.
	sort.Slice(out, func(i, j int) bool {
		return out[i].IdleSecs < out[j].IdleSecs
	})
	return out, nil
}

// idleSeconds returns whole seconds between an RFC3339 timestamp and now, clamped
// at 0. An unparseable timestamp yields 0 rather than a misleading huge value.
func idleSeconds(ts string, now time.Time) int {
	t, err := time.Parse(time.RFC3339, ts)
	if err != nil {
		return 0
	}
	d := now.Sub(t)
	if d < 0 {
		return 0
	}
	return int(d.Seconds())
}

// formatDuration renders a tool-call duration (milliseconds) as one compact
// token for the Recent-events time suffix: "2ms"/"246ms" under a second,
// "3.1s" under a minute, "2m"/"2m05s" beyond.
func formatDuration(ms int64) string {
	if ms < 1000 {
		return fmt.Sprintf("%dms", ms)
	}
	if ms < 60000 {
		return fmt.Sprintf("%.1fs", float64(ms)/1000)
	}
	totalSec := ms / 1000
	m, s := totalSec/60, totalSec%60
	if s == 0 {
		return fmt.Sprintf("%dm", m)
	}
	return fmt.Sprintf("%dm%02ds", m, s)
}

// EventRow is one rendered line in the Recent-events table after folding: the
// anchor Event (the Pre when a tool call has one) plus the derived outcome glyph
// and a preformatted duration. Non-tool events pass through with empty Outcome
// and Duration. Event is embedded so the template reaches its fields directly.
type EventRow struct {
	Event
	Outcome  string // "✓" resolved, "✗" failed, "⋯" in-flight, "" for non-tool rows
	Duration string // formatDuration of the resolving event, or "" if none yet
}

// Outcome glyphs for a folded tool-call row.
const (
	outcomeOK      = "✓"
	outcomeFail    = "✗"
	outcomeRunning = "⋯"
)

// foldToolEvents collapses each PreToolUse/PostToolUse(/Failure) pair — matched
// exactly by tool_use_id — into a single Recent-events row, so one tool call is
// one line with an outcome glyph and a duration. Events without a tool_use_id
// (prompts, Stop, Session*, …) pass through unchanged. The input is the
// id-descending slice queryEvents returns; output preserves that order, anchored
// at each call's Pre (start) event so a completing call does not jump to the top
// of the feed. Pure: no clock, no DB.
func foldToolEvents(events []Event) []EventRow {
	type pair struct {
		pre      *Event
		resolved *Event // PostToolUse or PostToolUseFailure
	}
	byID := map[string]*pair{}
	for i := range events {
		e := &events[i]
		if e.ToolUseID == "" {
			continue
		}
		p := byID[e.ToolUseID]
		if p == nil {
			p = &pair{}
			byID[e.ToolUseID] = p
		}
		switch e.EventType {
		case "PreToolUse":
			if p.pre == nil { // first-wins on a duplicate Pre (retry)
				p.pre = e
			}
		case "PostToolUse", "PostToolUseFailure":
			if p.resolved == nil {
				p.resolved = e
			}
		}
	}

	out := make([]EventRow, 0, len(events))
	emitted := map[string]bool{}
	for i := range events {
		e := events[i]
		if e.ToolUseID == "" {
			out = append(out, EventRow{Event: e}) // non-tool: pass through
			continue
		}
		if emitted[e.ToolUseID] {
			continue
		}
		p := byID[e.ToolUseID]
		// Anchor at the Pre when present (else the lone event we have). Emit only
		// at the anchor's walk position so the row lands at the start (Pre), not
		// the higher-id Post — otherwise a completing call would jump to the top.
		anchorIsPre := p.pre != nil
		if anchorIsPre && e.EventType != "PreToolUse" {
			continue // this is the resolving Post; its row is emitted at the Pre
		}
		emitted[e.ToolUseID] = true
		row := EventRow{Event: e}
		switch {
		case p.resolved != nil && p.resolved.EventType == "PostToolUseFailure":
			row.Outcome = outcomeFail
		case p.resolved != nil:
			row.Outcome = outcomeOK
		default:
			row.Outcome = outcomeRunning
		}
		if p.resolved != nil && p.resolved.DurationMs.Valid {
			row.Duration = formatDuration(p.resolved.DurationMs.Int64)
		}
		out = append(out, row)
	}
	return out
}

// humanizeSeconds renders a compact age like "12s", "4m", or "1h".
func humanizeSeconds(s int) string {
	switch {
	case s < 60:
		return fmt.Sprintf("%ds", s)
	case s < 3600:
		return fmt.Sprintf("%dm", s/60)
	default:
		return fmt.Sprintf("%dh", s/3600)
	}
}

// SourceCount is a per-(tmux_session, source_app, branch) activity tally for a
// recent window.
type SourceCount struct {
	TmuxSession string
	SourceApp   string
	Branch      string
	Count       int
}

// activeCounts returns per-(tmux_session, source_app, branch) event counts within
// the last window. Grouping by tmux session and branch as well as app means each
// concurrent tmux session / worktree shows up as its own row — the signal that
// tells you which session is busy, and which disambiguates look-alike branches.
// now is passed in (not read from the clock) so the result is deterministic
// under test, matching agentRoster.
func activeCounts(db *sql.DB, window time.Duration, now time.Time) ([]SourceCount, error) {
	since := now.UTC().Add(-window).Format(time.RFC3339)
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
	return out, rows.Err()
}
