package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
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
	if got, rebasing := gitBranch(dir); got != "fix-2499" || rebasing {
		t.Errorf("gitBranch = (%q, %v), want (fix-2499, false)", got, rebasing)
	}
}

func TestGitBranch_EmptyAndNonRepo(t *testing.T) {
	if got, rebasing := gitBranch(""); got != "" || rebasing {
		t.Errorf("empty cwd: want (\"\", false), got (%q, %v)", got, rebasing)
	}
	// "/" is the top of the tree: git cannot walk above it, so it is never inside
	// a repo. (A plain t.TempDir() is unreliable here — TMPDIR can resolve under
	// the checkout's own git work tree, in which case resolving a branch is the
	// correct result, not an empty one.)
	if got, rebasing := gitBranch("/"); got != "" || rebasing {
		t.Errorf("filesystem root: want (\"\", false), got (%q, %v)", got, rebasing)
	}
	if got, rebasing := gitBranch("/nonexistent/path/xyzzy"); got != "" || rebasing {
		t.Errorf("nonexistent dir: want (\"\", false), got (%q, %v)", got, rebasing)
	}
}

// TestGitBranch_Rebasing verifies that when HEAD is detached mid-rebase,
// gitBranch recovers the branch being rebased and flags it. It exercises both
// rebase layouts: rebase-merge (interactive / merge) and rebase-apply (am).
func TestGitBranch_Rebasing(t *testing.T) {
	for _, sub := range []string{"rebase-merge", "rebase-apply"} {
		t.Run(sub, func(t *testing.T) {
			dir := t.TempDir()
			if _, err := exec.LookPath("git"); err != nil {
				t.Skip("git not available")
			}
			run := func(args ...string) string {
				t.Helper()
				cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
				out, err := cmd.CombinedOutput()
				if err != nil {
					t.Fatalf("git %v: %v (%s)", args, err, out)
				}
				return strings.TrimSpace(string(out))
			}
			run("init")
			run("symbolic-ref", "HEAD", "refs/heads/fix-77")
			run("-c", "user.email=t@t", "-c", "user.name=t", "commit", "--allow-empty", "-m", "init")
			// Detach HEAD so symbolic-ref fails, mimicking the mid-rebase state.
			run("checkout", "--detach")

			// Sanity check: without rebase state, the detached HEAD yields no branch.
			if got, rebasing := gitBranch(dir); got != "" || rebasing {
				t.Fatalf("detached HEAD without rebase: want (\"\", false), got (%q, %v)", got, rebasing)
			}

			// Lay down the rebase state git would create, via the same per-worktree
			// path resolution gitBranch uses.
			headName := run("rev-parse", "--git-path", sub+"/head-name")
			if !filepath.IsAbs(headName) {
				headName = filepath.Join(dir, headName)
			}
			if err := os.MkdirAll(filepath.Dir(headName), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(headName, []byte("refs/heads/fix-77\n"), 0o644); err != nil {
				t.Fatal(err)
			}

			got, rebasing := gitBranch(dir)
			if got != "fix-77" || !rebasing {
				t.Errorf("mid-rebase: want (fix-77, true), got (%q, %v)", got, rebasing)
			}

			// A rebase begun from an already-detached HEAD records the literal
			// "detached HEAD" in head-name, not a refs/heads/ ref. Recovering that
			// as a branch would be bogus, so we fail closed to ("", false).
			if err := os.WriteFile(headName, []byte("detached HEAD\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			if got, rebasing := gitBranch(dir); got != "" || rebasing {
				t.Errorf("rebase from detached HEAD: want (\"\", false), got (%q, %v)", got, rebasing)
			}
		})
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
	if ev.Rebasing {
		t.Errorf("not rebasing, but Rebasing=true")
	}
}

// TestRebasingRoundTrip confirms the rebasing flag survives a write/read cycle,
// including the COALESCE default for pre-migration rows that predate the column.
func TestRebasingRoundTrip(t *testing.T) {
	db, err := openDB(filepath.Join(t.TempDir(), "events.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	now := time.Now().UTC().Format(time.RFC3339)
	insertEvent(db, Event{TS: now, SourceApp: "myapp", Branch: "fix-77", Rebasing: true, EventType: "PreToolUse", PayloadJSON: "{}"})
	insertEvent(db, Event{TS: now, SourceApp: "myapp", Branch: "main", EventType: "PreToolUse", PayloadJSON: "{}"})

	got, err := queryEvents(db, EventFilter{})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{"fix-77": true, "main": false}
	if len(got) != len(want) {
		t.Fatalf("want %d events, got %d", len(want), len(got))
	}
	for _, e := range got {
		if e.Rebasing != want[e.Branch] {
			t.Errorf("branch %q: Rebasing=%v, want %v", e.Branch, e.Rebasing, want[e.Branch])
		}
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
