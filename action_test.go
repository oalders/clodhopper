package main

import (
	"reflect"
	"testing"
)

func TestActionArgv(t *testing.T) {
	cases := []struct {
		action   string
		force    bool
		binary   string
		args     []string
		teardown bool
		ok       bool
	}{
		{"squash", false, "merge-pr", []string{"--squash"}, true, true},
		{"squash", true, "merge-pr", []string{"--squash", "--force"}, true, true},
		{"squash-admin", false, "merge-pr", []string{"--squash", "--admin"}, true, true},
		{"squash-admin", true, "merge-pr", []string{"--squash", "--admin", "--force"}, true, true},
		{"close", false, "merge-pr", []string{"--close"}, true, true},
		{"close", true, "merge-pr", []string{"--close", "--force"}, true, true},
		{"ready", false, "gh", []string{"pr", "ready"}, false, true},
		{"ready", true, "gh", []string{"pr", "ready"}, false, true}, // force ignored
		{"", false, "", nil, false, false},
		{"squash; rm -rf /", false, "", nil, false, false},
		{"--admin", false, "", nil, false, false},
	}
	for _, c := range cases {
		b, a, td, ok := actionArgv(c.action, c.force)
		if b != c.binary || td != c.teardown || ok != c.ok || !reflect.DeepEqual(a, c.args) {
			t.Errorf("actionArgv(%q,%v) = (%q,%v,%v,%v), want (%q,%v,%v,%v)",
				c.action, c.force, b, a, td, ok, c.binary, c.args, c.teardown, c.ok)
		}
	}
}

func TestInflightSet(t *testing.T) {
	s := newInflightSet()
	if !s.acquire("k") {
		t.Fatal("first acquire should succeed")
	}
	if s.acquire("k") {
		t.Fatal("second acquire of held key should fail")
	}
	if !s.acquire("other") {
		t.Fatal("distinct key should succeed")
	}
	s.release("k")
	if !s.acquire("k") {
		t.Fatal("acquire after release should succeed")
	}
}
