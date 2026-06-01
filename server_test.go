package main

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestHandleDashboard_RendersAndFilters(t *testing.T) {
	db, err := openDB(filepath.Join(t.TempDir(), "events.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	now := time.Now().UTC().Format(time.RFC3339)
	insertEvent(db, Event{TS: now, SourceApp: "myapp", EventType: "PreToolUse", ToolName: "Bash", Summary: "Bash: git status", PayloadJSON: "{}"})
	insertEvent(db, Event{TS: now, SourceApp: "other", EventType: "SessionStart", Summary: "SessionStart", PayloadJSON: "{}"})

	// No filter: both source apps present.
	body := getBody(t, db, "/")
	if !strings.Contains(body, "Bash: git status") || !strings.Contains(body, "SessionStart") {
		t.Errorf("dashboard missing events:\n%s", body)
	}

	// Filter by source_app.
	body = getBody(t, db, "/?source_app=other")
	if strings.Contains(body, "Bash: git status") {
		t.Errorf("source filter leaked myapp event:\n%s", body)
	}
	if !strings.Contains(body, "SessionStart") {
		t.Errorf("source filter dropped matching event:\n%s", body)
	}
}

func TestHandleDashboard_EscapesHTML(t *testing.T) {
	db, _ := openDB(filepath.Join(t.TempDir(), "events.db"))
	defer db.Close()
	now := time.Now().UTC().Format(time.RFC3339)
	insertEvent(db, Event{TS: now, SourceApp: "myapp", EventType: "PreToolUse", ToolName: "Bash", Summary: `<script>alert(1)</script>`, PayloadJSON: "{}"})

	body := getBody(t, db, "/")
	if strings.Contains(body, "<script>alert(1)</script>") {
		t.Errorf("summary was not HTML-escaped:\n%s", body)
	}
	if !strings.Contains(body, "&lt;script&gt;") {
		t.Errorf("expected escaped summary in output:\n%s", body)
	}
}

func TestHandleDashboard_RendersActivity(t *testing.T) {
	db, _ := openDB(filepath.Join(t.TempDir(), "events.db"))
	defer db.Close()
	now := time.Now().UTC().Format(time.RFC3339)
	insertEvent(db, Event{TS: now, SourceApp: "myapp", Branch: "fix-2499", EventType: "PreToolUse", PayloadJSON: "{}"})
	insertEvent(db, Event{TS: now, SourceApp: "myapp", Branch: "fix-2499", EventType: "Stop", PayloadJSON: "{}"})

	body := getBody(t, db, "/")
	if !strings.Contains(body, "Activity (last 30 min)") {
		t.Errorf("activity section missing:\n%s", body)
	}
}

func TestHandleState_ReturnsSignatureAndHTML(t *testing.T) {
	db, _ := openDB(filepath.Join(t.TempDir(), "events.db"))
	defer db.Close()
	now := time.Now().UTC().Format(time.RFC3339)
	insertEvent(db, Event{TS: now, SourceApp: "myapp", EventType: "PreToolUse", ToolName: "Bash", Summary: "Bash: git status", PayloadJSON: "{}"})

	req := httptest.NewRequest(http.MethodGet, "/api/state", nil)
	rec := httptest.NewRecorder()
	handleState(rec, req, db, newCICache())
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d", rec.Code)
	}
	var resp struct {
		Signature string `json:"signature"`
		HTML      string `json:"html"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v\nbody: %s", err, rec.Body.String())
	}
	if resp.Signature == "" {
		t.Error("empty signature")
	}
	if !strings.Contains(resp.HTML, "Bash: git status") {
		t.Errorf("fragment missing event:\n%s", resp.HTML)
	}
	// The fragment is the dynamic region only — no surrounding page chrome.
	if strings.Contains(resp.HTML, "<form") || strings.Contains(resp.HTML, "<script") {
		t.Errorf("fragment leaked page chrome:\n%s", resp.HTML)
	}
}

func TestViewSignature_IgnoresIdleButTracksEvents(t *testing.T) {
	base := dashboardData{
		Events:   []Event{{ID: 2}, {ID: 1}},
		Agents:   []Agent{{SessionID: "s1", Status: statusWorking, IdleSecs: 10, IdleSince: 100}},
		Activity: []SourceCount{{SourceApp: "myapp", Count: 2}},
	}

	// Only idle advanced (more seconds, earlier IdleSince) — signature must hold.
	idled := base
	idled.Agents = []Agent{{SessionID: "s1", Status: statusWorking, IdleSecs: 999, IdleSince: 1}}
	if viewSignature(base) != viewSignature(idled) {
		t.Error("signature changed on idle-only difference")
	}

	// A genuinely new event must move the signature.
	newEvent := base
	newEvent.Events = []Event{{ID: 3}, {ID: 2}, {ID: 1}}
	if viewSignature(base) == viewSignature(newEvent) {
		t.Error("signature unchanged after a new event")
	}

	// A status flip (e.g. agent went to "waiting for you") must move it too.
	flipped := base
	flipped.Agents = []Agent{{SessionID: "s1", Status: statusWaiting, IdleSecs: 10, IdleSince: 100}}
	if viewSignature(base) == viewSignature(flipped) {
		t.Error("signature unchanged after a status change")
	}
}

func TestSessColor(t *testing.T) {
	// Deterministic: same id -> same color across calls.
	first := sessColor("abc123")
	second := sessColor("abc123")
	if first != second {
		t.Errorf("sessColor not deterministic: %q != %q", first, second)
	}
	// Empty id -> empty string (no chip/tint).
	if got := sessColor(""); got != "" {
		t.Errorf("sessColor(\"\") = %q, want \"\"", got)
	}
	// Any non-empty id maps into the palette.
	for _, id := range []string{"a", "session-1", "7f3c9a01", "xyz", "deadbeef"} {
		if c := sessColor(id); !slices.Contains(sessPalette, c) {
			t.Errorf("sessColor(%q) = %q, not in palette", id, c)
		}
	}
}

func TestSessPaletteIsHex(t *testing.T) {
	re := regexp.MustCompile(`^#[0-9a-fA-F]{6}$`)
	if len(sessPalette) == 0 {
		t.Fatal("sessPalette is empty")
	}
	for _, c := range sessPalette {
		// Guards against a future non-hex token being silently blanked to
		// "ZgotmplZ" by html/template's style-attribute escaper.
		if !re.MatchString(c) {
			t.Errorf("palette entry %q is not a #rrggbb hex literal", c)
		}
	}
}

func getBody(t *testing.T, db *sql.DB, target string) string {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, target, nil)
	rec := httptest.NewRecorder()
	handleDashboard(rec, req, db, newCICache())
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d", rec.Code)
	}
	return rec.Body.String()
}
