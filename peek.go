package main

import (
	"context"
	"io"
	"net/http"
	"os/exec"
	"regexp"
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

// paneCache memoises the set of live tmux pane ids so the dashboard (which polls)
// does not shell out to tmux on every render. now is injected so the TTL is
// testable, matching ciCache.
type paneCache struct {
	mu  sync.Mutex
	set map[string]bool
	at  time.Time
}

func newPaneCache() *paneCache { return &paneCache{} }

// live reports whether pane is a currently-live tmux pane, refreshing the cached
// set via listPanes when it is unset or older than paneTTL. A tmux failure caches
// an empty set for the TTL — the peek is simply unavailable, never an error.
func (c *paneCache) live(pane string, now time.Time) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.set == nil || now.Sub(c.at) >= paneTTL {
		c.set = listPanes()
		c.at = now
	}
	return c.set[pane]
}

// listPanes returns every live tmux pane id across all sessions, or an empty set
// if tmux is missing, unreachable (different socket / container / not running),
// or times out. Best-effort, like lookupCI.
func listPanes() map[string]bool {
	set := map[string]bool{}
	if _, err := exec.LookPath("tmux"); err != nil {
		return set
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "tmux", "list-panes", "-a", "-F", "#{pane_id}").Output()
	if err != nil {
		return set
	}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if line != "" {
			set[line] = true
		}
	}
	return set
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
	setSecurityHeaders(w)
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
