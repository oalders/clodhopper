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
