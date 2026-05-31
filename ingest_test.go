package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// withStdin replaces os.Stdin with a file containing data for the duration of fn.
func withStdin(t *testing.T, data string, fn func()) {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "stdin")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(data); err != nil {
		t.Fatal(err)
	}
	f.Seek(0, 0)
	orig := os.Stdin
	os.Stdin = f
	defer func() { os.Stdin = orig; f.Close() }()
	fn()
}

func TestBuildEvent_ExtractsAndScrubs(t *testing.T) {
	raw := []byte(`{
		"hook_event_name": "PreToolUse",
		"session_id": "abc",
		"cwd": "/home/user/repo",
		"tool_name": "Bash",
		"tool_input": {"command": "echo HCLOUD_TOKEN=leakme0123456789"}
	}`)
	ev := buildEvent(raw, "myapp")

	if ev.EventType != "PreToolUse" || ev.ToolName != "Bash" || ev.SessionID != "abc" {
		t.Errorf("fields not extracted: %+v", ev)
	}
	if ev.SourceApp != "myapp" {
		t.Errorf("source app: %q", ev.SourceApp)
	}
	if !strings.HasPrefix(ev.Summary, "Bash: echo") {
		t.Errorf("summary: %q", ev.Summary)
	}
	if strings.Contains(ev.Summary, "leakme0123456789") || strings.Contains(ev.PayloadJSON, "leakme0123456789") {
		t.Errorf("secret survived in event: summary=%q payload=%q", ev.Summary, ev.PayloadJSON)
	}
}

func TestBuildSummary_UserPromptPreviewScrubbedAndBounded(t *testing.T) {
	long := "please run deploy with API_KEY=topsecretvalue and then " + strings.Repeat("z", 200)
	p := map[string]any{"hook_event_name": "UserPromptSubmit", "prompt": long}
	s := buildSummary("UserPromptSubmit", "", p)

	if !strings.Contains(s, "chars)") {
		t.Errorf("expected char count, got %q", s)
	}
	if strings.Contains(s, "topsecretvalue") {
		t.Errorf("secret leaked into prompt preview: %q", s)
	}
	if len([]rune(s)) > promptPreviewLen+60 { // count prefix + preview + ellipsis
		t.Errorf("preview not bounded: len=%d", len([]rune(s)))
	}
}

func TestRunIngest_WritesRow(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "events.db")
	t.Setenv("CLODHOPPER_DB", dbPath)

	withStdin(t, `{"hook_event_name":"SessionStart","source":"startup"}`, func() {
		if code := runIngest([]string{"--source-app", "myapp"}); code != 0 {
			t.Fatalf("runIngest exit=%d, want 0", code)
		}
	})

	db, err := openDB(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	got, _ := queryEvents(db, EventFilter{})
	if len(got) != 1 || got[0].EventType != "SessionStart" {
		t.Fatalf("want 1 SessionStart, got %+v", got)
	}
}

func TestRunIngest_DisabledNoOp(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "events.db")
	t.Setenv("CLODHOPPER_DB", dbPath)
	t.Setenv("CLODHOPPER_DISABLED", "1")

	withStdin(t, `{"hook_event_name":"PreToolUse"}`, func() {
		if code := runIngest([]string{"--source-app", "myapp"}); code != 0 {
			t.Fatalf("runIngest exit=%d, want 0", code)
		}
	})

	if _, err := os.Stat(dbPath); !os.IsNotExist(err) {
		t.Errorf("expected no DB created when disabled, stat err=%v", err)
	}
}

func TestRunIngest_MalformedJSONNeverFails(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "events.db")
	t.Setenv("CLODHOPPER_DB", dbPath)

	withStdin(t, `{this is not json`, func() {
		if code := runIngest([]string{"--source-app", "myapp"}); code != 0 {
			t.Fatalf("malformed input must exit 0, got %d", code)
		}
	})
	// A row is still written (event type Unknown) — capture must never break a
	// tool call, and a best-effort record is preferable to a lost event.
	db, _ := openDB(dbPath)
	defer db.Close()
	got, _ := queryEvents(db, EventFilter{})
	if len(got) != 1 || got[0].EventType != "Unknown" {
		t.Fatalf("want 1 Unknown event, got %+v", got)
	}
}

func TestRunIngest_EmptyStdinNoOp(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "events.db")
	t.Setenv("CLODHOPPER_DB", dbPath)
	withStdin(t, "   ", func() {
		if code := runIngest([]string{"--source-app", "myapp"}); code != 0 {
			t.Fatalf("empty stdin must exit 0, got %d", code)
		}
	})
	if _, err := os.Stat(dbPath); !os.IsNotExist(err) {
		t.Errorf("expected no DB created for empty stdin")
	}
}
