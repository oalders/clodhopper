package main

import (
	"bytes"
	"crypto/rand"
	"crypto/subtle"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net"
	"net/http"
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

// actionTimeout / readyTimeout bound the two action classes. Teardown does real
// work (docker-teardown, kill-worktree-procs, submodule-forced worktree remove,
// session kill), so it gets a generous ceiling; gh pr ready is a single API call.
const (
	actionTimeout = 120 * time.Second
	readyTimeout  = 20 * time.Second
)

// actionConfig is serve's write-path state. Binary names are injected so tests
// substitute stubs; token is the per-serve CSRF secret; allowedHosts extends the
// Host allowlist beyond loopback + bindHost.
type actionConfig struct {
	enabled      bool
	mergePR      string // path/name of the merge-pr binary
	gh           string // path/name of the gh binary
	token        string
	bindHost     string
	allowedHosts []string
	inflight     *inflightSet
}

// randomToken returns a 256-bit hex CSRF secret, regenerated each serve start.
func randomToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// handleAction serves POST /api/action: run one allowlisted PR action for a
// roster row's worktree. now is injected so the synthetic SessionEnd timestamp
// is deterministic in tests. Every early return uses a plain http status; only a
// completed run writes the JSON body.
func handleAction(w http.ResponseWriter, r *http.Request, db *sql.DB, cfg *actionConfig, now time.Time) {
	setSecurityHeaders(w)
	if !cfg.enabled {
		http.Error(w, "PR actions disabled", http.StatusForbidden)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !hostAllowed(r.Host, cfg.bindHost, cfg.allowedHosts) {
		http.Error(w, "host not allowed", http.StatusForbidden)
		return
	}
	if !tokenOK(r.Header.Get("X-Clodhopper-Token"), cfg.token) {
		http.Error(w, "bad token", http.StatusForbidden)
		return
	}

	sessionID := r.FormValue("session_id")
	action := r.FormValue("action")
	force := r.FormValue("force") == "true"

	binary, args, teardown, ok := actionArgv(action, force)
	if !ok {
		http.Error(w, "unknown action", http.StatusBadRequest)
		return
	}

	cwd, err := latestCwdForSession(db, sessionID)
	if err != nil {
		http.Error(w, "lookup failed", http.StatusInternalServerError)
		return
	}
	if cwd == "" {
		http.Error(w, "unknown session", http.StatusNotFound)
		return
	}

	if !cfg.inflight.acquire(sessionID) {
		http.Error(w, "action already running for this session", http.StatusConflict)
		return
	}
	defer cfg.inflight.release(sessionID)

	bin := cfg.mergePR
	timeout := actionTimeout
	if binary == "gh" {
		bin, timeout = cfg.gh, readyTimeout
	}
	res := runAction(bin, cwd, args, timeout)

	// A clean teardown hard-kills the session, so it never emits its own
	// SessionEnd; write a synthetic one so the row drops. Best-effort: a failure
	// here does not change what we report about the merge itself. A timeout is
	// NOT treated as success even at exit 0 (the deadline may have fired after
	// the merge but mid-teardown), so it never ends the session here.
	if teardown && res.ExitCode == 0 && !res.TimedOut {
		if _, err := endSessions(db, EndSelector{SessionID: sessionID}, now); err != nil {
			debugf("action: endSessions after merge: %v", err)
		}
	}

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"ok":       res.ExitCode == 0 && !res.TimedOut,
		"exitCode": res.ExitCode,
		"timedOut": res.TimedOut,
		"output":   scrubString(res.Output),
	})
}
