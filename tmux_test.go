package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// cleanSessionName strips unrenderable Nerd Font / devicon glyphs (Unicode
// Private Use Area) and control characters, and collapses alignment padding —
// so a real tmux name like the one below shows cleanly in a browser. The leading
// emoji (a real, renderable codepoint) is kept.
func TestCleanSessionName(t *testing.T) {
	cases := []struct{ name, in, want string }{
		{
			"strips PUA glyph and padding, keeps emoji and text",
			"👟  fix-2482             Blog post funniest / cleverest / most [in progress]",
			"👟 fix-2482 Blog post funniest / cleverest / most [in progress]",
		},
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
	want := truncate(scrubString(cleanSessionName(string(out))), maxFieldLen)
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
