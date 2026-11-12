package main

import (
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// cleanSessionName keeps the FIRST non-blank segment between unrenderable Nerd
// Font / devicon glyphs (Unicode Private Use Area) — the leading segment is the
// short handle, later ones are window titles — then drops control characters
// and collapses alignment padding. A name with no PUA glyph is left intact (a
// real emoji is not PUA, so it survives).
func TestCleanSessionName(t *testing.T) {
	cases := []struct{ name, in, want string }{
		{
			"keeps the handle before the PUA glyph (with its leading emoji), drops the title",
			"👟  fix-2482            \uf418 Blog post funniest / cleverest / most [in progress]",
			"👟 fix-2482",
		},
		{
			// The user's real session: an icon, the name, more padding, a second
			// icon, the name again. Keeping only the first segment yields the single
			// clean name (not the doubled "name name" the old strip produced).
			"repeated icon-prefixed name keeps only the first segment",
			"\ue627  tmux-session-name   \uf418 tmux-session-name",
			"tmux-session-name",
		},
		{
			// #115: a glyph separates a short branch handle from a long window
			// title; the handle is the useful label.
			"infix glyph keeps the leading handle, not the trailing title",
			"\ue627  fix-113    \uf418  Roster blanks the tmux session label when",
			"fix-113",
		},
		{"emoji is not PUA, so it and the text after it are kept", "👟 fix-2482", "👟 fix-2482"},
		{"plain name unchanged", "my-session", "my-session"},
		{"collapses whitespace and drops control chars", "  hi\tthere\n", "hi there"},
		{"blank leading segment is skipped", "\ue627 \uf418fix-115\uf418title", "fix-115"},
		{"all-glyph name becomes empty", "\uf418\ue0b0", ""},
	}
	for _, c := range cases {
		if got := cleanSessionName(c.in); got != c.want {
			t.Errorf("%s: cleanSessionName(%q) = %q, want %q", c.name, c.in, got, c.want)
		}
	}
}

// Outside tmux ($TMUX unset), both captures are empty and never error.
func TestTmuxContext_NotInTmux(t *testing.T) {
	t.Setenv("TMUX", "")
	sess, pane := tmuxContext()
	if sess != "" || pane != "" {
		t.Errorf("tmuxContext() outside tmux = (%q, %q), want (\"\", \"\")", sess, pane)
	}
}

// Inside a real tmux session, tmuxContext reports THIS pane ($TMUX_PANE) and the
// session that pane belongs to. Skipped unless the suite runs inside tmux with
// the binary present — tmuxContext reads the ambient tmux environment, so it
// cannot be faked hermetically.
func TestTmuxContext_InTmux(t *testing.T) {
	if os.Getenv("TMUX") == "" {
		t.Skip("not running inside tmux")
	}
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not available")
	}
	wantPane := os.Getenv("TMUX_PANE")
	if !paneIDRe.MatchString(wantPane) {
		t.Skipf("no usable $TMUX_PANE (%q)", wantPane)
	}
	// The session name is the one that owns THIS pane, resolved by targeting it —
	// exactly what tmuxContext does, and deliberately not the focused pane's.
	out, err := exec.Command("tmux", "display-message", "-t", wantPane, "-p", "#{session_name}").Output()
	if err != nil {
		t.Skipf("tmux display-message failed: %v", err)
	}
	wantSess := truncate(scrubString(cleanSessionName(strings.TrimRight(string(out), "\n"))), maxFieldLen)
	sess, pane := tmuxContext()
	if sess != wantSess {
		t.Errorf("tmuxContext() session = %q, want %q", sess, wantSess)
	}
	if pane != wantPane {
		t.Errorf("tmuxContext() pane = %q, want %q", pane, wantPane)
	}
}

// The pane id comes from $TMUX_PANE (the pane THIS process runs in), not from
// `display-message` (which resolves to the session's active pane — the bug where
// every peek showed one focused pane). Pointing $TMUX at a bogus socket forces
// the session-name lookup to fail, proving the pane id survives independently and
// a failed label lookup does not drop it.
func TestTmuxContext_PaneFromEnv(t *testing.T) {
	t.Setenv("TMUX", filepath.Join(t.TempDir(), "no-such-socket")+",0,0")
	t.Setenv("TMUX_PANE", "%99")
	sess, pane := tmuxContext()
	if pane != "%99" {
		t.Errorf("pane = %q, want %%99 (from $TMUX_PANE)", pane)
	}
	if sess != "" {
		t.Errorf("session = %q, want \"\" (lookup against a bogus socket must fail closed)", sess)
	}
}

// A malformed $TMUX_PANE yields no pane id: fail closed, because a peek that
// targets the wrong pane is worse than no peek.
func TestTmuxContext_RejectsBadPaneEnv(t *testing.T) {
	t.Setenv("TMUX", filepath.Join(t.TempDir(), "no-such-socket")+",0,0")
	t.Setenv("TMUX_PANE", "not-a-pane")
	if _, pane := tmuxContext(); pane != "" {
		t.Errorf("pane = %q, want \"\" for malformed $TMUX_PANE", pane)
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
	if ev.TmuxPane != "" {
		t.Errorf("tmux pane with TMUX unset = %q, want \"\"", ev.TmuxPane)
	}
}

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

	agents, err := agentRoster(db, 30*time.Minute, 0, now)
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

	counts, err := activeCounts(db, time.Hour, time.Now(), nil)
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

// A session whose tmux_session capture is sometimes blank must not split into a
// second, unlabelled activity row (#116): blanks inherit the session's latest
// name (s1's blank joins "new", not "old"), non-blank rows keep their own label,
// and the carry never crosses session_id (s2, never named, stays blank; nor do
// session-less events, which never share a session).
func TestActiveCounts_CarriesTmuxSessionAcrossBlankCaptures(t *testing.T) {
	db, err := openDB(filepath.Join(t.TempDir(), "events.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	now := time.Date(2026, 5, 17, 12, 0, 0, 0, time.UTC)
	at := func(minAgo int) string { return now.Add(-time.Duration(minAgo) * time.Minute).Format(time.RFC3339) }
	insertEvent(db, Event{TS: at(5), SourceApp: "myapp", Branch: "fix-116", TmuxSession: "old", SessionID: "s1", EventType: "PreToolUse", PayloadJSON: "{}"})
	insertEvent(db, Event{TS: at(3), SourceApp: "myapp", Branch: "fix-116", SessionID: "s1", EventType: "PreToolUse", PayloadJSON: "{}"})
	insertEvent(db, Event{TS: at(1), SourceApp: "myapp", Branch: "fix-116", TmuxSession: "new", SessionID: "s1", EventType: "Stop", PayloadJSON: "{}"})
	insertEvent(db, Event{TS: at(2), SourceApp: "myapp", Branch: "fix-116", SessionID: "s2", EventType: "PreToolUse", PayloadJSON: "{}"})
	insertEvent(db, Event{TS: at(4), SourceApp: "myapp", Branch: "legacy", TmuxSession: "stray", EventType: "PreToolUse", PayloadJSON: "{}"})
	insertEvent(db, Event{TS: at(2), SourceApp: "myapp", Branch: "legacy", EventType: "PreToolUse", PayloadJSON: "{}"})

	counts, err := activeCounts(db, time.Hour, now, nil)
	if err != nil {
		t.Fatal(err)
	}
	type key struct{ branch, tmux string }
	got := map[key]int{}
	for _, c := range counts {
		got[key{c.Branch, c.TmuxSession}] = c.Count
	}
	want := map[key]int{
		{"fix-116", "new"}: 2, {"fix-116", "old"}: 1, {"fix-116", ""}: 1,
		{"legacy", "stray"}: 1, {"legacy", ""}: 1,
	}
	if !maps.Equal(got, want) {
		t.Errorf("(branch, tmux_session) -> count = %v, want %v (blank should join the latest name, and not cross session_id)", got, want)
	}
}

// Live labels override stored ones (#180): two sessions relabelled to the same
// name on one branch merge into one summed row; an unmapped session keeps its own.
func TestActiveCounts_LiveLabels(t *testing.T) {
	db, err := openDB(filepath.Join(t.TempDir(), "events.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	now := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
	ts := now.Add(-time.Minute).Format(time.RFC3339)
	for _, e := range []Event{
		{SessionID: "s1"}, {SessionID: "s1"},
		{SessionID: "s2", TmuxSession: "old", Rebasing: true}, {SessionID: "s2", TmuxSession: "old"},
		{SessionID: "s3", TmuxSession: "kept"}, {SessionID: "s3", TmuxSession: "kept"}, {SessionID: "s3", TmuxSession: "kept"},
	} {
		e.TS, e.SourceApp, e.Branch, e.EventType, e.PayloadJSON = ts, "myapp", "fix-180", "PreToolUse", "{}"
		insertEvent(db, e)
	}

	counts, err := activeCounts(db, time.Hour, now, map[string]string{"s1": "work", "s2": "work"})
	if err != nil {
		t.Fatal(err)
	}
	got, rebasing := map[string]int{}, map[string]bool{}
	for _, c := range counts {
		got[c.TmuxSession], rebasing[c.TmuxSession] = c.Count, c.Rebasing
	}
	if want := map[string]int{"work": 4, "kept": 3}; !maps.Equal(got, want) || len(counts) != 2 {
		t.Errorf("tmux_session -> count = %v (%d rows), want %v", got, len(counts), want)
	}
	// SQL order is kept(3) ahead of the split s1/s2 rows (2 each); only the
	// post-merge re-sort puts the summed work row (4) first.
	if len(counts) > 0 && counts[0].TmuxSession != "work" {
		t.Errorf("counts[0] = %q, want work (re-sort after merge)", counts[0].TmuxSession)
	}
	if !rebasing["work"] {
		t.Error("merged work row lost Rebasing from s2")
	}
}

// A captured tmux pane id survives a write/read round-trip.
func TestTmuxPanePersists(t *testing.T) {
	db, err := openDB(filepath.Join(t.TempDir(), "events.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	now := time.Now().UTC().Format(time.RFC3339)
	insertEvent(db, Event{TS: now, SourceApp: "myapp", TmuxPane: "%7", EventType: "PreToolUse", PayloadJSON: "{}"})

	var got string
	if err := db.QueryRow(`SELECT tmux_pane FROM events LIMIT 1`).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != "%7" {
		t.Errorf("tmux_pane = %q, want %%7", got)
	}
}

// The roster folds in the latest tmux pane id per session (last write wins).
func TestAgentRoster_CarriesTmuxPane(t *testing.T) {
	db, err := openDB(filepath.Join(t.TempDir(), "events.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	now := time.Now().UTC()
	at := func(mins int) string { return now.Add(time.Duration(-mins) * time.Minute).Format(time.RFC3339) }
	insertEvent(db, Event{TS: at(5), SourceApp: "myapp", Branch: "b", TmuxPane: "%1", SessionID: "s1", EventType: "PreToolUse", PayloadJSON: "{}"})
	insertEvent(db, Event{TS: at(1), SourceApp: "myapp", Branch: "b", TmuxPane: "%2", SessionID: "s1", EventType: "Stop", PayloadJSON: "{}"})

	agents, err := agentRoster(db, 30*time.Minute, 0, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(agents) != 1 || agents[0].TmuxPane != "%2" {
		t.Fatalf("want 1 agent with pane %%2 (last write wins), got %+v", agents)
	}
}

// A later event whose pane capture transiently failed ("") must not clobber the
// pane id the session recorded earlier — otherwise the live-peek control blinks
// out whenever the newest event happened to miss the capture.
func TestAgentRoster_EmptyTmuxPaneDoesNotClobber(t *testing.T) {
	db, err := openDB(filepath.Join(t.TempDir(), "events.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	now := time.Now().UTC()
	at := func(mins int) string { return now.Add(time.Duration(-mins) * time.Minute).Format(time.RFC3339) }
	insertEvent(db, Event{TS: at(5), SourceApp: "myapp", Branch: "b", TmuxPane: "%1", SessionID: "s1", EventType: "PreToolUse", PayloadJSON: "{}"})
	insertEvent(db, Event{TS: at(1), SourceApp: "myapp", Branch: "b", TmuxPane: "", SessionID: "s1", EventType: "Stop", PayloadJSON: "{}"})

	agents, err := agentRoster(db, 30*time.Minute, 0, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(agents) != 1 || agents[0].TmuxPane != "%1" {
		t.Fatalf("want 1 agent keeping pane %%1 (empty later capture must not clobber), got %+v", agents)
	}
}

// A pane runs one Claude at a time, so a SessionStart in a pane after another
// session's last event there means that session's process is gone, even though
// it never sent SessionEnd (a hard kill, or an exit that skipped the hook).
func TestAgentRoster_DropsSessionSupersededInPane(t *testing.T) {
	db, err := openDB(filepath.Join(t.TempDir(), "events.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	now := time.Now().UTC()
	evS := func(secs int, sess, pane, tmuxSess, etype, payload string) {
		ts := now.Add(time.Duration(-secs) * time.Second).Format(time.RFC3339)
		insertEvent(db, Event{TS: ts, SourceApp: "myapp", Branch: "main", TmuxSession: tmuxSess, TmuxPane: pane, SessionID: sess, EventType: etype, PayloadJSON: payload})
	}
	ev := func(mins int, sess, pane, tmuxSess, etype, payload string) {
		evS(mins*60, sess, pane, tmuxSess, etype, payload)
	}
	// old exits without SessionEnd; new starts in the same pane.
	ev(50, "old", "%1", "app", "SessionStart", `{"source":"startup"}`)
	ev(45, "old", "%1", "app", "Stop", "{}")
	// same pane id under another tmux server/session label, last heard before
	// new starts: not evidence, so only the label keeps it.
	ev(44, "elsewhere", "%1", "other", "Stop", "{}")
	ev(40, "new", "%1", "app", "SessionStart", `{"source":"startup"}`)
	ev(39, "new", "%1", "app", "Stop", "{}")
	// parent spawns a headless child in its own pane, then keeps going: the
	// child's start must not hide the parent, whose later events prove it lives.
	ev(30, "parent", "%2", "app", "SessionStart", `{"source":"startup"}`)
	ev(29, "child", "%2", "app", "SessionStart", `{"source":"startup"}`)
	ev(28, "parent", "%2", "app", "Stop", "{}")
	// A subagent's event counts as its session's own.
	ev(27, "sub-parent", "%4", "app", "Stop", "{}")
	ev(26, "sub-child", "%4", "app", "SessionStart", `{"source":"startup"}`)
	ev(25, "sub-parent", "%4", "app", "SubagentStop", `{"agent_id":"a1"}`)
	// A parent mid tool call (its Bash running claude -p) emits nothing until
	// the call returns, so the child's start must not hide it meanwhile.
	evS(120, "busy", "%5", "app", "PreToolUse", "{}")
	evS(60, "busy-child", "%5", "app", "SessionStart", `{"source":"startup"}`)
	// ...but past staleInFlightSecs a hard kill mid tool call is the likelier story.
	ev(30, "stale-busy", "%11", "app", "PreToolUse", "{}")
	ev(29, "stale-busy-next", "%11", "app", "SessionStart", `{"source":"startup"}`)
	// A child a hook launches starts within supersedeGrace of the parent's Stop.
	evS(300, "quick", "%8", "app", "Stop", "{}")
	evS(295, "quick-child", "%8", "app", "SessionStart", `{"source":"startup"}`)
	// The grace boundary: exactly supersedeGrace later supersedes, a second less does not.
	evS(400, "at-grace", "%13", "app", "Stop", "{}")
	evS(370, "at-grace-next", "%13", "app", "SessionStart", `{"source":"startup"}`)
	evS(400, "in-grace", "%14", "app", "Stop", "{}")
	evS(371, "in-grace-child", "%14", "app", "SessionStart", `{"source":"startup"}`)
	// The grace still applies once a mid-tool session's in-flight bound lapses.
	ev(40, "stale-busy-quick", "%15", "app", "PreToolUse", "{}")
	evS(40*60-5, "stale-busy-quick-child", "%15", "app", "SessionStart", `{"source":"startup"}`)
	// A running background shell may launch the child at any time.
	ev(35, "shelled", "%16", "app", "PostToolUse", `{"background_task_id":"t1"}`)
	ev(34, "shelled", "%16", "app", "Stop", "{}")
	ev(20, "shelled-child", "%16", "app", "SessionStart", `{"source":"startup"}`)
	// A subagent's SessionStart does not take over the pane.
	ev(15, "sub-start", "%9", "app", "Stop", "{}")
	ev(14, "sub-starter", "%9", "app", "SessionStart", `{"agent_id":"a1"}`)
	// Only the pane's current holder ending releases it: an earlier holder's
	// late SessionEnd must not.
	ev(13, "ender", "%10", "app", "SessionStart", `{"source":"startup"}`)
	ev(12, "old2", "%10", "app", "Stop", "{}")
	ev(11, "new2", "%10", "app", "SessionStart", `{"source":"startup"}`)
	ev(10, "ender", "%10", "app", "SessionEnd", "{}")
	// `clodhopper end` writes its SessionEnd with no tmux fields; it still
	// releases the pane.
	ev(9, "q2", "%12", "app", "Stop", "{}")
	ev(8, "q2-child", "%12", "app", "SessionStart", `{"source":"startup"}`)
	ev(7, "q2-child", "", "", "SessionEnd", "{}")
	// A child that has ended no longer occupies the pane, even if the parent
	// (waiting for the user, or on a background shell) never speaks again.
	ev(22, "quiet", "%6", "app", "Stop", "{}")
	ev(21, "quiet-child", "%6", "app", "SessionStart", `{"source":"startup"}`)
	ev(20, "quiet-child", "%6", "app", "SessionEnd", "{}")
	// No pane or no label: nothing to compare, so nothing is superseded.
	ev(19, "nopane-a", "", "", "Stop", "{}")
	ev(18, "nopane-b", "", "", "SessionStart", `{"source":"startup"}`)
	ev(17, "nolabel-a", "%7", "", "Stop", "{}")
	ev(16, "nolabel-b", "%7", "", "SessionStart", `{"source":"startup"}`)
	// resuming old later makes it current again and supersedes the other.
	ev(6, "r-old", "%3", "app", "Stop", "{}")
	ev(5, "r-new", "%3", "app", "SessionStart", `{"source":"startup"}`)
	ev(4, "r-new", "%3", "app", "Stop", "{}")
	ev(3, "r-old", "%3", "app", "SessionStart", `{"source":"resume"}`)
	ev(2, "r-old", "%3", "app", "Stop", "{}")

	agents, err := agentRoster(db, time.Hour, 0, now)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, a := range agents {
		got = append(got, a.SessionID)
	}
	slices.Sort(got)
	want := []string{
		"at-grace-next", "busy", "busy-child", "child", "elsewhere", "in-grace", "in-grace-child", "new", "new2", "nolabel-a", "nolabel-b", "nopane-a", "nopane-b",
		"parent", "q2", "quick", "quick-child", "quiet", "r-old", "shelled", "shelled-child", "stale-busy-next", "stale-busy-quick", "stale-busy-quick-child", "sub-child", "sub-parent", "sub-start",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("sessions = %v, want %v", got, want)
	}
}

// A session id seen only on subagent lifecycle events (an orphan SubagentStop)
// has no main agent to report on: it used to sit on the roster as "working"
// forever, because nothing ever set its last event or timestamp.
func TestAgentRoster_DropsSubagentOnlySession(t *testing.T) {
	db, err := openDB(filepath.Join(t.TempDir(), "events.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	now := time.Now().UTC()
	ts := now.Add(-time.Minute).Format(time.RFC3339)
	insertEvent(db, Event{TS: ts, SourceApp: "myapp", Branch: "main", TmuxPane: "%1", SessionID: "ghost", EventType: "SubagentStop", PayloadJSON: `{"agent_id":"a1","agent_type":""}`})
	insertEvent(db, Event{TS: ts, SourceApp: "myapp", Branch: "main", TmuxPane: "%2", SessionID: "real", EventType: "Stop", PayloadJSON: "{}"})

	agents, err := agentRoster(db, time.Hour, 0, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(agents) != 1 || agents[0].SessionID != "real" {
		t.Fatalf("want only session real, got %+v", agents)
	}
}

func TestStartGap(t *testing.T) {
	for _, tc := range []struct {
		last, next string
		want       time.Duration
	}{
		{"2026-01-01T00:00:00Z", "2026-01-01T00:00:30Z", 30 * time.Second},
		{"2026-01-01T00:00:30Z", "2026-01-01T00:00:00Z", -30 * time.Second},
		{"bogus", "2026-01-01T00:00:00Z", -1},
		{"2026-01-01T00:00:00Z", "", -1},
	} {
		if got := startGap(tc.last, tc.next); got != tc.want {
			t.Errorf("startGap(%q, %q) = %v, want %v", tc.last, tc.next, got, tc.want)
		}
	}
}
