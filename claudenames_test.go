package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"testing"
	"time"
)

func writeSessionFile(t *testing.T, dir, file, body string, mtime time.Time) {
	t.Helper()
	p := filepath.Join(dir, file)
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(p, mtime, mtime); err != nil {
		t.Fatal(err)
	}
}

func TestLookupClaudeNames(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	sess := func(id, name string) string { return fmt.Sprintf(`{"sessionId":%q,"name":%q,"cwd":"/w"}`, id, name) }
	writeSessionFile(t, dir, "1.json", sess("ok", "fix-4265-a2"), now)
	writeSessionFile(t, dir, "2.json", `{"sessionId":"noname"}`, now)
	writeSessionFile(t, dir, "3.json", `{"sessionId":"bad",`, now)
	writeSessionFile(t, dir, "4.json", sess("long", strings.Repeat("a", claudeNameMaxRunes+1)), now)
	// At the cap in runes but twice that in bytes: the cap counts runes.
	writeSessionFile(t, dir, "4b.json", sess("runes", strings.Repeat("é", claudeNameMaxRunes)), now)
	writeSessionFile(t, dir, "5.json", sess("shell", "fix$(id)"), now)
	writeSessionFile(t, dir, "6.json", sess("secret", "ghp_"+strings.Repeat("a", 36)), now)
	writeSessionFile(t, dir, "7.json", sess("dup", "older"), now.Add(-time.Hour))
	writeSessionFile(t, dir, "8.json", sess("dup", "newer"), now)
	writeSessionFile(t, dir, "9.json", sess("stale", "named"), now.Add(-time.Hour))
	writeSessionFile(t, dir, "10.json", sess("stale", "bad;name"), now)
	writeSessionFile(t, dir, "11.pid.key", sess("key", "secret-key"), now)
	if err := os.Symlink(filepath.Join(dir, "11.pid.key"), filepath.Join(dir, "y.json")); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(filepath.Join(dir, "x.json"), 0o600); err != nil {
		t.Fatal(err)
	}

	done := make(chan map[string]string)
	go func() { done <- lookupClaudeNames(dir) }()
	var got map[string]string
	select {
	case got = <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("lookupClaudeNames hung (on the FIFO?)")
	}
	want := map[string]string{"ok": "fix-4265-a2", "dup": "newer", "runes": strings.Repeat("é", claudeNameMaxRunes)}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

// ReadDir's type check skips a FIFO first, so this covers readClaudeSession's own
// O_NONBLOCK + Stat defence against one swapped in after the listing.
func TestReadClaudeSession_FIFO(t *testing.T) {
	p := filepath.Join(t.TempDir(), "x.json")
	if err := syscall.Mkfifo(p, 0o600); err != nil {
		t.Fatal(err)
	}
	done := make(chan bool)
	go func() { _, _, _, ok := readClaudeSession(p); done <- ok }()
	select {
	case ok := <-done:
		if ok {
			t.Error("a FIFO was read as a sessions file")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("readClaudeSession hung on the FIFO")
	}
}

func TestLookupClaudeNames_MissingDir(t *testing.T) {
	if got := lookupClaudeNames(filepath.Join(t.TempDir(), "absent")); len(got) != 0 {
		t.Errorf("got %v, want nothing", got)
	}
}

func TestViewSignature_TracksClaudeName(t *testing.T) {
	base := dashboardData{Agents: []Agent{{SessionID: "s1", Status: statusWaiting}}}
	named := dashboardData{Agents: []Agent{{SessionID: "s1", Status: statusWaiting, ClaudeName: "fix-1"}}}
	if viewSignature(base) == viewSignature(named) {
		t.Error("signature unchanged after a Claude session name appeared")
	}
}

// Rendered through handleDashboard, so buildDashboardData's wiring of the names
// cache into both the roster and the events table is covered too.
func TestHandleDashboard_ClaudeName(t *testing.T) {
	db, _ := openDB(filepath.Join(t.TempDir(), "events.db"))
	defer db.Close()
	ts := time.Now().UTC().Add(-2 * time.Minute).Format(time.RFC3339)
	ids := []string{"aaaaaaaa-1111", "bbbbbbbb-2222", "cccccccc-3333"}
	for i, id := range ids {
		insertEvent(db, Event{TS: ts, SourceApp: "myapp", Branch: fmt.Sprintf("fix-%d", i), Cwd: fmt.Sprintf("/w/fix-%d", i),
			SessionID: id, EventType: "Stop", PayloadJSON: "{}"})
	}
	dir := t.TempDir()
	// Two sessions share a name; the third has no sessions file.
	writeSessionFile(t, dir, "1.json", `{"sessionId":"aaaaaaaa-1111","name":"fix-4265-a2"}`, time.Now())
	writeSessionFile(t, dir, "2.json", `{"sessionId":"bbbbbbbb-2222","name":"fix-4265-a2"}`, time.Now())

	req := httptest.NewRequest(http.MethodGet, "/?debug=1", nil)
	rec := httptest.NewRecorder()
	handleDashboard(rec, req, db, newCICache(), &peekConfig{cache: newPaneCache(), names: newClaudeNameCache(), namesDir: dir}, &actionConfig{})
	body := rec.Body.String()

	if !strings.Contains(body, "3 live") {
		t.Fatalf("both same-named sessions and the unnamed one should each render a row:\n%s", body)
	}
	for _, id := range ids[:2] {
		btn := regexp.MustCompile(`<td class="branch"[^\n]*<button type="button" class="copyname" data-session="` + id + `" data-name="fix-4265-a2"`)
		if !btn.MatchString(body) {
			t.Errorf("roster branch cell for %s has no copy-name button:\n%s", id, body)
		}
	}
	if strings.Contains(body, `class="copyname" data-session="cccccccc-3333"`) {
		t.Error("a session with no name must not offer a copy-name button")
	}
	// Counted per table so a regression in one cannot be masked by the other.
	roster, events, ok := strings.Cut(body, `<table class="events">`)
	if !ok {
		t.Fatalf("no events table:\n%s", body)
	}
	for _, c := range []struct {
		table, html, cell string
		want              int
	}{
		{"roster", roster, `<span title="fix-4265-a2">fix-4265-a2</span>`, 2},
		{"events", events, `<span title="fix-4265-a2">fix-4265-a2</span>`, 2},
		{"roster", roster, `<span title="cccccccc">cccccccc</span>`, 1},
		{"events", events, `<span title="cccccccc">cccccccc</span>`, 1},
	} {
		if n := strings.Count(c.html, c.cell); n != c.want {
			t.Errorf("%s: %s in %d id cells, want %d", c.table, c.cell, n, c.want)
		}
	}
}
