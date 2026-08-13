package main

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestActionArgv(t *testing.T) {
	cases := []struct {
		action   string
		force    bool
		binary   string
		args     []string
		teardown bool
		ok       bool
	}{
		{"squash", false, "merge-pr", []string{"--squash"}, true, true},
		{"squash", true, "merge-pr", []string{"--squash", "--force"}, true, true},
		{"squash-admin", false, "merge-pr", []string{"--squash", "--admin"}, true, true},
		{"squash-admin", true, "merge-pr", []string{"--squash", "--admin", "--force"}, true, true},
		{"close", false, "merge-pr", []string{"--close"}, true, true},
		{"close", true, "merge-pr", []string{"--close", "--force"}, true, true},
		{"ready", false, "gh", []string{"pr", "ready"}, false, true},
		{"ready", true, "gh", []string{"pr", "ready"}, false, true}, // force ignored
		{"", false, "", nil, false, false},
		{"squash; rm -rf /", false, "", nil, false, false},
		{"--admin", false, "", nil, false, false},
	}
	for _, c := range cases {
		b, a, td, ok := actionArgv(c.action, c.force)
		if b != c.binary || td != c.teardown || ok != c.ok || !reflect.DeepEqual(a, c.args) {
			t.Errorf("actionArgv(%q,%v) = (%q,%v,%v,%v), want (%q,%v,%v,%v)",
				c.action, c.force, b, a, td, ok, c.binary, c.args, c.teardown, c.ok)
		}
	}
}

func TestInflightSet(t *testing.T) {
	s := newInflightSet()
	if !s.acquire("k") {
		t.Fatal("first acquire should succeed")
	}
	if s.acquire("k") {
		t.Fatal("second acquire of held key should fail")
	}
	if !s.acquire("other") {
		t.Fatal("distinct key should succeed")
	}
	s.release("k")
	if !s.acquire("k") {
		t.Fatal("acquire after release should succeed")
	}
}

// writeStub writes an executable bash script to a temp dir and returns its path.
func writeStub(t *testing.T, name, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, []byte("#!/usr/bin/env bash\n"+body), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestRunActionSuccess(t *testing.T) {
	stub := writeStub(t, "ok", `echo "merged fine"; exit 0`)
	r := runAction(stub, t.TempDir(), []string{"--squash"}, 5*time.Second)
	if r.ExitCode != 0 || r.TimedOut {
		t.Fatalf("got %+v", r)
	}
	if !strings.Contains(r.Output, "merged fine") {
		t.Fatalf("output = %q", r.Output)
	}
}

func TestActionEnvStripsTmux(t *testing.T) {
	in := []string{"PATH=/bin", "TMUX=/tmp/tmux-1000/default,123,4", "HOME=/home/x", "TMUX_PANE=%7", "TMUXFOO=keep"}
	got := actionEnv(in)
	for _, kv := range got {
		if strings.HasPrefix(kv, "TMUX=") || strings.HasPrefix(kv, "TMUX_PANE=") {
			t.Fatalf("tmux var leaked: %q in %v", kv, got)
		}
	}
	// Only the two exact keys are dropped; unrelated vars (incl. TMUX-prefixed
	// names that aren't TMUX/TMUX_PANE) survive.
	for _, want := range []string{"PATH=/bin", "HOME=/home/x", "TMUXFOO=keep"} {
		found := false
		for _, kv := range got {
			if kv == want {
				found = true
			}
		}
		if !found {
			t.Fatalf("dropped unrelated var %q; got %v", want, got)
		}
	}
}

// End to end: a child spawned by runAction must not see serve's $TMUX, so
// merge-pr falls back to matching the worktree instead of killing serve's own
// session. Set TMUX in this process and confirm the subprocess reads it empty.
func TestRunActionChildHasNoTmux(t *testing.T) {
	t.Setenv("TMUX", "/tmp/tmux-1000/default,999,0")
	t.Setenv("TMUX_PANE", "%99")
	stub := writeStub(t, "envcheck", `echo "TMUX=[${TMUX:-}] PANE=[${TMUX_PANE:-}]"; exit 0`)
	r := runAction(stub, t.TempDir(), nil, 5*time.Second)
	if r.ExitCode != 0 || r.TimedOut {
		t.Fatalf("got %+v", r)
	}
	if !strings.Contains(r.Output, "TMUX=[] PANE=[]") {
		t.Fatalf("child saw serve's tmux env: %q", r.Output)
	}
}

func TestRunActionFailureCapturesStderr(t *testing.T) {
	stub := writeStub(t, "fail", `echo "boom" >&2; exit 3`)
	r := runAction(stub, t.TempDir(), nil, 5*time.Second)
	if r.ExitCode != 3 || r.TimedOut {
		t.Fatalf("got %+v", r)
	}
	if !strings.Contains(r.Output, "boom") {
		t.Fatalf("output = %q", r.Output)
	}
}

func TestRunActionStdinClosed(t *testing.T) {
	// A stub that blocks on `read` would hang forever if stdin were a live tty.
	// With stdin closed, read hits EOF immediately and the stub exits.
	stub := writeStub(t, "reads", `read -r x || true; echo done; exit 0`)
	done := make(chan actionResult, 1)
	go func() { done <- runAction(stub, t.TempDir(), nil, 5*time.Second) }()
	select {
	case r := <-done:
		if r.ExitCode != 0 {
			t.Fatalf("got %+v", r)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("runAction hung — stdin was not closed")
	}
}

func TestRunActionTimeoutKillsProcessGroup(t *testing.T) {
	// The stub spawns a long-lived child and records its PID, then sleeps past
	// the deadline. After the timeout, the child must be dead too — proving the
	// whole process group was killed, not just the bash parent.
	pidFile := filepath.Join(t.TempDir(), "child.pid")
	stub := writeStub(t, "hang", `sleep 30 & echo $! > `+pidFile+`; sleep 30`)
	start := time.Now()
	r := runAction(stub, t.TempDir(), nil, 500*time.Millisecond)
	if !r.TimedOut {
		t.Fatalf("expected TimedOut, got %+v", r)
	}
	if time.Since(start) > 3*time.Second {
		t.Fatal("runAction did not return promptly after timeout")
	}
	// Give the kill a moment to propagate, then assert the child is gone.
	time.Sleep(200 * time.Millisecond)
	b, err := os.ReadFile(pidFile)
	if err != nil {
		t.Skipf("child pid not recorded: %v", err) // stub race; not the unit under test
	}
	pid, _ := strconv.Atoi(strings.TrimSpace(string(b)))
	if pid > 0 && syscall.Kill(pid, 0) == nil {
		t.Fatalf("child pid %d still alive — process group was not killed", pid)
	}
}

func TestRunTmuxTimeoutKillsProcessGroup(t *testing.T) {
	// Mirror of TestRunActionTimeoutKillsProcessGroup for the tmux exec path: a
	// hung tmux invocation must be SIGKILLed as a whole process group after the
	// timeout, not left with a reparented child surviving.
	pidFile := filepath.Join(t.TempDir(), "child.pid")
	stub := writeStub(t, "hangtmux", `sleep 30 & echo $! > `+pidFile+`; sleep 30`)
	start := time.Now()
	_, r := runTmux(stub, t.TempDir(), []string{"send-keys"}, 500*time.Millisecond)
	if !r.TimedOut {
		t.Fatalf("expected TimedOut, got %+v", r)
	}
	if time.Since(start) > 3*time.Second {
		t.Fatal("runTmux did not return promptly after timeout")
	}
	// Give the kill a moment to propagate, then assert the child is gone.
	time.Sleep(200 * time.Millisecond)
	b, err := os.ReadFile(pidFile)
	if err != nil {
		t.Skipf("child pid not recorded: %v", err) // stub race; not the unit under test
	}
	pid, _ := strconv.Atoi(strings.TrimSpace(string(b)))
	if pid > 0 && syscall.Kill(pid, 0) == nil {
		t.Fatalf("child pid %d still alive — process group was not killed", pid)
	}
}

func TestHostAllowed(t *testing.T) {
	extra := []string{"box.tailnet.ts.net"}
	yes := []string{"127.0.0.1", "127.0.0.1:4555", "localhost:4555", "[::1]:4555", "box.tailnet.ts.net", "BOX.tailnet.ts.net:4555", "100.64.0.1:4555"}
	no := []string{"", "evil.example.com", "evil.example.com:4555", "attacker.tailnet.ts.net"}
	for _, h := range yes {
		if !hostAllowed(h, "100.64.0.1", extra) {
			t.Errorf("hostAllowed(%q) = false, want true", h)
		}
	}
	for _, h := range no {
		if hostAllowed(h, "100.64.0.1", extra) {
			t.Errorf("hostAllowed(%q) = true, want false", h)
		}
	}
}

func TestTokenOK(t *testing.T) {
	if !tokenOK("abc123", "abc123") {
		t.Fatal("equal tokens should pass")
	}
	if tokenOK("abc123", "abc124") || tokenOK("", "abc") || tokenOK("abc", "") {
		t.Fatal("mismatched/empty tokens must fail")
	}
}

// openTestDB opens a fresh sqlite db in a temp dir for handleAction tests.
func openTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := openDB(testDB(t))
	if err != nil {
		t.Fatalf("openDB: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// queryLatestEventType returns the event_type of sessionID's most recent row.
func queryLatestEventType(t *testing.T, db *sql.DB, sessionID string) string {
	t.Helper()
	var et string
	if err := db.QueryRow(
		`SELECT event_type FROM events WHERE session_id = ? ORDER BY id DESC LIMIT 1`,
		sessionID).Scan(&et); err != nil {
		t.Fatalf("queryLatestEventType(%q): %v", sessionID, err)
	}
	return et
}

func newActionCfg(mergePR, gh string) *actionConfig {
	return &actionConfig{
		enabled: true, mergePR: mergePR, gh: gh,
		token: "secret", bindHost: "127.0.0.1", inflight: newInflightSet(),
	}
}

func actionReq(sessionID, action string, force bool, token, host string) *http.Request {
	form := url.Values{"session_id": {sessionID}, "action": {action}}
	if force {
		form.Set("force", "true")
	}
	r := httptest.NewRequest(http.MethodPost, "/api/action", strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if token != "" {
		r.Header.Set("X-Clodhopper-Token", token)
	}
	if host != "" {
		r.Host = host
	}
	return r
}

func TestHandleActionDisabled(t *testing.T) {
	db := openTestDB(t)
	cfg := newActionCfg("/bin/true", "/bin/true")
	cfg.enabled = false
	w := httptest.NewRecorder()
	handleAction(w, actionReq("s", "squash", false, "secret", "127.0.0.1"), db, cfg, time.Now())
	if w.Code != http.StatusForbidden {
		t.Fatalf("code = %d, want 403", w.Code)
	}
}

func TestHandleActionMethodHostToken(t *testing.T) {
	db := openTestDB(t)
	cfg := newActionCfg("/bin/true", "/bin/true")
	// wrong method
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/api/action", nil)
	r.Host = "127.0.0.1"
	handleAction(w, r, db, cfg, time.Now())
	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("method: %d", w.Code)
	}
	// bad host
	w = httptest.NewRecorder()
	handleAction(w, actionReq("s", "squash", false, "secret", "evil.example.com"), db, cfg, time.Now())
	if w.Code != http.StatusForbidden {
		t.Fatalf("host: %d", w.Code)
	}
	// bad token
	w = httptest.NewRecorder()
	handleAction(w, actionReq("s", "squash", false, "wrong", "127.0.0.1"), db, cfg, time.Now())
	if w.Code != http.StatusForbidden {
		t.Fatalf("token: %d", w.Code)
	}
}

func TestHandleActionUnknownActionAndSession(t *testing.T) {
	db := openTestDB(t)
	cfg := newActionCfg("/bin/true", "/bin/true")
	// unknown action
	w := httptest.NewRecorder()
	handleAction(w, actionReq("s", "nuke", false, "secret", "127.0.0.1"), db, cfg, time.Now())
	if w.Code != http.StatusBadRequest {
		t.Fatalf("action: %d", w.Code)
	}
	// valid action but session has no cwd on record
	w = httptest.NewRecorder()
	handleAction(w, actionReq("ghost", "squash", false, "secret", "127.0.0.1"), db, cfg, time.Now())
	if w.Code != http.StatusNotFound {
		t.Fatalf("session: %d", w.Code)
	}
}

func TestHandleActionTeardownSuccessEndsSession(t *testing.T) {
	db := openTestDB(t)
	dir := t.TempDir()
	insertEvent(db, Event{TS: "2026-08-12T10:00:00Z", SourceApp: "x", SessionID: "live-1", Cwd: dir, EventType: "Stop", PayloadJSON: "{}"})
	stub := writeStub(t, "merge", `echo "Merged #7"; exit 0`)
	cfg := newActionCfg(stub, "/bin/true")

	w := httptest.NewRecorder()
	handleAction(w, actionReq("live-1", "squash", false, "secret", "127.0.0.1"), db, cfg, time.Date(2026, 8, 12, 10, 5, 0, 0, time.UTC))
	if w.Code != http.StatusOK {
		t.Fatalf("code = %d body=%s", w.Code, w.Body.String())
	}
	var got struct {
		OK       bool   `json:"ok"`
		ExitCode int    `json:"exitCode"`
		Output   string `json:"output"`
	}
	json.Unmarshal(w.Body.Bytes(), &got)
	if !got.OK || got.ExitCode != 0 || !strings.Contains(got.Output, "Merged #7") {
		t.Fatalf("resp = %+v", got)
	}
	// Session must now be ended (synthetic SessionEnd is the latest event).
	if cwd, _ := latestCwdForSession(db, "live-1"); cwd != dir {
		t.Fatalf("cwd changed unexpectedly: %q", cwd)
	}
	rows := queryLatestEventType(t, db, "live-1")
	if rows != "SessionEnd" {
		t.Fatalf("latest event = %q, want SessionEnd", rows)
	}
}

func TestHandleAction_ForceReachesArgv(t *testing.T) {
	db := openTestDB(t)
	dir := t.TempDir()
	insertEvent(db, Event{TS: "2026-08-12T10:00:00Z", SourceApp: "x", SessionID: "sfx", Cwd: dir, EventType: "Stop", PayloadJSON: "{}"})
	stub := writeStub(t, "merge-pr", `echo "$@"; exit 0`)
	cfg := newActionCfg(stub, "gh")
	cfg.enabled = true
	cfg.token = "tok"

	w := httptest.NewRecorder()
	handleAction(w, actionReq("sfx", "squash", true, "tok", "127.0.0.1"), db, cfg, time.Now())
	if w.Code != http.StatusOK {
		t.Fatalf("code = %d body=%s", w.Code, w.Body.String())
	}
	var got struct {
		OK       bool   `json:"ok"`
		ExitCode int    `json:"exitCode"`
		TimedOut bool   `json:"timedOut"`
		Output   string `json:"output"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !got.OK || !strings.Contains(got.Output, "--force") {
		t.Fatalf("resp = %+v, want output containing --force", got)
	}
}

func TestHandleActionReadyDoesNotEndSession(t *testing.T) {
	db := openTestDB(t)
	dir := t.TempDir()
	insertEvent(db, Event{TS: "2026-08-12T10:00:00Z", SourceApp: "x", SessionID: "live-2", Cwd: dir, EventType: "Stop", PayloadJSON: "{}"})
	stub := writeStub(t, "gh", `echo "marked ready"; exit 0`)
	cfg := newActionCfg("/bin/false", stub)
	w := httptest.NewRecorder()
	handleAction(w, actionReq("live-2", "ready", false, "secret", "127.0.0.1"), db, cfg, time.Now())
	if w.Code != http.StatusOK {
		t.Fatalf("code = %d", w.Code)
	}
	if queryLatestEventType(t, db, "live-2") == "SessionEnd" {
		t.Fatal("ready must not end the session")
	}
}

func TestHandleActionFailureSurfaced(t *testing.T) {
	db := openTestDB(t)
	dir := t.TempDir()
	insertEvent(db, Event{TS: "2026-08-12T10:00:00Z", SourceApp: "x", SessionID: "live-3", Cwd: dir, EventType: "Stop", PayloadJSON: "{}"})
	stub := writeStub(t, "merge", `echo "uncommitted changes" >&2; exit 1`)
	cfg := newActionCfg(stub, "/bin/true")
	w := httptest.NewRecorder()
	handleAction(w, actionReq("live-3", "close", false, "secret", "127.0.0.1"), db, cfg, time.Now())
	var got struct {
		OK     bool   `json:"ok"`
		Output string `json:"output"`
	}
	json.Unmarshal(w.Body.Bytes(), &got)
	if got.OK || !strings.Contains(got.Output, "uncommitted changes") {
		t.Fatalf("resp = %+v", got)
	}
	// Failure must NOT end the session.
	if queryLatestEventType(t, db, "live-3") == "SessionEnd" {
		t.Fatal("failed action must not end the session")
	}
}

func TestHandleActionConcurrent409(t *testing.T) {
	db := openTestDB(t)
	dir := t.TempDir()
	insertEvent(db, Event{TS: "2026-08-12T10:00:00Z", SourceApp: "x", SessionID: "live-4", Cwd: dir, EventType: "Stop", PayloadJSON: "{}"})
	cfg := newActionCfg("/bin/true", "/bin/true")
	// Pre-acquire to simulate an in-flight action for this session.
	cfg.inflight.acquire("live-4")
	w := httptest.NewRecorder()
	handleAction(w, actionReq("live-4", "squash", false, "secret", "127.0.0.1"), db, cfg, time.Now())
	if w.Code != http.StatusConflict {
		t.Fatalf("code = %d, want 409", w.Code)
	}
}

func TestLatestPaneForSession(t *testing.T) {
	db := openTestDB(t)
	if p, err := latestPaneForSession(db, "nope"); err != nil || p != "" {
		t.Fatalf("absent session: p=%q err=%v", p, err)
	}
	insertEvent(db, Event{TS: "2026-08-12T10:00:00Z", SourceApp: "x", SessionID: "s1", TmuxPane: "%5", EventType: "Stop", PayloadJSON: "{}"})
	insertEvent(db, Event{TS: "2026-08-12T10:01:00Z", SourceApp: "x", SessionID: "s1", TmuxPane: "%9", EventType: "Stop", PayloadJSON: "{}"})
	if p, _ := latestPaneForSession(db, "s1"); p != "%9" {
		t.Fatalf("want latest pane %%9, got %q", p)
	}
	// A newer event that recorded no pane must not shadow the last known one:
	// the query skips blank panes, so the most recent non-empty value wins.
	insertEvent(db, Event{TS: "2026-08-12T10:02:00Z", SourceApp: "x", SessionID: "s1", EventType: "Stop", PayloadJSON: "{}"})
	if p, _ := latestPaneForSession(db, "s1"); p != "%9" {
		t.Fatalf("blank pane shadowed the last known one: %q", p)
	}
}

// tmuxLogStub returns a fake tmux binary that appends each invocation's argv
// (space-joined) to a log file and prints the pane id %77 for split-window, so a
// test can assert the exact commands runSessionAction issues.
func tmuxLogStub(t *testing.T) (bin, logPath string) {
	t.Helper()
	logPath = filepath.Join(t.TempDir(), "tmux.log")
	bin = writeStub(t, "tmux", `echo "$@" >> `+logPath+`
if [ "$1" = "split-window" ]; then echo "%77"; fi
exit 0`)
	return bin, logPath
}

func TestHandleActionMonitorCIBuildsArgv(t *testing.T) {
	db := openTestDB(t)
	dir := t.TempDir()
	insertEvent(db, Event{TS: "2026-08-12T10:00:00Z", SourceApp: "x", SessionID: "live-m", Cwd: dir, TmuxPane: "%12", EventType: "Stop", PayloadJSON: "{}"})
	stub, logPath := tmuxLogStub(t)
	cfg := newActionCfg("/bin/true", "/bin/true")
	cfg.tmux = stub

	w := httptest.NewRecorder()
	handleAction(w, actionReq("live-m", "monitor-ci", false, "secret", "127.0.0.1"), db, cfg, time.Now())
	if w.Code != http.StatusOK {
		t.Fatalf("code = %d body=%s", w.Code, w.Body.String())
	}
	got, _ := os.ReadFile(logPath)
	want := "send-keys -t %12 /clear Enter /monitor-ci Enter\n"
	if string(got) != want {
		t.Fatalf("argv = %q, want %q", got, want)
	}
	// A session action never tears the session down.
	if queryLatestEventType(t, db, "live-m") == "SessionEnd" {
		t.Fatal("monitor-ci must not end the session")
	}
}

func TestHandleActionMonitorCIGuards(t *testing.T) {
	db := openTestDB(t)
	dir := t.TempDir()
	insertEvent(db, Event{TS: "2026-08-12T10:00:00Z", SourceApp: "x", SessionID: "g1", Cwd: dir, TmuxPane: "%1", EventType: "Stop", PayloadJSON: "{}"})
	stub, logPath := tmuxLogStub(t)

	// disabled
	cfg := newActionCfg("/bin/true", "/bin/true")
	cfg.tmux = stub
	cfg.enabled = false
	w := httptest.NewRecorder()
	handleAction(w, actionReq("g1", "monitor-ci", false, "secret", "127.0.0.1"), db, cfg, time.Now())
	if w.Code != http.StatusForbidden {
		t.Fatalf("disabled: %d", w.Code)
	}
	cfg.enabled = true

	// wrong method
	w = httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/api/action", nil)
	r.Host = "127.0.0.1"
	handleAction(w, r, db, cfg, time.Now())
	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("method: %d", w.Code)
	}
	// bad host
	w = httptest.NewRecorder()
	handleAction(w, actionReq("g1", "monitor-ci", false, "secret", "evil.example.com"), db, cfg, time.Now())
	if w.Code != http.StatusForbidden {
		t.Fatalf("host: %d", w.Code)
	}
	// bad token
	w = httptest.NewRecorder()
	handleAction(w, actionReq("g1", "monitor-ci", false, "wrong", "127.0.0.1"), db, cfg, time.Now())
	if w.Code != http.StatusForbidden {
		t.Fatalf("token: %d", w.Code)
	}
	// None of the rejected requests may have reached tmux.
	if _, err := os.Stat(logPath); err == nil {
		t.Fatal("tmux ran despite a rejected request")
	}
}

func TestHandleActionNewMonitorBuildsArgv(t *testing.T) {
	db := openTestDB(t)
	dir := t.TempDir()
	insertEvent(db, Event{TS: "2026-08-12T10:00:00Z", SourceApp: "x", SessionID: "nm", Cwd: dir, TmuxPane: "%3", EventType: "Stop", PayloadJSON: "{}"})
	stub, logPath := tmuxLogStub(t)
	cfg := newActionCfg("/bin/true", "/bin/true")
	cfg.tmux = stub

	w := httptest.NewRecorder()
	handleAction(w, actionReq("nm", "new-monitor", false, "secret", "127.0.0.1"), db, cfg, time.Now())
	if w.Code != http.StatusOK {
		t.Fatalf("code = %d body=%s", w.Code, w.Body.String())
	}
	got, _ := os.ReadFile(logPath)
	// split the new pane running `nn claude`, capture its id, then send the
	// command into it. The pane ids are server-resolved (%3) and stub-returned
	// (%77) — never client input.
	want := "split-window -t %3 -P -F #{pane_id} nn claude\nsend-keys -t %77 /monitor-ci Enter\n"
	if string(got) != want {
		t.Fatalf("argv = %q, want %q", got, want)
	}
	if queryLatestEventType(t, db, "nm") == "SessionEnd" {
		t.Fatal("new-monitor must not end the session")
	}
}

func TestHandleActionSessionActionRejectsBadPane(t *testing.T) {
	db := openTestDB(t)
	dir := t.TempDir()
	// session with a cwd but no pane on record
	insertEvent(db, Event{TS: "2026-08-12T10:00:00Z", SourceApp: "x", SessionID: "nopane", Cwd: dir, EventType: "Stop", PayloadJSON: "{}"})
	// session whose recorded pane is malformed (fails paneIDRe)
	insertEvent(db, Event{TS: "2026-08-12T10:00:00Z", SourceApp: "x", SessionID: "badpane", Cwd: dir, TmuxPane: "; rm -rf /", EventType: "Stop", PayloadJSON: "{}"})
	stub, logPath := tmuxLogStub(t)
	cfg := newActionCfg("/bin/true", "/bin/true")
	cfg.tmux = stub

	cases := []struct{ sess, action string }{
		{"nopane", "monitor-ci"},
		{"badpane", "monitor-ci"},
		{"ghost", "new-monitor"}, // wholly unknown session
	}
	for _, c := range cases {
		w := httptest.NewRecorder()
		handleAction(w, actionReq(c.sess, c.action, false, "secret", "127.0.0.1"), db, cfg, time.Now())
		if w.Code != http.StatusNotFound {
			t.Fatalf("%s/%s: code = %d, want 404", c.sess, c.action, w.Code)
		}
	}
	if _, err := os.Stat(logPath); err == nil {
		t.Fatal("tmux ran for a session with no valid pane")
	}
}
