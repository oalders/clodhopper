package main

import (
	"cmp"
	"context"
	"io"
	"maps"
	"net/http"
	"os/exec"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
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
	enabled  bool
	lines    int
	cache    *paneCache
	repos    *cwdCache[string]            // cwd → GitHub "owner/repo" for linking the footer's PR #N; nil resolves nothing
	roots    *cwdCache[string]            // cwd → its worktree root, for the branch column's dir suffix; nil resolves nothing
	names    *cwdCache[map[string]string] // namesDir → Claude session names (lookupClaudeNames); nil or "" resolves nothing
	namesDir string
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

// livePane is one listed tmux pane: its normalised session name (the matching
// key, in ingest's tmux_session form), the session name's trailing title segment
// ("" if it has none; see paneLabel), foreground command, current directory and
// the pane's root process id (0 if unparseable), whose tree paneCache.mem sums.
// sessionID, windowID and window (its scrubbed name, else its index) place it
// for a pane block (see applyPaneBlocks).
type livePane struct {
	session, title, command, path string
	pid                           int
	sessionID, windowID, window   string
}

// paneTitle returns the last PUA-delimited segment of a raw tmux session name
// when there is more than one ("<icon> fix-12 <icon> Fix the thing" → "Fix the
// thing"), else "". Read by serve from its own pane listing, so a sandboxed
// ingest needs no extra tmux access for it.
func paneTitle(raw string) string {
	segs := sessionNameSegments(raw)
	if len(segs) < 2 {
		return ""
	}
	return truncate(scrubString(segs[len(segs)-1]), maxFieldLen)
}

// paneLabel is the label a live row shows: the session's title instead of its
// leading handle when that handle only repeats something the branch column already
// shows (shown: the row's branch and worktree dir, either of which a worktree
// session may be named after), else the handle. A decorated handle ("👟 fix-12")
// or an app-prefixed one ("muevelo/fix-12") still counts as repeating it. A
// title with no letter or digit (a bare status emoji such as "🟢") names
// nothing, so the handle stays.
func paneLabel(p livePane, shown ...string) string {
	if !strings.ContainsFunc(p.title, func(r rune) bool { return unicode.IsLetter(r) || unicode.IsDigit(r) }) {
		return p.session
	}
	for _, s := range shown {
		if s != "" && (p.session == s || strings.HasSuffix(p.session, " "+s) ||
			strings.HasSuffix(p.session, "/"+s)) {
			return p.title
		}
	}
	return p.session
}

// paneCache memoises the live tmux panes (pane id → livePane) so the dashboard
// (which polls) does not shell out to tmux on every render. now is injected so
// the TTL is testable, matching cwdCache.
type paneCache struct {
	mu   sync.Mutex
	set  map[string]livePane
	at   time.Time
	memo paneMem // see mem
	fg   paneFG  // see foreground
	// lapsed holds the pane blocks applyPaneBlocks saw lapse, under mu (see lapses).
	lapsed map[PaneBlock]bool
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

// paneFG memoises paneCache.foreground under its own lock, so a ps call never holds up panes().
type paneFG struct {
	mu sync.Mutex
	m  map[string]int
	at time.Time
}

// foreground returns pane id → its tty's foreground process group (see
// paneTpgids), refreshed once per paneTTL. Like mem it is lazy, so the
// panes()/live() callers (/api/pane, the action paths) never run ps; only a
// render with a pane block or an exec peer's block picker calls it. A nil cache
// returns nil.
func (c *paneCache) foreground(now time.Time) map[string]int {
	if c == nil {
		return nil
	}
	panes := c.panes(now)
	f := &c.fg
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.m == nil || now.Sub(f.at) >= paneTTL {
		f.m, f.at = paneTpgids(panes), now
	}
	return f.m
}

// reading returns the pane listing and foreground groups, and when the older
// of the two was taken, so applyPaneBlocks can ignore a reading that predates a
// block. A nil cache returns nothing, at the zero time.
func (c *paneCache) reading(now time.Time) (map[string]livePane, map[string]int, time.Time) {
	if c == nil {
		return nil, nil, time.Time{}
	}
	fg := c.foreground(now)
	c.fg.mu.Lock()
	fgAt := c.fg.at
	c.fg.mu.Unlock()
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.at.Before(fgAt) {
		return c.set, fg, c.at
	}
	return c.set, fg, fgAt
}

// lapses forgets the lapsed pane blocks no longer in stored (a re-block's new
// ts is a new key, and this bounds the set) and returns the rest. A nil cache
// remembers none.
func (c *paneCache) lapses(stored []PaneBlock) map[PaneBlock]bool {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	maps.DeleteFunc(c.lapsed, func(b PaneBlock, _ bool) bool { return !slices.Contains(stored, b) })
	return maps.Clone(c.lapsed)
}

// lapse records pane blocks applyPaneBlocks saw lapse.
func (c *paneCache) lapse(bs []PaneBlock) {
	if c == nil || len(bs) == 0 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.lapsed == nil {
		c.lapsed = map[PaneBlock]bool{}
	}
	for _, b := range bs {
		c.lapsed[b] = true
	}
}

// paneBusy reports whether pane id (p) runs a job: its foreground process
// group is not its shell's own. known is false when fg has no group for it.
// pane_current_command cannot tell this, since a running script shows as its shell.
func paneBusy(id string, p livePane, fg map[string]int) (busy, known bool) {
	g, ok := fg[id]
	return ok && g != p.pid, ok
}

// readTpgids is a var so a test can stand in for ps.
var readTpgids = psTpgids

// paneTpgids maps each listed pane with a pid to its tty's foreground process
// group, via one readTpgids call; a pane ps did not report is absent.
func paneTpgids(panes map[string]livePane) map[string]int {
	var pids []int
	for _, p := range panes {
		if p.pid > 0 {
			pids = append(pids, p.pid)
		}
	}
	tp := readTpgids(pids)
	out := map[string]int{}
	for id, p := range panes {
		if g, ok := tp[p.pid]; ok && p.pid > 0 {
			out[id] = g
		}
	}
	return out
}

// psTpgids reads pid → tpgid with one ps call, or nil with no pids. pids are
// integers from the pane listing, never client input. stdout is parsed even on
// a non-zero exit, since ps -p exits 1 when any pid has gone.
func psTpgids(pids []int) map[int]int {
	if len(pids) == 0 {
		return nil
	}
	list := make([]string, len(pids))
	for i, p := range pids {
		list[i] = strconv.Itoa(p)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	out, _ := exec.CommandContext(ctx, "ps", "-o", "pid=,tpgid=", "-p", strings.Join(list, ",")).Output()
	return parseTpgids(out)
}

// parseTpgids parses `ps -o pid=,tpgid=` output, keeping only positive
// tpgids (-1 means no controlling tty) and skipping malformed lines.
func parseTpgids(out []byte) map[int]int {
	m := map[int]int{}
	for line := range strings.SplitSeq(string(out), "\n") {
		f := strings.Fields(line)
		if len(f) != 2 {
			continue
		}
		pid, err1 := strconv.Atoi(f[0])
		g, err2 := strconv.Atoi(f[1])
		if err1 == nil && err2 == nil && g > 0 {
			m[pid] = g
		}
	}
	return m
}

// paneFormat is listPanes' -F: fixed-format fields, then free text, path last.
const paneFormat = "#{pane_id}\t#{pane_pid}\t#{session_id}\t#{window_id}\t#{window_index}\t#{session_name}\t#{pane_current_command}\t#{window_name}\t#{pane_current_path}"

var (
	windowIDRe = regexp.MustCompile(`^@\d+$`)
	digitsRe   = regexp.MustCompile(`^\d+$`)
)

// listPanes returns every live tmux pane id across all sessions, mapped to its
// session name (normalised exactly as ingest stores tmux_session), foreground
// command, current path and root pid, or an empty map if tmux is missing, unreachable
// (different socket / container / not running), or times out. Best-effort, like lookupCI.
func listPanes() map[string]livePane {
	set := map[string]livePane{}
	if _, err := exec.LookPath("tmux"); err != nil {
		return set
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "tmux", "list-panes", "-a", "-F", paneFormat).Output()
	if err != nil {
		return set
	}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		// The fixed-format fields come first and must validate, so a tab in a
		// free-text one cannot shift them; path is last, so one in it survives.
		f := strings.SplitN(line, "\t", 9)
		if len(f) != 9 || !paneIDRe.MatchString(f[0]) || !tmuxSessionIDRe.MatchString(f[2]) || !windowIDRe.MatchString(f[3]) || !digitsRe.MatchString(f[4]) {
			continue
		}
		pid, _ := strconv.Atoi(f[1])
		set[f[0]] = livePane{
			session: tmuxSessionLabel(f[5]), title: paneTitle(f[5]), command: f[6], path: f[8], pid: pid,
			sessionID: f[2], windowID: f[3], window: cmp.Or(truncate(scrubString(f[7]), maxFieldLen), f[4]),
		}
	}
	return set
}

// launcherCommands are the pane foreground commands of a running agent (Agent.Launcher's values).
var launcherCommands = map[string]bool{"claude": true, "nono": true}

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
// stored label goes stale when the name or its normalisation changes (#173) —
// but only while the pane runs an agent launcher and no other row claims the
// pane, by label or by this same fallback: a reused pane id holding some other
// process is not this row's, and two candidates cannot be told apart. Group
// flags (GroupStart, Grouped) are recomputed over the surviving rows (#177).
// A surviving row matched to its pane (by label or the launch-dir fallback) takes
// the pane's current session name as its label (#180), shown via paneLabel.
func dropDeadPanes(agents []Agent, panes map[string]livePane) []Agent {
	alive, matched := paneMatchers(agents, panes)
	if !slices.ContainsFunc(agents, alive) {
		return agents
	}
	kept := slices.DeleteFunc(agents, func(a Agent) bool {
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
	// After DeleteFunc, so every row was judged by its stored label; a blank label alone is no match.
	for i := range kept {
		if p := panes[kept[i].TmuxPane]; p.session != "" && matched(kept[i]) {
			kept[i].TmuxSession = paneLabel(p, cmp.Or(kept[i].Branch, kept[i].BranchGuess), kept[i].Worktree)
		}
	}
	markGroups(kept)
	return kept
}

// paneMatchers returns dropDeadPanes' tests over agents' stored labels: matched
// (the listed pane is the row's, by label or the launch-dir fallback) and alive
// (listed, and matched unless the row recorded no label).
func paneMatchers(agents []Agent, panes map[string]livePane) (alive, matched func(Agent) bool) {
	fallback := func(a Agent, p livePane) bool {
		dir := a.launchCwd()
		return launcherCommands[p.command] && p.path != "" && dir != "" && cwdKey(p.path) == cwdKey(dir)
	}
	claimed, candidates := map[string]bool{}, map[string]int{}
	for _, a := range agents {
		p, ok := panes[a.TmuxPane]
		switch {
		case !ok:
		case a.TmuxSession != "" && a.TmuxSession == p.session:
			claimed[a.TmuxPane] = true
		case fallback(a, p):
			candidates[a.TmuxPane]++
		}
	}
	matched = func(a Agent) bool {
		p, ok := panes[a.TmuxPane]
		if !ok {
			return false
		}
		return a.TmuxSession == p.session ||
			(!claimed[a.TmuxPane] && candidates[a.TmuxPane] == 1 && fallback(a, p))
	}
	alive = func(a Agent) bool {
		_, ok := panes[a.TmuxPane]
		return ok && (a.TmuxSession == "" || matched(a))
	}
	return alive, matched
}

// capturePane returns the last `lines` rows of pane, scrubbed for secret-shaped
// substrings. ok is false on any error/timeout. pane MUST already be validated
// against paneIDRe by the caller; it is passed as a discrete argument after -t,
// never through a shell. Trailing blank rows (tmux pads the visible screen below
// the cursor) are trimmed to one newline so the bottom-scrolled peek shows output.
func capturePane(pane string, lines int) (string, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "tmux", "capture-pane", "-p", "-t", pane, "-S", "-"+strconv.Itoa(lines)).Output()
	if err != nil {
		return "", false
	}
	return scrubString(strings.TrimRight(string(out), " \t\n") + "\n"), true
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

// lookupWorktreeRoot returns the top-level directory of cwd's worktree, or "" when
// git fails. Best-effort, like lookupRepo.
func lookupWorktreeRoot(cwd string) string {
	out, _ := gitProbe("git", cwd, "rev-parse", "--show-toplevel")
	return out
}
