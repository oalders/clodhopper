//go:build e2eseed

package main

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestE2ESeed writes the worst-case roster fixtures the Playwright suite in
// e2e/ measures. It is not a test of anything: e2e/serve.sh runs it (build tag
// e2eseed keeps it out of `go test ./...`) against a throwaway CLODHOPPER_DB.
//
// Ages are relative to now and sit far from every state boundary (working goes
// idle and a subagent stops counting at staleWorkingSecs), so each row keeps its
// status for the whole run.
func TestE2ESeed(t *testing.T) {
	path := os.Getenv("CLODHOPPER_DB")
	if path == "" {
		t.Fatal("CLODHOPPER_DB must be set; refusing to fall back to the real event DB")
	}
	db, err := openDB(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	now := time.Now()
	// Cwds never exist, so serve's git/gh lookups find nothing.
	root := filepath.Join(t.TempDir(), "nonexistent")
	const (
		appA = "clodhopper"
		appB = "a-second-project-with-a-long-source-app-name"
	)
	longBranch := "fix-9999-an-exceptionally-long-branch-name-that-must-truncate-in-every-layout"

	var evs []Event
	add := func(age time.Duration, app, branch, cwd, sess, etype, tool, payload string) {
		if payload == "" {
			payload = "{}"
		}
		evs = append(evs, Event{
			TS: now.Add(-age).UTC().Format(time.RFC3339), SourceApp: app, Branch: branch,
			Cwd: filepath.Join(root, cwd), SessionID: sess, EventType: etype, ToolName: tool,
			Summary: tool, PayloadJSON: payload,
		})
	}

	// Widest badge: "waiting on 12 agents · 23h" — 12 subagents started 23h ago,
	// each heard from a minute ago, then the main turn's Stop.
	sub := "e2e-subagents-0000-1111-2222-333333333333"
	for i := range 12 {
		add(23*time.Hour, appA, longBranch, "worktree-subagents", sub, "SubagentStart", "", fmt.Sprintf(`{"agent_id":"agent-%02d"}`, i))
	}
	for i := range 12 {
		add(time.Minute, appA, longBranch, "worktree-subagents", sub, "PreToolUse", "Bash", fmt.Sprintf(`{"agent_id":"agent-%02d"}`, i))
	}
	add(30*time.Second, appA, longBranch, "worktree-subagents", sub, "Stop", "", "")

	// One row per remaining status, across both apps and several branches.
	add(5*time.Second, appA, longBranch, "worktree-working", "e2e-working-0000-1111-2222-333333333333", "PreToolUse", "Edit", "")
	add(2*time.Minute, appA, "main", "worktree-approval", "e2e-approval-0000-1111-2222-333333333333", "PermissionRequest", "Bash", "")
	add(3*time.Minute, appB, "feature/another-long-branch-name-for-the-second-app", "worktree-input", "e2e-input-0000-1111-2222-333333333333", "PermissionRequest", "AskUserQuestion", "")
	add(4*time.Minute, appB, "main", "worktree-needs", "e2e-needs-0000-1111-2222-333333333333", "Notification", "", "")
	add(10*time.Hour, appB, "main", "worktree-background", "e2e-background-0000-1111-2222-333333333333", "PreToolUse", "ScheduleWakeup", "")
	add(9*time.Hour, appB, "main", "worktree-background", "e2e-background-0000-1111-2222-333333333333", "Notification", "", `{"notification_type":"idle_prompt"}`)
	add(22*time.Hour, appA, "main", "worktree-waiting", "e2e-waiting-0000-1111-2222-333333333333", "Stop", "", "")
	// Idle with a two-digit hour count; branchless, so the cwd basename stands in.
	add(23*time.Hour, appB, "", "a-very-long-worktree-directory-name-standing-in-for-the-branch", "e2e-idle-0000-1111-2222-333333333333", "PreToolUse", "Read", "")

	for _, ev := range evs {
		if err := insertEvent(db, ev); err != nil {
			t.Fatal(err)
		}
	}
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM events`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != len(evs) {
		t.Fatalf("seeded %d rows, want %d (CLODHOPPER_DB must start empty)", n, len(evs))
	}
}
