package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// actionArgv maps a validated dashboard action to the binary and exact argument
// vector to run. This is the security boundary: only these fixed vectors are
// ever executed, and no request string reaches the command line. binary is a
// logical name ("merge-pr", "gh" or "git") that handleAction resolves to an
// injected path. teardown is true for the merge-pr actions, whose success hard-kills the
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
	case "rebase":
		// rebase is a SEQUENCE of git invocations, not one argv, so the vector is
		// built by rebaseSteps from a server-derived, regex-validated base branch
		// (see runRebase). It still passes through this allowlist — the boundary
		// for anything that execs — which is what makes "git" a legal binary at
		// all; the argv itself is assembled below the line, never from a request
		// string. Non-destructive to the row (no teardown) and --force is
		// meaningless here (ignored): the push safety comes from
		// --force-with-lease.
		return "git", nil, false, true
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

// sidKey and cwdKey namespace the two kinds of lock that share one inflightSet,
// so a session id that literally reads "cwd:/some/path" cannot contend with a
// worktree lock (or vice versa). Cosmetic today — acquisition order is fixed
// (session, then worktree) so no deadlock is reachable either way — but it keeps
// the two key spaces provably disjoint.
func sidKey(sessionID string) string { return "sid:" + sessionID }

// cwdKey normalises the path first: two spellings of ONE worktree ("/a/b" and
// "/a/b/", or a symlinked and a real path) must take the SAME lock, or both
// requests run git in the same directory concurrently — the thing the worktree
// lock exists to prevent. EvalSymlinks is best-effort (a path that no longer
// exists keeps its cleaned form), which is fine: the fallback is the old
// behaviour, never a wrongly SHARED key.
func cwdKey(cwd string) string {
	p := filepath.Clean(cwd)
	if resolved, err := filepath.EvalSymlinks(p); err == nil {
		p = resolved
	}
	return "cwd:" + p
}

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
// survives if only the bash parent is signalled). The timeout is enforced by
// context + Cmd.WaitDelay rather than by waiting on Wait(), so a grandchild that
// escaped the process group cannot keep this call (and the caller's inflight
// locks) alive by holding the output pipe open — see waitDelay. Best-effort by
// construction: a binary that cannot start returns ExitCode -1, never a panic.
func runAction(bin, dir string, args []string, timeout time.Duration) actionResult {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Dir = dir
	cmd.Stdin = nil
	cmd.Env = actionEnv(os.Environ())
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	// Setpgid makes the child's pgid equal its pid, so -pid targets the whole
	// group (the default Cancel would signal the direct child only).
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = waitDelay
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf

	err := cmd.Run()
	if ctx.Err() != nil || errors.Is(err, exec.ErrWaitDelay) {
		return actionResult{ExitCode: -1, TimedOut: true, Output: buf.String()}
	}
	// A binary that could not start never produced a ProcessState; report its
	// error text rather than the (empty) captured output.
	if err != nil && cmd.ProcessState == nil {
		return actionResult{ExitCode: -1, Output: err.Error()}
	}
	return actionResult{ExitCode: exitCode(err), Output: buf.String()}
}

// runTmux runs the tmux binary with a FIXED argument vector (no shell, no
// request string on the command line) and returns its clean stdout separately
// from the combined diagnostic output. Unlike runAction it does NOT strip
// $TMUX/$TMUX_PANE: targeting is by the explicit -t pane id the caller resolved
// server-side, and the command must reach the SAME tmux server whose pane ids the
// peek feature enumerated — which is the server named by serve's inherited $TMUX.
// Best-effort by construction: a tmux that cannot start returns ExitCode -1. Its
// timeout is enforced exactly as runAction's (context + Cmd.WaitDelay).
func runTmux(tmuxBin, dir string, args []string, timeout time.Duration) (stdout string, res actionResult) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, tmuxBin, args...)
	cmd.Dir = dir
	cmd.Stdin = nil
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = waitDelay
	var outBuf, errBuf bytes.Buffer
	cmd.Stdout = &outBuf
	cmd.Stderr = &errBuf

	err := cmd.Run()
	if ctx.Err() != nil || errors.Is(err, exec.ErrWaitDelay) {
		return outBuf.String(), actionResult{ExitCode: -1, TimedOut: true, Output: errBuf.String()}
	}
	if err != nil && cmd.ProcessState == nil {
		return "", actionResult{ExitCode: -1, Output: err.Error()}
	}
	out := errBuf.String()
	if out == "" {
		out = outBuf.String()
	}
	return outBuf.String(), actionResult{ExitCode: exitCode(err), Output: out}
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
	if ee, ok := errors.AsType[*exec.ExitError](err); ok {
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

// tsULARange is Tailscale's IPv6 ULA prefix (fd7a:115c:a1e0::/48). Like
// cgnatRange (its IPv4 counterpart, reused from bind.go) it is not
// internet-routable: a peer with an address in it reached us over the tailnet.
var tsULARange = mustCIDR("fd7a:115c:a1e0::/48")

// remoteAllowed reports whether the request's PEER is on a network we let drive
// commands on this host: loopback (127.0.0.0/8, ::1) or Tailscale (100.64.0.0/10
// CGNAT, fd7a:115c:a1e0::/48 ULA). Everything else is denied.
//
// This is a different job from hostAllowed. hostAllowed inspects the Host
// HEADER to stop DNS rebinding, and it allowlists loopback names - so with
// `serve --host 0.0.0.0` any peer on the LAN could connect, GET the page to
// read the CSRF token out of the HTML, and send `Host: 127.0.0.1` to satisfy
// that check. Only the peer address closes that hole, so both guards run.
//
// Deliberately NEVER consults X-Forwarded-For, X-Real-IP, or any other header:
// those are peer-supplied, so trusting them would hand the bypass straight back.
// Do not "improve" this by adding proxy-header support. Fails closed: an
// address we cannot parse is denied.
func remoteAllowed(remoteAddr string) bool {
	host := remoteAddr
	if h, _, err := net.SplitHostPort(remoteAddr); err == nil {
		host = h
	}
	// A RemoteAddr without a port reaches SplitHostPort's error path and is used
	// whole; brackets around a bare IPv6 literal are stripped either way.
	ip := net.ParseIP(strings.Trim(host, "[]"))
	if ip == nil {
		return false
	}
	// Unwrap IPv4-mapped IPv6 (::ffff:127.0.0.1) so it classifies as the IPv4
	// address it actually is.
	if v4 := ip.To4(); v4 != nil {
		ip = v4
	}
	// cgnatRange is RFC 6598 CGNAT, which Tailscale uses but does not own:
	// mobile hotspots, Starlink, some WISPs and consumer routers also hand out
	// addresses in 100.64.0.0/10. On a `--host 0.0.0.0` serve behind such an
	// uplink a neighbouring device can land in that range and pass this check.
	// Documented in README; binding the tailnet address directly (--tailscale)
	// sidesteps it, since nothing off the tailnet can connect at all.
	return ip.IsLoopback() || cgnatRange.Contains(ip) || tsULARange.Contains(ip)
}

// actionExecs reports whether an action can run a command on the host, which is
// what remoteAllowed gates. Only "end" is exempt: it execs nothing at all, it
// just writes a synthetic SessionEnd to drop a roster row (the same reason it
// deliberately never reaches actionArgv, the allowlist boundary for things that
// do exec). Everything else - the merge-pr/gh PR actions, the tmux session
// actions, rebase - spawns a subprocess.
//
// Phrased as "is this the one exec-free action" rather than as a list of the
// exec-backed ones so the gate fails CLOSED: an action added later is
// network-gated automatically instead of quietly skipping the check. An unknown
// action is likewise gated, and still 400s afterwards.
func actionExecs(action string) bool { return action != "end" }

// forwardingHeaders are the headers a reverse proxy adds when it relays a
// request. Their presence — never their value — is what the peer gate refuses
// on. Spelled in the usual wire form because the names are echoed into the 403
// body; Header.Get canonicalizes before it looks anything up, so the CHECK is
// unaffected by the capitalisation used here.
var forwardingHeaders = []string{"X-Forwarded-For", "X-Real-IP", "Forwarded", "X-Forwarded-Host"}

// forwardingHeader returns the name of the first forwarding header present and
// non-empty in h, or "" when none is.
func forwardingHeader(h http.Header) string {
	for _, name := range forwardingHeaders {
		if strings.TrimSpace(h.Get(name)) != "" {
			return name
		}
	}
	return ""
}

// execPeerAllowed is the SINGLE gate every endpoint that can reach the host
// runs, so /api/action and /api/pane cannot drift apart again. It returns ok
// plus the reason to put in the 403 body, and performs, in order:
//
//   - the forwarding-header refusal, applied UNCONDITIONALLY (even for a request
//     that execs nothing). It is not about exec, it is about attribution: behind
//     a proxy every peer arrives on a loopback socket, so we cannot say who asked
//     for anything at all. A proxied peer must not get to dismiss roster rows
//     either.
//   - hostAllowed, the DNS-rebinding defence. Without it an attacker page that
//     rebinds its own name to 127.0.0.1 is same-origin under that name, and a GET
//     endpoint (/api/pane) needs no token at all.
//   - remoteAllowed, the peer-network gate, but only when execs is true. "end" is
//     the sole exec-free action and stays reachable from any attributable peer on
//     an allowed Host.
//
// It NEVER consults a forwarding header's value — see remoteAllowed.
func execPeerAllowed(r *http.Request, bindHost string, allowed []string, execs bool) (bool, string) {
	if h := forwardingHeader(r.Header); h != "" {
		return false, "request arrived through a proxy (" + h + " present) and cannot be " +
			"attributed to a peer; this endpoint needs a direct connection"
	}
	if !hostAllowed(r.Host, bindHost, allowed) {
		return false, "host not allowed"
	}
	if execs && !remoteAllowed(r.RemoteAddr) {
		return false, "peer not on an allowed network: reading a live pane or running " +
			"a command is restricted to loopback and Tailscale peers"
	}
	return true, ""
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
	// waitDelay is the hard stop applied after a timeout kills the process
	// group. cmd.Stdout is a *bytes.Buffer, so os/exec inserts an os.Pipe plus a
	// copier goroutine, and Wait() blocks until EVERY writer of that pipe closes.
	// A grandchild that called setsid (ssh ControlMaster, git-credential-cache--daemon,
	// anything that daemonises) is in a different process group, survives the
	// kill, and holds the write end open — which used to block the handler
	// goroutine forever and leak its inflight locks. WaitDelay makes os/exec close
	// the pipe and return exec.ErrWaitDelay instead of waiting on such a process.
	waitDelay    = 2 * time.Second
	readyTimeout = 20 * time.Second
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
	// rebaseTimeout bounds EACH step of the rebase sequence (fetch, pull
	// --rebase, push). A fetch or a force-push against a slow remote is the same
	// order of work as a teardown, so it gets the same generous ceiling.
	rebaseTimeout = 120 * time.Second
	// rebaseTotalTimeout bounds the WHOLE sequence, not just each step: without
	// it three 120s steps plus an abort could pin a serve goroutine (and the
	// row's inflight lock) for ~360s+. Each step gets min(rebaseTimeout,
	// remaining budget), and an exhausted budget stops the sequence and reports a
	// timeout instead of starting the next step. Ceiling for a rebase attempt:
	// the probes that run BEFORE the budget starts (default branch, current
	// branch, dirty check, lease — up to six sequential gitProbeTimeout calls),
	// then rebaseTotalTimeout, then rebaseAbortTimeout for the cleanup.
	rebaseTotalTimeout = 180 * time.Second
	// rebaseAbortTimeout bounds `git rebase --abort`, deliberately OUTSIDE the
	// sequence budget: the abort is the cleanup that keeps a work tree from being
	// left mid-rebase, so a sequence that ran out of time must still get to run it.
	rebaseAbortTimeout = 30 * time.Second
	// gitProbeTimeout bounds the read-only ref lookups that resolve the default
	// and current branch. Local ref reads, so a couple of seconds is plenty —
	// same tight, best-effort budget as gitBranch in the capture path.
	gitProbeTimeout = 5 * time.Second
)

// branchNameRe is the strict shape a branch name must have before it may become
// an argv element. Nothing user-supplied ever reaches here — the base is derived
// server-side from git refs — but the rebase argv is the one place a *derived*
// string joins a command line, so it is validated anyway: defence in depth
// against a repo whose refs were crafted to look like git options or paths.
// Leading "-" (an option), ".." (a range/traversal) and anything outside
// [A-Za-z0-9._/-] fail closed.
var branchNameRe = regexp.MustCompile(`^[A-Za-z0-9._][A-Za-z0-9._/-]*$`)

// maxBranchNameLen caps how long a derived branch name may be before it becomes
// an argv element. Git itself has no hard limit, but a pathological ref name is
// never something we want to hand to a command line, so cap it well above any
// real branch.
const maxBranchNameLen = 255

// validBranchName reports whether name is safe to place in an argv.
func validBranchName(name string) bool {
	return name != "" && len(name) <= maxBranchNameLen &&
		!strings.Contains(name, "..") && branchNameRe.MatchString(name)
}

// leaseSHARe is the strict shape a captured lease SHA must have before it joins
// a --force-with-lease argv. Same defence-in-depth posture as validBranchName:
// the value is derived server-side from a git ref, but it is still validated
// before it reaches a command line, and anything else refuses the whole action.
var leaseSHARe = regexp.MustCompile(`^[0-9a-f]{7,64}$`)

// The two ways resolving the default branch can fail, kept distinct so the row
// can say which happened instead of blaming detection for a rejected name.
var (
	errNoDefaultBranch = errors.New("could not resolve the default branch " +
		"(tried origin/HEAD, origin/main, origin/master); nothing was run")
	errAmbiguousDefaultBranch = errors.New("origin/HEAD is missing and BOTH origin/main and " +
		"origin/master exist; refusing to guess the default branch — nothing was run")
)

// gitProbe runs a READ-ONLY git command in dir and returns its trimmed stdout.
// Best-effort with a tight timeout, mirroring gitBranch in the capture path: any
// failure (no git, not a repo, timeout, non-zero exit) yields ("", false) and the
// caller fails closed rather than guessing.
func gitProbe(gitBin, dir string, args ...string) (string, bool) {
	if gitBin == "" || dir == "" {
		return "", false
	}
	ctx, cancel := context.WithTimeout(context.Background(), gitProbeTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, gitBin, append([]string{"-C", dir}, args...)...)
	cmd.Stdin = nil
	// Same process-group + WaitDelay treatment runAction documents at length: a
	// git that leaves a daemonising grandchild (credential cache, ssh
	// ControlMaster) would otherwise outlive gitProbeTimeout holding the output
	// pipe open, blocking the handler while it holds BOTH inflight locks.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = waitDelay
	var buf bytes.Buffer
	cmd.Stdout = &buf
	if err := cmd.Run(); err != nil || ctx.Err() != nil {
		return "", false
	}
	return strings.TrimSpace(buf.String()), true
}

// defaultBranch resolves the repo's default branch for the work tree at dir:
// origin/HEAD when the remote HEAD ref is present locally, else whichever of
// origin/main / origin/master exists.
//
// The fallback is accepted ONLY when exactly one of the two conventional names
// exists. If both do and origin/HEAD is unresolvable, the answer is genuinely
// ambiguous — a stale origin/main lingering in a repo whose real default is,
// say, develop would otherwise make the "never force-push the default branch"
// guard compare against the wrong name and wave the real default through. Every
// failure returns an error and callers treat it as "refuse, run nothing".
func defaultBranch(gitBin, dir string) (string, error) {
	if out, ok := gitProbe(gitBin, dir, "symbolic-ref", "--quiet", "--short", "refs/remotes/origin/HEAD"); ok {
		if b, found := strings.CutPrefix(out, "origin/"); found && b != "" {
			return b, nil
		}
	}
	// origin/HEAD is only created by clone (or an explicit set-head), so plenty
	// of worktrees lack it. Fall back to the two conventional names.
	var found []string
	for _, b := range []string{"main", "master"} {
		if _, ok := gitProbe(gitBin, dir, "rev-parse", "--verify", "--quiet", "refs/remotes/origin/"+b); ok {
			found = append(found, b)
		}
	}
	switch len(found) {
	case 1:
		return found[0], nil
	case 0:
		return "", errNoDefaultBranch
	default:
		return "", errAmbiguousDefaultBranch
	}
}

// worktreeBranch returns the branch currently checked out in dir, or "" if HEAD
// is detached (including mid-rebase) or dir is not a repo. Unlike gitBranch it
// takes the git binary by name so tests can inject a stub, and it deliberately
// does NOT recover a mid-rebase branch: a work tree in that state must not be
// rebased again.
func worktreeBranch(gitBin, dir string) string {
	out, _ := gitProbe(gitBin, dir, "symbolic-ref", "--quiet", "--short", "HEAD")
	return out
}

// rebaseStep is one argument vector in the rebase sequence plus the metadata
// the runner needs. abortOnFail says whether a failure at THIS step can have
// left a half-applied rebase behind and therefore needs `git rebase --abort`.
// Carrying it on the step (rather than keying cleanup off an index into the
// slice) means inserting or reordering a step cannot silently detach the
// cleanup from the step that needs it.
type rebaseStep struct {
	args        []string
	abortOnFail bool
}

// rebaseSteps is the fixed argument-vector sequence the rebase action runs, in
// order. Every element is a literal except base, cur and lease, none of which
// runAction sees until validBranchName / leaseSHARe have accepted them. Kept
// separate from the run loop so a test can assert the exact vectors.
//
// The push names its refspec explicitly (origin cur) rather than relying on a
// bare `git push`: with push.default = matching, or a remote.origin.push
// refspec in config, a bare force-push can rewrite EVERY matching local branch.
// The lease is pinned to the SHA captured before the fetch, because a bare
// --force-with-lease compares against the local remote-tracking ref, which any
// intervening fetch (another agent, an IDE, a background job — all likely in
// the worktrees clodhopper supervises) silently updates, defeating the lease.
// An empty lease means the branch has no remote-tracking ref at all, i.e. it
// was never pushed: creating it needs no force, and asking for one would only
// fail.
//
// Every vector starts with `-C dir` exactly as the probes do, rather than
// relying on cmd.Dir alone. If serve was ever started from a shell with GIT_DIR
// or GIT_WORK_TREE exported, cmd.Dir would be overridden by that environment and
// every step — the force-push included — would operate on a repo other than the
// roster row's worktree, while the guards were computed from the `-C dir` probes
// and would not notice. (Scrubbing git's env is deliberately out of scope; `-C`
// closes the same hole without an env policy.)
func rebaseSteps(dir, base, cur, lease string) []rebaseStep {
	at := func(args ...string) []string { return append([]string{"-C", dir}, args...) }
	push := at("push", "origin", cur)
	if lease != "" {
		push = at("push", "--force-with-lease="+cur+":"+lease, "origin", cur)
	}
	return []rebaseStep{
		{args: at("fetch", "origin", base)},
		// Only the rebase itself can stop mid-flight: a failed fetch has touched
		// nothing, and a failed push happens after the rebase already succeeded.
		{args: at("pull", "--rebase", "origin", base), abortOnFail: true},
		{args: push},
	}
}

// rebaseAbortArgv undoes a rebase that stopped on a conflict, so the work tree
// is never left mid-rebase for the user to discover. `-C dir` for the same
// reason rebaseSteps uses it.
func rebaseAbortArgv(dir string) []string { return []string{"-C", dir, "rebase", "--abort"} }

// rebaseInProgress reports whether dir is actually sitting mid-rebase, by asking
// git where it would keep the interactive/apply state and checking whether that
// path exists.
//
// This is what keeps the cleanup honest. `git pull --rebase` refuses BEFORE it
// starts anything when the work tree has unstaged changes — the normal state of a
// live agent's worktree — so an unconditional `git rebase --abort` there exits
// 128 with "No rebase in progress?" and the user gets told their worktree may be
// half-rebased when nothing was touched at all. Fails closed the other way: a
// probe we cannot run reads as "no rebase in progress", which only skips a
// cleanup that had nothing to clean.
func rebaseInProgress(gitBin, dir string) bool {
	for _, name := range []string{"rebase-merge", "rebase-apply"} {
		p, ok := gitProbe(gitBin, dir, "rev-parse", "--git-path", name)
		if !ok || p == "" {
			continue
		}
		// --git-path answers relative to git's own working directory, which is dir.
		if !filepath.IsAbs(p) {
			p = filepath.Join(dir, p)
		}
		if _, err := os.Stat(p); err == nil {
			return true
		}
	}
	return false
}

// dirtyWorktree reports whether dir has uncommitted changes (ok=false when the
// probe itself could not run, which callers treat as "refuse, run nothing").
func dirtyWorktree(gitBin, dir string) (dirty, ok bool) {
	out, ok := gitProbe(gitBin, dir, "status", "--porcelain")
	return out != "", ok
}

// auditw is where the rebase audit line is written. A package var so tests can
// capture it; serve leaves it on stderr next to the other "clodhopper serve:"
// diagnostics.
var auditw io.Writer = os.Stderr

// auditRebase records one rebase attempt. Unlike debugf this is NOT gated on
// CLODHOPPER_DEBUG: a rebase force-pushes rewritten history, the only
// irreversible thing this tool does, so every attempt leaves a line behind
// whether or not debugging is on.
func auditRebase(now time.Time, sessionID, dir, base, cur, lease, outcome string) {
	fmt.Fprintf(auditw, "clodhopper serve: audit rebase ts=%s session=%q cwd=%q base=%q cur=%q lease=%q outcome=%q\n",
		now.UTC().Format(time.RFC3339), sessionID, dir, base, cur, lease, outcome)
}

// runRebase rebases the work tree at dir onto the repo's default branch and
// force-pushes (with a pinned lease) so the PR is actually updated. Everything
// it needs is derived server-side; the caller passes no request data at all.
//
// Refuses, running nothing, when the default branch cannot be resolved or is
// ambiguous, when either branch name fails validation, when the work tree is not
// on a branch (detached / mid-rebase), when it is sitting ON the default branch,
// or when its branch is literally main/master (a hardcoded backstop that holds
// even if detection resolved something else) — force-pushing the trunk is
// exactly the accident these guards exist to prevent. A rebase that fails or
// times out is aborted and reported as "needs manual rebase"; the push is never
// reached. stepTimeout bounds each step, totalTimeout the whole sequence.
func runRebase(gitBin, dir, sessionID string, stepTimeout, totalTimeout time.Duration, now time.Time) actionResult {
	base, cur, lease := "", "", ""
	outcome := "refused"
	defer func() { auditRebase(now, sessionID, dir, base, cur, lease, outcome) }()
	refuse := func(msg string) actionResult {
		outcome = "refused: " + msg
		return actionResult{ExitCode: -1, Output: msg}
	}

	base, err := defaultBranch(gitBin, dir)
	if err != nil {
		base = ""
		return refuse(err.Error())
	}
	if !validBranchName(base) {
		bad := base
		base = ""
		return refuse("the resolved default branch " + strconv.Quote(bad) +
			" is not a name we will put on a command line; nothing was run")
	}
	cur = worktreeBranch(gitBin, dir)
	if cur == "" {
		return refuse("worktree is not on a named branch (detached HEAD or mid-rebase); nothing was run")
	}
	if !validBranchName(cur) {
		bad := cur
		cur = ""
		return refuse("the checked-out branch " + strconv.Quote(bad) +
			" is not a name we will put on a command line; nothing was run")
	}
	if cur == base {
		return refuse("worktree is on the default branch (" + base + "); refusing to rebase and force-push it")
	}
	// Backstop independent of detection: whatever base resolved to, main and
	// master are never rewritten from here.
	if cur == "main" || cur == "master" {
		return refuse("worktree is on " + cur + "; refusing to rebase and force-push it")
	}

	// Pre-flight the work tree's cleanliness. `git pull --rebase` refuses before
	// starting when there are unstaged changes, which is the ordinary state of a
	// live agent's worktree — the whole population this dashboard supervises — so
	// without this the sequence would half-run and then report a cleanup scare.
	// Refuse up front, accurately, having run nothing. A probe that could not run
	// at all refuses too, matching every other guard here.
	dirty, ok := dirtyWorktree(gitBin, dir)
	if !ok {
		return refuse("could not read the worktree's status (git status --porcelain failed); nothing was run")
	}
	if dirty {
		return refuse("worktree has uncommitted changes; git pull --rebase would refuse to start. " +
			"Commit or stash them first — nothing was run")
	}

	// Capture the lease BEFORE the fetch — that is the whole point. Pinning it
	// afterwards would pin whatever the fetch just wrote, which is precisely the
	// value a bare --force-with-lease already (uselessly) compares against.
	if got, ok := gitProbe(gitBin, dir, "rev-parse", "--verify", "--quiet", "refs/remotes/origin/"+cur); ok {
		if !leaseSHARe.MatchString(got) {
			return refuse("origin/" + cur + " resolved to something that is not a commit id; nothing was run")
		}
		lease = got
	}

	var out strings.Builder
	// The budget is ELAPSED time, so it is measured against the wall clock, not
	// against the injected now (which is the request timestamp and exists purely
	// for the audit line). Mixing the two would make a caller that follows this
	// repo's usual "fixed test date" convention see the budget as already spent
	// before the first step.
	deadline := time.Now().Add(totalTimeout)
	for _, st := range rebaseSteps(dir, base, cur, lease) {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			// Nothing to abort: this step never started, so the work tree is in
			// whatever (clean) state the previous step left it.
			outcome = "timeout: overall budget exhausted"
			out.WriteString("\nrebase timed out (overall budget " + totalTimeout.String() +
				" exhausted); the remaining steps were not run\n")
			return actionResult{ExitCode: -1, TimedOut: true, Output: out.String()}
		}
		fmt.Fprintf(&out, "$ git %s\n", strings.Join(st.args, " "))
		res := runAction(gitBin, dir, st.args, min(stepTimeout, remaining))
		out.WriteString(res.Output)
		if res.ExitCode == 0 && !res.TimedOut {
			continue
		}
		outcome = "failed: git " + stepName(st.args)
		if res.TimedOut {
			outcome = "timeout: git " + stepName(st.args)
		}
		return rebaseAbort(gitBin, dir, st, &out, res, &outcome)
	}
	outcome = "pushed"
	return actionResult{ExitCode: 0, Output: out.String()}
}

// stepName is the git subcommand a step runs ("fetch", "pull", "push"), i.e. the
// first argv element that is not part of the leading `-C dir`. Used for the
// audit line, so it never reaches a command line itself.
func stepName(args []string) string {
	for i := 0; i < len(args); i++ {
		if args[i] == "-C" {
			i++
			continue
		}
		return args[i]
	}
	return "git"
}

// rebaseAbort finishes a failed sequence: it runs `git rebase --abort` when the
// step that failed was one that can leave a half-applied rebase behind, and
// reports the abort's OWN outcome honestly — an abort that itself failed means
// the work tree really is left mid-rebase, and saying otherwise would be a lie
// the user only discovers later.
func rebaseAbort(gitBin, dir string, st rebaseStep, out *strings.Builder, res actionResult, outcome *string) actionResult {
	if st.abortOnFail && !rebaseInProgress(gitBin, dir) {
		// The step that CAN leave a rebase half-applied failed without starting
		// one — `git pull --rebase` refusing on a dirty tree is the common case.
		// Aborting here would fail with "No rebase in progress?" and dispatch the
		// operator to clean up a perfectly healthy worktree.
		out.WriteString("\nno rebase was started (nothing to abort); the worktree is unchanged\n")
		*outcome += "; no rebase started"
		res.Output = out.String()
		return res
	}
	if st.abortOnFail {
		fmt.Fprintf(out, "$ git %s\n", strings.Join(rebaseAbortArgv(dir), " "))
		abort := runAction(gitBin, dir, rebaseAbortArgv(dir), rebaseAbortTimeout)
		out.WriteString(abort.Output)
		if abort.ExitCode == 0 && !abort.TimedOut {
			out.WriteString("\nrebase stopped and was aborted — needs manual rebase\n")
			*outcome += "; aborted"
		} else {
			out.WriteString("\nrebase stopped AND `git rebase --abort` failed — " +
				"the worktree may be left mid-rebase and needs manual cleanup\n")
			*outcome += "; abort FAILED, worktree may be mid-rebase"
		}
	}
	res.Output = out.String()
	return res
}

// actionConfig is serve's write-path state. Binary names are injected so tests
// substitute stubs; token is the per-serve CSRF secret; allowedHosts extends the
// Host allowlist beyond loopback + bindHost.
type actionConfig struct {
	enabled bool
	mergePR string // path/name of the merge-pr binary
	gh      string // path/name of the gh binary
	tmux    string // path/name of the tmux binary (session actions); tests stub it
	git     string // path/name of the git binary (rebase action); tests stub it
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
	// Attribution first, before the body is even read: the proxy refusal and the
	// Host allowlist apply to EVERY action, including the exec-free "end". The
	// peer-network half of the gate needs the action name, so it runs again below
	// with execs set (execPeerAllowed is idempotent and cheap).
	if ok, why := execPeerAllowed(r, cfg.bindHost, cfg.allowedHosts, false); !ok {
		http.Error(w, why, http.StatusForbidden)
		return
	}
	if !tokenOK(r.Header.Get("X-Clodhopper-Token"), cfg.token) {
		http.Error(w, "bad token", http.StatusForbidden)
		return
	}

	// Bound the body before parsing it: every field this endpoint reads is a
	// short identifier, so 64 KiB is generous, and a client cannot make the
	// handler buffer an unbounded form in memory.
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)

	sessionID := r.FormValue("session_id")
	action := r.FormValue("action")
	force := r.FormValue("force") == "true"

	// Network gate for everything that can run a command on this host. It sits
	// in the SHARED guard chain, before any lookup or subprocess, and keys off
	// actionExecs rather than repeating a list of action names, so no action
	// added below can quietly skip it. Distinct message from the Host-header 403
	// above so the two are debuggable apart. The proxy and Host halves already
	// ran unconditionally above; this pass adds the peer-network check.
	if ok, why := execPeerAllowed(r, cfg.bindHost, cfg.allowedHosts, actionExecs(action)); !ok {
		http.Error(w, why, http.StatusForbidden)
		return
	}

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
		if !cfg.inflight.acquire(sidKey(sessionID)) {
			http.Error(w, "action already running for this session", http.StatusConflict)
			return
		}
		defer cfg.inflight.release(sidKey(sessionID))
		// The split runs in the agent's worktree if we know it; tmux inherits the
		// target pane's cwd regardless, so an empty dir is harmless.
		cwd, _ := latestCwdForSession(db, sessionID)
		res := runSessionAction(cfg.tmux, action, pane, cwd, cfg.clearDelay)
		writeActionResult(w, res)
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
		if !cfg.inflight.acquire(sidKey(sessionID)) {
			http.Error(w, "action already running for this session", http.StatusConflict)
			return
		}
		defer cfg.inflight.release(sidKey(sessionID))
		writeActionResult(w, endSession(db, sessionID, now))
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
	// cwd comes from a stored hook payload and becomes exec.Command.Dir below. A
	// relative path would be resolved against serve's OWN working directory,
	// running the action somewhere other than the roster row's worktree; refuse
	// rather than guess.
	if !filepath.IsAbs(cwd) {
		http.Error(w, "session has no absolute working directory", http.StatusNotFound)
		return
	}

	if !cfg.inflight.acquire(sidKey(sessionID)) {
		http.Error(w, "action already running for this session", http.StatusConflict)
		return
	}
	defer cfg.inflight.release(sidKey(sessionID))

	// Dedupe on the WORK TREE as well as the session. Two roster rows can resolve
	// to the same cwd (a resumed or restarted agent, a shared checkout), and two
	// concurrent git/merge-pr runs in one directory means interleaved operations,
	// index.lock contention, or a push firing against whatever branch the other
	// one left checked out — which would sidestep the cur == base guard entirely.
	// The key is namespaced ("cwd:") so it cannot collide with a session id, and
	// acquisition order is fixed (session, then cwd) so two requests can never
	// deadlock against each other. The session lock is released by its defer.
	if !cfg.inflight.acquire(cwdKey(cwd)) {
		http.Error(w, "action already running for this worktree", http.StatusConflict)
		return
	}
	defer cfg.inflight.release(cwdKey(cwd))

	bin := cfg.mergePR
	timeout := actionTimeout
	switch binary {
	case "gh":
		bin, timeout = cfg.gh, readyTimeout
	case "git":
		bin, timeout = cfg.git, rebaseTimeout
	}
	// The git binary drives a multi-step sequence rather than one argv (see
	// runRebase); actionArgv returned no args for it precisely because they are
	// assembled server-side from a validated base branch. timeout bounds each
	// step; rebaseTotalTimeout bounds the sequence as a whole.
	var res actionResult
	if binary == "git" {
		res = runRebase(bin, cwd, sessionID, timeout, rebaseTotalTimeout, now)
	} else {
		res = runAction(bin, cwd, args, timeout)
	}

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

	writeActionResult(w, res)
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
		if amb, ok := errors.AsType[*AmbiguousSessionError](err); ok {
			return actionResult{ExitCode: -1, Output: "session id matches " +
				strconv.Itoa(len(amb.IDs)) + " live sessions; ended nothing"}
		}
		// The raw driver error stays server-side (debugf); the client gets a
		// generic message rather than SQLite detail.
		debugf("action: end %s: %v", sessionID, err)
		return actionResult{ExitCode: -1, Output: "could not end session"}
	}
	if n == 0 {
		return actionResult{ExitCode: -1, Output: "no live session matched (already ended?)"}
	}
	return actionResult{ExitCode: 0}
}

// writeActionResult emits the JSON envelope every action returns, so the
// dashboard's fire() handles each of them — subprocess-backed or not —
// identically.
func writeActionResult(w http.ResponseWriter, res actionResult) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"ok":       res.ExitCode == 0 && !res.TimedOut,
		"exitCode": res.ExitCode,
		"timedOut": res.TimedOut,
		"output":   scrubString(res.Output),
	})
}
