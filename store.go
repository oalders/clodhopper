package main

import (
	"database/sql"
	"encoding/hex"
	"errors"
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
	ID           int64
	TS           string // RFC3339 UTC, ingest time
	SourceApp    string
	Branch       string // git branch of Cwd at capture time, "" if unknown
	Rebasing     bool   // true if the work tree at Cwd was mid-rebase at capture, so Branch was recovered from rebase state
	Cwd          string
	TmuxSession  string // tmux session name at capture time, "" if not in tmux
	TmuxPane     string // tmux pane id ("%N") of the Claude pane at capture time, "" if not in tmux
	Launcher     string // "nono" if claude ran inside the nono sandbox, else "claude"; "" on legacy rows
	SlashCommand string // first slash-command token from a UserPromptSubmit prompt, "" otherwise (no arguments retained)
	SessionID    string
	EventType    string
	ToolName     string
	Summary      string
	ToolUseID    string        // hook tool_use_id; pairs a Pre with its Post/Failure
	DurationMs   sql.NullInt64 // tool-call duration (Post* only); NULL otherwise
	PayloadJSON  string
}

const schema = `
CREATE TABLE IF NOT EXISTS events (
  id           INTEGER PRIMARY KEY AUTOINCREMENT,
  ts           TEXT NOT NULL,
  source_app   TEXT NOT NULL,
  branch       TEXT,
  rebasing     INTEGER,
  cwd          TEXT,
  tmux_session TEXT,
  tmux_pane    TEXT,
  launcher     TEXT,
  session_id   TEXT,
  event_type   TEXT NOT NULL,
  tool_name    TEXT,
  summary      TEXT,
  slash_command TEXT,
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
	`ALTER TABLE events ADD COLUMN rebasing INTEGER`,
	`ALTER TABLE events ADD COLUMN slash_command TEXT`,
	`ALTER TABLE events ADD COLUMN tmux_pane TEXT`,
	`ALTER TABLE events ADD COLUMN launcher TEXT`,
	// A block relates two sessions rather than happening in one, so it is not an event row; see applyBlocks.
	`CREATE TABLE IF NOT EXISTS blocks (
	   blocked_session TEXT NOT NULL,
	   blocker_session TEXT NOT NULL,
	   ts              TEXT NOT NULL,
	   PRIMARY KEY (blocked_session, blocker_session))`,
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
		`INSERT INTO events (ts, source_app, branch, rebasing, cwd, tmux_session, tmux_pane, launcher, session_id, event_type, tool_name, summary, slash_command, tool_use_id, duration_ms, payload_json)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		ev.TS, ev.SourceApp, ev.Branch, ev.Rebasing, ev.Cwd, ev.TmuxSession, ev.TmuxPane, ev.Launcher, ev.SessionID, ev.EventType, ev.ToolName, ev.Summary, ev.SlashCommand, ev.ToolUseID, ev.DurationMs, ev.PayloadJSON,
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

// AmbiguousSessionError is returned by liveSessions when a --session prefix
// matches more than one live session. The roster only shows a leading fragment
// of each session id, so a short prefix can be ambiguous; rather than act on the
// wrong agent, liveSessions reports the candidates so the caller can retry with a
// longer prefix.
type AmbiguousSessionError struct {
	Prefix string
	IDs    []string
}

func (e *AmbiguousSessionError) Error() string {
	return fmt.Sprintf("session prefix %q matches %d live sessions: %s",
		e.Prefix, len(e.IDs), strings.Join(e.IDs, ", "))
}

// likePrefix turns a literal fragment into a LIKE pattern matching strings that
// start with it, escaping LIKE's own wildcards so the fragment is matched
// verbatim (session ids never contain %/_/\, but the fragment is user input).
// Pair it with ESCAPE '\' in the query.
func likePrefix(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return r.Replace(s) + "%"
}

// liveSession is one session whose latest event is not a SessionEnd, with that
// event's identifying columns.
type liveSession struct{ sess, app, branch, cwd string }

// liveSessions returns every live session (one whose latest event is not
// already SessionEnd) matching sel; an empty sel matches them all. Branch and
// cwd are matched against each session's latest row, so they reflect its
// current values. SessionID is matched as a prefix so the leading fragment shown
// on the roster resolves a session; if that prefix matches more than one live
// session, an *AmbiguousSessionError lists the candidates rather than guess.
// (Branch/cwd are intentionally bulk selectors, so they are exempt.)
func liveSessions(db *sql.DB, sel EndSelector) ([]liveSession, error) {
	q := `SELECT session_id, source_app, COALESCE(branch,''), COALESCE(cwd,''), event_type
	      FROM events e
	      WHERE session_id IS NOT NULL AND session_id <> ''
	        AND id = (SELECT MAX(id) FROM events WHERE session_id = e.session_id)`
	var args []any
	if sel.SessionID != "" {
		q += ` AND session_id LIKE ? ESCAPE '\'`
		args = append(args, likePrefix(sel.SessionID))
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
		return nil, err
	}
	defer rows.Close()

	var live []liveSession
	for rows.Next() {
		var l liveSession
		var etype string
		if err := rows.Scan(&l.sess, &l.app, &l.branch, &l.cwd, &etype); err != nil {
			return nil, err
		}
		if etype == "SessionEnd" {
			continue // already ended
		}
		live = append(live, l)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if sel.SessionID != "" && len(live) > 1 {
		return nil, &AmbiguousSessionError{Prefix: sel.SessionID, IDs: sessionIDs(live)}
	}
	return live, nil
}

// sessionIDs returns the sorted ids of live, for listing candidates.
func sessionIDs(live []liveSession) []string {
	ids := make([]string, len(live))
	for i, l := range live {
		ids[i] = l.sess
	}
	sort.Strings(ids)
	return ids
}

// endSessions writes a synthetic SessionEnd row for every live session matching
// sel (see liveSessions), so an agent that was hard-killed — and therefore never
// emitted its own SessionEnd — drops off the roster at once instead of lingering
// until the waiting cap. It returns the number of sessions ended; an ambiguous
// session prefix ends nothing. now is injected for deterministic tests.
func endSessions(db *sql.DB, sel EndSelector, now time.Time) (int, error) {
	targets, err := liveSessions(db, sel)
	if err != nil {
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

// Block records that the Blocked session cannot move until the Blocker session
// lands. Both are full session ids, resolved when the block was created.
type Block struct {
	Blocked string
	Blocker string
}

var (
	errSelfBlock  = errors.New("a session cannot be blocked on itself")
	errBlockCycle = errors.New("the blocker is already (transitively) blocked on this session; that cycle would silence both rows")
)

// resolveLiveSession resolves sel to exactly one live session id. Unlike end,
// a block names ONE row on each side, so a branch shared by several live
// sessions is refused with the candidates, like an ambiguous prefix.
func resolveLiveSession(db *sql.DB, sel EndSelector) (string, error) {
	live, err := liveSessions(db, sel)
	if err != nil {
		return "", err
	}
	switch len(live) {
	case 0:
		return "", errors.New("no live session matches")
	case 1:
		return live[0].sess, nil
	}
	return "", fmt.Errorf("%d live sessions match; pick one by session id prefix: %s",
		len(live), strings.Join(sessionIDs(live), ", "))
}

// listBlocks returns every stored block in a stable order, live or not:
// applyBlocks decides which still apply.
func listBlocks(db *sql.DB) ([]Block, error) {
	rows, err := db.Query(`SELECT blocked_session, blocker_session FROM blocks ORDER BY blocked_session, blocker_session`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Block
	for rows.Next() {
		var b Block
		if err := rows.Scan(&b.Blocked, &b.Blocker); err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// blockedOn reports whether from is (transitively) blocked on to, following
// only edges whose two sessions are both in live — any other edge is inert, so
// it must not refuse a block either.
func blockedOn(blocks []Block, live map[string]bool, from, to string) bool {
	seen := map[string]bool{}
	next := []string{from}
	for len(next) > 0 {
		s := next[len(next)-1]
		next = next[:len(next)-1]
		if seen[s] {
			continue
		}
		seen[s] = true
		for _, b := range blocks {
			if b.Blocked != s || !live[b.Blocked] || !live[b.Blocker] {
				continue
			}
			if b.Blocker == to {
				return true
			}
			next = append(next, b.Blocker)
		}
	}
	return false
}

// addBlock records that blocked waits on blocker, refusing a self-block and a
// cycle through the live edges (live holds the ids that count as live). Both
// ids travel only as bound parameters. now is injected for deterministic tests.
func addBlock(db *sql.DB, blocked, blocker string, live map[string]bool, now time.Time) error {
	if blocked == blocker {
		return errSelfBlock
	}
	blocks, err := listBlocks(db)
	if err != nil {
		return err
	}
	if blockedOn(blocks, live, blocker, blocked) {
		return errBlockCycle
	}
	_, err = db.Exec(`INSERT OR REPLACE INTO blocks (blocked_session, blocker_session, ts) VALUES (?, ?, ?)`,
		blocked, blocker, now.UTC().Format(time.RFC3339))
	return err
}

// unblockSession drops every block whose blocked side is sessionID and returns
// how many it removed.
func unblockSession(db *sql.DB, sessionID string) (int64, error) {
	res, err := db.Exec(`DELETE FROM blocks WHERE blocked_session = ?`, sessionID)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// latestCwdForSession returns the worktree cwd of sessionID's most recent event
// that recorded one, sanitized the same way agentRoster sanitizes cwd on the way
// out (legacy rows may hold un-sanitized values). It returns "" (nil error) when
// the session is unknown or never recorded a cwd — the caller treats that as
// "no target" and 404s, so a missing row is not an error.
func latestCwdForSession(db *sql.DB, sessionID string) (string, error) {
	if sessionID == "" {
		return "", nil
	}
	var cwd string
	err := db.QueryRow(
		`SELECT COALESCE(cwd,'') FROM events
		 WHERE session_id = ? AND cwd IS NOT NULL AND cwd <> ''
		 ORDER BY id DESC LIMIT 1`, sessionID).Scan(&cwd)
	if err == sql.ErrNoRows {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return truncate(refuseControl(cwd), maxPathLen), nil
}

// latestPaneForSession returns the tmux pane id of sessionID's most recent event
// that recorded one. It mirrors latestCwdForSession: "" (nil error) when the
// session is unknown or never recorded a pane, which the caller treats as "no
// live pane to target". The value is NOT trusted here — the caller re-validates
// it against paneIDRe before it ever reaches a tmux command.
func latestPaneForSession(db *sql.DB, sessionID string) (string, error) {
	if sessionID == "" {
		return "", nil
	}
	var pane string
	err := db.QueryRow(
		`SELECT COALESCE(tmux_pane,'') FROM events
		 WHERE session_id = ? AND tmux_pane IS NOT NULL AND tmux_pane <> ''
		 ORDER BY id DESC LIMIT 1`, sessionID).Scan(&pane)
	if err == sql.ErrNoRows {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return pane, nil
}

// pruneOld deletes events older than the given number of days, and the blocks
// left without a session, and returns the number of events removed.
func pruneOld(db *sql.DB, days int) (int64, error) {
	if days <= 0 {
		return 0, nil
	}
	cutoff := time.Now().UTC().AddDate(0, 0, -days).Format(time.RFC3339)
	res, err := db.Exec(`DELETE FROM events WHERE ts < ?`, cutoff)
	if err != nil {
		return 0, err
	}
	// A block whose session has no events left is inert (applyBlocks skips it); sweep it.
	if _, err := db.Exec(`DELETE FROM blocks
		WHERE NOT EXISTS (SELECT 1 FROM events WHERE session_id = blocked_session)
		   OR NOT EXISTS (SELECT 1 FROM events WHERE session_id = blocker_session)`); err != nil {
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
	q := `SELECT id, ts, source_app, branch, COALESCE(rebasing,0), cwd, session_id, event_type, tool_name, summary, COALESCE(tool_use_id,''), duration_ms, payload_json
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
		if err := rows.Scan(&e.ID, &e.TS, &e.SourceApp, &branch, &e.Rebasing, &cwd, &sess, &e.EventType, &tool, &summary, &e.ToolUseID, &e.DurationMs, &e.PayloadJSON); err != nil {
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
	SessionID        string
	SourceApp        string
	Branch           string
	Rebasing         bool   // true if the session's latest event was captured mid-rebase, so Branch was recovered from rebase state
	BranchGuess      string // basename of Cwd, shown (italicised) ONLY when Branch is unknown, as a hint for locating the worktree/tmux session; "" when Branch is known or Cwd is empty
	Worktree         string // basename of Cwd's worktree root, set by the server layer only when Branch is known, not main/master, and differs from it; display only
	Cwd              string
	LaunchCwd        string // cwd of the session's earliest in-window event: where it was launched, which `claude --resume` looks a session up by, whereas Cwd follows a later `cd`
	TmuxSession      string // tmux session name, the disambiguating label
	TmuxPane         string // tmux pane id ("%N") of the latest event's Claude pane; targets the live pane peek, "" if unknown
	HasPane          bool   // true when the session recorded a tmux pane id (TmuxPane != ""); gates the session-action buttons under --enable-merge, decoupled from --pane-peek (LiveTmux is only set when peek is on)
	Launcher         string // "nono" or "claude" from the latest event that recorded one; "" if unknown (legacy rows), which renders no marker
	LiveTmux         bool   // true when TmuxPane is currently a live tmux pane (set by the server layer when --pane-peek is on); drives the peek control
	Repo             string // GitHub "owner/repo" of Cwd's origin remote, resolved only for LiveTmux rows so the peek can link the footer's "PR #N"; "" if not a github.com remote
	Status           string // human label (see status* constants)
	StatusRank       int    // sort key; lower = more urgent
	Doing            string // most recent skill/command, else latest tool/event
	LastCommand      string // last slash command the session ran (e.g. "/code-review"), "" if none
	LastCommandSince int64  // unix seconds of the event that supplied LastCommand; 0 when no slash command was seen
	DoingActive      bool   // true while the agent is still working the phase; false once it has stopped/gone idle, when Doing is the last *completed* thing
	LastEvent        string
	Idle             string // humanised time since last event ("4m", "1h")
	IdleSecs         int
	IdleSince        int64  // unix seconds of the last event, so the client can tick idle in place
	Subagents        int    // background subagents still running while the main agent waits; set only for statusSubagents
	Shells           int    // background shells still running while the main agent waits; set only for statusSubagents
	WaitingOn        string // the badge's count text for Subagents and Shells ("1 agent", "2 shells", "3 tasks"); see waitingOn
	WaitingOnDetail  string // when WaitingOn says "tasks", their breakdown ("1 agent, 2 shells") for the badge's title and screen readers
	SubagentSince    int64  // unix seconds of the earliest start among those subagents and shells, so the client can tick their running time in place
	SubagentAge      string // humanised time since SubagentSince, the initial value the client then ticks
	CI               string // merge-readiness ("", "pending", "green", "failing", "soft-failing", "ejected"); filled by the server layer via gh
	CIFails          int    // failed checks on the PR, shown inside the red/orange badge
	CIPasses         int    // passed checks on the PR, shown inside the green badge
	CIConflict       bool   // the PR has a merge conflict with its base branch
	CIDraft          bool   // the PR is a draft, shown as a muted chip after the badge
	CIDone           string // "merged" or "closed" once the PR is no longer open, shown as a muted chip like CIDraft
	CIQueue          string // merge-queue state ("", "queued", "queue-failing"), the merge-queue section's pill state
	CIQueuePos       int    // 1-based merge-queue position shown in that pill
	CIQueueETA       int64  // unix seconds the PR is expected to merge; 0 when unknown
	CIQueueSince     int64  // unix seconds the PR entered the queue; 0 when unknown
	CIQueueLabel     string // the pill's title/aria-label, built from the three above at render time
	CIQueueReason    string // why GitHub removed an "ejected" PR from the merge queue, shown in the badge's label
	CIQueueStuck     bool   // the queue entry has likely stalled (queueStuck), marked on the pill; set by splitQueued
	PRNumber         int    // the PR's number, 0 if none; with PRRepo, links a merge-queue ledger row to its PR
	PRRepo           string // the PR's GitHub "owner/repo", "" if unknown
	PRMergedAt       int64  // unix seconds the PR merged; 0 when not merged or unknown
	BlockedOn        string // the blocker row's BlockLabel while a block applies (see applyBlocks), shown as the "blocked on" chip; "" otherwise
	BlockedOnSession string // the blocker's session id, so the chip can jump to its row
	GroupStart       bool   // true on the first roster row of each (SourceApp, Branch) group except the very first row overall; drives the divider between branch groups in the dashboard
	Grouped          bool   // true when this row's (SourceApp, Branch) group has 2+ live members; drives the left accent bar that binds a multi-session branch cluster (a worktree with several agents). Singleton branches stay false (no bar)
	GroupKey         string // hex of the internal group key (per (SourceApp, Branch); per-session for branchless rows). Stable across polls and HTML-attribute-safe, so the dashboard's "pin order" toggle can hold each group's rows together while freezing group order. Not displayed
	firstSeq         int    // arrival order (0-based) within the roster window; drives stable color assignment, not displayed
}

// Status labels. Kept as constants so tests and the sort agree on the wording.
const (
	statusWaiting    = "waiting for you"
	statusNeedsYou   = "needs you"
	statusApproval   = "needs approval"
	statusInput      = "needs input"       // AskUserQuestion: a question to answer, not an action to approve
	statusBackground = "waiting"           // parked on background work, not blocked on the user
	statusSubagents  = "waiting on agents" // turn ended but background subagents or shells are still running
	statusWorking    = "working"
	statusIdle       = "idle"
	statusEnded      = "ended"
	statusBlocked    = "blocked" // set only by the server layer (applyBlocks): waiting on another session, not on the user
)

// rankBackground is the sort/style rank for statusBackground. It sits above the
// alert threshold (StatusRank <= 1 styles a row as needing attention) so a
// parked agent reads as non-urgent, but below "working"/"idle" so it stays
// visually grouped with the live-but-quiet sessions. Shared by deriveStatus and
// the server-layer CI demotion so the two never drift.
const rankBackground = 4

// staleWorkingSecs is how long a "working" session may go silent before the
// roster stops calling it working. An active agent emits a hook on every tool
// use, so a mid-flight session quiet this long was killed, crashed, or never
// fired its Stop hook — it is idle, not working. (Sessions that ended cleanly
// carry a Stop/SessionEnd event and never reach this branch.)
//
// A session whose latest event is a PreToolUse is mid tool call (e.g. a long
// test run) and gets staleInFlightSecs instead. An idle session is kept on the
// board (it does not drop) until it leaves the waitingCap query window or its
// session ends — see agentRoster.
const staleWorkingSecs = 5 * 60

// staleInFlightSecs is the silence allowed while a tool call is in flight: a
// trailing PreToolUse with no PostToolUse yet. A foreground Bash call is capped
// at 10 minutes, after which it moves to the background and fires PostToolUse,
// so a call open past that (plus slack for hook latency) was killed, interrupted
// (Esc fires no PostToolUse) or crashed — idle, not working. The cap applies
// to every tool, so a tool that legitimately runs longer (a slow MCP call) still
// flips to idle. A foreground subagent's own events keep lastTS fresh, so it only
// hits the cap if the subagent itself goes silent that long.
const staleInFlightSecs = 11 * 60

// deriveStatus maps a session's most recent event_type (and how long it has been
// silent) to a status label, a sort rank (lower = more urgent), and whether the
// agent is still active (an ended session drops off the board). idleSecs only
// matters for the working case: a mid-flight session quiet past
// staleWorkingSecs (staleInFlightSecs while a tool call is open) is reported as
// idle rather than working, since a genuinely busy agent keeps emitting hooks.
//
// notifType and lastTool disambiguate Notification, which Claude Code overloads:
// "permission_prompt" is a genuine "Claude needs your input" prompt, but
// "idle_prompt" is just the ~60s idle reminder fired after a turn ends. An idle
// reminder must NOT read as the urgent "needs you" — and when the agent parked
// itself on background work (its last tool was ScheduleWakeup), it will resume
// itself, so it is waiting on that, not on the user.
//
// lastTool also splits the permission-prompt cases: AskUserQuestion blocks on the
// user the same way, but it is a question to answer, not an action to approve —
// "needs input" reads truer than "needs approval". Both the PermissionRequest and
// the trailing permission_prompt Notification it emits carry that distinction
// (the Notification's empty tool_name leaves lastTool holding "AskUserQuestion").
func deriveStatus(lastEvent, notifType, lastTool string, idleSecs int) (label string, rank int, active bool) {
	// promptLabel picks "needs input" vs "needs approval" for a blocking prompt.
	promptLabel := func() string {
		if lastTool == "AskUserQuestion" {
			return statusInput
		}
		return statusApproval
	}
	switch lastEvent {
	case "Stop":
		return statusWaiting, 0, true
	case "Notification":
		switch notifType {
		case "permission_prompt":
			return promptLabel(), 1, true
		case "idle_prompt":
			if lastTool == "ScheduleWakeup" {
				return statusBackground, rankBackground, true
			}
			// A plain idle reminder: the turn ended and Claude is nudging. That is
			// the same honest state as a Stop — waiting for you, not "needs you".
			return statusWaiting, 0, true
		default:
			// Missing/unknown type (older Claude Code, or a shape we don't model):
			// keep the conservative "needs you" rather than risk hiding a real
			// input prompt. The scrub layer's fail-closed bias, applied to status.
			return statusNeedsYou, 1, true
		}
	case "PermissionRequest":
		return promptLabel(), 1, true
	case "SessionEnd":
		return statusEnded, 9, false
	default:
		stale := staleWorkingSecs
		if lastEvent == "PreToolUse" {
			stale = staleInFlightSecs
		}
		if idleSecs >= stale {
			return statusIdle, 6, true
		}
		return statusWorking, 5, true
	}
}

// agentRoster folds recent events into one row per session, reflecting each
// agent's current state. Every live session within waitingCap is kept — a
// working agent that goes quiet is relabeled idle (see deriveStatus) rather than
// dropped, so it survives a lunch or overnight gap and only leaves the board on
// a SessionEnd or once it falls out of the waitingCap query window (serve also
// hides a row whose tmux pane has died; see dropDeadPanes). now is injected so
// the result is deterministic under test.
func agentRoster(db *sql.DB, waitingCap time.Duration, now time.Time) ([]Agent, error) {
	since := now.Add(-waitingCap).UTC().Format(time.RFC3339)
	rows, err := db.Query(
		`SELECT ts, source_app, COALESCE(branch,''), COALESCE(rebasing,0), COALESCE(cwd,''), COALESCE(tmux_session,''), COALESCE(tmux_pane,''), COALESCE(launcher,''), COALESCE(session_id,''),
		        event_type, COALESCE(tool_name,''), COALESCE(summary,''), COALESCE(slash_command,''),
		        COALESCE(CASE WHEN json_valid(payload_json) THEN json_extract(payload_json,'$.notification_type') END,''),
		        COALESCE(CASE WHEN json_valid(payload_json) THEN json_extract(payload_json,'$.agent_id') END,''),
		        COALESCE(CASE WHEN json_valid(payload_json) THEN json_extract(payload_json,'$.source') END,''),
		        COALESCE(CASE WHEN json_valid(payload_json) THEN json_extract(payload_json,'$.background_task_id') END,''),
		        COALESCE(CASE WHEN json_valid(payload_json) THEN COALESCE(json_extract(payload_json,'$.task_notification_id'),
		                 json_extract(payload_json,'$.tool_input.task_id'), json_extract(payload_json,'$.tool_input.shell_id')) END,'')
		 FROM events WHERE ts >= ? AND session_id IS NOT NULL AND session_id <> ''
		 ORDER BY id ASC`, since)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	// subagent tracks one background subagent by its start and latest event.
	type subagent struct {
		start, lastSeen time.Time
		lastEvent       string
	}
	type state struct {
		a         Agent
		lastTS    string
		lastCmdTS string
		lastTool  string
		lastNotif string
		subs      map[string]*subagent // running subagents by agent_id
		subPerms  map[string]string    // subagents blocked on a permission request: agent_id -> tool
		shells    map[string]time.Time // the main agent's running background shells: task id -> start
	}
	byID := map[string]*state{}
	nextSeq := 0 // rows are id-ascending, so first sighting order == arrival order
	for rows.Next() {
		var ts, app, branch, cwd, tmuxSess, tmuxPane, launcher, sess, etype, tool, summary, slashCmd, notifType, agentID, source, shellStart, shellEnd string
		var rebasing bool
		if err := rows.Scan(&ts, &app, &branch, &rebasing, &cwd, &tmuxSess, &tmuxPane, &launcher, &sess, &etype, &tool, &summary, &slashCmd, &notifType, &agentID, &source, &shellStart, &shellEnd); err != nil {
			return nil, err
		}
		s := byID[sess]
		if s == nil {
			s = &state{a: Agent{SessionID: sess, firstSeq: nextSeq}, subs: map[string]*subagent{}, subPerms: map[string]string{}, shells: map[string]time.Time{}}
			byID[sess] = s
			nextSeq++
		}
		// Ascending scan: the last write wins, so these hold the latest values.
		// Cwd is re-sanitized on the way out, not just on the way in: rows written
		// before ingest started stripping (and capping) are still in the database,
		// and the roster renders this value into a title and an aria-label as well
		// as the copy target — html/template escapes neither a newline nor any
		// other control character in an attribute value. The client refuses such a
		// value too; doing the work here as well makes the two layers genuinely
		// independent rather than leaving the server side resting on a comment.
		// The cap is re-applied for the same reason as the strip: an oversized
		// legacy path would otherwise be rendered three times per roster row on
		// every page render and every /api/state poll.
		s.a.SourceApp = app
		// TmuxSession comes from the same tmuxContext call as TmuxPane and follows
		// the same keep-the-last-non-empty rule, for the reason spelled out below.
		if tmuxSess != "" {
			s.a.TmuxSession = tmuxSess
		}
		// TmuxPane follows the same keep-the-last-non-empty rule as Branch below:
		// tmuxContext is best-effort and transiently returns "" (a timed-out
		// `tmux display-message`), and an empty capture must not clobber a pane id
		// the session recorded earlier, or the live-pane peek control would blink
		// out whenever the latest event happened to miss it. The server layer's
		// live-set check already guards against a since-dead id.
		if tmuxPane != "" {
			s.a.TmuxPane = tmuxPane
		}
		// Same keep-the-last-non-empty rule, so legacy NULL rows don't clobber it.
		if launcher != "" {
			s.a.Launcher = launcher
		}
		s.a.Cwd = truncate(refuseControl(cwd), maxPathLen)
		if s.a.LaunchCwd == "" {
			s.a.LaunchCwd = s.a.Cwd // first non-empty wins, unlike the last-write Cwd
		}
		// Branch is a stable property of the worktree, but gitBranch is best-effort
		// and transiently returns "" under concurrent-worktree load (a timed-out
		// `git symbolic-ref`). An empty capture must not clobber a branch the
		// session recorded on earlier events, or a live agent's branch would blink
		// out whenever its most recent event happened to be one that failed — same
		// last-write-wins-but-keep-the-last-non-empty rule as slash_command and tool
		// below. Rebasing rides with the branch it describes so the two stay in sync.
		if branch != "" {
			s.a.Branch, s.a.Rebasing = branch, rebasing
		}
		// Subagent lifecycle events are bookkeeping only: they must not move the
		// session's status, so an orphan SubagentStop (housekeeping agents emit
		// those with no start) leaves a stopped session waiting for you.
		switch etype {
		case "SubagentStart":
			// An id-less start would match every main-agent event; an unparseable
			// ts is dropped so the session fails toward "waiting for you".
			if t, err := time.Parse(time.RFC3339, ts); agentID != "" && err == nil {
				s.subs[agentID] = &subagent{start: t, lastSeen: t}
			}
			continue
		case "SubagentStop":
			delete(s.subs, agentID)
			delete(s.subPerms, agentID)
			continue
		}
		if sub := s.subs[agentID]; sub != nil {
			if t, err := time.Parse(time.RFC3339, ts); err == nil {
				sub.lastSeen, sub.lastEvent = t, etype
			}
		}
		// A subagent's own events are activity, not the main agent's state:
		// letting them set LastEvent flips a parked session between "working" and
		// "waiting for you" on every background tool call. They keep the session
		// fresh (lastTS) so a main agent blocked on a foreground subagent does not
		// go stale. A subagent permission prompt (a PermissionRequest, or the
		// permission_prompt Notification deriveStatus also treats as blocking)
		// genuinely blocks on the user, so it is held aside in subPerms
		// (overriding the status below) until that subagent's next event shows it
		// was answered — rather than written to LastEvent, where no later subagent
		// event could ever clear it. Main-agent events deliberately do not clear
		// it: a finished background task re-invokes the main agent as a
		// UserPromptSubmit while another subagent's dialog may still be open. So a
		// request abandoned with no further subagent event stays visible until
		// SubagentStop or the waitingCap window — fail toward showing it.
		if agentID != "" {
			s.lastTS = ts
			if etype == "PermissionRequest" || (etype == "Notification" && notifType == "permission_prompt") {
				// The Notification trailing a request carries no tool_name; keep the
				// request's tool so AskUserQuestion still reads "needs input".
				if _, ok := s.subPerms[agentID]; !ok || tool != "" {
					s.subPerms[agentID] = tool
				}
			} else {
				delete(s.subPerms, agentID)
			}
			continue
		}
		// Background shells: a Bash result naming a backgroundTaskId (explicit
		// run_in_background, or a call that timed out into the background) starts
		// one; the task-notification prompt it sends on finishing, or a TaskStop
		// naming it, ends it. A restart kills them all, so a SessionStart other
		// than a compaction (nn --resume keeps the session id) clears the set.
		// Ending only on those signals, with no staleness cutoff, means a lost
		// notification holds the row until waitingCap; a shell emits no events
		// while it runs, so aging it out would drop every long CI watch instead.
		if shellStart != "" {
			if t, err := time.Parse(time.RFC3339, ts); err == nil {
				s.shells[shellStart] = t
			}
		}
		if shellEnd != "" && (etype == "UserPromptSubmit" || tool == "TaskStop" || tool == "KillShell") {
			delete(s.shells, shellEnd)
		}
		if etype == "SessionStart" && source != "compact" {
			clear(s.shells)
		}
		s.a.LastEvent = etype
		s.lastTS = ts
		// notifType mirrors LastEvent (last-write-wins); deriveStatus only reads it
		// when the latest event is a Notification, where this holds that event's type.
		s.lastNotif = notifType
		if tool != "" {
			s.lastTool = tool
		}
		// LastCommand tracks the most recent slash command (last-write-wins);
		// empty values (non-command events) must not clobber it.
		if slashCmd != "" {
			s.a.LastCommand, s.lastCmdTS = slashCmd, ts
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
		label, rank, active := deriveStatus(s.a.LastEvent, s.lastNotif, s.lastTool, idleSecs)
		if !active {
			continue
		}
		// A subagent blocked on a permission request outranks whatever the main
		// agent is doing; the label follows the same input-vs-approval split.
		for _, tool := range s.subPerms {
			label, rank, _ = deriveStatus("PermissionRequest", "", tool, 0)
			if label == statusApproval {
				break // an approval anywhere wins over a question, independent of map order
			}
		}
		a := s.a
		a.Status, a.StatusRank = label, rank
		// A turn that ended with subagents or shells still running is not waiting
		// on the user yet; only subagents heard from within staleWorkingSecs
		// (staleInFlightSecs while one is mid tool call, as for the main agent) count.
		if label == statusWaiting {
			var earliest time.Time
			for _, sub := range s.subs {
				stale := staleWorkingSecs
				if sub.lastEvent == "PreToolUse" {
					stale = staleInFlightSecs
				}
				if now.Sub(sub.lastSeen) >= time.Duration(stale)*time.Second {
					continue
				}
				if earliest.IsZero() || sub.start.Before(earliest) {
					earliest = sub.start
				}
				a.Subagents++
			}
			for _, start := range s.shells {
				if earliest.IsZero() || start.Before(earliest) {
					earliest = start
				}
				a.Shells++
			}
			if a.Subagents+a.Shells > 0 {
				a.Status, a.StatusRank = statusSubagents, rankBackground
				a.WaitingOn = waitingOn(a.Subagents, a.Shells)
				if a.Subagents > 0 && a.Shells > 0 {
					a.WaitingOnDetail = waitingOn(a.Subagents, 0) + ", " + waitingOn(0, a.Shells)
				}
				a.SubagentSince = earliest.Unix()
				a.SubagentAge = humanizeSeconds(int(max(0, now.Unix()-a.SubagentSince)))
			}
		}
		// Pane presence is resolved here (independent of --pane-peek) so the
		// session-action buttons can gate on it under --enable-merge alone; the
		// backend re-resolves and re-validates the pane server-side before any
		// tmux command runs, so presence is the correct decoupled gate.
		a.HasPane = a.TmuxPane != ""
		// When git never resolved a branch for this session (a detached HEAD that
		// is not a rebase — see gitBranch), fall back to the cwd's basename so the
		// operator still has a handle for the worktree / tmux session. It is only a
		// hint (the last segment can be a subdir like ".../fix-3653/go"), so the
		// dashboard italicises it; here we just supply the value when there is no
		// branch and a path to take it from.
		if a.Branch == "" && a.Cwd != "" {
			a.BranchGuess = filepath.Base(a.Cwd)
		}
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
		// Absolute instant of the slash-command event, parsed directly rather than
		// via idleSeconds: that helper returns 0 for an *unparseable* timestamp,
		// which an age-bounded consumer reads as "just now" and would hold
		// suppression open indefinitely. Leaving LastCommandSince at its 0 zero
		// value instead yields an age of now.Unix(), far outside any sane window —
		// fail closed. Same for a session that never ran a slash command. A
		// future-dated event is handled by the explicit age < 0 guard in
		// suppressWatchedCI.
		if s.lastCmdTS != "" {
			if t, err := time.Parse(time.RFC3339, s.lastCmdTS); err == nil {
				a.LastCommandSince = t.Unix()
			}
		}
		out = append(out, a)
	}
	// Order the roster by (SourceApp, Branch) group, not by raw idle time, so the
	// two sessions sharing a worktree's branch sit next to each other and a branch
	// can be taken in at a glance instead of being scattered down the table. We
	// still want freshest-on-top, just at the group level: a group floats by its
	// most recently active member, so the branch you just touched stays at the top
	// exactly as plain idle sort did — only its same-branch sibling rides along
	// instead of getting stranded elsewhere.
	//
	// Branchless sessions (Branch == "") get a per-session-unique key so they are
	// NOT clumped into one pseudo-group; each behaves as its own group ordered by
	// its own idle, preserving the old per-session placement for them.
	// groupMin holds each group's freshest member (its smallest IdleSecs), the
	// value the group sorts by.
	groupMin := map[string]int{}
	for _, a := range out {
		k := groupKey(a)
		if m, ok := groupMin[k]; !ok || a.IdleSecs < m {
			groupMin[k] = a.IdleSecs
		}
	}
	sort.Slice(out, func(i, j int) bool {
		gi, gj := groupKey(out[i]), groupKey(out[j])
		// 1. Group with the freshest member first (preserves freshest-on-top).
		if groupMin[gi] != groupMin[gj] {
			return groupMin[gi] < groupMin[gj]
		}
		// 2. Tiebreak on the key so two equally-fresh groups stay contiguous
		//    rather than interleaving.
		if gi != gj {
			return gi < gj
		}
		// 3. Within a group, least-idle (most recently active) first.
		if out[i].IdleSecs != out[j].IdleSecs {
			return out[i].IdleSecs < out[j].IdleSecs
		}
		// 4. Final deterministic tiebreak.
		return out[i].SessionID < out[j].SessionID
	})
	markGroups(out)
	return out, nil
}

// groupKey is the roster row's grouping key. Two disjoint key-spaces with a
// leading discriminator byte so a branchless session can never collide with a
// real (app, branch) group: "u" = unbranched (one group per session, so they
// aren't clumped), "b" = branched.
func groupKey(a Agent) string {
	if a.Branch == "" {
		return "u\x00" + a.SessionID
	}
	return "b\x00" + a.SourceApp + "\x00" + a.Branch
}

// markGroups sets the group flags on an already-sorted roster. It tallies over
// exactly the rows given, so a caller that later drops rows (dropDeadPanes) must
// call it again on the survivors.
func markGroups(out []Agent) {
	// groupCount holds each group's membership so we can mark the rows of a
	// multi-session cluster (2+ members) for the binding left accent bar.
	groupCount := map[string]int{}
	for _, a := range out {
		groupCount[groupKey(a)]++
	}
	// Mark each group's first row so the dashboard can draw a divider between
	// branch groups (the very first row overall stays false — no leading divider),
	// and flag every row of a 2+ member group so it gets the left accent bar that
	// binds the cluster.
	for k := range out {
		out[k].GroupStart = k > 0 && groupKey(out[k]) != groupKey(out[k-1])
		out[k].Grouped = groupCount[groupKey(out[k])] >= 2
		// Hex so the raw key's \x00 separator can't emit an invalid NUL into the
		// data-group attribute; stable across polls since it is a pure function of
		// (SourceApp, Branch) / SessionID.
		out[k].GroupKey = hex.EncodeToString([]byte(groupKey(out[k])))
	}
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
// waitingOn is the count text of the "waiting on …" badge: "1 agent", "2 shells",
// or, when both kinds are running, their sum as "tasks" (Claude Code's own word
// for either) so the nowrap badge stays within the status column's width: the
// 32ch budget in dashboard.html's roster <colgroup> comment assumes no noun
// here is longer than "agents". Callers pass a nonzero total.
func waitingOn(subagents, shells int) string {
	n, noun := subagents+shells, "task"
	switch {
	case shells == 0:
		noun = "agent"
	case subagents == 0:
		noun = "shell"
	}
	if n != 1 {
		noun += "s"
	}
	return fmt.Sprintf("%d %s", n, noun)
}

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
	Rebasing    bool   // true if any event in the group was captured mid-rebase, so Branch was recovered from rebase state
	Worktree    string // the matching roster rows' Worktree, filled by the server layer; "" when they disagree; display only
	Count       int
}

// activeCounts returns per-(tmux_session, source_app, branch) event counts within
// the last window. Grouping by tmux session and branch as well as app means each
// concurrent tmux session / worktree shows up as its own row — the signal that
// tells you which session is busy, and which disambiguates look-alike branches.
// Rebasing is MAX-aggregated per group, so a row is flagged when any of its
// events was captured mid-rebase. A blank tmux_session capture inherits the
// latest non-blank one from the same session_id (as agentRoster does, #114), so
// a flaky capture doesn't split one session into two rows. Like agentRoster, it
// only looks inside the window, so a session whose only named capture is older
// stays blank. labels (session_id → live tmux label, from dropDeadPanes; nil for
// none) overrides the stored label, merging rows that then coincide (#180). now is
// passed in (not read from the clock) so the result is deterministic under test,
// matching agentRoster.
func activeCounts(db *sql.DB, window time.Duration, now time.Time, labels map[string]string) ([]SourceCount, error) {
	since := now.UTC().Add(-window).Format(time.RFC3339)
	rows, err := db.Query(
		`WITH w AS (
		   SELECT COALESCE(session_id, '') AS sid, source_app, branch, rebasing, COALESCE(NULLIF(tmux_session, ''),
		     CASE WHEN COALESCE(session_id, '') <> '' THEN FIRST_VALUE(NULLIF(tmux_session, '')) OVER (
		       PARTITION BY session_id ORDER BY NULLIF(tmux_session, '') IS NULL, id DESC) END,
		     '') AS tmux
		   FROM events WHERE ts >= ?)
		 SELECT sid, tmux, source_app, COALESCE(branch, ''), COALESCE(MAX(rebasing), 0), COUNT(*) FROM w
		 GROUP BY sid, tmux, source_app, branch ORDER BY COUNT(*) DESC`,
		since,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SourceCount
	type key struct{ tmux, app, branch string }
	idx := map[key]int{} // → index in out
	for rows.Next() {
		var sid string
		var sc SourceCount
		if err := rows.Scan(&sid, &sc.TmuxSession, &sc.SourceApp, &sc.Branch, &sc.Rebasing, &sc.Count); err != nil {
			return nil, err
		}
		if l := labels[sid]; l != "" {
			sc.TmuxSession = l
		}
		k := key{sc.TmuxSession, sc.SourceApp, sc.Branch}
		if i, ok := idx[k]; ok {
			out[i].Count += sc.Count
			out[i].Rebasing = out[i].Rebasing || sc.Rebasing
			continue
		}
		idx[k] = len(out)
		out = append(out, sc)
	}
	// Re-sort: grouping by session_id split rows that merging has now summed.
	sort.SliceStable(out, func(i, j int) bool { return out[i].Count > out[j].Count })
	return out, rows.Err()
}
