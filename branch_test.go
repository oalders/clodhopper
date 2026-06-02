package main

import (
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// gitInitOnBranch creates a bare-minimum git work tree at dir whose HEAD points
// at branch, without needing a commit or user config. Skips the test if git is
// unavailable.
func gitInitOnBranch(t *testing.T, dir, branch string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v (%s)", args, err, out)
		}
	}
	run("init")
	run("symbolic-ref", "HEAD", "refs/heads/"+branch)
}

// gitAddWorktree creates a linked worktree of the repo at dir, checked out at a
// new branch, rooted at wtDir. It needs a commit to branch from, so it makes an
// empty one with inline identity config. Skips if git is unavailable.
func gitAddWorktree(t *testing.T, dir, wtDir, branch string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v (%s)", args, err, out)
		}
	}
	run("-c", "user.email=t@t", "-c", "user.name=t", "commit", "--allow-empty", "-m", "init")
	run("worktree", "add", "-b", branch, wtDir)
}

func TestGitBranch(t *testing.T) {
	dir := t.TempDir()
	gitInitOnBranch(t, dir, "fix-2499")
	if got := gitBranch(dir); got != "fix-2499" {
		t.Errorf("gitBranch = %q, want fix-2499", got)
	}
}

func TestGitBranch_EmptyAndNonRepo(t *testing.T) {
	if got := gitBranch(""); got != "" {
		t.Errorf("empty cwd: want \"\", got %q", got)
	}
	// "/" is the top of the tree: git cannot walk above it, so it is never inside
	// a repo. (A plain t.TempDir() is unreliable here — TMPDIR can resolve under
	// the checkout's own git work tree, in which case resolving a branch is the
	// correct result, not an empty one.)
	if got := gitBranch("/"); got != "" {
		t.Errorf("filesystem root: want \"\", got %q", got)
	}
	if got := gitBranch("/nonexistent/path/xyzzy"); got != "" {
		t.Errorf("nonexistent dir: want \"\", got %q", got)
	}
}

func TestBuildEvent_PopulatesBranch(t *testing.T) {
	dir := t.TempDir()
	gitInitOnBranch(t, dir, "fix-2499")
	raw := []byte(`{"hook_event_name":"PreToolUse","cwd":"` + dir + `","tool_name":"Bash","tool_input":{"command":"echo hi"}}`)
	ev := buildEvent(raw, "myapp")
	if ev.Branch != "fix-2499" {
		t.Errorf("branch not populated: %q", ev.Branch)
	}
}

func TestBranchRoundTripAndFilter(t *testing.T) {
	db, err := openDB(filepath.Join(t.TempDir(), "events.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	now := time.Now().UTC().Format(time.RFC3339)
	insertEvent(db, Event{TS: now, SourceApp: "myapp", Branch: "fix-2499", EventType: "PreToolUse", PayloadJSON: "{}"})
	insertEvent(db, Event{TS: now, SourceApp: "myapp", Branch: "fix-1234", EventType: "PreToolUse", PayloadJSON: "{}"})

	got, _ := queryEvents(db, EventFilter{Branch: "fix-2499"})
	if len(got) != 1 || got[0].Branch != "fix-2499" {
		t.Fatalf("branch filter: want 1 fix-2499, got %+v", got)
	}

	branches, _ := distinctBranches(db)
	if len(branches) != 2 {
		t.Errorf("distinct branches: want 2, got %v", branches)
	}

	counts, _ := activeCounts(db, time.Hour, time.Now())
	if len(counts) != 2 {
		t.Fatalf("active counts: want 2 source/branch groups, got %+v", counts)
	}
	for _, c := range counts {
		if c.Branch == "" {
			t.Errorf("active count missing branch: %+v", c)
		}
	}
}
