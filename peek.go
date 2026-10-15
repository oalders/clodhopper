package main

import (
	"context"
	"io"
	"net/http"
	"os/exec"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

// paneIDRe matches a tmux pane id ("%" followed by digits, e.g. "%3"). Pane ids
// are the peek feature's targeting key: they are ASCII, stable within a pane's
// life, and — unlike session names — need no cleaning. Any value that fails this
// pattern is rejected before it reaches a tmux command.
var paneIDRe = regexp.MustCompile(`^%\d+$`)

const (
	paneLinesDefault = 40
	paneLinesMax     = 2000
	paneTTL          = 5 * time.Second
)

// peekConfig holds the serve-time state for the live pane peek. When enabled is
// false the feature is entirely off: no roster control renders and /api/pane
// returns 403.
type peekConfig struct {
	enabled bool
	lines   int
	cache   *paneCache
	repos   *cwdCache[string] // cwd → GitHub "owner/repo" for linking the footer's PR #N; nil resolves nothing
	// bindHost/allowedHosts mirror actionConfig's: /api/pane execs tmux and
	// streams live transcript text, so it runs the SAME gate the exec-backed
	// actions do (execPeerAllowed), which needs the Host allowlist too.
	bindHost     string
	allowedHosts []string
}

// clampPaneLines bounds the --pane-lines value; anything out of [1, paneLinesMax]
// falls back to the default rather than producing a malformed capture range.
func clampPaneLines(n int) int {
	if n < 1 || n > paneLinesMax {
		return paneLinesDefault
	}
	return n
}

// livePane is one listed tmux pane: its normalised session name, foreground
// command and current directory.
type livePane struct{ session, command, path string }

// paneCache memoises the live tmux panes (pane id → livePane) so the dashboard
// (which polls) does not shell out to tmux on every render. now is injected so
// the TTL is testable, matching cwdCache.
type paneCache struct {
	mu  sync.Mutex
	set map[string]livePane
	at  time.Time
}

func newPaneCache() *paneCache { return &paneCache{} }

// panes returns the live pane listing, refreshing it via listPanes when it is
// unset or older than paneTTL. A tmux failure caches an empty listing for the
// TTL — the peek is simply unavailable, never an error. A nil cache lists
// nothing, so a config built without one hides no rows.
func (c *paneCache) panes(now time.Time) map[string]livePane {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.set == nil || now.Sub(c.at) >= paneTTL {
		c.set = listPanes()
		c.at = now
	}
	return c.set
}

// live reports whether pane is a currently-live tmux pane.
func (c *paneCache) live(pane string, now time.Time) bool {
	_, ok := c.panes(now)[pane]
	return ok
}

// listPanes returns every live tmux pane id across all sessions, mapped to its
// session name (normalised exactly as ingest stores tmux_session), foreground
// command and current path, or an empty map if tmux is missing, unreachable
// (different socket / container / not running), or times out. Best-effort, like lookupCI.
func listPanes() map[string]livePane {
	set := map[string]livePane{}
	if _, err := exec.LookPath("tmux"); err != nil {
		return set
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "tmux", "list-panes", "-a", "-F", "#{pane_id}\t#{session_name}\t#{pane_current_command}\t#{pane_current_path}").Output()
	if err != nil {
		return set
	}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		// Path last, and SplitN, so a tab inside it cannot shift the other fields.
		if f := strings.SplitN(line, "\t", 4); len(f) == 4 {
			set[f[0]] = livePane{session: tmuxSessionLabel(f[1]), command: f[2], path: f[3]}
		}
	}
	return set
}

// dropDeadPanes filters out roster rows whose tmux pane is gone: the pane was
// closed or the tmux server restarted, so the session never sent SessionEnd. A
// pane listed under a different session than the row recorded is gone too (pane
// ids restart from %0 with a new server). Hiding a live agent is the worse
// mistake, so it fails safe: rows without a pane id are always kept, and nothing
// is hidden unless at least one row matches the listing — an empty or unrelated
// listing (tmux missing, or serve on another socket) is not evidence of death.
// A live pane back at a shell in the row's launch directory is hidden too: claude
// exited without SessionEnd (a crash) and the pane survived. A pane in the row's
// launch directory counts as the row's even under another session label: the
// stored label goes stale when the name or its normalisation changes (#173).
func dropDeadPanes(agents []Agent, panes map[string]livePane) []Agent {
	alive := func(a Agent) bool {
		p, ok := panes[a.TmuxPane]
		if !ok {
			return false
		}
		dir := a.launchCwd()
		return a.TmuxSession == "" || a.TmuxSession == p.session ||
			(p.path != "" && dir != "" && cwdKey(p.path) == cwdKey(dir))
	}
	if !slices.ContainsFunc(agents, alive) {
		return agents
	}
	return slices.DeleteFunc(agents, func(a Agent) bool {
		if !paneIDRe.MatchString(a.TmuxPane) {
			return false
		}
		if !alive(a) {
			return true
		}
		// Requiring the launch dir guards against a reused pane id holding an unrelated shell.
		p, dir := panes[a.TmuxPane], a.launchCwd()
		return shellCommands[p.command] && p.path != "" && dir != "" && cwdKey(p.path) == cwdKey(dir)
	})
}

// capturePane returns the last `lines` rows of pane, scrubbed for secret-shaped
// substrings. ok is false on any error/timeout. pane MUST already be validated
// against paneIDRe by the caller; it is passed as a discrete argument after -t,
// never through a shell.
func capturePane(pane string, lines int) (string, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "tmux", "capture-pane", "-p", "-t", pane, "-S", "-"+strconv.Itoa(lines)).Output()
	if err != nil {
		return "", false
	}
	return scrubString(string(out)), true
}

// handlePane serves GET /api/pane?pane=%N: the live pane's last N lines as
// text/plain. It is gated (403 when disabled), validates the pane id against
// paneIDRe AND the live set (double gate; the id is never a shell token), and
// degrades to 404 on any miss or capture failure — never 500. now is injected for
// testing.
func handlePane(w http.ResponseWriter, r *http.Request, peek *peekConfig, now time.Time) {
	if !peek.enabled {
		http.Error(w, "pane peek disabled", http.StatusForbidden)
		return
	}
	// capturePane execs tmux and streams LIVE pane text — the very content the
	// storage invariant keeps out of the database — so it runs the SAME gate the
	// exec-backed dashboard actions run: proxy refusal, Host allowlist, peer
	// network. The Host half matters especially here because this is a GET and
	// therefore carries no CSRF token: without it an attacker page that rebinds
	// its name to the dashboard's address could walk the (trivially enumerable)
	// pane ids and read transcripts same-origin.
	if ok, why := execPeerAllowed(r, peek.bindHost, peek.allowedHosts, true); !ok {
		http.Error(w, why, http.StatusForbidden)
		return
	}
	pane := r.URL.Query().Get("pane")
	if !paneIDRe.MatchString(pane) || !peek.cache.live(pane, now) {
		http.NotFound(w, r)
		return
	}
	out, ok := capturePane(pane, peek.lines)
	if !ok {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = io.WriteString(w, out)
}

// githubRemoteRe matches the github.com origin URL shapes (scp-style ssh,
// ssh://, https) and captures owner and repo. The character classes are strict
// because the result is spliced into an href; anything else fails closed to "".
var githubRemoteRe = regexp.MustCompile(`^(?:git@github\.com:|ssh://git@github\.com/|https://github\.com/)([A-Za-z0-9-]+)/([A-Za-z0-9._-]+?)(?:\.git)?$`)

// parseGitHubRepo returns "owner/repo" for a github.com remote URL, else "".
func parseGitHubRepo(remote string) string {
	m := githubRemoteRe.FindStringSubmatch(remote)
	if m == nil || m[2] == "." || m[2] == ".." {
		return ""
	}
	return m[1] + "/" + m[2]
}

// lookupRepo resolves cwd's origin remote to a GitHub "owner/repo", or "" when
// git fails or the remote is not on github.com. Best-effort, like lookupCI.
func lookupRepo(cwd string) string {
	out, _ := gitProbe("git", cwd, "remote", "get-url", "origin")
	return parseGitHubRepo(out)
}
