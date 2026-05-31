package main

import (
	"database/sql"
	"net/http"
	"net/http/httptest"
	"path/filepath"
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
