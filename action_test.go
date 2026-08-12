package main

import (
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
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

// writeStub writes an executable bash script to a temp dir and returns its path.
func writeStub(t *testing.T, name, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, []byte("#!/usr/bin/env bash\n"+body), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestRunActionSuccess(t *testing.T) {
	stub := writeStub(t, "ok", `echo "merged fine"; exit 0`)
	r := runAction(stub, t.TempDir(), []string{"--squash"}, 5*time.Second)
	if r.ExitCode != 0 || r.TimedOut {
		t.Fatalf("got %+v", r)
	}
	if !strings.Contains(r.Output, "merged fine") {
		t.Fatalf("output = %q", r.Output)
	}
}

func TestRunActionFailureCapturesStderr(t *testing.T) {
	stub := writeStub(t, "fail", `echo "boom" >&2; exit 3`)
	r := runAction(stub, t.TempDir(), nil, 5*time.Second)
	if r.ExitCode != 3 || r.TimedOut {
		t.Fatalf("got %+v", r)
	}
	if !strings.Contains(r.Output, "boom") {
		t.Fatalf("output = %q", r.Output)
	}
}

func TestRunActionStdinClosed(t *testing.T) {
	// A stub that blocks on `read` would hang forever if stdin were a live tty.
	// With stdin closed, read hits EOF immediately and the stub exits.
	stub := writeStub(t, "reads", `read -r x || true; echo done; exit 0`)
	done := make(chan actionResult, 1)
	go func() { done <- runAction(stub, t.TempDir(), nil, 5*time.Second) }()
	select {
	case r := <-done:
		if r.ExitCode != 0 {
			t.Fatalf("got %+v", r)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("runAction hung — stdin was not closed")
	}
}

func TestRunActionTimeoutKillsProcessGroup(t *testing.T) {
	// The stub spawns a long-lived child and records its PID, then sleeps past
	// the deadline. After the timeout, the child must be dead too — proving the
	// whole process group was killed, not just the bash parent.
	pidFile := filepath.Join(t.TempDir(), "child.pid")
	stub := writeStub(t, "hang", `sleep 30 & echo $! > `+pidFile+`; sleep 30`)
	start := time.Now()
	r := runAction(stub, t.TempDir(), nil, 500*time.Millisecond)
	if !r.TimedOut {
		t.Fatalf("expected TimedOut, got %+v", r)
	}
	if time.Since(start) > 3*time.Second {
		t.Fatal("runAction did not return promptly after timeout")
	}
	// Give the kill a moment to propagate, then assert the child is gone.
	time.Sleep(200 * time.Millisecond)
	b, err := os.ReadFile(pidFile)
	if err != nil {
		t.Skipf("child pid not recorded: %v", err) // stub race; not the unit under test
	}
	pid, _ := strconv.Atoi(strings.TrimSpace(string(b)))
	if pid > 0 && syscall.Kill(pid, 0) == nil {
		t.Fatalf("child pid %d still alive — process group was not killed", pid)
	}
}

func TestHostAllowed(t *testing.T) {
	extra := []string{"box.tailnet.ts.net"}
	yes := []string{"127.0.0.1", "127.0.0.1:4555", "localhost:4555", "[::1]:4555", "box.tailnet.ts.net", "BOX.tailnet.ts.net:4555", "100.64.0.1:4555"}
	no := []string{"", "evil.example.com", "evil.example.com:4555", "attacker.tailnet.ts.net"}
	for _, h := range yes {
		if !hostAllowed(h, "100.64.0.1", extra) {
			t.Errorf("hostAllowed(%q) = false, want true", h)
		}
	}
	for _, h := range no {
		if hostAllowed(h, "100.64.0.1", extra) {
			t.Errorf("hostAllowed(%q) = true, want false", h)
		}
	}
}

func TestTokenOK(t *testing.T) {
	if !tokenOK("abc123", "abc123") {
		t.Fatal("equal tokens should pass")
	}
	if tokenOK("abc123", "abc124") || tokenOK("", "abc") || tokenOK("abc", "") {
		t.Fatal("mismatched/empty tokens must fail")
	}
}
