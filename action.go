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
	"os"
	"os/exec"
	"strconv"
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
	cmd.Env = actionEnv(os.Environ())
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

// runTmux runs the tmux binary with a FIXED argument vector (no shell, no
// request string on the command line) and returns its clean stdout separately
// from the combined diagnostic output. Unlike runAction it does NOT strip
// $TMUX/$TMUX_PANE: targeting is by the explicit -t pane id the caller resolved
// server-side, and the command must reach the SAME tmux server whose pane ids the
// peek feature enumerated — which is the server named by serve's inherited $TMUX.
// Best-effort by construction: a tmux that cannot start returns ExitCode -1.
func runTmux(tmuxBin, dir string, args []string, timeout time.Duration) (stdout string, res actionResult) {
	cmd := exec.Command(tmuxBin, args...)
	cmd.Dir = dir
	cmd.Stdin = nil
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	var outBuf, errBuf bytes.Buffer
	cmd.Stdout = &outBuf
	cmd.Stderr = &errBuf

	if err := cmd.Start(); err != nil {
		return "", actionResult{ExitCode: -1, Output: err.Error()}
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-timer.C:
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		<-done
		return outBuf.String(), actionResult{ExitCode: -1, TimedOut: true, Output: errBuf.String()}
	case err := <-done:
		out := errBuf.String()
		if out == "" {
			out = outBuf.String()
		}
		return outBuf.String(), actionResult{ExitCode: exitCode(err), Output: out}
	}
}

// runSessionAction executes one of the tmux session actions against a pane the
// caller has already resolved server-side and validated with paneIDRe. The
// command strings (/clear, /monitor-ci, nn claude) are FIXED constants; only pane
// ids vary, and any pane split-window emits is re-validated before reuse.
//
//   - monitor-ci: send /clear into the agent's own pane, pause, then send
//     /monitor-ci (/clear and /monitor-ci are literal keys, Enter is tmux's key
//     name for the return that submits each). This is deliberately TWO send-keys
//     calls: /clear makes Claude Code tear down and redraw its input UI, and in a
//     sandboxed session that redraw is slow enough that keys arriving in the same
//     burst land before the input line can accept them and are swallowed rather
//     than queued. clearDelay (actionConfig) is the pause between the two; a zero
//     delay reproduces the old single-burst timing and is what tests use.
//   - new-monitor: split a NEW pane in the same window running `nn claude`,
//     capture its pane id, then send /monitor-ci to it. The new pane's TUI must
//     finish booting before those keys land, so there is an inherent startup race
//     — we send immediately after the split (no shell sleep is available). It
//     does NOT tear the session down.
func runSessionAction(tmuxBin, action, pane, dir string, clearDelay time.Duration) actionResult {
	switch action {
	case "monitor-ci":
		_, res := runTmux(tmuxBin, dir,
			[]string{"send-keys", "-t", pane, "/clear", "Enter"}, tmuxTimeout)
		// If /clear never reached the pane there is no redraw to wait out, and no
		// point sending a command into a pane we could not talk to.
		if res.ExitCode != 0 || res.TimedOut {
			return res
		}
		// Sleep(0) is a no-op, so a zero clearDelay simply sends both back to back.
		time.Sleep(clearDelay)
		_, res2 := runTmux(tmuxBin, dir,
			[]string{"send-keys", "-t", pane, "/monitor-ci", "Enter"}, tmuxTimeout)
		return res2
	case "new-monitor":
		out, res := runTmux(tmuxBin, dir,
			[]string{"split-window", "-t", pane, "-P", "-F", "#{pane_id}", "nn", "claude"}, tmuxTimeout)
		if res.ExitCode != 0 || res.TimedOut {
			return res
		}
		newPane := strings.TrimSpace(out)
		if !paneIDRe.MatchString(newPane) {
			return actionResult{ExitCode: -1, Output: "split-window returned no valid pane id"}
		}
		_, res2 := runTmux(tmuxBin, dir,
			[]string{"send-keys", "-t", newPane, "/monitor-ci", "Enter"}, tmuxTimeout)
		return res2
	default:
		return actionResult{ExitCode: -1, Output: "unknown session action"}
	}
}

// actionEnv strips serve's own tmux context ($TMUX / $TMUX_PANE) from the child
// environment. An action's cwd already points at the target row's worktree, but
// merge-pr resolves which tmux session to kill from $TMUX first ("run from inside
// the worktree's session") and only falls back to matching a pane's start-path
// against the worktree when $TMUX is unset. Inherited unchanged, that first branch
// names the session serve itself runs in — so a merge would tear down the
// dashboard's own tmux session instead of the merged branch's. Dropping these two
// forces the worktree-matching fallback, which keys off the cwd we already set.
// Same reason ingest is careful never to trust an ambient $TMUX_PANE.
func actionEnv(env []string) []string {
	out := env[:0:0]
	for _, kv := range env {
		if strings.HasPrefix(kv, "TMUX=") || strings.HasPrefix(kv, "TMUX_PANE=") {
			continue
		}
		out = append(out, kv)
	}
	return out
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
	// tmuxTimeout bounds a session action's tmux invocations. send-keys and
	// split-window are quick local RPCs to the tmux server (split-window returns
	// as soon as the pane exists, it does not wait for `nn claude`), so a few
	// seconds is generous.
	tmuxTimeout = 10 * time.Second
	// monitorCIClearDelay is how long the monitor-ci action waits after /clear
	// before sending /monitor-ci, so the agent's TUI can finish redrawing its
	// input line first. Long enough to cover a slow (e.g. sandboxed) redraw,
	// short enough that the dashboard's action request still feels immediate.
	monitorCIClearDelay = 750 * time.Millisecond
)

// actionConfig is serve's write-path state. Binary names are injected so tests
// substitute stubs; token is the per-serve CSRF secret; allowedHosts extends the
// Host allowlist beyond loopback + bindHost.
type actionConfig struct {
	enabled bool
	mergePR string // path/name of the merge-pr binary
	gh      string // path/name of the gh binary
	tmux    string // path/name of the tmux binary (session actions); tests stub it
	// clearDelay is the pause monitor-ci leaves between its /clear and
	// /monitor-ci send-keys calls (see runSessionAction). serve sets it to
	// monitorCIClearDelay; the zero value sends both back to back, which is what
	// tests want so they never sleep.
	clearDelay   time.Duration
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

	// Session actions (monitor-ci / new-monitor) act on the agent's LIVE tmux
	// pane rather than merging a PR, so they take a separate path: the pane is
	// resolved server-side from session_id and validated against paneIDRe before
	// any tmux command runs. They reuse every guard already checked above
	// (enabled, method, host, token) plus the inflight dedupe below.
	if action == "monitor-ci" || action == "new-monitor" {
		pane, err := latestPaneForSession(db, sessionID)
		if err != nil {
			http.Error(w, "lookup failed", http.StatusInternalServerError)
			return
		}
		// A missing/blank/malformed pane means there is nothing live to target
		// (unknown session, or the agent is not in tmux). Same 4xx shape as the
		// no-session case; the client only offers these behind .LiveTmux anyway.
		if !paneIDRe.MatchString(pane) {
			http.Error(w, "unknown session", http.StatusNotFound)
			return
		}
		if !cfg.inflight.acquire(sessionID) {
			http.Error(w, "action already running for this session", http.StatusConflict)
			return
		}
		defer cfg.inflight.release(sessionID)
		// The split runs in the agent's worktree if we know it; tmux inherits the
		// target pane's cwd regardless, so an empty dir is harmless.
		cwd, _ := latestCwdForSession(db, sessionID)
		res := runSessionAction(cfg.tmux, action, pane, cwd, cfg.clearDelay)
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ok":       res.ExitCode == 0 && !res.TimedOut,
			"exitCode": res.ExitCode,
			"timedOut": res.TimedOut,
			"output":   scrubString(res.Output),
		})
		return
	}

	// "end" dismisses a roster row from the dashboard: it writes the same
	// synthetic SessionEnd that `clodhopper end --session` does, so agentRoster
	// drops the session and a genuinely-live agent reappears on its next event.
	// It runs NO subprocess, so it deliberately never reaches actionArgv (the
	// argv allowlist stays the boundary for things that do exec). It also must
	// NOT require a live tmux pane — stale rows with no pane are exactly the ones
	// worth dismissing. session_id only ever travels as a bound SQL parameter.
	if action == "end" {
		if sessionID == "" {
			http.Error(w, "unknown session", http.StatusNotFound)
			return
		}
		if !cfg.inflight.acquire(sessionID) {
			http.Error(w, "action already running for this session", http.StatusConflict)
			return
		}
		defer cfg.inflight.release(sessionID)
		writeEndResult(w, endSession(db, sessionID, now))
		return
	}

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

// endSession marks one dashboard session ended and returns the actionResult the
// End action reports. Every failure mode is reported as a visible, non-ok
// result rather than a silent no-op: an ambiguous prefix (session ids reach us
// in full, but endSessions matches by prefix, so a shorter id that prefixes a
// longer one is still possible), a lookup error, and a zero count (the session
// is unknown or was already ended) each get their own message.
func endSession(db *sql.DB, sessionID string, now time.Time) actionResult {
	n, err := endSessions(db, EndSelector{SessionID: sessionID}, now)
	if err != nil {
		var amb *AmbiguousSessionError
		if errors.As(err, &amb) {
			return actionResult{ExitCode: -1, Output: "session id matches " +
				strconv.Itoa(len(amb.IDs)) + " live sessions; ended nothing"}
		}
		debugf("action: end %s: %v", sessionID, err)
		return actionResult{ExitCode: -1, Output: "could not end session: " + err.Error()}
	}
	if n == 0 {
		return actionResult{ExitCode: -1, Output: "no live session matched (already ended?)"}
	}
	return actionResult{ExitCode: 0}
}

// writeEndResult emits the same JSON envelope every action returns, so the
// dashboard's fire() handles End exactly like the other actions.
func writeEndResult(w http.ResponseWriter, res actionResult) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"ok":       res.ExitCode == 0 && !res.TimedOut,
		"exitCode": res.ExitCode,
		"timedOut": res.TimedOut,
		"output":   scrubString(res.Output),
	})
}
