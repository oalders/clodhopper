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
	"sync"
	"time"
)

//go:embed templates/dashboard.html
var dashboardHTML string

var dashboardTmpl = template.Must(
	template.New("dashboard").Funcs(template.FuncMap{
		"short":   shortID,
		"shortTS": shortTS,
	}).Parse(dashboardHTML),
)

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

// dashboardData is the view model rendered by the dashboard template.
type dashboardData struct {
	Agents         []Agent
	Activity       []SourceCount
	Events         []Event
	SourceApps     []string
	Branches       []string
	FilterSource   string
	FilterBranch   string
	FilterType     string
	RefreshSecs    int // 0 = auto-refresh disabled
	RefreshOptions []refreshOption
	Generated      string
	Now            time.Time // render time, passed to shortTS so it can hide same-day dates
	Signature      string    // fingerprint of the report-worthy view; see viewSignature
}

// refreshOption is one entry in the auto-refresh interval dropdown.
type refreshOption struct {
	Secs     int
	Label    string
	Selected bool
}

// agentWindow bounds how long after its last event a session still counts as a
// live agent on the roster.
const agentWindow = 30 * time.Minute

// refreshMax caps the auto-refresh interval (and the env default) at a sane hour.
const refreshMax = 3600

// refreshPresets are the dropdown choices; 0 means "off".
var refreshPresets = []int{0, 2, 5, 10, 30, 60}

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

// runServe starts the read-only dashboard HTTP server. It binds 127.0.0.1 by
// default; --host 0.0.0.0 (or CLODHOPPER_HOST) exposes it on all interfaces, which
// is needed when the browser is on a different machine/namespace than the
// server (e.g. a container or VM) — at the cost of LAN exposure.
func runServe(args []string) int {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	port := fs.Int("port", defaultPort(), "port to listen on")
	host := fs.String("host", defaultHost(), "address to bind (127.0.0.1 default; 0.0.0.0 for LAN/container access)")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	db, err := openDB(defaultDBPath())
	if err != nil {
		fmt.Fprintln(os.Stderr, "clodhopper serve: open db:", err)
		return 1
	}
	defer db.Close()

	ci := newCICache()
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		handleDashboard(w, r, db, ci)
	})
	// JSON polling endpoint: the dashboard's JS hits this on the refresh interval
	// and re-renders only when the returned signature changes.
	mux.HandleFunc("/api/state", func(w http.ResponseWriter, r *http.Request) {
		handleState(w, r, db, ci)
	})

	addr := net.JoinHostPort(*host, strconv.Itoa(*port))
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

func handleDashboard(w http.ResponseWriter, r *http.Request, db *sql.DB, ci *ciCache) {
	data, err := buildDashboardData(r, db, ci)
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
func handleState(w http.ResponseWriter, r *http.Request, db *sql.DB, ci *ciCache) {
	data, err := buildDashboardData(r, db, ci)
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
func buildDashboardData(r *http.Request, db *sql.DB, ci *ciCache) (dashboardData, error) {
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

	now := time.Now()
	waitingCap := time.Duration(waitingRetainHours()) * time.Hour
	agents, err := agentRoster(db, waitingCap, now)
	if err != nil {
		return dashboardData{}, fmt.Errorf("roster: %w", err)
	}
	enrichCI(agents, ci, now)

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
		SourceApps:     apps,
		Branches:       branches,
		FilterSource:   source,
		FilterBranch:   branch,
		FilterType:     etype,
		RefreshSecs:    refresh,
		RefreshOptions: refreshOptions(refresh),
		Generated:      now.Format("15:04:05"),
		Now:            now,
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
		fmt.Fprintf(h, "|a:%s:%s:%s:%s:%s:%s", a.SessionID, a.SourceApp, a.Branch, a.Status, a.Doing, a.CI)
	}

	activity := append([]SourceCount(nil), d.Activity...)
	sort.Slice(activity, func(i, j int) bool {
		if activity[i].SourceApp != activity[j].SourceApp {
			return activity[i].SourceApp < activity[j].SourceApp
		}
		return activity[i].Branch < activity[j].Branch
	})
	for _, c := range activity {
		fmt.Fprintf(h, "|c:%s:%s:%d", c.SourceApp, c.Branch, c.Count)
	}

	return strconv.FormatUint(h.Sum64(), 16)
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
