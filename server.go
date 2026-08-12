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
		"short":    shortID,
		"shortTS":  shortTS,
		"rowClass": rowClass,
	}).Parse(dashboardHTML),
)

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
	Now            time.Time         // render time, passed to shortTS so it can hide same-day dates
	SessColors     map[string]string // session id -> chip/tint color; see assignSessColors
	Signature      string            // fingerprint of the report-worthy view; see viewSignature
	PeekEnabled    bool              // true when serve --pane-peek is set; gates the roster's live-pane peek control
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
	peek := &peekConfig{enabled: *panePeek, lines: clampPaneLines(*paneLines), cache: newPaneCache()}
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		handleDashboard(w, r, db, ci, peek)
	})
	// JSON polling endpoint: the dashboard's JS hits this on the refresh interval
	// and re-renders only when the returned signature changes.
	mux.HandleFunc("/api/state", func(w http.ResponseWriter, r *http.Request) {
		handleState(w, r, db, ci, peek)
	})
	mux.HandleFunc("/api/pane", func(w http.ResponseWriter, r *http.Request) {
		handlePane(w, r, peek, time.Now())
	})

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
		Handler:           mux,
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

// ciCache memoises per-cwd CI lookups so the dashboard (which auto-refreshes)
// does not shell out to gh on every render. Keyed by cwd; entries expire after
// ciTTL.
type ciCache struct {
	mu sync.Mutex
	m  map[string]ciEntry
}

type ciEntry struct {
	status string
	at     time.Time
}

const ciTTL = 60 * time.Second

func newCICache() *ciCache { return &ciCache{m: map[string]ciEntry{}} }

// status returns a cached CI summary for the PR on the branch checked out at cwd,
// refreshing via gh when the cached value is stale. now is injected for testing.
func (c *ciCache) status(cwd string, now time.Time) string {
	if cwd == "" {
		return ""
	}
	c.mu.Lock()
	e, ok := c.m[cwd]
	c.mu.Unlock()
	if ok && now.Sub(e.at) < ciTTL {
		return e.status
	}
	s := lookupCI(cwd)
	c.mu.Lock()
	c.m[cwd] = ciEntry{status: s, at: now}
	c.mu.Unlock()
	return s
}

// lookupCI shells out to `gh pr checks` in cwd and summarises the result. It is
// strictly best-effort: a missing gh, no PR, an unauthenticated host, or a
// timeout all yield "" (rendered as "—"), never an error.
func lookupCI(cwd string) string {
	if _, err := exec.LookPath("gh"); err != nil {
		return ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "gh", "pr", "checks", "--json", "bucket")
	cmd.Dir = cwd
	out, err := cmd.Output()
	if err != nil {
		return "" // no PR for branch, not a repo, gh unauth, or timeout
	}
	var checks []struct {
		Bucket string `json:"bucket"`
	}
	if err := json.Unmarshal(out, &checks); err != nil {
		return ""
	}
	buckets := make([]string, len(checks))
	for i, c := range checks {
		buckets[i] = c.Bucket
	}
	return summarizeChecks(buckets)
}

// summarizeChecks reduces gh's per-check buckets to one label. Any failure wins;
// otherwise any still-pending check keeps it amber; an all-clear run is green.
func summarizeChecks(buckets []string) string {
	if len(buckets) == 0 {
		return ""
	}
	var pending bool
	for _, b := range buckets {
		switch b {
		case "fail", "cancel":
			return "failing"
		case "pending":
			pending = true
		}
	}
	if pending {
		return "pending"
	}
	return "green"
}

// setSecurityHeaders hardens both dashboard responses. Framing is the one that
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
// Called FIRST in each handler, before any error path can return: http.Error
// writes a response, and headers set afterwards are lost, so an error response
// would otherwise ship unhardened. Setting them up front is safe — nothing is
// written yet — and makes "every response carries these" structural rather than
// something each new return statement has to remember.
func setSecurityHeaders(w http.ResponseWriter) {
	w.Header().Set("X-Frame-Options", "DENY")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("Content-Security-Policy", "frame-ancestors 'none'; base-uri 'none'; object-src 'none'")
}

func handleDashboard(w http.ResponseWriter, r *http.Request, db *sql.DB, ci *ciCache, peek *peekConfig) {
	setSecurityHeaders(w)
	data, err := buildDashboardData(r, db, ci, peek)
	if err != nil {
		http.Error(w, "dashboard error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := dashboardTmpl.Execute(w, data); err != nil {
		http.Error(w, "render error", http.StatusInternalServerError)
	}
}

// handleState serves the dashboard's dynamic region as JSON for the JS poller:
// {"signature": ..., "html": ...}. The client compares the signature to what it
// last rendered and only swaps the DOM when it differs, so a quiet board never
// repaints.
func handleState(w http.ResponseWriter, r *http.Request, db *sql.DB, ci *ciCache, peek *peekConfig) {
	setSecurityHeaders(w)
	data, err := buildDashboardData(r, db, ci, peek)
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
func buildDashboardData(r *http.Request, db *sql.DB, ci *ciCache, peek *peekConfig) (dashboardData, error) {
	q := r.URL.Query()
	source := q.Get("source_app")
	branch := q.Get("branch")
	etype := q.Get("event_type")

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
	enrichCI(agents, ci, now)
	demotePendingCI(agents)
	if peek.enabled {
		for i := range agents {
			agents[i].LiveTmux = agents[i].TmuxPane != "" && peek.cache.live(agents[i].TmuxPane, now)
		}
	}

	activity, err := activeCounts(db, agentWindow, now)
	if err != nil {
		return dashboardData{}, fmt.Errorf("activity: %w", err)
	}

	events, err := queryEvents(db, EventFilter{SourceApp: source, Branch: branch, EventType: etype, Limit: 300})
	if err != nil {
		return dashboardData{}, fmt.Errorf("events: %w", err)
	}
	apps, _ := distinctSourceApps(db)
	branches, _ := distinctBranches(db)

	data := dashboardData{
		Agents:         agents,
		Activity:       activity,
		Events:         events,
		EventRows:      foldToolEvents(events),
		SourceApps:     apps,
		Branches:       branches,
		FilterSource:   source,
		FilterBranch:   branch,
		FilterType:     etype,
		RefreshSecs:    refresh,
		RefreshOptions: refreshOptions(refresh),
		WindowDays:     windowDays,
		WindowOptions:  windowOptions(windowDays),
		Generated:      now.Format("15:04:05"),
		Now:            now,
		SessColors:     assignSessColors(agents, events),
		PeekEnabled:    peek.enabled,
	}
	data.Signature = viewSignature(data)
	return data, nil
}

// viewSignature fingerprints only the *report-worthy* parts of the view: the
// event list, each agent's status/doing/CI, and the activity tallies. It
// deliberately omits cosmetic, purely time-derived values — the "updated" clock,
// idle counters, and the idle-based row ordering — so the dashboard's poller
// redraws when something actually happened, not merely because time passed.
// Components are folded in a stable, idle-independent order for the same reason.
func viewSignature(d dashboardData) string {
	h := fnv.New64a()

	// Events are append-only and immutable, so count plus the id bounds detect any
	// insert or prune without hashing every row.
	fmt.Fprintf(h, "e:%d", len(d.Events))
	if len(d.Events) > 0 {
		fmt.Fprintf(h, ":%d:%d", d.Events[0].ID, d.Events[len(d.Events)-1].ID)
	}

	agents := append([]Agent(nil), d.Agents...)
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
		fmt.Fprintf(h, "|a:%q:%q:%q:%q:%q:%q:%q:%q", a.SessionID, a.TmuxSession, a.SourceApp, a.Branch, a.Cwd, a.Status, a.Doing, a.CI)
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
		fmt.Fprintf(h, "|c:%q:%q:%q:%d", c.TmuxSession, c.SourceApp, c.Branch, c.Count)
	}

	return strconv.FormatUint(h.Sum64(), 16)
}

// demotePendingCI relaxes a "needs you"/"waiting for you" row to the non-urgent
// statusBackground when its CI is still running. A pending check means the agent
// most likely pushed and is waiting on that run, not blocked on the user — so the
// alert styling would be misleading (issue #31). This runs in the server layer,
// not in deriveStatus, because CI is fetched here via gh and is deliberately kept
// out of the deterministic roster.
//
// Tradeoff (intentional, matching the roster's other heuristics): an agent that
// genuinely finished and is awaiting you, but whose earlier push still has CI in
// flight, is also relaxed for as long as that run stays pending. We accept that
// over the louder false positive of flagging a busy-waiting agent as needing
// you. A real "needs approval" (a permission prompt) is left alone — that blocks
// on you regardless of CI.
func demotePendingCI(agents []Agent) {
	for i := range agents {
		if agents[i].CI != "pending" {
			continue
		}
		if agents[i].Status == statusNeedsYou || agents[i].Status == statusWaiting {
			agents[i].Status = statusBackground
			agents[i].StatusRank = rankBackground
		}
	}
}

// enrichCI fills each agent's CI field, fetching distinct cwds concurrently so a
// roster of N worktrees costs one slow gh round-trip, not N sequential ones.
func enrichCI(agents []Agent, ci *ciCache, now time.Time) {
	results := make(map[string]string)
	var mu sync.Mutex
	var wg sync.WaitGroup
	sem := make(chan struct{}, 6)
	seen := map[string]bool{}
	for _, a := range agents {
		if a.Cwd == "" || seen[a.Cwd] {
			continue
		}
		seen[a.Cwd] = true
		wg.Add(1)
		go func(cwd string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			s := ci.status(cwd, now)
			mu.Lock()
			results[cwd] = s
			mu.Unlock()
		}(a.Cwd)
	}
	wg.Wait()
	for i := range agents {
		agents[i].CI = results[agents[i].Cwd]
	}
}
