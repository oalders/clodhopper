package main

import (
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func testDB(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "events.db")
}

func TestOpenInsertQuery(t *testing.T) {
	db, err := openDB(testDB(t))
	if err != nil {
		t.Fatalf("openDB: %v", err)
	}
	defer db.Close()

	ev := Event{
		TS:          time.Now().UTC().Format(time.RFC3339),
		SourceApp:   "myapp",
		Cwd:         "/repo",
		SessionID:   "sess1",
		EventType:   "PreToolUse",
		ToolName:    "Bash",
		Summary:     "Bash: git status",
		PayloadJSON: `{"ok":true}`,
	}
	if err := insertEvent(db, ev); err != nil {
		t.Fatalf("insertEvent: %v", err)
	}

	got, err := queryEvents(db, EventFilter{Limit: 10})
	if err != nil {
		t.Fatalf("queryEvents: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("want 1 event, got %d", len(got))
	}
	if got[0].Summary != "Bash: git status" || got[0].SourceApp != "myapp" {
		t.Errorf("round-trip mismatch: %+v", got[0])
	}
}

func TestQueryFilters(t *testing.T) {
	db, _ := openDB(testDB(t))
	defer db.Close()
	now := time.Now().UTC().Format(time.RFC3339)
	insertEvent(db, Event{TS: now, SourceApp: "myapp", EventType: "PreToolUse", PayloadJSON: "{}"})
	insertEvent(db, Event{TS: now, SourceApp: "other", EventType: "PreToolUse", PayloadJSON: "{}"})
	insertEvent(db, Event{TS: now, SourceApp: "myapp", EventType: "SessionStart", PayloadJSON: "{}"})

	bySource, _ := queryEvents(db, EventFilter{SourceApp: "myapp"})
	if len(bySource) != 2 {
		t.Errorf("source filter: want 2, got %d", len(bySource))
	}
	byType, _ := queryEvents(db, EventFilter{EventType: "SessionStart"})
	if len(byType) != 1 {
		t.Errorf("type filter: want 1, got %d", len(byType))
	}

	apps, _ := distinctSourceApps(db)
	if len(apps) != 2 {
		t.Errorf("distinct sources: want 2, got %d (%v)", len(apps), apps)
	}
}

func TestPruneOldDeletesOnlyOld(t *testing.T) {
	db, _ := openDB(testDB(t))
	defer db.Close()

	old := time.Now().UTC().AddDate(0, 0, -30).Format(time.RFC3339)
	recent := time.Now().UTC().Format(time.RFC3339)
	insertEvent(db, Event{TS: old, SourceApp: "myapp", EventType: "PreToolUse", PayloadJSON: "{}"})
	insertEvent(db, Event{TS: recent, SourceApp: "myapp", EventType: "PreToolUse", PayloadJSON: "{}"})

	n, err := pruneOld(db, 14)
	if err != nil {
		t.Fatalf("pruneOld: %v", err)
	}
	if n != 1 {
		t.Errorf("want 1 pruned, got %d", n)
	}
	remaining, _ := queryEvents(db, EventFilter{})
	if len(remaining) != 1 {
		t.Errorf("want 1 remaining, got %d", len(remaining))
	}
}

func TestConcurrentWritersWAL(t *testing.T) {
	path := testDB(t)
	const writers = 8
	const perWriter = 10
	var wg sync.WaitGroup
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			db, err := openDB(path)
			if err != nil {
				t.Errorf("openDB: %v", err)
				return
			}
			defer db.Close()
			for i := 0; i < perWriter; i++ {
				insertEvent(db, Event{
					TS:          time.Now().UTC().Format(time.RFC3339),
					SourceApp:   "myapp",
					EventType:   "PreToolUse",
					PayloadJSON: "{}",
				})
			}
		}()
	}
	wg.Wait()

	db, _ := openDB(path)
	defer db.Close()
	got, _ := queryEvents(db, EventFilter{})
	if len(got) != writers*perWriter {
		t.Errorf("want %d events from concurrent writers, got %d", writers*perWriter, len(got))
	}
}
