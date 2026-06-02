package main

import (
	"database/sql"
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

	agents, err := agentRoster(db, 16*time.Hour, now)
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

func TestToolCallColumnsRoundTrip(t *testing.T) {
	db, _ := openDB(testDB(t))
	defer db.Close()
	now := time.Now().UTC().Format(time.RFC3339)
	insertEvent(db, Event{TS: now, SourceApp: "a", SessionID: "s", EventType: "PostToolUse",
		ToolName: "Bash", ToolUseID: "toolu_abc", DurationMs: sql.NullInt64{Int64: 246, Valid: true}, PayloadJSON: "{}"})
	insertEvent(db, Event{TS: now, SourceApp: "a", SessionID: "s", EventType: "PreToolUse",
		ToolName: "Bash", ToolUseID: "toolu_abc", PayloadJSON: "{}"})

	got, err := queryEvents(db, EventFilter{Limit: 10})
	if err != nil {
		t.Fatalf("queryEvents: %v", err)
	}
	var post, pre *Event
	for i := range got {
		switch got[i].EventType {
		case "PostToolUse":
			post = &got[i]
		case "PreToolUse":
			pre = &got[i]
		}
	}
	if post == nil || pre == nil {
		t.Fatalf("expected both rows, got %+v", got)
	}
	if post.ToolUseID != "toolu_abc" {
		t.Errorf("post tool_use_id = %q, want toolu_abc", post.ToolUseID)
	}
	if !post.DurationMs.Valid || post.DurationMs.Int64 != 246 {
		t.Errorf("post duration = %+v, want {246 true}", post.DurationMs)
	}
	if pre.DurationMs.Valid {
		t.Errorf("pre duration should be NULL, got %+v", pre.DurationMs)
	}
}

func TestFormatDuration(t *testing.T) {
	cases := []struct {
		ms   int64
		want string
	}{
		{0, "0ms"}, {2, "2ms"}, {246, "246ms"}, {999, "999ms"},
		{1000, "1.0s"}, {1050, "1.1s"}, {3100, "3.1s"}, {59000, "59.0s"},
		{60000, "1m"}, {64000, "1m04s"}, {120000, "2m"}, {125000, "2m05s"},
	}
	for _, c := range cases {
		if got := formatDuration(c.ms); got != c.want {
			t.Errorf("formatDuration(%d) = %q, want %q", c.ms, got, c.want)
		}
	}
}
