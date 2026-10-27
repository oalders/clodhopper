package main

import (
	"bytes"
	"context"
	"database/sql"
	_ "embed"
	"encoding/json"
	"flag"
	"fmt"
	"hash/fnv"
	"html/template"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

//go:embed templates/dashboard.html
var dashboardHTML string

var dashboardTmpl = template.Must(
	template.New("dashboard").Funcs(template.FuncMap{
		"short":       shortID,
		"shortTS":     shortTS,
		"rowClass":    rowClass,
		"ciMarkSlots": ciMarkSlots,
		"blockChip":   func(a Agent, exec bool) blockChipView { return blockChipView{a, exec} },
		"blockGroups": blockGroups,
	}).Parse(dashboardHTML),
)

// blockGroup is one <optgroup> of the block picker: the rows of one app.
type blockGroup struct {
	App  string
	Rows []Agent
}

// blockGroups lists the rows self could be blocked on, grouped by app: self's
// own app first (a block is usually within one project), then the rest
// alphabetically. Rows keep their board order within a group, roster before
// queued.
func blockGroups(agents, queued []Agent, self Agent) []blockGroup {
	byApp := map[string][]Agent{}
	for _, a := range append(slices.Clone(agents), queued...) {
		if a.SessionID == "" || a.SessionID == self.SessionID {
			continue
		}
		byApp[a.SourceApp] = append(byApp[a.SourceApp], a)
	}
	apps := make([]string, 0, len(byApp))
	for app := range byApp {
		apps = append(apps, app)
	}
	sort.Slice(apps, func(i, j int) bool {
		if (apps[i] == self.SourceApp) != (apps[j] == self.SourceApp) {
			return apps[i] == self.SourceApp
		}
		return apps[i] < apps[j]
	})
	groups := make([]blockGroup, len(apps))
	for i, app := range apps {
		groups[i] = blockGroup{App: app, Rows: byApp[app]}
	}
	return groups
}

// blockChipView feeds the "blockchip" template, which renders in both the
// roster and the merge-queue table and gates its ✕ on Exec.
type blockChipView struct {
	Agent
	Exec bool
}

// ciMarkSlots is the most 🔀/draft-chip/done-chip markers any one roster row
// carries (0–2: a finished PR carries no draft flag, so those share a slot).
// The template turns it into a ci-mN class on the table, which sizes the CI
// column to fit, so a board with no markers spends no width on them. It is
// recomputed on every render, so the column follows /api/state polls.
func ciMarkSlots(agents []Agent) int {
	most := 0
	for _, a := range agents {
		n := 0
		if a.CIConflict {
			n++
		}
		if a.CIDraft || a.CIDone != "" {
			n++
		}
		most = max(most, n)
	}
	return most
}

// rowClass builds the space-separated CSS class list for a roster <tr> from the
// row's flags: "alert" for an attention state, "group-start" for the divider
// above a new branch group, and "grouped" for the left accent bar that binds a
// multi-session branch cluster. Returns "" when none apply, so the template emits
// no class attribute at all. Keeping the assembly here (not inline in the
// template) keeps it readable and lets TestRowClass pin every combination.
func rowClass(a Agent) string {
	var classes []string
	if a.StatusRank <= 1 {
		classes = append(classes, "alert")
	}
	if a.GroupStart {
		classes = append(classes, "group-start")
	}
	if a.Grouped {
		classes = append(classes, "grouped")
	}
	return strings.Join(classes, " ")
}

// shortID trims a session UUID to a glanceable prefix for the roster.
func shortID(s string) string {
	if len(s) <= 8 {
		return s
	}
	return s[:8]
}

// shortTS renders a stored timestamp for the events table. It drops the year and
// the RFC3339 T/Z, and omits the month-day for same-day events (the common case),
// which only need a wall-clock time; older rows keep "MM-DD" so they are not
// mistaken for today. now is injected (not read from the clock) so the output
// stays deterministic under test. Falls back to the raw value if it does not parse.
func shortTS(ts string, now time.Time) string {
	t, err := time.Parse(time.RFC3339, ts)
	if err != nil {
		return ts
	}
	tu, nu := t.UTC(), now.UTC()
	if tu.Year() == nu.Year() && tu.YearDay() == nu.YearDay() {
		return tu.Format("15:04:05")
	}
	return tu.Format("01-02 15:04:05")
}

// sessPalette is the set of colors sessColor cycles through. Entries MUST be
// escaper-safe #rrggbb hex literals: html/template passes a bare hex color
// through a style attribute unchanged, but a non-color token would be rewritten
// to "ZgotmplZ". TestSessPaletteIsHex enforces the format.
// The dashboard is dual light/dark (color-scheme: light dark), so every entry
// must be a mid-tone that shows on BOTH a white and a dark background — no
// near-white or near-black. That band holds only ~12-14 truly distinct colors;
// beyond that, family cousins (blue/navy, green/olive, gold/amber, purple/
// violet) appear and lean on lightness/hue offset to stay apart.
//
// ORDER MATTERS: assignSessColors resolves a collision by probing to the *next*
// slot, so two agents visible at once can be handed adjacent entries — keep
// adjacent (and wrap-around) entries perceptually far apart, with the cousins
// scattered across the slice rather than neighbouring. This only bounds the
// probe case: two ids whose hashes land directly on cousin slots (both free)
// can still look similar, so the short id beside the chip stays the real
// identifier. TestSessPaletteIsHex enforces the #rrggbb format (a non-color
// token would be rewritten to "ZgotmplZ" by html/template's style escaper).
var sessPalette = []string{
	"#2563eb", // blue
	"#ea580c", // orange
	"#16a34a", // green
	"#be123c", // crimson
	"#ca8a04", // gold
	"#9333ea", // purple
	"#0d9488", // teal
	"#dc2626", // red
	"#0891b2", // cyan
	"#d97706", // amber
	"#c026d3", // fuchsia
	"#4d7c0f", // olive
	"#1e40af", // navy
	"#db2777", // pink
	"#a16207", // bronze
	"#6d28d9", // violet
	"#475569", // slate
	"#92400e", // brown
	"#4f46e5", // indigo
	"#78716c", // stone
}

// sessColor maps a session id to its hash-preferred palette color (the
// *uncoordinated* color, before any deconfliction). The dashboard renders
// chips/tints from assignSessColors instead; sessColor is the plain hash view
// of that mapping, used to document and test the id->palette contract. An empty
// id yields "" so callers can skip the chip/tint entirely.
func sessColor(s string) string {
	if s == "" {
		return ""
	}
	return sessPalette[sessIndex(s)]
}

// sessIndex is the palette slot a session prefers before any collision handling.
// assignSessColors uses it as the start of its linear probe.
func sessIndex(s string) int {
	h := fnv.New32a()
	h.Write([]byte(s))
	return int(h.Sum32() % uint32(len(sessPalette)))
}

// assignSessColors hands every session shown on the dashboard a palette color,
// deconflicting the visible set so concurrent agents stay easy to tell apart.
// Live roster agents go first, in arrival order (firstSeq); sessions that only
// appear in the events log are appended after, in events-table order (newest
// first) — they are typically ended/aged-out agents nobody is tracking, so their
// relative order is not load-bearing. Each session takes its hash-preferred
// color (sessIndex); if that slot is already taken, it probes forward to the
// next free one. Because roster newcomers carry the largest firstSeq they are
// processed last, so a newly arrived agent can only claim a free/bumped slot — it
// never recolors an agent already on the board ("incumbents keep their color").
// A session leaving can still shift a *newer* agent that had bumped off a
// now-freed color; that is the accepted cost of staying stateless. Once every
// color is in use, remaining sessions fall back to their hash color and
// collisions resume — there is simply nothing free left to hand out.
func assignSessColors(agents []Agent, events []Event) map[string]string {
	ordered := make([]string, 0, len(agents)+len(events))
	seen := make(map[string]bool, len(agents)+len(events))
	add := func(id string) {
		if id == "" || seen[id] {
			return
		}
		seen[id] = true
		ordered = append(ordered, id)
	}

	roster := append([]Agent(nil), agents...)
	sort.SliceStable(roster, func(i, j int) bool { return roster[i].firstSeq < roster[j].firstSeq })
	for _, a := range roster {
		add(a.SessionID)
	}
	for _, e := range events {
		add(e.SessionID)
	}

	out := make(map[string]string, len(ordered))
	used := make(map[string]bool, len(sessPalette))
	for _, id := range ordered {
		start := sessIndex(id)
		color := sessPalette[start] // fallback once the palette is exhausted
		if len(used) < len(sessPalette) {
			for i := range len(sessPalette) {
				if c := sessPalette[(start+i)%len(sessPalette)]; !used[c] {
					color = c
					break
				}
			}
			used[color] = true
		}
		out[id] = color
	}
	return out
}

// dashboardData is the view model rendered by the dashboard template.
type dashboardData struct {
	Agents         []Agent
	QueuedAgents   []Agent     // every merge-queue row, folded under the roster; see splitQueued
	QueueNextETA   string      // earliest queued ETA as an etaPhrase; "" when none is known
	LedgerRows     []ledgerRow // rows that left the merge queue, kept in its table until dismissed; see queueLedger
	Activity       []SourceCount
	Events         []Event
	EventRows      []EventRow // Events folded for display; see foldToolEvents
	SourceApps     []string
	Branches       []string
	FilterSource   string
	FilterBranch   string
	FilterType     string
	RefreshSecs    int // 0 = auto-refresh disabled
	RefreshOptions []refreshOption
	WindowDays     int // roster lookback override in days; 0 = full configured cap
	WindowOptions  []windowOption
	Generated      string
	Host           string            // this machine's hostname; the browser-tab <title>, so tabs on different boxes are distinguishable
	Version        string            // short build version (the `version` var); shown inline in the footer
	VersionDetail  string            // full versionString(); commit + build date, surfaced in the footer's title tooltip
	Now            time.Time         // render time, passed to shortTS so it can hide same-day dates
	SessColors     map[string]string // session id -> chip/tint color; see assignSessColors
	Signature      string            // fingerprint of the report-worthy view; see viewSignature
	// PeekEnabled is true when serve --pane-peek is set AND this REQUEST's peer
	// passes remoteAllowed. /api/pane execs tmux and streams live pane text, so
	// it is peer-gated exactly like the actions are; rendering the ⤢ control to a
	// peer that the endpoint will 403 would only be a broken button.
	PeekEnabled bool
	CSRFToken   string // per-serve secret; emitted into the page only when ExecEnabled
	// ExecEnabled gates the whole write path in the UI: it is true only when
	// serve --enable-merge is set AND this REQUEST's peer passes remoteAllowed
	// (loopback or Tailscale). A peer that fails it gets a genuinely read-only
	// board: no action buttons at all, and — because data-csrf is emitted under
	// the same condition — no token with which to drive even the exec-free "end"
	// action by hand. This is UX and defence in depth; the gate that matters is
	// the one in handleAction.
	ExecEnabled bool
	Debug       bool // true when ?debug=1; renders the Activity + Recent-events diagnostics sections (off by default: the roster is the whole board)
}

// refreshOption is one entry in the auto-refresh interval dropdown.
type refreshOption struct {
	Secs     int
	Label    string
	Selected bool
}

// windowOption is one entry in the roster-window dropdown. Days == 0 means "all"
// (the full configured waiting-retention cap).
type windowOption struct {
	Days     int
	Label    string
	Selected bool
}

// agentWindow bounds how long after its last event a session still counts as a
// live agent on the roster.
const agentWindow = 30 * time.Minute

// refreshMax caps the auto-refresh interval (and the env default) at a sane hour.
const refreshMax = 3600

// windowMaxDays bounds a hand-typed ?window=N so an absurd day count in the URL
// can't produce a silly cap. It is only an upper bound on the query param; the
// effective roster window is still min(this, the configured waiting cap).
const windowMaxDays = 365

// refreshPresets are the dropdown choices; 0 means "off".
var refreshPresets = []int{0, 2, 5, 10, 30, 60}

// windowPresets are the roster-window dropdown choices, in days; 0 means "all"
// (the full configured waiting-retention cap). The default cap is generous so
// every live agent stays visible — this lets you narrow the board to just the
// recently active sessions for a view, without changing the configured cap.
var windowPresets = []int{0, 1, 2, 7, 14, 30}

// refreshOptions builds the dropdown, ensuring the current value is selectable
// even if it is not one of the presets (e.g. set via CLODHOPPER_REFRESH_SECS).
func refreshOptions(current int) []refreshOption {
	secs := append([]int(nil), refreshPresets...)
	if !slices.Contains(secs, current) {
		secs = append(secs, current)
		sort.Ints(secs)
	}
	out := make([]refreshOption, 0, len(secs))
	for _, s := range secs {
		label := "off"
		if s > 0 {
			label = strconv.Itoa(s) + "s"
		}
		out = append(out, refreshOption{Secs: s, Label: label, Selected: s == current})
	}
	return out
}

// windowOptions builds the roster-window dropdown, ensuring the current value is
// selectable even if it is not one of the presets (e.g. a hand-typed ?window=N).
func windowOptions(current int) []windowOption {
	days := append([]int(nil), windowPresets...)
	if !slices.Contains(days, current) {
		days = append(days, current)
		sort.Ints(days)
	}
	out := make([]windowOption, 0, len(days))
	for _, d := range days {
		label := "all"
		switch {
		case d == 1:
			label = "1 day"
		case d > 1:
			label = strconv.Itoa(d) + " days"
		}
		out = append(out, windowOption{Days: d, Label: label, Selected: d == current})
	}
	return out
}

// newServeConfigs builds the peek and action configs from the parsed serve
// flags. It is a named function purely so a test can pin the WIRING: /api/pane
// and /api/action share one gate (execPeerAllowed), which means both configs
// must be handed the same Host-allowlist inputs. Dropping them here would not
// open a hole — the gate fails closed — but it would silently 403 peek and every
// action for every Tailscale/magicDNS peer, which is the whole point of the
// allowlist.
func newServeConfigs(host string, panePeek bool, paneLines int, enableMerge bool) (*peekConfig, *actionConfig) {
	allowed := allowedHosts()
	peek := &peekConfig{
		enabled: panePeek, lines: clampPaneLines(paneLines), cache: newPaneCache(),
		repos: newRepoCache(), roots: newRootCache(), bindHost: host, allowedHosts: allowed,
	}
	act := &actionConfig{
		enabled:           enableMerge,
		mergePR:           "merge-pr",
		gh:                "gh",
		tmux:              "tmux",
		clearDelay:        monitorCIClearDelay,
		restartPoll:       restartPollInterval,
		restartExitWait:   restartExitWait,
		restartResumeWait: restartResumeWait,
		bindHost:          host,
		allowedHosts:      allowed,
		inflight:          newInflightSet(),
		panes:             peek.cache,
		ledger:            newQueueLedger(),
	}
	return peek, act
}

// newServeMux registers the dashboard routes behind one middleware, so the
// security headers and the Host allowlist cover every response — including the
// catch-all 404 — rather than relying on each handler to remember them.
func newServeMux(db *sql.DB, ci *cwdCache[ciResult], peek *peekConfig, act *actionConfig) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		handleDashboard(w, r, db, ci, peek, act)
	})
	// JSON polling endpoint: the dashboard's JS hits this on the refresh interval
	// and re-renders only when the returned signature changes.
	mux.HandleFunc("/api/state", func(w http.ResponseWriter, r *http.Request) {
		handleState(w, r, db, ci, peek, act)
	})
	mux.HandleFunc("/api/pane", func(w http.ResponseWriter, r *http.Request) {
		handlePane(w, r, peek, time.Now())
	})
	mux.HandleFunc("/api/action", func(w http.ResponseWriter, r *http.Request) {
		handleAction(w, r, db, act, time.Now())
	})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		setSecurityHeaders(w)
		// DNS rebinding: an attacker page whose name resolves here is same-origin
		// and could otherwise read the roster (paths, prompt previews, branches).
		if !hostAllowed(r.Host, act.bindHost, act.allowedHosts) {
			http.Error(w, "unrecognised Host; add it to CLODHOPPER_ALLOWED_HOSTS", http.StatusMisdirectedRequest)
			return
		}
		mux.ServeHTTP(w, r)
	})
}

// runServe starts the read-only dashboard HTTP server. It binds 127.0.0.1 by
// default; --host 0.0.0.0 (or CLODHOPPER_HOST) exposes it on all interfaces, which
// is needed when the browser is on a different machine/namespace than the
// server (e.g. a container or VM) — at the cost of LAN exposure.
func runServe(args []string) int {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	port := fs.Int("port", defaultPort(), "port to listen on")
	host := fs.String("host", defaultHost(), "address to bind (127.0.0.1 default; 0.0.0.0 for LAN/container access)")
	tailscale := fs.Bool("tailscale", false, "bind to this machine's Tailscale IPv4 (`tailscale ip -4`); cannot be combined with --host")
	allowPub := fs.Bool("allow-public", allowPublic(), "allow binding to a public IP (UNSAFE: dashboard has no auth or TLS)")
	panePeek := fs.Bool("pane-peek", false, "enable the live tmux pane peek (streams pane content to the dashboard; use only on a trusted network such as your tailnet)")
	paneLines := fs.Int("pane-lines", paneLinesDefault, "lines shown in a pane peek (1..2000)")
	enableMerge := fs.Bool("enable-merge", false, "enable PR-action buttons on the roster (merge-pr --squash/--admin/--close, gh pr ready, and session actions such as rebase via /kitchen-sink:git-rebase); writes to your repos — use only on a trusted network. Whatever the server is bound to, the actions that run commands are accepted only from loopback and Tailscale peers")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	// --tailscale derives --host from `tailscale ip -4`, so passing both is a
	// usage error. Only an explicit --host conflicts; the defaulted value (and
	// CLODHOPPER_HOST) is overridden silently.
	hostSet := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "host" {
			hostSet = true
		}
	})
	if *tailscale {
		if hostSet {
			fmt.Fprintln(os.Stderr, "clodhopper serve: --tailscale and --host cannot be used together")
			return 2
		}
		ip, err := tailscaleLookup()
		if err != nil {
			fmt.Fprintln(os.Stderr, "clodhopper serve:", err)
			return 1
		}
		*host = ip
	}

	db, err := openDB(defaultDBPath())
	if err != nil {
		fmt.Fprintln(os.Stderr, "clodhopper serve: open db:", err)
		return 1
	}
	defer db.Close()

	ci := newCICache()
	peek, act := newServeConfigs(*host, *panePeek, *paneLines, *enableMerge)
	if act.enabled {
		tok, err := randomToken()
		if err != nil {
			fmt.Fprintln(os.Stderr, "clodhopper serve: token:", err)
			return 1
		}
		act.token = tok
	}
	addr := net.JoinHostPort(*host, strconv.Itoa(*port))
	if err := guardPublicBind(*host, *allowPub); err != nil {
		fmt.Fprintln(os.Stderr, "clodhopper serve:", err)
		return 1
	}
	if *host != "127.0.0.1" && *host != "localhost" && *host != "::1" {
		fmt.Printf("clodhopper: WARNING binding %s — dashboard is reachable beyond loopback\n", addr)
	}
	fmt.Printf("clodhopper dashboard: http://%s  (db: %s)\n", addr, defaultDBPath())
	// A configured server (not http.ListenAndServe's zero-timeout default) bounds
	// header/idle time so a slow or stalled client cannot tie up the dashboard —
	// it matters because --host 0.0.0.0 deliberately exposes this beyond loopback.
	// WriteTimeout is left unset: a cold render can fan out several multi-second
	// `gh` CI lookups, and cutting that off mid-response would corrupt the page.
	srv := &http.Server{
		Addr:              addr,
		Handler:           newServeMux(db, ci, peek, act),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       15 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	if err := srv.ListenAndServe(); err != nil {
		fmt.Fprintln(os.Stderr, "clodhopper serve:", err)
		return 1
	}
	return 0
}

// cwdCache memoises per-cwd lookups (CI status; the peek's GitHub repo) so the
// dashboard (which auto-refreshes) does not shell out on every render. Keyed by
// cwd; entries expire after cwdTTL, or early once expire marks them stale.
// The key need not be a cwd: queueLedger's finals use "owner/repo#N".
type cwdCache[T any] struct {
	mu       sync.Mutex
	m        map[string]cwdEntry[T]
	lookup   func(cwd string) T
	forcedAt time.Time             // entries stamped before this are stale
	inflight map[string]*flight[T] // one lookup per cwd; concurrent gets wait on it
}

type cwdEntry[T any] struct {
	val T
	at  time.Time
}

type flight[T any] struct {
	done chan struct{}
	val  T
}

const (
	cwdTTL = 60 * time.Second
	// ciForceMinInterval caps how often a client's "refresh CI" can bypass the cache.
	ciForceMinInterval = 10 * time.Second
)

func newCICache() *cwdCache[ciResult] {
	return &cwdCache[ciResult]{m: map[string]cwdEntry[ciResult]{}, lookup: lookupCI}
}

func newRepoCache() *cwdCache[string] {
	return &cwdCache[string]{m: map[string]cwdEntry[string]{}, lookup: lookupRepo}
}

func newRootCache() *cwdCache[string] {
	return &cwdCache[string]{m: map[string]cwdEntry[string]{}, lookup: lookupWorktreeRoot}
}

// expire marks every current entry stale so the next get re-runs its lookup,
// unless the last expire was under ciForceMinInterval ago; it reports whether
// it took effect. now is injected for testing.
func (c *cwdCache[T]) expire(now time.Time) bool {
	if c == nil {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.forcedAt.IsZero() && now.Sub(c.forcedAt) < ciForceMinInterval {
		return false
	}
	c.forcedAt = now
	return true
}

// get returns the cached lookup result for cwd (e.g. the CI summary for the
// PR on its branch), refreshing it when stale. now is injected for testing. A nil
// cache yields the zero value, so a config built without one simply never
// resolves; a cache with no lookup serves what it holds and yields zero on a miss.
// Concurrent gets for one stale cwd share a single lookup.
func (c *cwdCache[T]) get(cwd string, now time.Time) T {
	var zero T
	if c == nil || cwd == "" {
		return zero
	}
	c.mu.Lock()
	e, ok := c.m[cwd]
	if ok && now.Sub(e.at) < cwdTTL && !e.at.Before(c.forcedAt) {
		c.mu.Unlock()
		return e.val
	}
	if c.lookup == nil {
		c.mu.Unlock()
		return zero
	}
	if f, ok := c.inflight[cwd]; ok {
		c.mu.Unlock()
		<-f.done
		return f.val
	}
	f := &flight[T]{done: make(chan struct{})}
	if c.inflight == nil {
		c.inflight = map[string]*flight[T]{}
	}
	c.inflight[cwd] = f
	c.mu.Unlock()
	// Deferred so a panicking lookup cannot leave waiters, and every later get for cwd, blocked forever.
	defer func() {
		c.mu.Lock()
		delete(c.inflight, cwd)
		c.mu.Unlock()
		close(f.done)
	}()

	f.val = c.lookup(cwd)
	c.mu.Lock()
	c.m[cwd] = cwdEntry[T]{val: f.val, at: now}
	c.mu.Unlock()
	return f.val
}

// ciResult is one PR's CI verdict: State is "", "pending", "green", "failing",
// "soft-failing" (only non-required checks failed) or "ejected" (removed from
// the merge queue, with no check started since); Fails and Passes count failed
// and passed checks (skips are neither).
type ciResult struct {
	State       string
	Fails       int
	Passes      int
	Conflict    bool      // merge conflict with the base branch
	Draft       bool      // the PR is a draft
	Done        string    // "merged" or "closed" once the PR is no longer open
	Queue       string    // "", "queued" or "queue-failing" (the queue entry is UNMERGEABLE)
	QueuePos    int       // 1-based merge-queue position, 0 when not queued
	QueueETA    time.Time // expected merge time, zero when GitHub gave no estimate
	QueueSince  time.Time // when the PR entered the queue, zero when not queued
	QueueReason string    // GitHub's reason for an "ejected" PR's removal from the queue
	PRNumber    int       // the PR's number, in every state
	PRRepo      string    // the PR's GitHub "owner/repo", parsed from its url; "" if not github.com
	MergedAt    time.Time // when a merged PR merged, zero otherwise
	RollupSince time.Time // newest check start in the rollup, zero when none reported one
}

// lookupCI shells out to `gh pr view` in cwd and summarises the result. It is
// strictly best-effort: a missing gh, no PR, an unauthenticated host, or a
// timeout all yield "" (rendered as "—"), never an error.
func lookupCI(cwd string) ciResult {
	if _, err := exec.LookPath("gh"); err != nil {
		return ciResult{}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "gh", "pr", "view", "--json", "id,number,url,state,mergedAt,isDraft,mergeStateStatus,statusCheckRollup")
	cmd.Dir = cwd
	out, err := cmd.Output()
	if err != nil {
		return ciResult{} // no PR for branch, not a repo, gh unauth, or timeout
	}
	r, id := parsePRView(out)
	// Queue checks run on a temporary gh-readonly-queue commit the rollup never
	// sees, so ask for the queue entry directly (#183); same ctx, same deadline.
	return withMergeQueue(ctx, cwd, r, id)
}

// withMergeQueue folds open PR id's merge-queue entry into r via gh, run in dir
// under ctx's deadline. Best-effort: no id or any error leaves r unchanged.
func withMergeQueue(ctx context.Context, dir string, r ciResult, id string) ciResult {
	if id == "" {
		return r
	}
	cmd := exec.CommandContext(ctx, "gh", "api", "graphql", "-f", "id="+id, "-f", "query="+mergeQueueQuery)
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		return r // no queue signal, never an error
	}
	return applyMergeQueue(r, out, time.Now())
}

// mergeQueueQuery fetches a PR's live merge-queue entry and its newest queue
// add/remove events; the removal event is the only trace an ejection leaves.
const mergeQueueQuery = `query($id:ID!){node(id:$id){... on PullRequest{mergeQueueEntry{state position estimatedTimeToMerge enqueuedAt} timelineItems(last:5,itemTypes:[ADDED_TO_MERGE_QUEUE_EVENT,REMOVED_FROM_MERGE_QUEUE_EVENT]){nodes{__typename ... on RemovedFromMergeQueueEvent{reason createdAt}}}}}}`

// applyMergeQueue folds the mergeQueueQuery output into r. A live entry sets
// Queue; with none, a removal as the newest event means the PR was ejected,
// which overrides the head commit's (possibly green) state until a check starts
// after the removal (#215); a removal time that won't parse keeps the ejection
// (fail toward showing it). Any other or unparseable output leaves r unchanged.
// The relative ETA is pinned to now, as the result sits in cwdCache and a
// relative one would drift.
func applyMergeQueue(r ciResult, out []byte, now time.Time) ciResult {
	var resp struct {
		Data struct {
			Node struct {
				MergeQueueEntry *struct {
					State                string    `json:"state"`
					Position             int       `json:"position"`
					EstimatedTimeToMerge *int      `json:"estimatedTimeToMerge"`
					EnqueuedAt           time.Time `json:"enqueuedAt"`
				} `json:"mergeQueueEntry"`
				TimelineItems struct {
					Nodes []struct {
						Typename string `json:"__typename"`
						Reason   string `json:"reason"`
						// a string, so a bad value cannot fail the whole Unmarshal and hide the ejection
						CreatedAt string `json:"createdAt"`
					} `json:"nodes"`
				} `json:"timelineItems"`
			} `json:"node"`
		} `json:"data"`
	}
	if err := json.Unmarshal(out, &resp); err != nil {
		return r
	}
	node := resp.Data.Node
	if e := node.MergeQueueEntry; e != nil {
		r.Queue = "queued"
		if e.State == "UNMERGEABLE" {
			r.Queue = "queue-failing"
		}
		r.QueuePos, r.QueueSince = e.Position, e.EnqueuedAt
		if s := e.EstimatedTimeToMerge; s != nil {
			r.QueueETA = now.Add(time.Duration(*s) * time.Second)
		}
		return r
	}
	if n := node.TimelineItems.Nodes; len(n) > 0 && n[len(n)-1].Typename == "RemovedFromMergeQueueEvent" {
		if at, err := time.Parse(time.RFC3339, n[len(n)-1].CreatedAt); err == nil && !at.After(r.RollupSince) {
			return r // CI re-ran on the head since the removal, so its verdict is current
		}
		// reason is GitHub free text rendered into an attribute; drop control runes as for any rendered field
		r.State, r.QueueReason = "ejected", stripControl(n[len(n)-1].Reason)
	}
	return r
}

// rollupNode is one statusCheckRollup entry: a CheckRun (name/status/conclusion)
// or a StatusContext (context/state), told apart by __typename. gh reports both
// kinds' start as startedAt (a StatusContext's is its createdAt).
type rollupNode struct {
	Typename   string    `json:"__typename"`
	Name       string    `json:"name"`
	Status     string    `json:"status"`
	Conclusion string    `json:"conclusion"`
	StartedAt  time.Time `json:"startedAt"`
	Context    string    `json:"context"`
	State      string    `json:"state"`
}

// parsePRView turns `gh pr view --json id,number,url,state,mergedAt,isDraft,mergeStateStatus,statusCheckRollup`
// output into a ciResult, plus the PR's node id when it is still OPEN (only an
// open PR can be in the merge queue). The PR's number, repo and merge time are
// kept in every state, for the merge-queue ledger (see queueLedger). The rollup drives the state;
// mergeStateStatus only refines it, because BLOCKED also means "review missing"
// with green checks. UNKNOWN never flags a conflict: every fresh PR is UNKNOWN
// for a moment. A MERGED or CLOSED PR yields only Done: its rollup is stale.
func parsePRView(out []byte) (ciResult, string) {
	var pr struct {
		ID                string       `json:"id"`
		Number            int          `json:"number"`
		URL               string       `json:"url"`
		State             string       `json:"state"`
		MergedAt          time.Time    `json:"mergedAt"`
		IsDraft           bool         `json:"isDraft"`
		MergeStateStatus  string       `json:"mergeStateStatus"`
		StatusCheckRollup []rollupNode `json:"statusCheckRollup"`
	}
	if err := json.Unmarshal(out, &pr); err != nil {
		return ciResult{}, ""
	}
	repoURL, _, _ := strings.Cut(pr.URL, "/pull/")
	if pr.State == "MERGED" || pr.State == "CLOSED" {
		return ciResult{Done: strings.ToLower(pr.State), PRNumber: pr.Number, PRRepo: parseGitHubRepo(repoURL), MergedAt: pr.MergedAt}, ""
	}
	r := classifyRollup(pr.StatusCheckRollup)
	r.PRNumber, r.PRRepo = pr.Number, parseGitHubRepo(repoURL)
	if r.State == "failing" && pr.MergeStateStatus == "UNSTABLE" {
		r.State = "soft-failing" // GitHub says only non-required checks failed
	}
	r.Conflict = pr.MergeStateStatus == "DIRTY"
	r.Draft = pr.IsDraft
	for _, n := range pr.StatusCheckRollup {
		if n.StartedAt.After(r.RollupSince) {
			r.RollupSince = n.StartedAt
		}
	}
	if pr.State != "OPEN" {
		return r, "" // an unexpected state: not queueable either
	}
	return r, pr.ID
}

// classifyRollup reduces the rollup to one state plus a fail count. Any failure
// wins; otherwise any still-pending check keeps it amber; passes and skips alone
// are green. A pending status named after a finished check run is ignored. An
// unrecognised value counts as a failure (fail closed).
func classifyRollup(nodes []rollupNode) ciResult {
	if len(nodes) == 0 {
		return ciResult{}
	}
	done := map[string]bool{}
	for _, n := range nodes {
		if n.Typename == "CheckRun" && n.Status == "COMPLETED" && n.Name != "" {
			done[n.Name] = true
		}
	}
	var r ciResult
	var pending bool
	for _, n := range nodes {
		switch rollupBucket(n) {
		case "fail":
			r.Fails++
		case "pending":
			// A same-named job already finished, so this status is a gate
			// waiting on something else, usually the user (#201).
			if n.Typename == "StatusContext" && done[n.Context] {
				continue
			}
			pending = true
		case "pass":
			r.Passes++
		}
	}
	switch {
	case r.Fails > 0:
		r.State = "failing"
	case pending:
		r.State = "pending"
	default:
		r.State = "green"
	}
	return r
}

// rollupBucket maps one node to "pass", "skip", "pending" or "fail".
func rollupBucket(n rollupNode) string {
	if n.Typename == "StatusContext" {
		switch n.State {
		case "EXPECTED", "PENDING":
			return "pending"
		case "SUCCESS":
			return "pass"
		}
		return "fail"
	}
	if n.Typename != "CheckRun" {
		return "fail"
	}
	if n.Status != "COMPLETED" {
		return "pending"
	}
	switch n.Conclusion {
	case "SUCCESS", "NEUTRAL":
		return "pass"
	case "SKIPPED", "STALE":
		return "skip"
	}
	return "fail"
}

// setSecurityHeaders hardens every dashboard response. Framing is the one that
// matters here: the clipboard fallback the dashboard uses on a non-secure origin
// (document.execCommand, the path serve --tailscale takes) still works inside a
// frame under a user gesture, unlike navigator.clipboard, so a page that framed
// the board could overlay a decoy and have the operator copy a path of its
// choosing. nosniff and no-referrer are cheap companions.
//
// The CSP carries only the directives that need no nonces: frame-ancestors is
// the standardised equivalent of X-Frame-Options (both are sent, since the
// header is what older browsers honour), and base-uri/object-src close two
// injection avenues for free. Deliberately no script-src or style-src: the
// page's inline <style> and <script> would need nonces or hashes, which means
// threading a per-response nonce through the template, and that is a bigger
// change than this commit is buying. Nor are the fetch directives
// (default-src/connect-src/img-src/form-action) set here — the page loads a
// data: favicon and same-origin fetches only, so they could be locked down and
// would still hold even alongside 'unsafe-inline'; that is worth doing, and is
// tracked as its own change rather than grown onto this one.
//
// newServeMux's middleware applies it to every response, before any handler (or
// the mux's own 404) can write: http.Error writes a response, and headers set
// afterwards are lost, so doing it once up front makes "every response carries
// these" structural rather than something each handler has to remember.
func setSecurityHeaders(w http.ResponseWriter) {
	w.Header().Set("X-Frame-Options", "DENY")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("Content-Security-Policy", "frame-ancestors 'none'; base-uri 'none'; object-src 'none'")
}

func handleDashboard(w http.ResponseWriter, r *http.Request, db *sql.DB, ci *cwdCache[ciResult], peek *peekConfig, act *actionConfig) {
	data, err := buildDashboardData(r, db, ci, peek, act)
	if err != nil {
		http.Error(w, "dashboard error", http.StatusInternalServerError)
		return
	}
	// Render into a buffer, as handleState does: streaming to w meant a failed
	// write (a client that reloaded mid-render) fell through to http.Error after
	// the 200 was already sent — "superfluous response.WriteHeader".
	var buf bytes.Buffer
	if err := dashboardTmpl.Execute(&buf, data); err != nil {
		http.Error(w, "render error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = buf.WriteTo(w)
}

// handleState serves the dashboard's dynamic region as JSON for the JS poller:
// {"signature": ..., "html": ...}. The client compares the signature to what it
// last rendered and only swaps the DOM when it differs, so a quiet board never
// repaints.
func handleState(w http.ResponseWriter, r *http.Request, db *sql.DB, ci *cwdCache[ciResult], peek *peekConfig, act *actionConfig) {
	// The "refresh CI" button: no gate beyond the Host allowlist, since it only
	// re-runs the read-only gh lookup every poll already does, and expire is throttled.
	if r.URL.Query().Get("force") == "1" {
		ci.expire(time.Now())
	}
	data, err := buildDashboardData(r, db, ci, peek, act)
	if err != nil {
		http.Error(w, "dashboard error", http.StatusInternalServerError)
		return
	}
	var buf bytes.Buffer
	if err := dashboardTmpl.ExecuteTemplate(&buf, "content", data); err != nil {
		http.Error(w, "render error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(map[string]string{
		"signature": data.Signature,
		"html":      buf.String(),
	})
}

// buildDashboardData assembles the view model shared by the full page and the
// JSON poll endpoint, including its signature.
// dashboardTitle is the browser-tab <title>: this machine's hostname (e.g.
// "olaf-dev"), so several dashboard tabs open on different boxes are told apart
// at a glance — the favicon already marks a tab as clodhopper's, so the title
// carries the host instead. Computed once (the hostname is fixed for the
// process's life) and falls back to "clodhopper" when the OS reports no name,
// so the tab is never blank.
var dashboardTitle = sync.OnceValue(func() string {
	if h, err := os.Hostname(); err == nil && h != "" {
		return h
	}
	return "clodhopper"
})

func buildDashboardData(r *http.Request, db *sql.DB, ci *cwdCache[ciResult], peek *peekConfig, act *actionConfig) (dashboardData, error) {
	q := r.URL.Query()
	source := q.Get("source_app")
	branch := q.Get("branch")
	debug := q.Get("debug") == "1"
	etype := ""
	if debug { // only debug renders the event_type box, so a stale link cannot filter invisibly
		etype = q.Get("event_type")
	}

	refresh := defaultRefreshSecs()
	if v := q.Get("refresh"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 && n <= refreshMax {
			refresh = n
		}
	}

	// window narrows the roster to the last N days for this view only; 0 (the
	// default) uses the full configured cap. It never widens past the cap that
	// bounds the query, so it can only ever hide rows, not surface stale ones.
	windowDays := 0
	if v := q.Get("window"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= windowMaxDays {
			windowDays = n
		}
	}

	now := time.Now()
	waitingCap := time.Duration(waitingRetainHours()) * time.Hour
	if windowDays > 0 {
		if narrowed := time.Duration(windowDays) * 24 * time.Hour; narrowed < waitingCap {
			waitingCap = narrowed
		} else {
			// The requested window meets or exceeds the configured cap, so it
			// narrows nothing (e.g. ?window=30 at the default 30-day cap). Treat
			// it as "all" so the dropdown and URL reflect that no narrowing
			// happened, rather than showing a preset that is a silent no-op.
			windowDays = 0
		}
	}
	agents, err := agentRoster(db, waitingCap, now)
	if err != nil {
		return dashboardData{}, fmt.Errorf("roster: %w", err)
	}
	// Before dropDeadPanes, whose paneLabel compares the handle against Worktree too.
	setWorktrees(agents, peek.roots, now)
	// Before enrichCI, so no CI is fetched for a row that will not render.
	agents = dropDeadPanes(agents, peek.cache.panes(now))
	// After dropDeadPanes, so a blocker whose pane died already counts as gone.
	blocks, err := listBlocks(db)
	if err != nil {
		return dashboardData{}, fmt.Errorf("blocks: %w", err)
	}
	applyBlocks(agents, blocks)
	enrichCI(agents, ci, now)
	suppressWatchedCI(agents, now)
	// Before the split, so queued, ledgered and roster rows all keep their peek.
	if peek.enabled {
		for i := range agents {
			agents[i].LiveTmux = agents[i].TmuxPane != "" && peek.cache.live(agents[i].TmuxPane, now)
			if agents[i].LiveTmux {
				agents[i].Repo = peek.repos.get(agents[i].Cwd, now)
			}
		}
	}
	// splitQueued and demotePendingCI both key on CIQueue, so their order does not matter (#219).
	roster, queued := splitQueued(agents, now)
	act.ledger.record(queued, now)
	// Before demotePendingCI, which relaxes every merged "needs you" row.
	roster, ledger := act.ledger.carryOver(roster, queued, now)
	demotePendingCI(agents)

	// Live labels from dropDeadPanes, so activity rows match the roster's (#180).
	labels := map[string]string{}
	for _, a := range agents {
		if a.TmuxSession != "" {
			labels[a.SessionID] = a.TmuxSession
		}
	}
	activity, err := activeCounts(db, agentWindow, now, labels)
	if err != nil {
		return dashboardData{}, fmt.Errorf("activity: %w", err)
	}
	// (app, branch) finds the row's dir unless separate clones share both, so a
	// key whose roster rows disagree gets no suffix rather than a wrong one.
	worktrees, mixed := map[[2]string]string{}, map[[2]string]bool{}
	for _, a := range agents {
		k := [2]string{a.SourceApp, a.Branch}
		if w, ok := worktrees[k]; ok && w != a.Worktree {
			mixed[k] = true
		}
		worktrees[k] = a.Worktree
	}
	for i, c := range activity {
		if k := [2]string{c.SourceApp, c.Branch}; !mixed[k] {
			activity[i].Worktree = worktrees[k]
		}
	}

	events, err := queryEvents(db, EventFilter{SourceApp: source, Branch: branch, EventType: etype, Limit: 300})
	if err != nil {
		return dashboardData{}, fmt.Errorf("events: %w", err)
	}
	apps, _ := distinctSourceApps(db)

	data := dashboardData{
		Agents:         roster,
		QueuedAgents:   queued,
		LedgerRows:     ledger,
		Activity:       activity,
		Events:         events,
		EventRows:      foldToolEvents(events),
		SourceApps:     apps,
		Branches:       rosterBranches(agents, branch),
		FilterSource:   source,
		FilterBranch:   branch,
		FilterType:     etype,
		RefreshSecs:    refresh,
		RefreshOptions: refreshOptions(refresh),
		WindowDays:     windowDays,
		WindowOptions:  windowOptions(windowDays),
		Generated:      now.Format("15:04:05"),
		Host:           dashboardTitle(),
		Version:        version,
		VersionDetail:  versionString(),
		Now:            now,
		SessColors:     assignSessColors(agents, events),
		PeekEnabled:    peek.enabled && peerOK(r, peek.bindHost, peek.allowedHosts),
		ExecEnabled:    act.enabled && peerOK(r, act.bindHost, act.allowedHosts),
		CSRFToken:      act.token,
		Debug:          debug,
	}
	data.filterRows(source, branch)
	data.QueueNextETA = queueNextETA(data.QueuedAgents, now)
	data.Signature = viewSignature(data)
	return data, nil
}

// filterRows narrows the roster, merge-queue and activity rows to a source_app
// and branch ("" matches any). It is a view only, applied last so CI, blocks and
// the ledger saw every row; it clones because roster and queued share a backing array.
func (d *dashboardData) filterRows(source, branch string) {
	if source == "" && branch == "" {
		return
	}
	drop := func(app, br string) bool { return (source != "" && app != source) || (branch != "" && br != branch) }
	d.Agents = slices.DeleteFunc(slices.Clone(d.Agents), func(a Agent) bool { return drop(a.SourceApp, a.Branch) })
	markGroups(d.Agents) // a dropped first group would leave a leading divider
	d.QueuedAgents = slices.DeleteFunc(slices.Clone(d.QueuedAgents), func(a Agent) bool { return drop(a.SourceApp, a.Branch) })
	d.LedgerRows = slices.DeleteFunc(d.LedgerRows, func(r ledgerRow) bool { return drop(r.SourceApp, r.Branch) })
	d.Activity = slices.DeleteFunc(d.Activity, func(c SourceCount) bool { return drop(c.SourceApp, c.Branch) })
}

// rosterBranches lists the branch dropdown's options: the live roster's branches
// (queued rows included), plus selected so a branch off the roster stays selected.
func rosterBranches(agents []Agent, selected string) []string {
	var out []string
	for _, a := range agents {
		if a.Branch != "" {
			out = append(out, a.Branch)
		}
	}
	if selected != "" {
		out = append(out, selected)
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// viewSignature fingerprints only the *report-worthy* parts of the view: the
// event list, each agent's status/doing/CI, and the activity tallies. It
// deliberately omits cosmetic, purely time-derived values — the "updated" clock,
// idle counters, and the idle-based row ordering — so the dashboard's poller
// redraws when something actually happened, not merely because time passed. The
// one exception is the queue card's next-merge countdown (see below).
// Components are folded in a stable, idle-independent order for the same reason.
func viewSignature(d dashboardData) string {
	h := fnv.New64a()

	// Events are append-only and immutable, so count plus the id bounds detect any
	// insert or prune without hashing every row.
	fmt.Fprintf(h, "e:%d", len(d.Events))
	if len(d.Events) > 0 {
		fmt.Fprintf(h, ":%d:%d", d.Events[0].ID, d.Events[len(d.Events)-1].ID)
	}

	// Unlike the per-row queue labels, the queue card's countdown is hashed so it
	// cannot freeze on a quiet board; it ticks once a minute until its last minute.
	fmt.Fprintf(h, "|n:%q", d.QueueNextETA)

	// The section tag makes a row moving between the roster and the queue repaint.
	for _, sec := range []struct {
		tag  string
		rows []Agent
	}{{"a", d.Agents}, {"q", d.QueuedAgents}} {
		agents := append([]Agent(nil), sec.rows...)
		sort.Slice(agents, func(i, j int) bool { return agents[i].SessionID < agents[j].SessionID })
		for _, a := range agents {
			// Cwd is folded in because the roster renders it (the branch cell's
			// click-to-copy target). A session can move to another worktree on the
			// same branch, and under an event_type filter that move need not touch
			// the event bounds above — without this the board would keep offering
			// the old path until some unrelated change forced a repaint.
			//
			// %q, not %s: the fields are free text that can contain the ":" used as
			// the delimiter, so unquoted they let one field's tail masquerade as the
			// next field's head (branch "main:/w" + cwd "one" hashing the same as
			// branch "main" + cwd "/w/one"). A collision here is a repaint that never
			// happens — a stale board, and now a stale copy target.
			// Subagents, Shells and SubagentSince feed the "waiting on N agents" badge: the
			// start anchor is hashed, the ticking elapsed (SubagentAge) is not.
			// Launcher feeds the 🔒/🤖 marker, which a resume can flip in place.
			// Likewise the queue pill's anchors are hashed, its ticking CIQueueLabel is not;
			// CIQueueStuck flips with time while those anchors hold, so it is hashed too.
			fmt.Fprintf(h, "|%s:%q:%q:%q:%q:%q:%q:%q:%q:%d:%d:%t:%t:%q:%q:%d:%d:%d:%t:%q:%q:%d:%d:%d:%q:%q:%q", sec.tag, a.SessionID, a.TmuxSession, a.SourceApp, a.Branch, a.Cwd, a.Status, a.Doing, a.CI, a.CIFails, a.CIPasses, a.CIConflict, a.CIDraft, a.CIDone, a.CIQueue, a.CIQueuePos, a.CIQueueETA, a.CIQueueSince, a.CIQueueStuck, a.CIQueueReason, a.Launcher, a.Subagents, a.Shells, a.SubagentSince, a.Worktree, a.BlockedOn, a.BlockedOnSession)
		}
	}

	// Ledger rows come sorted; PRMergedAt is hashed, its ticking MergedAgo is not.
	for _, r := range d.LedgerRows {
		fmt.Fprintf(h, "|l:%q:%q:%q:%q:%q:%t:%q:%d:%q:%d:%d", r.SessionID, r.TmuxSession, r.SourceApp, r.Branch, r.Worktree, r.LiveTmux, r.Final, r.PRMergedAt, r.PRRepo, r.PRNumber, r.CIQueuePos)
	}

	activity := append([]SourceCount(nil), d.Activity...)
	sort.Slice(activity, func(i, j int) bool {
		if activity[i].SourceApp != activity[j].SourceApp {
			return activity[i].SourceApp < activity[j].SourceApp
		}
		if activity[i].Branch != activity[j].Branch {
			return activity[i].Branch < activity[j].Branch
		}
		return activity[i].TmuxSession < activity[j].TmuxSession
	})
	for _, c := range activity {
		fmt.Fprintf(h, "|c:%q:%q:%q:%d:%q", c.TmuxSession, c.SourceApp, c.Branch, c.Count, c.Worktree)
	}

	return strconv.FormatUint(h.Sum64(), 16)
}

// ciWatchSecs bounds how long a CI-watch command suppresses the previous run's
// verdict on that row. Long enough for a freshly triggered run to start
// reporting, short enough that a genuinely red current run is hidden only
// briefly. Mirrors staleWorkingSecs, the roster's other recency window.
const ciWatchSecs = 5 * 60

// suppressWatchedCI flips a row's CI verdict to "pending" when the session just
// ran a CI-watch command, for ciWatchSecs after that command. The red (or green)
// dot then reflects the *previous* run, which the operator has just superseded,
// while gh legitimately keeps reporting the old checks alongside
// the new ones during the overlap — so re-querying gh cannot resolve the
// contradiction and the fix has to be driven by the re-run signal itself
// (issue #111).
//
// This rests on a convention, not on something the code can verify: neither
// /monitor-ci nor /poll-ci pushes or re-runs anything: both are watchers. The
// assumption is that the operator runs them only after triggering a fresh run.
// An operator who instead runs /poll-ci to *look at* an already-red PR will see
// that row suppressed for the window.
//
// Tradeoff (stated in the style of demotePendingCI): because this runs before
// demotePendingCI, a suppressed "needs you"/"waiting for you" row is also
// relaxed to statusBackground / rankBackground and drops out of the alert band.
// That is intended — under the convention a fresh run is in flight, so there is
// nothing to act on yet.
//
// Known limitation: the dashboard's new-monitor action splits a *new* pane
// running a *new* Claude session and sends /monitor-ci there, so that prompt
// lands under a different session_id and the original red row is never
// suppressed (a new roster row appears instead). Only the in-pane monitor-ci
// action produces the signal this keys on.
//
// now is injected per the project's time-injection convention.
func suppressWatchedCI(agents []Agent, now time.Time) {
	for i := range agents {
		if agents[i].CI != "failing" && agents[i].CI != "soft-failing" && agents[i].CI != "green" {
			continue
		}
		if !isCIWatchCommand(agents[i].LastCommand) {
			continue
		}
		// Bound the window at BOTH ends: a nonsensical (future-dated) timestamp
		// yields a negative age, and that must not hide a failing CI verdict until
		// the wall clock catches up. Fail toward showing the red.
		age := now.Unix() - agents[i].LastCommandSince
		if age < 0 || age >= ciWatchSecs {
			continue
		}
		// CIFails/CIPasses/CIConflict/CIDraft stay: the counts only render on red/orange/green, and a conflict or draft is independent of the run.
		agents[i].CI = "pending"
	}
}

// isCIWatchCommand reports whether cmd is one of the slash commands taken by
// convention to mean "a fresh CI run was just triggered for this session" — a
// convention the code cannot verify. The match is made after stripping a plugin
// namespace: if the command starts with "/" and contains a ":", everything from
// just after the leading "/" through the FIRST ":" is removed
// ("/kitchen-sink:poll-ci" -> "/poll-ci"). Only the first colon is stripped, so
// "/a:b:poll-ci" -> "/b:poll-ci" and does not match. Matching is exact equality,
// never a prefix or a whitespace split: ingest keeps only the first token of the
// prompt, so LastCommand can never carry arguments.
func isCIWatchCommand(cmd string) bool {
	if strings.HasPrefix(cmd, "/") {
		if i := strings.Index(cmd, ":"); i >= 0 {
			cmd = "/" + cmd[i+1:]
		}
	}
	switch cmd {
	case "/monitor-ci", "/poll-ci":
		return true
	}
	return false
}

// demotePendingCI relaxes a "needs you"/"waiting for you" row to the non-urgent
// statusBackground when its CI is still running, only non-required checks
// failed (the PR is still mergeable), or the PR is merged or closed (nothing is
// left to act on; end dismisses the row). A pending check means the agent most
// likely pushed and is waiting on that run, not blocked on the user — so the
// alert styling would be misleading (issue #31). This runs in the server layer,
// not in deriveStatus, because CI is fetched here via gh and is deliberately
// kept out of the deterministic roster.
//
// Tradeoff (intentional, matching the roster's other heuristics): an agent that
// genuinely finished and is awaiting you, but whose earlier push still has CI in
// flight, is also relaxed for as long as that run stays pending. We accept that
// over the louder false positive of flagging a busy-waiting agent as needing
// you. A real "needs approval" (a permission prompt) is left alone — that blocks
// on you regardless of CI.
//
// A row in the merge queue (any CIQueue, stuck or failing included) is the
// exception: splitQueued has folded it into the merge-queue section, which never
// alerts (#219), so every alert status there, approval and input too, is relaxed.
func demotePendingCI(agents []Agent) {
	for i := range agents {
		if agents[i].CIQueue != "" {
			if agents[i].StatusRank <= 1 {
				agents[i].Status = statusBackground
				agents[i].StatusRank = rankBackground
			}
			continue
		}
		if agents[i].CI != "pending" && agents[i].CI != "soft-failing" && agents[i].CIDone == "" {
			continue
		}
		if agents[i].Status == statusNeedsYou || agents[i].Status == statusWaiting {
			agents[i].Status = statusBackground
			agents[i].StatusRank = rankBackground
		}
	}
}

// applyBlocks marks each row of agents that a stored block holds on another row
// (see `clodhopper block`). A block applies only while BOTH sessions are in
// agents, so it lapses once the blocker ends, ages out or loses its pane, and a
// later session on the blocker's branch has a new id and cannot revive it. A
// "waiting for you"/"needs you" row is relaxed to statusBlocked at rankBackground:
// nothing done in its pane helps until the blocker lands. A permission prompt or
// question is left alone, as in demotePendingCI, since it blocks on the user
// regardless; it and a working/idle row keep their status and just get the chip.
func applyBlocks(agents []Agent, blocks []Block) {
	idx := make(map[string]int, len(agents))
	for i, a := range agents {
		idx[a.SessionID] = i
	}
	for _, b := range blocks {
		i, ok := idx[b.Blocked]
		j, ok2 := idx[b.Blocker]
		if !ok || !ok2 {
			continue
		}
		agents[i].BlockedOn = agents[j].BlockLabel()
		agents[i].BlockedOnSession = b.Blocker
		if agents[i].Status == statusNeedsYou || agents[i].Status == statusWaiting {
			agents[i].Status = statusBlocked
			agents[i].StatusRank = rankBackground
		}
	}
}

// BlockLabel names a row as the "blocked on" chip and the block picker show it:
// its branch, else its worktree dir, else its session id as the roster abbreviates it.
func (a Agent) BlockLabel() string {
	if a.Branch != "" {
		return a.Branch
	}
	if a.BranchGuess != "" {
		return a.BranchGuess
	}
	return shortID(a.SessionID)
}

// A queued PR past its ETA by queueOverdueSecs, or with no ETA queued longer
// than queueStaleSecs, has likely stalled; its queue pill says so (#195, #219).
const (
	queueOverdueSecs = 10 * 60
	queueStaleSecs   = 45 * 60
)

// queueStuck reports whether a's merge-queue entry has stalled; see queueOverdueSecs.
func queueStuck(a Agent, now time.Time) bool {
	if a.CIQueueETA != 0 {
		return now.Unix() > a.CIQueueETA+queueOverdueSecs
	}
	return a.CIQueueSince != 0 && now.Unix()-a.CIQueueSince > queueStaleSecs
}

// splitQueued folds every merge-queue row (any CIQueue, whatever its status,
// stuck included) off the roster into the merge-queue section, ordered by ETA
// (unknown last): the queue has the next move, and a stalled entry only marks
// its pill (CIQueueStuck) rather than alerting (#219). It partitions agents in
// place and returns views of it, so later edits through agents (demotion)
// reach both halves.
func splitQueued(agents []Agent, now time.Time) (roster, queued []Agent) {
	var q []Agent
	n := 0
	for _, a := range agents {
		if a.CIQueue != "" {
			a.CIQueueStuck = queueStuck(a, now)
			q = append(q, a)
		} else {
			agents[n] = a
			n++
		}
	}
	// Positions are per repo, so only the ETA orders rows across repos.
	sort.SliceStable(q, func(i, j int) bool {
		ei, ej := q[i].CIQueueETA, q[j].CIQueueETA
		return ei != 0 && (ej == 0 || ei < ej)
	})
	copy(agents[n:], q)
	return agents[:n], agents[n:]
}

// setAndForget reports whether a row with status can leave the roster for the
// merge-queue table: nothing in it is waiting on you.
func setAndForget(status string) bool {
	return status == statusWaiting || status == statusBackground || status == statusIdle || status == statusBlocked
}

// queueNextETA is the earliest known ETA among queued as an etaPhrase, or "".
func queueNextETA(queued []Agent, now time.Time) string {
	var next int64
	for _, a := range queued {
		// A stalled entry's past ETA would pin the header at "now".
		if !a.CIQueueStuck && a.CIQueueETA != 0 && (next == 0 || a.CIQueueETA < next) {
			next = a.CIQueueETA
		}
	}
	if next == 0 {
		return ""
	}
	return etaPhrase(time.Unix(next, 0), now)
}

// etaPhrase is eta relative to now ("in ~12m", or "now" once due): a clock time
// would be in the server's timezone, not the viewer's.
func etaPhrase(eta, now time.Time) string {
	if d := eta.Sub(now); d >= time.Second {
		return "in ~" + humanizeSeconds(int(d/time.Second))
	}
	return "now"
}

// setWorktrees sets each agent's Worktree to its cwd's worktree-root basename when
// that differs from the branch, so the branch column can show both (#184). Rows
// with no known branch are skipped: their BranchGuess already names the dir.
// main/master are skipped too: the primary checkout, whose dir is just the repo's name.
func setWorktrees(agents []Agent, roots *cwdCache[string], now time.Time) {
	noSuffix := func(a Agent) bool { return a.Branch == "" || a.Branch == "main" || a.Branch == "master" }
	found := lookupCwds(slices.DeleteFunc(slices.Clone(agents), noSuffix), roots, now)
	for i, a := range agents {
		if noSuffix(a) {
			continue
		}
		// Guarded: filepath.Base("") is ".", not "". Comparing to the branch's last
		// segment keeps feature/foo in dir foo from reading feature/foo/foo.
		if root := found[a.Cwd]; root != "" && filepath.Base(root) != path.Base(a.Branch) {
			agents[i].Worktree = filepath.Base(root)
		}
	}
}

// lookupCwds resolves each distinct non-empty cwd through c; see lookupKeys.
func lookupCwds[T any](agents []Agent, c *cwdCache[T], now time.Time) map[string]T {
	cwds := make([]string, 0, len(agents))
	for _, a := range agents {
		cwds = append(cwds, a.Cwd)
	}
	return lookupKeys(cwds, c, now)
}

// lookupKeys resolves each distinct non-empty key through c concurrently, so a
// roster of N worktrees costs one slow round-trip, not N sequential ones.
func lookupKeys[T any](keys []string, c *cwdCache[T], now time.Time) map[string]T {
	results := make(map[string]T)
	var mu sync.Mutex
	var wg sync.WaitGroup
	sem := make(chan struct{}, 6)
	seen := map[string]bool{}
	for _, k := range keys {
		if k == "" || seen[k] {
			continue
		}
		seen[k] = true
		wg.Add(1)
		go func(key string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			s := c.get(key, now)
			mu.Lock()
			results[key] = s
			mu.Unlock()
		}(k)
	}
	wg.Wait()
	return results
}

// enrichCI fills each agent's CI fields.
func enrichCI(agents []Agent, ci *cwdCache[ciResult], now time.Time) {
	results := lookupCwds(agents, ci, now)
	for i := range agents {
		r := results[agents[i].Cwd]
		agents[i].CI, agents[i].CIFails, agents[i].CIPasses, agents[i].CIConflict = r.State, r.Fails, r.Passes, r.Conflict
		agents[i].CIDraft, agents[i].CIDone = r.Draft, r.Done
		agents[i].PRNumber, agents[i].PRRepo, agents[i].PRMergedAt = r.PRNumber, r.PRRepo, unixOrZero(r.MergedAt)
		agents[i].CIQueue, agents[i].CIQueueReason = r.Queue, r.QueueReason
		if r.Queue != "" {
			agents[i].CIQueuePos, agents[i].CIQueueETA, agents[i].CIQueueSince = r.QueuePos, unixOrZero(r.QueueETA), unixOrZero(r.QueueSince)
			agents[i].CIQueueLabel = queueLabel(r, now)
		}
	}
}

// unixOrZero is t in unix seconds, keeping a zero time as 0 ("unknown").
func unixOrZero(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.Unix()
}

// queueLabel spells out the queue pill for its title and aria-label, dropping
// each clause whose field GitHub left null.
func queueLabel(r ciResult, now time.Time) string {
	parts := []string{fmt.Sprintf("merge queue position %d", r.QueuePos)}
	if r.Queue == "queue-failing" {
		parts[0] += ", failing"
	}
	if !r.QueueETA.IsZero() {
		parts = append(parts, "merge expected "+etaPhrase(r.QueueETA, now))
	}
	if !r.QueueSince.IsZero() {
		parts = append(parts, "queued "+humanizeSeconds(int(max(0, now.Sub(r.QueueSince)/time.Second)))+" ago")
	}
	return strings.Join(parts, " · ")
}
