package main

import "sync"

// actionArgv maps a validated dashboard action to the binary and exact argument
// vector to run. This is the security boundary: only these fixed vectors are
// ever executed, and no request string reaches the command line. binary is a
// logical name ("merge-pr" or "gh") that handleAction resolves to an injected
// path. teardown is true for the merge-pr actions, whose success hard-kills the
// session and therefore must be followed by a synthetic SessionEnd. ok is false
// for any unknown action, which the caller rejects with 400.
func actionArgv(action string, force bool) (binary string, args []string, teardown bool, ok bool) {
	switch action {
	case "squash":
		binary, args, teardown = "merge-pr", []string{"--squash"}, true
	case "squash-admin":
		binary, args, teardown = "merge-pr", []string{"--squash", "--admin"}, true
	case "close":
		binary, args, teardown = "merge-pr", []string{"--close"}, true
	case "ready":
		// gh pr ready is non-destructive: the session keeps running, so no
		// teardown, and --force is meaningless here (ignored).
		return "gh", []string{"pr", "ready"}, false, true
	default:
		return "", nil, false, false
	}
	if force {
		args = append(args, "--force")
	}
	return binary, args, teardown, true
}

// inflightSet tracks which sessions have an action currently running, so a
// double-click or a second tab cannot spawn two merge-pr subprocesses racing on
// the same worktree. Keyed by session id.
type inflightSet struct {
	mu sync.Mutex
	m  map[string]bool
}

func newInflightSet() *inflightSet { return &inflightSet{m: map[string]bool{}} }

// acquire marks key as in-flight, returning false if it already is.
func (s *inflightSet) acquire(key string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.m[key] {
		return false
	}
	s.m[key] = true
	return true
}

func (s *inflightSet) release(key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.m, key)
}
