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
	want := map[string]string{"ok": "fix-4265-a2", "dup": "newer"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("got %v, want %v", got, want)
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
	// Each session renders one roster id cell (debug) and one events id cell.
	if n := strings.Count(body, `<span title="fix-4265-a2">fix-4265-a2</span>`); n != 4 {
		t.Errorf("name replaced the short id in %d id cells, want 4", n)
	}
	if n := strings.Count(body, `<span title="cccccccc">cccccccc</span>`); n != 2 {
		t.Errorf("unnamed session shows its short id in %d id cells, want 2", n)
	}
}
