package main

import (
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestVersionString_DefaultsAreEmbedded(t *testing.T) {
	got := versionString()
	for _, want := range []string{"clodhopper", version, commit, date} {
		if !strings.Contains(got, want) {
			t.Errorf("versionString() = %q, missing %q", got, want)
		}
	}
}

// TestVersionString_Format pins the exact layout (and the "commit"/"built"
// labels) so a formatting regression is caught even when a value coincidentally
// matches a label.
func TestVersionString_Format(t *testing.T) {
	defer func(v, c, d string) { version, commit, date = v, c, d }(version, commit, date)
	version, commit, date = "1.2.3", "abc1234", "2026-01-01T00:00:00Z"
	if got, want := versionString(), "clodhopper 1.2.3 (commit abc1234, built 2026-01-01T00:00:00Z)"; got != want {
		t.Errorf("versionString() = %q, want %q", got, want)
	}
}

func TestRun_VersionFlagExitsZero(t *testing.T) {
	for _, arg := range []string{"--version", "-v", "version"} {
		if code := run([]string{arg}); code != 0 {
			t.Errorf("run(%q) = %d, want 0", arg, code)
		}
	}
}

func TestRun_EndRequiresSelector(t *testing.T) {
	// No selector must fail fast (exit 2) before touching the database.
	if code := run([]string{"end"}); code != 2 {
		t.Errorf("run(end) with no selector = %d, want 2", code)
	}
}

func TestWaitingRetainHours_DefaultAndOverride(t *testing.T) {
	if got := waitingRetainHours(); got != 720 {
		t.Errorf("default waitingRetainHours() = %d, want 720", got)
	}
	t.Setenv("CLODHOPPER_WAITING_RETAIN_HOURS", "24")
	if got := waitingRetainHours(); got != 24 {
		t.Errorf("override waitingRetainHours() = %d, want 24", got)
	}
	t.Setenv("CLODHOPPER_WAITING_RETAIN_HOURS", "0") // non-positive is ignored
	if got := waitingRetainHours(); got != 720 {
		t.Errorf("zero override should fall back to 720, got %d", got)
	}
}

func TestStaleShell_DefaultAndOverride(t *testing.T) {
	for _, c := range []struct {
		env  string
		want time.Duration
	}{
		{"", 2 * time.Hour},
		{"30m", 30 * time.Minute},
		{"0", 0}, // disables
		{"-1h", defaultStaleShell},
		{"soon", defaultStaleShell},
	} {
		t.Setenv("CLODHOPPER_STALE_SHELL", c.env)
		if got := staleShell(); got != c.want {
			t.Errorf("CLODHOPPER_STALE_SHELL=%q: got %v, want %v", c.env, got, c.want)
		}
	}
}

// block takes exactly one kind of blocker: a row or a positive PR number.
func TestRunBlockBlockerFlags(t *testing.T) {
	t.Setenv("CLODHOPPER_DB", filepath.Join(t.TempDir(), "events.db"))
	for _, args := range [][]string{
		{"--branch", "b"},
		{"--branch", "b", "--on-branch", "c", "--on-pr", "7"},
		{"--branch", "b", "--on-session", "c", "--on-pr", "7"},
		{"--branch", "b", "--on-pr", "-7"},
	} {
		if code := runBlock(args); code != 2 {
			t.Errorf("runBlock(%q) = %d, want 2", args, code)
		}
	}
}
