package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// cleanSessionName keeps only the text after the LAST unrenderable Nerd Font /
// devicon glyph (Unicode Private Use Area) — these glyphs are decorative
// prefixes/separators, so the meaningful name follows the final one — then drops
// control characters and collapses alignment padding. A name with no PUA glyph
// is left intact (a real emoji is not PUA, so it survives).
func TestCleanSessionName(t *testing.T) {
	cases := []struct{ name, in, want string }{
		{
			"drops everything up to and including the PUA glyph (even a leading emoji)",
			"👟  fix-2482             Blog post funniest / cleverest / most [in progress]",
			"Blog post funniest / cleverest / most [in progress]",
		},
		{
			// The user's real session: an icon, the name, more padding, a second
			// icon, the name again. Keeping the tail after the LAST glyph yields the
			// single clean name (not the doubled "name name" the old strip produced).
			"repeated icon-prefixed name keeps only the tail after the last glyph",
			"  tmux-session-name    tmux-session-name",
			"tmux-session-name",
		},
		{"emoji is not PUA, so it and the text after it are kept", "👟 fix-2482", "👟 fix-2482"},
		{"plain name unchanged", "my-session", "my-session"},
		{"collapses whitespace and drops control chars", "  hi\tthere\n", "hi there"},
		{"all-glyph name becomes empty", "", ""},
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

// Inside a real tmux session, tmuxContext matches tmux's own display-message.
// Skipped unless the suite runs inside tmux with the binary present — tmuxContext
// reads ambient $TMUX, so it cannot be faked hermetically.
func TestTmuxContext_InTmux(t *testing.T) {
	if os.Getenv("TMUX") == "" {
		t.Skip("not running inside tmux")
	}
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not available")
	}
	out, err := exec.Command("tmux", "display-message", "-p", "#{pane_id}\t#{session_name}").Output()
	if err != nil {
		t.Skipf("tmux display-message failed: %v", err)
	}
	parts := strings.SplitN(strings.TrimRight(string(out), "\n"), "\t", 2)
	wantPane := parts[0]
	wantSess := truncate(scrubString(cleanSessionName(parts[1])), maxFieldLen)
	sess, pane := tmuxContext()
	if sess != wantSess {
		t.Errorf("tmuxContext() session = %q, want %q", sess, wantSess)
	}
	if pane != wantPane {
		t.Errorf("tmuxContext() pane = %q, want %q", pane, wantPane)
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

	agents, err := agentRoster(db, 30*time.Minute, now)
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

	agents, err := agentRoster(db, 30*time.Minute, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(agents) != 1 || agents[0].TmuxPane != "%1" {
		t.Fatalf("want 1 agent keeping pane %%1 (empty later capture must not clobber), got %+v", agents)
	}
}
