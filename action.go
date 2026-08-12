package main

import (
	"bytes"
	"crypto/subtle"
	"errors"
	"net"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"
)

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

// actionResult is the outcome of one subprocess run. Output is the combined
// stdout+stderr, unscrubbed (the handler scrubs before returning it to a client).
type actionResult struct {
	ExitCode int
	TimedOut bool
	Output   string
}

// runAction runs bin with args in dir, capturing combined output. stdin is left
// nil (closed): merge-pr's one interactive prompt then hits EOF and fails closed
// instead of hanging a serve goroutine. The child gets its own process group so a
// timeout can SIGKILL the whole tree (an in-flight `gh`/`git` child reparents and
// survives if only the bash parent is signalled). Best-effort by construction: a
// binary that cannot start returns ExitCode -1, never a panic.
func runAction(bin, dir string, args []string, timeout time.Duration) actionResult {
	cmd := exec.Command(bin, args...)
	cmd.Dir = dir
	cmd.Stdin = nil
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf

	if err := cmd.Start(); err != nil {
		return actionResult{ExitCode: -1, Output: err.Error()}
	}

	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-timer.C:
		// Setpgid makes the child's pgid equal its pid, so -pid targets the
		// whole group.
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		<-done
		return actionResult{ExitCode: -1, TimedOut: true, Output: buf.String()}
	case err := <-done:
		return actionResult{ExitCode: exitCode(err), Output: buf.String()}
	}
}

// exitCode extracts a process exit code from Cmd.Wait's error: 0 on success, the
// real code for a normal non-zero exit, -1 for anything else (signalled, etc.).
func exitCode(err error) int {
	if err == nil {
		return 0
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode()
	}
	return -1
}

// hostAllowed defends the write endpoint against DNS-rebinding: even with the
// custom-header CSRF token, a page on attacker.example that rebinds its name to
// the dashboard's IP becomes same-origin and could read the token, so we also
// require the Host header to name this dashboard. Loopback is always allowed;
// bindHost covers the --host/--tailscale address; extra carries operator-declared
// names (CLODHOPPER_ALLOWED_HOSTS) such as a Tailscale magicDNS name.
func hostAllowed(hostHeader, bindHost string, extra []string) bool {
	if hostHeader == "" {
		return false
	}
	h := hostHeader
	if host, _, err := net.SplitHostPort(hostHeader); err == nil {
		h = host
	}
	h = strings.ToLower(strings.Trim(h, "[]"))
	switch h {
	case "127.0.0.1", "::1", "localhost":
		return true
	}
	if bindHost != "" && h == strings.ToLower(strings.Trim(bindHost, "[]")) {
		return true
	}
	for _, e := range extra {
		if e != "" && h == strings.ToLower(e) {
			return true
		}
	}
	return false
}

// tokenOK compares a presented CSRF token against the per-serve secret in
// constant time. An empty secret (feature misconfigured) always fails.
func tokenOK(got, want string) bool {
	if want == "" || len(got) != len(want) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(got), []byte(want)) == 1
}
