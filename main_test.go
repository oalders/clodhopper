package main

import (
	"strings"
	"testing"
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
