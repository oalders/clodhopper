package main

import (
	"bytes"
	"cmp"
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
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// actionArgv maps a validated dashboard action to the binary and exact argument
// vector to run. This is the security boundary: only these fixed vectors are
// ever executed, and no request string reaches the command line. binary is a
// logical name ("merge-pr" or "gh") that handleAction resolves to an
// injected path. inTmux is true for the merge-pr actions, which run detached in a
// new window of the row's tmux session (see startMergeWindow) rather than as a
// subprocess of serve. ok is false for any unknown action, which the caller
// rejects with 400.
func actionArgv(action string, force bool) (binary string, args []string, inTmux bool, ok bool) {
	switch action {
	case "squash":
		binary, args, inTmux = "merge-pr", []string{"--squash"}, true
	case "squash-admin":
		binary, args, inTmux = "merge-pr", []string{"--squash", "--admin"}, true
	case "close":
		binary, args, inTmux = "merge-pr", []string{"--close"}, true
	case "ready":
		// gh pr ready is a quick subprocess, and --force is meaningless here
		// (ignored).
		return "gh", []string{"pr", "ready"}, false, true
	default:
		return "", nil, false, false
	}
	if force {
		args = append(args, "--force")
	}
	return binary, args, inTmux, true
}

// inflightSet tracks which sessions have an action currently running, so a
// double-click or a second tab cannot start two actions racing on the same
// worktree. Keyed by session id. For the merge actions the lock covers only
// startMergeWindow, not merge-pr's run in the new window: issue #188 accepts
// that a second click starts a second merge-pr window, and merge-pr itself
// handles a PR that is already merged or closed.
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
// requests act on the same directory concurrently — the thing the worktree
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
	Rows     []reportRow // broadcast only; unscrubbed like Output
}

// reportRow is one broadcast target's outcome, for the dashboard's report table.
type reportRow struct {
	Result string `json:"result"` // sent, skipped or error
	Target string `json:"target"`
	Dir    string `json:"dir"` // the session's launch directory, so a row says where on disk
	Reason string `json:"reason"`
}

// runAction runs bin with args in dir, capturing combined output. stdin is left
// nil (closed): any interactive prompt then hits EOF and fails closed instead of
// hanging a serve goroutine. The child gets its own process group so a
// timeout can SIGKILL the whole tree (a child `gh` spawns, such as `git`,
// reparents and survives if only `gh` itself is signalled). The timeout is enforced by
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
// from the combined diagnostic output. It keeps serve's inherited $TMUX:
// targeting is by the explicit -t pane id the caller resolved server-side, and
// the command must reach the SAME tmux server whose pane ids the peek feature
// enumerated — which is the server named by serve's inherited $TMUX.
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
// command strings (/clear, /monitor-ci, /reload-plugins, /kitchen-sink:git-rebase,
// nn claude, nn --resume) are FIXED constants; only pane ids vary, and any pane
// split-window emits is re-validated before reuse.
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
//   - reload-plugins: send /reload-plugins into the agent's own pane. One
//     send-keys call: nothing redraws first, so there is no swallowed-keys race.
//   - rebase: send /kitchen-sink:git-rebase into the agent's own pane, the same
//     single send-keys call. No /clear first: the agent's context helps it
//     resolve conflicts.
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
	case "reload-plugins":
		_, res := runTmux(tmuxBin, dir,
			[]string{"send-keys", "-t", pane, "/reload-plugins", "Enter"}, tmuxTimeout)
		return res
	case "rebase":
		_, res := runTmux(tmuxBin, dir,
			[]string{"send-keys", "-t", pane, "/kitchen-sink:git-rebase", "Enter"}, tmuxTimeout)
		return res
	default:
		return actionResult{ExitCode: -1, Output: "unknown session action"}
	}
}

// tmuxSessionIDRe is the shape of a tmux session id ("$3"), checked before the
// id display-message printed becomes part of a -t target.
var tmuxSessionIDRe = regexp.MustCompile(`^\$\d+$`)

// mergeStarted is what a merge button reports: merge-pr's own outcome lands in
// its tmux window, not in this response.
const mergeStarted = "merge started in tmux window merge-pr"

// startMergeWindow runs merge-pr (args from actionArgv) detached in a new
// `merge-pr` window of the tmux session that owns pane, and returns as soon as
// the window exists. merge-pr can wait many minutes on a merge queue, so it must
// not be a subprocess serve times out and kills; and from inside that session
// its $TMUX/$TMUX_PANE name exactly the session it should tear down on success.
//
// new-window's -t takes a window, not a pane, so the pane's session id is looked
// up first. -a after {end} makes the new window the session's new {end}, and the
// set-option runs in the same tmux command queue pass (";" is tmux's separator,
// not a shell's), so even a merge-pr that fails instantly is still there for
// remain-on-exit to keep its message on screen. With -d, new-window leaves the
// queue's current target alone, which is why set-option names the window again.
func startMergeWindow(tmuxBin, mergePR, pane, dir string, args []string) actionResult {
	// tmux format-expands -c, so a stored cwd holding "#(...)" would run a shell
	// command; such paths are vanishingly rare, so refuse rather than escape.
	if strings.Contains(dir, "#") {
		return actionResult{ExitCode: -1, Output: "worktree path contains '#', which tmux would expand; run merge-pr from the worktree"}
	}
	// The tmux server's PATH is not serve's, so hand it an absolute path.
	// No '#' check: tmux format-expands -c's value, not the command argv.
	bin, err := exec.LookPath(mergePR)
	if err == nil {
		bin, err = filepath.Abs(bin)
	}
	if err != nil {
		return actionResult{ExitCode: -1, Output: "merge-pr not found: " + err.Error()}
	}
	out, res := runTmux(tmuxBin, dir,
		[]string{"display-message", "-p", "-t", pane, "#{session_id}"}, tmuxTimeout)
	if res.ExitCode != 0 || res.TimedOut {
		return res
	}
	sid := strings.TrimSpace(out)
	if !tmuxSessionIDRe.MatchString(sid) {
		return actionResult{ExitCode: -1, Output: "display-message returned no valid session id"}
	}
	target := sid + ":{end}"
	// args is never empty, so the command is 2+ argv elements and tmux execs it
	// directly rather than through a shell.
	argv := append([]string{"new-window", "-a", "-d", "-t", target, "-c", dir, "-n", "merge-pr", bin}, args...)
	argv = append(argv, ";", "set-option", "-p", "-t", target, "remain-on-exit", "failed")
	if _, res := runTmux(tmuxBin, dir, argv, tmuxTimeout); res.ExitCode != 0 || res.TimedOut {
		return res
	}
	return actionResult{Output: mergeStarted}
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

// hostAllowed defends every route against DNS-rebinding: a page on
// attacker.example that rebinds its name to the dashboard's IP becomes
// same-origin and could read the roster and the CSRF token, so we require the
// Host header to name this dashboard. Loopback is always allowed;
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
// what remoteAllowed gates. Only "end", "block" and "unblock" are exempt: they
// exec nothing at all, they just write a synthetic SessionEnd or a blocks row to
// change how a roster row shows (the same reason they deliberately never reach
// actionArgv, the allowlist boundary for things that do exec). Everything else -
// the merge-pr/gh PR actions, the tmux session actions (rebase included) - runs
// tmux or gh.
//
// Phrased as "is this an exec-free action" rather than as a list of the
// exec-backed ones so the gate fails CLOSED: an action added later is
// network-gated automatically instead of quietly skipping the check. An unknown
// action is likewise gated, and still 400s afterwards.
func actionExecs(action string) bool {
	switch action {
	case "end", "block", "unblock":
		return false
	}
	return true
}

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
//   - remoteAllowed, the peer-network gate, but only when execs is true. The
//     exec-free actions (see actionExecs) stay reachable from any attributable
//     peer on an allowed Host.
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

// peerOK is the boolean-only form of execPeerAllowed, for the dashboard RENDER,
// which has no 403 body to fill in. The render runs the FULL gate (execs: true,
// so proxy refusal + Host allowlist + peer network) rather than bare
// remoteAllowed, because the page carries the CSRF token: a DNS-rebinding page
// is a loopback peer as far as remoteAllowed can tell, and while its own fetches
// would still fail hostAllowed, the token it scraped can be replayed by a
// non-browser peer that is free to set Host itself. One gate, one answer.
func peerOK(r *http.Request, bindHost string, allowed []string) bool {
	ok, _ := execPeerAllowed(r, bindHost, allowed, true)
	return ok
}

// tokenOK compares a presented CSRF token against the per-serve secret in
// constant time. An empty secret (feature misconfigured) always fails.
func tokenOK(got, want string) bool {
	if want == "" || len(got) != len(want) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(got), []byte(want)) == 1
}

const (
	// waitDelay is the hard stop applied after a timeout kills the process
	// group. cmd.Stdout is a *bytes.Buffer, so os/exec inserts an os.Pipe plus a
	// copier goroutine, and Wait() blocks until EVERY writer of that pipe closes.
	// A grandchild that called setsid (ssh ControlMaster, git-credential-cache--daemon,
	// anything that daemonises) is in a different process group, survives the
	// kill, and holds the write end open — which used to block the handler
	// goroutine forever and leak its inflight locks. WaitDelay makes os/exec close
	// the pipe and return exec.ErrWaitDelay instead of waiting on such a process.
	waitDelay = 2 * time.Second
	// readyTimeout bounds gh pr ready, a single API call.
	readyTimeout = 20 * time.Second
	// tmuxTimeout bounds a session action's tmux invocations. send-keys,
	// split-window and new-window are quick local RPCs to the tmux server (the
	// last two return as soon as the pane exists, they do not wait for `nn
	// claude` or merge-pr), so a few seconds is generous.
	tmuxTimeout = 10 * time.Second
	// monitorCIClearDelay is how long the monitor-ci action waits after /clear
	// before sending /monitor-ci, so the agent's TUI can finish redrawing its
	// input line first. Long enough to cover a slow (e.g. sandboxed) redraw,
	// short enough that the dashboard's action request still feels immediate.
	monitorCIClearDelay = 750 * time.Millisecond
	// restartPollInterval is how often broadcast-restart re-reads a pane's
	// foreground command; restartExitWait bounds the wait for /exit to drop the
	// pane back to its shell, restartResumeWait the wait for the resumed claude
	// to take it over again (and for the shell after Escape cancels nono's
	// denied-paths review). Exiting can flush hooks and MCP shutdown, so the
	// first is generous; a resume that has not left the shell after a few
	// seconds most likely never started (a typo'd alias, a missing nn).
	restartPollInterval = 250 * time.Millisecond
	restartExitWait     = 15 * time.Second
	restartResumeWait   = 5 * time.Second
	// restartProbeFloor is the least tmux time one pane probe gets, however
	// little of the wait is left: display-message is a local RPC that answers in
	// milliseconds, so a second is generous yet keeps the overrun small.
	restartProbeFloor = time.Second
	// gitProbeTimeout bounds gitProbe's read-only lookups (the peek's origin
	// remote and worktree root). Local reads, so a couple of seconds is plenty —
	// same tight, best-effort budget as gitBranch in the capture path.
	gitProbeTimeout = 5 * time.Second
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
	// pipe open, blocking the caller.
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

// auditw is where the broadcast audit lines are written. A package
// var so tests can capture it; serve leaves it on stderr next to the other
// "clodhopper serve:" diagnostics.
var auditw io.Writer = os.Stderr

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
	clearDelay time.Duration
	// restartPoll / restartExitWait / restartResumeWait pace broadcast-restart's
	// pane polling (see runRestart). serve sets them from the restart* constants;
	// the zero values probe each pane exactly once, so tests never sleep.
	restartPoll       time.Duration
	restartExitWait   time.Duration
	restartResumeWait time.Duration
	token             string
	bindHost          string
	allowedHosts      []string
	inflight          *inflightSet
	// panes is the live tmux pane listing (shared with the peek) the broadcast
	// uses to skip rows whose pane has died; nil lists nothing and skips none.
	panes *paneCache
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

	// Session actions (monitor-ci / new-monitor / reload-plugins / rebase) act on
	// the agent's LIVE tmux pane rather than merging a PR, so they take a
	// separate path: the pane is resolved server-side from session_id and validated
	// against paneIDRe before any tmux command runs. They reuse every guard
	// already checked above (enabled, method, host, token) plus the inflight
	// dedupe below.
	if action == "monitor-ci" || action == "new-monitor" || action == "reload-plugins" || action == "rebase" {
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
		// reload-plugins and rebase send Enter, which would answer a pending
		// prompt, so the same prompt check the broadcast applies holds for an
		// explicit click too. Working rows stay allowed here: the command just queues.
		// The check is keyed on the PANE as well as the session: after a /clear
		// the old row lingers on the roster sharing the pane with the new live
		// session, and Enter lands on whatever owns the pane now. A session not
		// on the roster is refused: the button is only offered on roster rows,
		// so fail closed rather than send blind.
		if action == "reload-plugins" || action == "rebase" {
			agents, err := agentRoster(db, time.Duration(waitingRetainHours())*time.Hour, now)
			if err != nil {
				http.Error(w, "lookup failed", http.StatusInternalServerError)
				return
			}
			onRoster := false
			for _, a := range agents {
				if a.SessionID == sessionID {
					onRoster = true
				}
				if (a.SessionID == sessionID || a.TmuxPane == pane) && promptBlocked(a.Status) {
					http.Error(w, "session is "+promptBlockedReason, http.StatusConflict)
					return
				}
			}
			if !onRoster {
				http.Error(w, "unknown session", http.StatusNotFound)
				return
			}
		}
		sessionLock := sidKey(sessionID)
		if !cfg.inflight.acquire(sessionLock) {
			http.Error(w, "action already running for this session", http.StatusConflict)
			return
		}
		defer cfg.inflight.release(sessionLock)
		// The split runs in the agent's worktree if we know it; tmux inherits the
		// target pane's cwd regardless, so an empty dir is harmless.
		cwd, _ := latestCwdForSession(db, sessionID)
		res := runSessionAction(cfg.tmux, action, pane, cwd, cfg.clearDelay)
		writeActionResult(w, res)
		return
	}

	// broadcast-reload sends /reload-plugins to every eligible roster row at
	// once; broadcast-restart exits and resumes every eligible row's claude. They
	// take no session_id: the targets are re-derived here from a fresh roster
	// (never from the client), minus rows whose tmux pane has died (they would
	// only report "can't find pane"), and every pane still passes paneIDRe. One
	// lock per broadcast kind keeps two of a kind from interleaving; each send
	// also takes its row's session lock, so it cannot race a per-row action or
	// the other broadcast.
	if action == "broadcast-reload" || action == "broadcast-restart" {
		broadcastLock := "broadcast:reload-plugins"
		if action == "broadcast-restart" {
			broadcastLock = "broadcast:restart"
		}
		if !cfg.inflight.acquire(broadcastLock) {
			http.Error(w, "broadcast already running", http.StatusConflict)
			return
		}
		defer cfg.inflight.release(broadcastLock)
		agents, err := agentRoster(db, time.Duration(waitingRetainHours())*time.Hour, now)
		if err != nil {
			http.Error(w, "lookup failed", http.StatusInternalServerError)
			return
		}
		agents = dropDeadPanes(agents, cfg.panes.panes(now))
		if action == "broadcast-restart" {
			writeActionResult(w, broadcastRestart(cfg, agents, now))
		} else {
			writeActionResult(w, broadcastReload(cfg, agents, r.FormValue("include_busy") == "true", now))
		}
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
		sessionLock := sidKey(sessionID)
		if !cfg.inflight.acquire(sessionLock) {
			http.Error(w, "action already running for this session", http.StatusConflict)
			return
		}
		defer cfg.inflight.release(sessionLock)
		writeActionResult(w, endSession(db, sessionID, now))
		return
	}

	// "block" marks this row blocked on another (see applyBlocks) and "unblock"
	// clears that. Like "end" they run no subprocess and never reach actionArgv;
	// both ids are checked against a fresh roster and only ever travel as bound
	// SQL parameters.
	if action == "block" || action == "unblock" {
		if sessionID == "" {
			http.Error(w, "unknown session", http.StatusNotFound)
			return
		}
		if action == "unblock" {
			writeActionResult(w, unblockAction(db, sessionID))
		} else {
			writeActionResult(w, blockAction(db, sessionID, r.FormValue("blocker_session_id"), now))
		}
		return
	}

	_, args, inTmux, ok := actionArgv(action, force)
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

	// Compute each lock key ONCE and release the very same string.
	//
	// cwdKey's answer is NOT stable across the action: it runs EvalSymlinks, and
	// the worktree can be REMOVED meanwhile (merge-pr tears it down, and its
	// window outlives this request) — after which the same path resolves to its
	// unresolved spelling instead. Releasing under
	// a re-derived key would delete nothing and pin the entry for the life of the
	// process, 409-ing every later session that resolves there. Today
	// `defer release(cwdKey(cwd))` would happen to be safe (a deferred call's
	// arguments are evaluated where the defer is written, i.e. before the
	// subprocess runs), but that safety is invisible at the call site and one
	// refactor into a closure away from a permanent lock leak. Naming the key
	// states the invariant instead of relying on it. sidKey is stable, but every
	// acquire/release pair in this handler — including the tmux-session and `end`
	// branches above — is spelled the same way, so the pattern is uniform.
	sessionLock := sidKey(sessionID)
	if !cfg.inflight.acquire(sessionLock) {
		http.Error(w, "action already running for this session", http.StatusConflict)
		return
	}
	defer cfg.inflight.release(sessionLock)

	// Dedupe on the WORK TREE as well as the session. Two roster rows can resolve
	// to the same cwd (a resumed or restarted agent, a shared checkout), and two
	// concurrent actions in one directory would race each other on one PR.
	// The key is namespaced ("cwd:") so it cannot collide with a session id, and
	// acquisition order is fixed (session, then cwd) so two requests can never
	// deadlock against each other. The session lock is released by its defer.
	worktreeLock := cwdKey(cwd)
	if !cfg.inflight.acquire(worktreeLock) {
		http.Error(w, "action already running for this worktree", http.StatusConflict)
		return
	}
	defer cfg.inflight.release(worktreeLock)

	// The merge-pr actions only START merge-pr, in the row's own tmux session
	// (see startMergeWindow), so a row with no live pane has nowhere to run it.
	// No synthetic SessionEnd: the row drops like any session merge-pr kills from
	// a terminal, once dropDeadPanes sees its pane gone.
	if inTmux {
		pane, err := latestPaneForSession(db, sessionID)
		if err != nil {
			http.Error(w, "lookup failed", http.StatusInternalServerError)
			return
		}
		if !paneIDRe.MatchString(pane) {
			writeActionResult(w, actionResult{ExitCode: -1, Output: "session is not in tmux; run merge-pr from a terminal in its worktree"})
			return
		}
		writeActionResult(w, startMergeWindow(cfg.tmux, cfg.mergePR, pane, cwd, args))
		return
	}

	writeActionResult(w, runAction(cfg.gh, cwd, args, readyTimeout))
}

// reloadSkipReason says why broadcast-reload must NOT send /reload-plugins to a
// roster row, or "" when it should. Enter is part of the send, so a row parked
// on a permission prompt or a question is never a target, include_busy or not:
// the keystrokes would answer the prompt rather than run the command. Unknown
// statuses fail closed.
func reloadSkipReason(a Agent, includeBusy bool) string {
	if !paneIDRe.MatchString(a.TmuxPane) {
		return "not in tmux — reload manually"
	}
	if promptBlocked(a.Status) {
		return promptBlockedReason
	}
	switch a.Status {
	case statusWorking:
		if includeBusy {
			return ""
		}
		return "working — retry, or include working"
	case statusWaiting, statusIdle, statusBackground:
		return ""
	}
	return "status " + strconv.Quote(a.Status) + " — not sent"
}

const promptBlockedReason = "blocked on a prompt — Enter would answer it"

// promptBlocked reports whether a roster status means the agent is parked on a
// permission prompt or a question, where a send ending in Enter would answer it.
// Shared by the per-row reload-plugins/rebase guard and the broadcast.
func promptBlocked(status string) bool {
	switch status {
	case statusApproval, statusInput, statusNeedsYou:
		return true
	}
	return false
}

// broadcastReload sends /reload-plugins to every eligible row in agents (see
// broadcast). Each broadcast leaves one audit line, since it types into every
// agent at once.
func broadcastReload(cfg *actionConfig, agents []Agent, includeBusy bool, now time.Time) actionResult {
	res, sent, skipped, errs := broadcast(cfg, agents,
		func(a Agent) string { return reloadSkipReason(a, includeBusy) },
		func(a Agent) actionResult { return runSessionAction(cfg.tmux, "reload-plugins", a.TmuxPane, a.Cwd, 0) })
	fmt.Fprintf(auditw, "clodhopper serve: audit broadcast-reload ts=%s include_busy=%t sent=%d skipped=%d errors=%d\n",
		now.UTC().Format(time.RFC3339), includeBusy, sent, skipped, errs)
	return res
}

// broadcast runs send against every row in agents that skip has no reason to
// pass over, and returns a one-line-per-row report, problems first. Rows
// sharing a pane (a resumed or restarted agent leaves its old session on the
// roster) are sent to once, via the newest session, which is the one actually
// running there. Each send holds its row's session lock, and sends to different
// rows run concurrently. ok is false iff a send failed or timed out; skips are
// expected and do not fail it.
func broadcast(cfg *actionConfig, agents []Agent, skip func(Agent) string, send func(Agent) actionResult) (res actionResult, sent, skipped, errs int) {
	if len(agents) == 0 {
		return actionResult{Output: "no sessions on the roster"}, 0, 0, 0
	}
	newest := map[string]Agent{}
	for _, a := range agents {
		if cur, ok := newest[a.TmuxPane]; a.TmuxPane != "" && (!ok || a.IdleSince > cur.IdleSince) {
			newest[a.TmuxPane] = a
		}
	}
	type target struct {
		a           Agent
		label, lock string
	}
	var targets []target
	for _, a := range agents {
		// Project, then branch, then session: a bare branch like "main" does not
		// say which repo the row is.
		var parts []string
		for _, p := range []string{a.SourceApp, cmp.Or(a.Branch, a.BranchGuess, a.TmuxSession), shortID(a.SessionID)} {
			if p != "" {
				parts = append(parts, p)
			}
		}
		label := strings.Join(parts, " ")
		reason := skip(a)
		if n, ok := newest[a.TmuxPane]; reason == "" && ok && n.SessionID != a.SessionID {
			reason = "same pane as newer session " + shortID(n.SessionID)
		}
		lock := sidKey(a.SessionID)
		if reason == "" && !cfg.inflight.acquire(lock) {
			reason = "action already running"
		}
		if reason != "" {
			skipped++
			res.Rows = append(res.Rows, reportRow{Result: "skipped", Target: label, Dir: a.launchCwd(), Reason: reason})
			continue
		}
		targets = append(targets, target{a: a, label: label, lock: lock})
	}
	// Sends run concurrently: a restart can spend ~20s on one pane, and the
	// panes are independent (each target already holds its own session lock).
	results := make([]actionResult, len(targets))
	var wg sync.WaitGroup
	for i, tg := range targets {
		wg.Go(func() {
			defer cfg.inflight.release(tg.lock)
			results[i] = send(tg.a)
		})
	}
	wg.Wait()
	for i, tg := range targets {
		if r := results[i]; r.ExitCode != 0 || r.TimedOut {
			res.ExitCode = -1
			errs++
			res.Rows = append(res.Rows, reportRow{Result: "error", Target: tg.label, Dir: tg.a.launchCwd(), Reason: strings.TrimSpace(r.Output)})
			continue
		}
		sent++
		res.Rows = append(res.Rows, reportRow{Result: "sent", Target: tg.label, Dir: tg.a.launchCwd()})
	}
	// Problems first, so the rows needing a look are never below the fold.
	rank := map[string]int{"error": 0, "skipped": 1, "sent": 2}
	slices.SortFunc(res.Rows, func(x, y reportRow) int {
		return cmp.Or(cmp.Compare(rank[x.Result], rank[y.Result]), cmp.Compare(x.Target, y.Target))
	})
	var out strings.Builder
	for _, row := range res.Rows {
		out.WriteString(row.Result + " " + row.Target)
		if row.Result != "sent" {
			out.WriteString(": " + row.Reason)
		}
		out.WriteString("\n")
	}
	res.Output = strings.TrimRight(out.String(), "\n")
	return res, sent, skipped, errs
}

// sessionIDRe is the shape a session id must have before it joins the resume
// command line: a lowercase UUID, which is what Claude Code issues. The id comes
// from a stored hook payload, so this is defence in depth against a crafted one smuggling options or shell syntax into the pane.
var sessionIDRe = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// shellCommands are the pane foreground commands that mean claude has exited
// and the pane is back at its shell. Anything else (claude itself, node, a
// wrapper) fails closed: the restart keeps waiting and then gives up.
var shellCommands = map[string]bool{"bash": true, "zsh": true, "fish": true, "sh": true}

// restartSkipReason says why broadcast-restart must NOT exit and resume a
// roster row, or "" when it should. Stricter than reload: /exit kills the
// process, so only a row with nothing in flight is a target — mid-turn work,
// background work and running subagents would all be lost. Everything that
// reaches the resume argv (session id, and the cwd the pane must be in) is
// checked HERE, before /exit is sent, so a row we could not resume is never
// killed. Unknown statuses fail closed.
func restartSkipReason(a Agent) string {
	if !paneIDRe.MatchString(a.TmuxPane) {
		return "not in tmux — restart manually"
	}
	if promptBlocked(a.Status) {
		return promptBlockedReason
	}
	switch a.Status {
	case statusIdle, statusWaiting:
	case statusWorking:
		return "working — its turn would be cut off"
	case statusBackground:
		return "waiting on background work — it would be lost"
	case statusSubagents:
		return "waiting on agents or shells — /exit would kill them"
	default:
		return "status " + strconv.Quote(a.Status) + " — not restarted"
	}
	if !sessionIDRe.MatchString(a.SessionID) {
		return "session id is not a UUID — restart manually"
	}
	if !filepath.IsAbs(a.launchCwd()) {
		return "no working directory recorded — restart manually"
	}
	return ""
}

// launchCwd is where a was launched, falling back to its latest Cwd when the
// roster did not record one (an Agent built outside agentRoster).
func (a Agent) launchCwd() string {
	if a.LaunchCwd != "" {
		return a.LaunchCwd
	}
	return a.Cwd
}

// broadcastRestart exits and resumes claude in every eligible row (see
// restartSkipReason and runRestart), with one audit line per broadcast.
func broadcastRestart(cfg *actionConfig, agents []Agent, now time.Time) actionResult {
	res, sent, skipped, errs := broadcast(cfg, agents, restartSkipReason,
		func(a Agent) actionResult { return runRestart(cfg, a) })
	fmt.Fprintf(auditw, "clodhopper serve: audit broadcast-restart ts=%s sent=%d skipped=%d errors=%d\n",
		now.UTC().Format(time.RFC3339), sent, skipped, errs)
	return res
}

// runRestart sends /exit into a's pane, waits for the pane to drop back to a
// shell, checks that shell is in a's launch directory, then types the resume
// command. If nono's exit-time denied-paths review is the last thing on screen
// instead, it sends Escape to cancel that and waits for the shell again. A pane
// already at a shell gets nothing: the roster row is stale and /exit would land
// in the shell. The resume is the FIXED string `nn --resume <id>` (nn already
// execs claude, so naming claude again would pass it as a prompt), the id
// validated by restartSkipReason; never --continue or --fork-session, which
// would pick or mint a different session. A pane that never reaches a shell, or
// reaches one somewhere else, gets nothing more typed into it: a cd is never
// interpolated. Finally it waits for claude to take the pane over, so a resume
// that silently failed to start is reported rather than counted as sent.
func runRestart(cfg *actionConfig, a Agent) actionResult {
	// The launch dir, not the latest Cwd: `claude --resume` finds a session by
	// where it was started, and a cd'd-into scratch dir may since be gone.
	pane, dir := a.TmuxPane, a.launchCwd()
	if pollShell(cfg, pane, dir, true, 0) {
		return actionResult{ExitCode: -1, Output: "Claude isn't running in this pane (it's at a shell) — nothing sent"}
	}
	if _, res := runTmux(cfg.tmux, dir, []string{"send-keys", "-t", pane, "/exit", "Enter"}, tmuxTimeout); res.ExitCode != 0 || res.TimedOut {
		return res
	}
	if !pollShell(cfg, pane, dir, true, cfg.restartExitWait) {
		// nono can hold the pane after claude exits with an interactive "Review
		// denied paths" prompt. nn runs claude as a child of nono, so a pane
		// command of "nono" cannot tell that prompt from a claude that ignored
		// /exit — only the screen can (nonoReviewOnScreen). Unless the review is
		// the last thing on it (or if the capture fails) nothing is typed.
		screen, res := runTmux(cfg.tmux, dir, []string{"capture-pane", "-p", "-t", pane}, tmuxTimeout)
		if res.ExitCode != 0 || res.TimedOut || !nonoReviewOnScreen(screen) {
			return actionResult{ExitCode: -1, Output: "still running " + cfg.restartExitWait.String() + " after /exit (background shells/agents, or the wrapper didn't exit) — nothing more sent"}
		}
		// Checked against nono 0.79.0's review: Esc cancels and grants nothing,
		// whereas Enter would confirm its default "grant" — so Escape is the only
		// key sent.
		if _, res := runTmux(cfg.tmux, dir, []string{"send-keys", "-t", pane, "Escape"}, tmuxTimeout); res.ExitCode != 0 || res.TimedOut {
			return res
		}
		if !pollShell(cfg, pane, dir, true, cfg.restartResumeWait) {
			return actionResult{ExitCode: -1, Output: "nono's denied-paths review still open after Escape — nothing more sent"}
		}
	}
	path, res := runTmux(cfg.tmux, dir, []string{"display-message", "-p", "-t", pane, "#{pane_current_path}"}, tmuxTimeout)
	if res.ExitCode != 0 || res.TimedOut {
		return res
	}
	// cwdKey cleans and resolves symlinks, so two spellings of one worktree match.
	if path = strings.TrimSpace(path); path == "" || cwdKey(path) != cwdKey(dir) {
		return actionResult{ExitCode: -1, Output: "exited, but the shell is in " + strconv.Quote(path) + ", not " + strconv.Quote(dir) + " where the session started — resume manually"}
	}
	if _, res := runTmux(cfg.tmux, dir, []string{"send-keys", "-t", pane, "nn --resume " + a.SessionID, "Enter"}, tmuxTimeout); res.ExitCode != 0 || res.TimedOut {
		return res
	}
	if !pollShell(cfg, pane, dir, false, cfg.restartResumeWait) {
		return actionResult{ExitCode: -1, Output: "exited but did not resume — check the pane"}
	}
	return actionResult{}
}

// nonoReviewOnScreen reports whether nono 0.79.0's denied-paths review is the
// last thing on screen: its title line, its "Esc cancel" hint, and no claude
// input box ("───" rule) drawn below it — so a live claude merely showing the
// phrase (in a transcript, a diff, a tool-output gutter) is very unlikely to
// get Escape.
func nonoReviewOnScreen(screen string) bool {
	lines := strings.Split(screen, "\n")
	title := -1
	for i, l := range lines {
		if strings.HasPrefix(strings.TrimLeft(l, " "), "[nono] Review denied paths") {
			title = i
		}
	}
	if title < 0 {
		return false
	}
	hint := false
	for i, l := range lines[title:] {
		if i > 0 && strings.Contains(l, "───") {
			return false
		}
		hint = hint || strings.Contains(l, "Esc cancel")
	}
	return hint
}

// pollShell reads pane's foreground command every cfg.restartPoll until
// whether it is a shell equals atShell, giving up after wait. A failed or empty
// read never counts as a match either way, so a tmux error cannot pass for an exit
// or a resume. Wall-clock bounded: each probe's tmux
// timeout is capped by the time left, so the whole wait overruns wait by at most
// restartProbeFloor — the minimum a probe gets, so a zero wait still probes once.
func pollShell(cfg *actionConfig, pane, dir string, atShell bool, wait time.Duration) bool {
	deadline := time.Now().Add(wait)
	for {
		timeout := min(tmuxTimeout, max(time.Until(deadline), restartProbeFloor))
		out, res := runTmux(cfg.tmux, dir, []string{"display-message", "-p", "-t", pane, "#{pane_current_command}"}, timeout)
		if cmd := strings.TrimSpace(out); res.ExitCode == 0 && !res.TimedOut && cmd != "" && shellCommands[cmd] == atShell {
			return true
		}
		left := time.Until(deadline)
		if left <= 0 {
			return false
		}
		time.Sleep(min(cfg.restartPoll, left))
	}
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

// blockAction records that sessionID is blocked on blockerID for the dashboard's
// block action. Both must be live on a fresh roster (the picker only offers
// roster rows, so fail closed on anything else); a self-block or a cycle is
// refused with its reason. A lookup error stays server-side, as in endSession.
func blockAction(db *sql.DB, sessionID, blockerID string, now time.Time) actionResult {
	agents, err := agentRoster(db, time.Duration(waitingRetainHours())*time.Hour, now)
	if err != nil {
		debugf("action: block %s: %v", sessionID, err)
		return actionResult{ExitCode: -1, Output: "could not block session"}
	}
	live := map[string]bool{}
	for _, a := range agents {
		live[a.SessionID] = true
	}
	if !live[sessionID] || !live[blockerID] {
		return actionResult{ExitCode: -1, Output: "both rows must be live on the roster"}
	}
	if err := addBlock(db, sessionID, blockerID, live, now); err != nil {
		if errors.Is(err, errSelfBlock) || errors.Is(err, errBlockCycle) {
			return actionResult{ExitCode: -1, Output: err.Error()}
		}
		debugf("action: block %s: %v", sessionID, err)
		return actionResult{ExitCode: -1, Output: "could not block session"}
	}
	return actionResult{ExitCode: 0}
}

// unblockAction drops every block on sessionID for the chip's ✕.
func unblockAction(db *sql.DB, sessionID string) actionResult {
	if _, err := unblockSession(db, sessionID); err != nil {
		debugf("action: unblock %s: %v", sessionID, err)
		return actionResult{ExitCode: -1, Output: "could not unblock session"}
	}
	return actionResult{ExitCode: 0}
}

// writeActionResult emits the JSON envelope every action returns, so the
// dashboard's fire() handles each of them — subprocess-backed or not —
// identically.
func writeActionResult(w http.ResponseWriter, res actionResult) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	body := map[string]any{
		"ok":       res.ExitCode == 0 && !res.TimedOut,
		"exitCode": res.ExitCode,
		"timedOut": res.TimedOut,
		"output":   scrubString(res.Output),
	}
	if res.Rows != nil {
		rows := make([]reportRow, len(res.Rows))
		for i, r := range res.Rows {
			rows[i] = reportRow{Result: r.Result, Target: scrubString(r.Target), Dir: scrubString(r.Dir), Reason: scrubString(r.Reason)}
		}
		body["rows"] = rows
	}
	_ = json.NewEncoder(w).Encode(body)
}
