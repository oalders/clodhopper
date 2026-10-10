package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode"
)

func TestBuildEvent_ExtractsToolUseIDAndDuration(t *testing.T) {
	raw := []byte(`{"hook_event_name":"PostToolUse","tool_name":"Bash","tool_use_id":"toolu_xyz","duration_ms":246,"session_id":"s"}`)
	ev := buildEvent(raw, "app")
	if ev.ToolUseID != "toolu_xyz" {
		t.Errorf("tool_use_id = %q, want toolu_xyz", ev.ToolUseID)
	}
	if !ev.DurationMs.Valid || ev.DurationMs.Int64 != 246 {
		t.Errorf("duration = %+v, want {246 true}", ev.DurationMs)
	}
}

func TestBuildEvent_NoDurationWhenAbsent(t *testing.T) {
	raw := []byte(`{"hook_event_name":"PreToolUse","tool_name":"Bash","tool_use_id":"toolu_xyz"}`)
	ev := buildEvent(raw, "app")
	if ev.ToolUseID != "toolu_xyz" {
		t.Errorf("tool_use_id = %q, want toolu_xyz", ev.ToolUseID)
	}
	if ev.DurationMs.Valid {
		t.Errorf("duration should be NULL when absent, got %+v", ev.DurationMs)
	}
}

// cwd is the one free-text field the dashboard hands to the clipboard, and
// html/template leaves a newline in an attribute value alone — so a crafted
// payload could otherwise plant a newline-terminated command in a path the
// operator is about to paste into a terminal. Every control character goes; a
// real path has none.
func TestBuildEvent_StripsControlCharsFromCwd(t *testing.T) {
	raw := []byte(`{"hook_event_name":"Stop","session_id":"s","cwd":"/w/a\nrm -rf /\u0007\tb"}`)
	ev := buildEvent(raw, "app")
	if want := "/w/arm -rf /b"; ev.Cwd != want {
		t.Errorf("cwd = %q, want %q", ev.Cwd, want)
	}
	if strings.ContainsFunc(ev.Cwd, unicode.IsControl) {
		t.Errorf("control character survived in cwd: %q", ev.Cwd)
	}
}

// cwd is used as a path, not just displayed: the operator pastes it into a
// terminal and lookupCI shells `gh pr checks -C <cwd>` at it. maxFieldLen (300)
// sits well inside the range of real nested worktree paths, and a value cut
// there — with a "…" appended — names a directory that does not exist while
// looking like it might. So this field gets maxPathLen instead, above any real
// path, and stays bounded there.
func TestBuildEvent_CwdIsNotTruncatedAtTheFieldCap(t *testing.T) {
	long := "/home/me/" + strings.Repeat("nested/", 100) + "wt"
	if len([]rune(long)) <= maxFieldLen {
		t.Fatalf("test path is only %d runes; it must exceed maxFieldLen to be a test", len([]rune(long)))
	}
	raw := []byte(`{"hook_event_name":"Stop","session_id":"s","cwd":"` + long + `"}`)
	if got := buildEvent(raw, "app").Cwd; got != long {
		t.Errorf("cwd was altered: got %q (%d runes), want the path intact (%d runes)", got, len([]rune(got)), len([]rune(long)))
	}

	// Still bounded, though — just at a cap nothing genuine reaches.
	huge := "/" + strings.Repeat("a", maxPathLen+50)
	raw = []byte(`{"hook_event_name":"Stop","session_id":"s","cwd":"` + huge + `"}`)
	got := buildEvent(raw, "app").Cwd
	if n := len([]rune(got)); n != maxPathLen+1 { // +1 for the appended "…"
		t.Errorf("cwd = %d runes, want it truncated to maxPathLen (%d) plus the ellipsis", n, maxPathLen)
	}
	if !strings.HasSuffix(got, "…") {
		t.Errorf("a truncated cwd must be marked with the ellipsis the client refuses to copy: %q", got[len(got)-8:])
	}
}

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

func TestSlashCommand(t *testing.T) {
	tests := []struct {
		name      string
		eventType string
		prompt    string
		want      string
	}{
		{"leading slash", "UserPromptSubmit", "/code-review", "/code-review"},
		{"args dropped", "UserPromptSubmit", "/git-rebase main onto x", "/git-rebase"},
		{"prose mention", "UserPromptSubmit", "you can do it via /quick-pr", ""},
		{"leading whitespace", "UserPromptSubmit", "  /foo", "/foo"},
		{"bare slash", "UserPromptSubmit", "/", ""},
		{"empty prompt", "UserPromptSubmit", "", ""},
		{"non-prompt event", "PreToolUse", "/code-review", ""},
		{"multi-line", "UserPromptSubmit", "/foo\nmore text", "/foo"},
		{"token scrubbed", "UserPromptSubmit", "/deploy?API_KEY=hunter2", "/deploy?API_KEY=«redacted»"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := slashCommand(tt.eventType, tt.prompt); got != tt.want {
				t.Errorf("slashCommand(%q, %q) = %q, want %q", tt.eventType, tt.prompt, got, tt.want)
			}
		})
	}
}

func TestBuildEvent_SlashCommand(t *testing.T) {
	raw := []byte(`{"hook_event_name":"UserPromptSubmit","prompt":"/code-review --foo bar"}`)
	if ev := buildEvent(raw, "app"); ev.SlashCommand != "/code-review" {
		t.Errorf("SlashCommand = %q, want /code-review", ev.SlashCommand)
	}

	raw = []byte(`{"hook_event_name":"UserPromptSubmit","prompt":"please review my code"}`)
	if ev := buildEvent(raw, "app"); ev.SlashCommand != "" {
		t.Errorf("SlashCommand = %q, want empty for prose prompt", ev.SlashCommand)
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

// withStdout replaces os.Stdout with a temp file for the duration of fn and
// returns what fn wrote to it.
func withStdout(t *testing.T, fn func()) string {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "stdout")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	orig := os.Stdout
	os.Stdout = f
	defer func() { os.Stdout = orig }()
	fn()
	out, err := os.ReadFile(f.Name())
	if err != nil {
		t.Fatal(err)
	}
	return string(out)
}

func TestStamp(t *testing.T) {
	now := time.Date(2026, 10, 10, 14, 5, 9, 0, time.FixedZone("EDT", -4*3600))
	tests := []struct {
		name    string
		raw     string
		context bool
		want    string // "" means nothing printed
	}{
		{"stop", `{"hook_event_name":"Stop"}`, false,
			`{"systemMessage":"Finished 14:05:09"}`},
		{"notification", `{"hook_event_name":"Notification","message":"Claude needs your permission"}`, false,
			`{"systemMessage":"Claude needs your permission — 14:05:09"}`},
		{"notification without message", `{"hook_event_name":"Notification"}`, false,
			`{"systemMessage":"Waiting — 14:05:09"}`},
		{"message first chunk", `{"hook_event_name":"MessageDisplay","index":0,"delta":"Hello"}`, false,
			`{"hookSpecificOutput":{"displayContent":"[14:05:09] Hello","hookEventName":"MessageDisplay"}}`},
		{"message later chunk", `{"hook_event_name":"MessageDisplay","index":3,"delta":" world"}`, false,
			`{"hookSpecificOutput":{"displayContent":" world","hookEventName":"MessageDisplay"}}`},
		{"message malformed", `{"hook_event_name":"MessageDisplay","index":"0","delta":7}`, false, ""},
		{"other event", `{"hook_event_name":"PreToolUse","tool_name":"Bash"}`, false, ""},
		{"prompt, context off", `{"hook_event_name":"UserPromptSubmit","prompt":"hi"}`, false, ""},
		{"prompt, context on", `{"hook_event_name":"UserPromptSubmit","prompt":"hi"}`, true,
			`{"hookSpecificOutput":{"additionalContext":"Message sent at local time 14:05:09 EDT","hookEventName":"UserPromptSubmit"}}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.context {
				t.Setenv("CLODHOPPER_STAMP_CONTEXT", "1")
			}
			var buf bytes.Buffer
			stamp([]byte(tt.raw), now, &buf)
			got := buf.String()
			if tt.want == "" {
				if got != "" {
					t.Fatalf("want no output, got %q", got)
				}
				return
			}
			// Exactly one JSON object, and nothing after it.
			dec := json.NewDecoder(strings.NewReader(got))
			var obj any
			if err := dec.Decode(&obj); err != nil {
				t.Fatalf("output is not JSON: %q (%v)", got, err)
			}
			if dec.More() {
				t.Fatalf("want exactly one JSON object, got %q", got)
			}
			if got != tt.want+"\n" {
				t.Errorf("got  %q\nwant %q", got, tt.want+"\n")
			}
		})
	}
}

func TestRunIngest_MessageDisplayWritesNoRow(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "events.db")
	t.Setenv("CLODHOPPER_DB", dbPath)
	out := withStdout(t, func() {
		withStdin(t, `{"hook_event_name":"MessageDisplay","session_id":"s","index":0,"delta":"hi"}`, func() {
			runIngest([]string{"--source-app", "myapp"})
		})
	})
	if !strings.Contains(out, "] hi") {
		t.Errorf("want the stamped message, got %q", out)
	}
	if _, err := os.Stat(dbPath); !os.IsNotExist(err) {
		t.Errorf("MessageDisplay must not touch the DB, stat err=%v", err)
	}
}

func TestRunIngest_StampSurvivesUnwritableDB(t *testing.T) {
	// A regular file as the DB's parent directory makes openDB fail.
	blocker := filepath.Join(t.TempDir(), "blocker")
	if err := os.WriteFile(blocker, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLODHOPPER_DB", filepath.Join(blocker, "events.db"))
	var code int
	out := withStdout(t, func() {
		withStdin(t, `{"hook_event_name":"Stop","session_id":"s"}`, func() {
			code = runIngest([]string{"--source-app", "myapp"})
		})
	})
	if code != 0 {
		t.Fatalf("runIngest exit=%d, want 0", code)
	}
	if !strings.Contains(out, `"systemMessage":"Finished `) {
		t.Errorf("want the Finished stamp despite the DB failure, got %q", out)
	}
}

func TestRunIngest_StampsOff(t *testing.T) {
	t.Setenv("CLODHOPPER_DB", filepath.Join(t.TempDir(), "events.db"))
	t.Setenv("CLODHOPPER_STAMPS", "0")
	out := withStdout(t, func() {
		withStdin(t, `{"hook_event_name":"Stop","session_id":"s"}`, func() {
			runIngest([]string{"--source-app", "myapp"})
		})
	})
	if out != "" {
		t.Errorf("CLODHOPPER_STAMPS=0 must print nothing, got %q", out)
	}
}

func TestRunIngest_DisabledStillStamps(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "events.db")
	t.Setenv("CLODHOPPER_DB", dbPath)
	t.Setenv("CLODHOPPER_DISABLED", "1")
	out := withStdout(t, func() {
		withStdin(t, `{"hook_event_name":"Stop","session_id":"s"}`, func() {
			runIngest([]string{"--source-app", "myapp"})
		})
	})
	if !strings.Contains(out, `"systemMessage":"Finished `) {
		t.Errorf("DISABLED suppresses capture only; want the stamp, got %q", out)
	}
	if _, err := os.Stat(dbPath); !os.IsNotExist(err) {
		t.Errorf("expected no DB created when disabled, stat err=%v", err)
	}
}
