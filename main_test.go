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

func TestRun_VersionFlagExitsZero(t *testing.T) {
	for _, arg := range []string{"--version", "-v", "version"} {
		if code := run([]string{arg}); code != 0 {
			t.Errorf("run(%q) = %d, want 0", arg, code)
		}
	}
}
