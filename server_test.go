package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
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

func TestAssignSessColors(t *testing.T) {
	mkAgents := func(ids ...string) []Agent {
		out := make([]Agent, len(ids))
		for i, id := range ids {
			out[i] = Agent{SessionID: id, firstSeq: i} // firstSeq = arrival order
		}
		return out
	}

	// Distinctness: a roster no larger than the palette gets unique colors,
	// regardless of hash collisions — the greedy probe deconflicts them.
	ids := []string{"alpha", "bravo", "charlie", "delta", "echo", "foxtrot", "golf"}
	got := assignSessColors(mkAgents(ids...), nil)
	owner := map[string]string{}
	for _, id := range ids {
		c := got[id]
		if c == "" {
			t.Fatalf("no color for %q", id)
		}
		if prev, ok := owner[c]; ok {
			t.Errorf("color %s shared by %q and %q; want distinct", c, prev, id)
		}
		owner[c] = id
	}

	// Incumbents keep their color when a newer agent arrives.
	before := assignSessColors(mkAgents("alpha", "bravo", "charlie"), nil)
	after := assignSessColors(mkAgents("alpha", "bravo", "charlie", "delta"), nil)
	for _, id := range []string{"alpha", "bravo", "charlie"} {
		if before[id] != after[id] {
			t.Errorf("incumbent %q recolored on add: %s -> %s", id, before[id], after[id])
		}
	}
	if after["delta"] == "" {
		t.Error("newcomer delta got no color")
	}

	// Empty session ids are skipped (no chip/tint to render).
	withEmpty := assignSessColors(mkAgents("alpha"), []Event{{SessionID: ""}})
	if _, ok := withEmpty[""]; ok {
		t.Error("empty session id should not be assigned a color")
	}

	// A session that appears only in the events log (not on the roster) still gets
	// a color, deconflicted against the roster agents.
	mixed := assignSessColors(mkAgents("alpha", "bravo"), []Event{{SessionID: "log-only"}, {SessionID: "alpha"}})
	if mixed["log-only"] == "" {
		t.Error("events-only session got no color")
	}
	if mixed["log-only"] == mixed["alpha"] || mixed["log-only"] == mixed["bravo"] {
		t.Errorf("events-only session %s collided with a roster agent", mixed["log-only"])
	}

	// More sessions than colors: everyone still gets a palette color (collisions
	// resume past the palette, but nothing panics or goes blank).
	many := make([]string, len(sessPalette)+3)
	for i := range many {
		many[i] = fmt.Sprintf("sess-%d", i)
	}
	all := assignSessColors(mkAgents(many...), nil)
	for _, id := range many {
		if !slices.Contains(sessPalette, all[id]) {
			t.Errorf("overflow session %q got %q, not in palette", id, all[id])
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

func TestHandleDashboard_SessionColors(t *testing.T) {
	db, _ := openDB(filepath.Join(t.TempDir(), "events.db"))
	defer db.Close()
	now := time.Now().UTC().Format(time.RFC3339)
	// One event with a session id, one without (some hooks carry no session_id).
	insertEvent(db, Event{TS: now, SourceApp: "myapp", Branch: "br", SessionID: "sess-abc",
		EventType: "PreToolUse", ToolName: "Bash", Summary: "Bash: ls", PayloadJSON: "{}"})
	insertEvent(db, Event{TS: now, SourceApp: "myapp", EventType: "SessionStart",
		Summary: "SessionStart", PayloadJSON: "{}"})

	body := getBody(t, db, "/")
	// Only one session is visible, so assignSessColors hits no collision and hands
	// it its hash-preferred color — i.e. exactly sessColor("sess-abc").
	want := sessColor("sess-abc")

	// A colored chip is rendered for the session.
	if !strings.Contains(body, `class="chip"`) {
		t.Errorf("expected a session chip in output:\n%s", body)
	}
	// The session's color appears (chip + row tint both derive from it).
	if !strings.Contains(body, want) {
		t.Errorf("expected session color %q in output:\n%s", want, body)
	}
	// The row is tinted via color-mix against Canvas.
	if !strings.Contains(body, "color-mix") {
		t.Errorf("expected a color-mix row tint:\n%s", body)
	}
	// The roster's first column is now headed "session" (the tmux session name).
	if !strings.Contains(body, "<th>session</th>") {
		t.Errorf("expected a session column header:\n%s", body)
	}
	// Exactly one row carries an inline tint: the session-bearing Recent-events
	// row. The empty-session row's {{ if .SessionID }} guard skips the style attr
	// entirely, and the roster chip uses a solid background (not color-mix), so
	// neither matches. This proves the empty-session row is left untinted.
	if n := strings.Count(body, `style="background: color-mix`); n != 1 {
		t.Errorf("expected exactly 1 tinted row, got %d:\n%s", n, body)
	}
}

func TestViewSignature_TracksTmuxSession(t *testing.T) {
	base := dashboardData{
		Agents:   []Agent{{SessionID: "s1", TmuxSession: "alpha", Status: statusWorking}},
		Activity: []SourceCount{{SourceApp: "myapp", TmuxSession: "alpha", Count: 2}},
	}

	// Roster row differs only by tmux session name.
	rosterDiff := base
	rosterDiff.Agents = []Agent{{SessionID: "s1", TmuxSession: "beta", Status: statusWorking}}
	if viewSignature(base) == viewSignature(rosterDiff) {
		t.Error("signature unchanged after a roster tmux-session change")
	}

	// Activity row differs only by tmux session name.
	activityDiff := base
	activityDiff.Activity = []SourceCount{{SourceApp: "myapp", TmuxSession: "beta", Count: 2}}
	if viewSignature(base) == viewSignature(activityDiff) {
		t.Error("signature unchanged after an activity tmux-session change")
	}
}

func TestHandleDashboard_RendersTmuxSession(t *testing.T) {
	db, _ := openDB(filepath.Join(t.TempDir(), "events.db"))
	defer db.Close()
	now := time.Now().UTC().Format(time.RFC3339)
	insertEvent(db, Event{TS: now, SourceApp: "myapp", Branch: "fix-1710", TmuxSession: "roster-colors",
		SessionID: "sess-a", EventType: "Stop", Summary: "Stop", PayloadJSON: "{}"})

	body := getBody(t, db, "/")
	for _, want := range []string{
		"roster-colors",         // the disambiguating name appears
		"fix-1710",              // branch shown in its own column
		"<th>session</th>",      // roster's session-name column
		"<th>branch</th>",       // roster keeps a distinct branch column
		"<th>session name</th>", // activity table's first column
		"<th>id</th>",           // the renamed session-id chip column
	} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q in dashboard:\n%s", want, body)
		}
	}
	// The branch is its own column, not a dimmed sub-label stacked under the name.
	if strings.Contains(body, `<span class="sub">`) {
		t.Errorf("stacked branch sub-label should be gone:\n%s", body)
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
