package main

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
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
		// rebase carries no argv of its own: runRebase assembles the sequence from
		// a server-validated base branch. Not a teardown, and force is ignored.
		{"rebase", false, "git", nil, false, true},
		{"rebase", true, "git", nil, false, true},
		{"", false, "", nil, false, false},
		{"squash; rm -rf /", false, "", nil, false, false},
		{"rebase; rm -rf /", false, "", nil, false, false},
		{"REBASE", false, "", nil, false, false},
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
		enabled: true, mergePR: mergePR, gh: gh, git: "git",
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
	// Two separate send-keys calls, not one burst: /clear must be delivered and
	// the TUI given a chance to redraw before /monitor-ci is submitted.
	want := "send-keys -t %12 /clear Enter\nsend-keys -t %12 /monitor-ci Enter\n"
	if string(got) != want {
		t.Fatalf("argv = %q, want %q", got, want)
	}
	// A session action never tears the session down.
	if queryLatestEventType(t, db, "live-m") == "SessionEnd" {
		t.Fatal("monitor-ci must not end the session")
	}
}

// A failed /clear must abort the action: there is no redraw to wait out and no
// working channel to the pane, so /monitor-ci is not sent into the dark.
func TestRunSessionActionMonitorCIStopsWhenClearFails(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "tmux.log")
	stub := writeStub(t, "tmux", `echo "$@" >> `+logPath+`
exit 3`)

	res := runSessionAction(stub, "monitor-ci", "%12", t.TempDir(), 0)
	if res.ExitCode != 3 {
		t.Fatalf("exit code = %d, want the failing /clear's 3", res.ExitCode)
	}
	got, _ := os.ReadFile(logPath)
	if want := "send-keys -t %12 /clear Enter\n"; string(got) != want {
		t.Fatalf("argv = %q, want only the /clear call %q", got, want)
	}
}

// The pause between the two send-keys calls is what makes the fix work in a
// sandboxed session, where the post-/clear redraw swallows keys that arrive too
// soon. Assert it is actually waited out rather than silently skipped.
func TestRunSessionActionMonitorCIWaitsAfterClear(t *testing.T) {
	stub, logPath := tmuxLogStub(t)
	const delay = 150 * time.Millisecond

	start := time.Now()
	res := runSessionAction(stub, "monitor-ci", "%12", t.TempDir(), delay)
	elapsed := time.Since(start)
	if res.ExitCode != 0 || res.TimedOut {
		t.Fatalf("res = %+v", res)
	}
	if elapsed < delay {
		t.Fatalf("elapsed %v < clearDelay %v: the pause after /clear was skipped", elapsed, delay)
	}
	got, _ := os.ReadFile(logPath)
	want := "send-keys -t %12 /clear Enter\nsend-keys -t %12 /monitor-ci Enter\n"
	if string(got) != want {
		t.Fatalf("argv = %q, want %q", got, want)
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

// decodeActionJSON unpacks the JSON envelope every action handler writes.
func decodeActionJSON(t *testing.T, w *httptest.ResponseRecorder) struct {
	OK       bool   `json:"ok"`
	ExitCode int    `json:"exitCode"`
	TimedOut bool   `json:"timedOut"`
	Output   string `json:"output"`
} {
	t.Helper()
	var got struct {
		OK       bool   `json:"ok"`
		ExitCode int    `json:"exitCode"`
		TimedOut bool   `json:"timedOut"`
		Output   string `json:"output"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode %q: %v", w.Body.String(), err)
	}
	return got
}

// The End action reuses handleAction's whole guard chain: disabled, wrong
// method, unknown Host, and a bad CSRF token must all be rejected before
// anything is written to the db.
func TestHandleActionEndRejectedByGuards(t *testing.T) {
	now := time.Date(2026, 8, 20, 10, 0, 0, 0, time.UTC)
	seed := func() (*sql.DB, *actionConfig) {
		db := openTestDB(t)
		insertEvent(db, Event{TS: "2026-08-20T09:00:00Z", SourceApp: "x", SessionID: "live-1", Cwd: t.TempDir(), EventType: "Stop", PayloadJSON: "{}"})
		return db, newActionCfg("/bin/true", "/bin/true")
	}

	// disabled
	db, cfg := seed()
	cfg.enabled = false
	w := httptest.NewRecorder()
	handleAction(w, actionReq("live-1", "end", false, "secret", "127.0.0.1"), db, cfg, now)
	if w.Code != http.StatusForbidden {
		t.Fatalf("disabled: %d, want 403", w.Code)
	}
	if et := queryLatestEventType(t, db, "live-1"); et == "SessionEnd" {
		t.Fatal("disabled request ended the session")
	}

	// wrong method
	db, cfg = seed()
	w = httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/api/action?session_id=live-1&action=end", nil)
	r.Host = "127.0.0.1"
	r.Header.Set("X-Clodhopper-Token", "secret")
	handleAction(w, r, db, cfg, now)
	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET: %d, want 405", w.Code)
	}

	// bad host
	db, cfg = seed()
	w = httptest.NewRecorder()
	handleAction(w, actionReq("live-1", "end", false, "secret", "evil.example.com"), db, cfg, now)
	if w.Code != http.StatusForbidden {
		t.Fatalf("host: %d, want 403", w.Code)
	}

	// bad token
	db, cfg = seed()
	w = httptest.NewRecorder()
	handleAction(w, actionReq("live-1", "end", false, "wrong", "127.0.0.1"), db, cfg, now)
	if w.Code != http.StatusForbidden {
		t.Fatalf("token: %d, want 403", w.Code)
	}
	if et := queryLatestEventType(t, db, "live-1"); et == "SessionEnd" {
		t.Fatal("bad token ended the session")
	}
}

// A successful End writes a synthetic SessionEnd and the row leaves the roster,
// even though the session has no tmux pane — stale, pane-less rows are exactly
// what End exists for. No subprocess runs, so the binaries are unusable stubs.
func TestHandleActionEndDismissesPanelessRow(t *testing.T) {
	db := openTestDB(t)
	now := time.Date(2026, 8, 20, 10, 0, 0, 0, time.UTC)
	insertEvent(db, Event{TS: "2026-08-20T09:00:00Z", SourceApp: "x", Branch: "feature", SessionID: "live-1", Cwd: t.TempDir(), EventType: "Stop", PayloadJSON: "{}"})
	// No TmuxPane on the row at all.
	if pane, _ := latestPaneForSession(db, "live-1"); pane != "" {
		t.Fatalf("fixture has a pane: %q", pane)
	}
	if roster, err := agentRoster(db, 16*time.Hour, now); err != nil || len(roster) != 1 {
		t.Fatalf("roster before = %v (err %v), want 1 row", roster, err)
	}

	cfg := newActionCfg("/nonexistent/merge-pr", "/nonexistent/gh")
	w := httptest.NewRecorder()
	handleAction(w, actionReq("live-1", "end", false, "secret", "127.0.0.1"), db, cfg, now)
	if w.Code != http.StatusOK {
		t.Fatalf("code = %d body=%s", w.Code, w.Body.String())
	}
	got := decodeActionJSON(t, w)
	if !got.OK || got.ExitCode != 0 || got.TimedOut {
		t.Fatalf("resp = %+v, want ok", got)
	}
	if et := queryLatestEventType(t, db, "live-1"); et != "SessionEnd" {
		t.Fatalf("latest event = %q, want SessionEnd", et)
	}
	roster, err := agentRoster(db, 16*time.Hour, now)
	if err != nil {
		t.Fatalf("agentRoster: %v", err)
	}
	if len(roster) != 0 {
		t.Fatalf("roster after = %+v, want empty", roster)
	}
}

// An unknown or already-ended session must report a visible failure, never a
// false OK (which would leave the row on screen with a "dismissed" message).
func TestHandleActionEndUnknownSessionReportsFailure(t *testing.T) {
	db := openTestDB(t)
	now := time.Date(2026, 8, 20, 10, 0, 0, 0, time.UTC)
	insertEvent(db, Event{TS: "2026-08-20T09:00:00Z", SourceApp: "x", SessionID: "live-1", Cwd: t.TempDir(), EventType: "Stop", PayloadJSON: "{}"})
	cfg := newActionCfg("/bin/true", "/bin/true")

	// never seen
	w := httptest.NewRecorder()
	handleAction(w, actionReq("ghost", "end", false, "secret", "127.0.0.1"), db, cfg, now)
	if w.Code != http.StatusOK {
		t.Fatalf("code = %d body=%s", w.Code, w.Body.String())
	}
	if got := decodeActionJSON(t, w); got.OK || got.Output == "" {
		t.Fatalf("unknown session resp = %+v, want a non-ok result with a message", got)
	}

	// already ended: the first End succeeds, a second must not claim success
	w = httptest.NewRecorder()
	handleAction(w, actionReq("live-1", "end", false, "secret", "127.0.0.1"), db, cfg, now)
	if got := decodeActionJSON(t, w); !got.OK {
		t.Fatalf("first end resp = %+v, want ok", got)
	}
	w = httptest.NewRecorder()
	handleAction(w, actionReq("live-1", "end", false, "secret", "127.0.0.1"), db, cfg, now)
	if got := decodeActionJSON(t, w); got.OK || got.Output == "" {
		t.Fatalf("repeat end resp = %+v, want a non-ok result with a message", got)
	}
	// A blank session id targets nothing rather than every live session.
	w = httptest.NewRecorder()
	handleAction(w, actionReq("", "end", false, "secret", "127.0.0.1"), db, cfg, now)
	if w.Code != http.StatusNotFound {
		t.Fatalf("blank session: %d, want 404", w.Code)
	}
}

// A second End for the same session while one is in flight is refused by the
// shared inflight set, exactly as the merge actions are.
func TestHandleActionEndConcurrentIsConflict(t *testing.T) {
	db := openTestDB(t)
	now := time.Date(2026, 8, 20, 10, 0, 0, 0, time.UTC)
	insertEvent(db, Event{TS: "2026-08-20T09:00:00Z", SourceApp: "x", SessionID: "live-1", Cwd: t.TempDir(), EventType: "Stop", PayloadJSON: "{}"})
	cfg := newActionCfg("/bin/true", "/bin/true")
	if !cfg.inflight.acquire("live-1") {
		t.Fatal("acquire failed")
	}
	defer cfg.inflight.release("live-1")

	w := httptest.NewRecorder()
	handleAction(w, actionReq("live-1", "end", false, "secret", "127.0.0.1"), db, cfg, now)
	if w.Code != http.StatusConflict {
		t.Fatalf("code = %d, want 409", w.Code)
	}
	if et := queryLatestEventType(t, db, "live-1"); et == "SessionEnd" {
		t.Fatal("conflicting request ended the session anyway")
	}
}

// An ambiguous session prefix must surface as a visible failure, not a silent
// no-op and not the wrong agent being dismissed. The dashboard sends full
// session ids, but endSessions matches by prefix, so one id that prefixes
// another is still reachable.
func TestHandleActionEndAmbiguousPrefixFails(t *testing.T) {
	db := openTestDB(t)
	now := time.Date(2026, 8, 20, 10, 0, 0, 0, time.UTC)
	insertEvent(db, Event{TS: "2026-08-20T09:00:00Z", SourceApp: "x", SessionID: "abc", Cwd: t.TempDir(), EventType: "Stop", PayloadJSON: "{}"})
	insertEvent(db, Event{TS: "2026-08-20T09:00:01Z", SourceApp: "x", SessionID: "abcdef", Cwd: t.TempDir(), EventType: "Stop", PayloadJSON: "{}"})
	cfg := newActionCfg("/bin/true", "/bin/true")

	w := httptest.NewRecorder()
	handleAction(w, actionReq("abc", "end", false, "secret", "127.0.0.1"), db, cfg, now)
	if w.Code != http.StatusOK {
		t.Fatalf("code = %d body=%s", w.Code, w.Body.String())
	}
	got := decodeActionJSON(t, w)
	if got.OK || !strings.Contains(got.Output, "2 live sessions") {
		t.Fatalf("resp = %+v, want a non-ok ambiguity message", got)
	}
	// Nothing may have been ended.
	for _, id := range []string{"abc", "abcdef"} {
		if et := queryLatestEventType(t, db, id); et == "SessionEnd" {
			t.Fatalf("%s was ended despite the ambiguity", id)
		}
	}
}

// A lookup failure inside endSessions must reach the client as a fixed, generic
// message: the raw driver text goes to debugf only. Dropping the events table
// forces a real (non-ambiguity) error out of endSessions, which is the branch
// that would otherwise leak SQLite internals if someone reinstated err.Error().
func TestHandleActionEndGenericErrorHidesDriverDetail(t *testing.T) {
	db := openTestDB(t)
	now := time.Date(2026, 8, 20, 10, 0, 0, 0, time.UTC)
	insertEvent(db, Event{TS: "2026-08-20T09:00:00Z", SourceApp: "x", SessionID: "live-1", Cwd: t.TempDir(), EventType: "Stop", PayloadJSON: "{}"})
	if _, err := db.Exec("DROP TABLE events"); err != nil {
		t.Fatalf("drop events: %v", err)
	}
	cfg := newActionCfg("/bin/true", "/bin/true")

	w := httptest.NewRecorder()
	handleAction(w, actionReq("live-1", "end", false, "secret", "127.0.0.1"), db, cfg, now)
	if w.Code != http.StatusOK {
		t.Fatalf("code = %d body=%s", w.Code, w.Body.String())
	}
	got := decodeActionJSON(t, w)
	if got.OK {
		t.Fatalf("resp = %+v, want a non-ok result", got)
	}
	if got.Output != "could not end session" {
		t.Fatalf("output = %q, want the generic message", got.Output)
	}
	for _, leak := range []string{"no such table", "events", "sqlite", "SQL"} {
		if strings.Contains(got.Output, leak) {
			t.Fatalf("output %q leaks driver detail %q", got.Output, leak)
		}
	}
}

// ---------------------------------------------------------------------------
// rebase action
// ---------------------------------------------------------------------------

func TestRebaseSteps(t *testing.T) {
	got := rebaseSteps("main", "feature", "deadbeefcafe123")
	want := []rebaseStep{
		{args: []string{"fetch", "origin", "main"}},
		{args: []string{"pull", "--rebase", "origin", "main"}, abortOnFail: true},
		{args: []string{"push", "--force-with-lease=feature:deadbeefcafe123", "origin", "feature"}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("rebaseSteps = %v, want %v", got, want)
	}
	// Only the rebase itself can leave a half-applied state behind, so it is the
	// only step that asks for cleanup. Keyed off the step, not its index.
	for i, st := range got {
		if wantAbort := st.args[0] == "pull"; st.abortOnFail != wantAbort {
			t.Errorf("step %d (%v): abortOnFail = %v, want %v", i, st.args, st.abortOnFail, wantAbort)
		}
	}
	// No remote-tracking ref (never pushed): a plain push creates the branch, and
	// asking for a force we cannot lease would only fail.
	noLease := rebaseSteps("main", "feature", "")
	if !reflect.DeepEqual(noLease[2].args, []string{"push", "origin", "feature"}) {
		t.Fatalf("no-lease push = %v", noLease[2].args)
	}
	// The base is the only thing that varies in the first two argvs.
	if !reflect.DeepEqual(rebaseSteps("release/2.x", "feat", "abc1234")[:2], []rebaseStep{
		{args: []string{"fetch", "origin", "release/2.x"}},
		{args: []string{"pull", "--rebase", "origin", "release/2.x"}, abortOnFail: true},
	}) {
		t.Fatalf("release/2.x = %v", rebaseSteps("release/2.x", "feat", "abc1234"))
	}
	if !reflect.DeepEqual(rebaseAbortArgv(), []string{"rebase", "--abort"}) {
		t.Fatalf("rebaseAbortArgv() = %v", rebaseAbortArgv())
	}
}

// validBranchName is the last line of defence before a derived branch name joins
// an argv, so anything option-shaped, path-traversing, shell-ish, line-broken or
// absurdly long must fail.
func TestValidBranchName(t *testing.T) {
	cases := []struct {
		name string
		ok   bool
	}{
		{"main", true},
		{"master", true},
		{"release/2.x", true},
		{"feat_1.2-rc", true},
		// Slashes are legal in branch names (release/2.x), so a branch literally
		// called "origin/main" passes here; stripping the remote prefix is
		// defaultBranch's job, not this predicate's.
		{"origin/main", true},
		{strings.Repeat("a", maxBranchNameLen), true},
		{strings.Repeat("a", maxBranchNameLen+1), false},
		{"", false},
		{"-", false},
		{"--upload-pack=evil", false},
		{"-o=x", false},
		{"../../x", false},
		{"a..b", false},
		{"; rm -rf /", false},
		{"main; rm -rf /", false},
		{"main branch", false},
		{"main\nmaster", false},
		{"main\rmaster", false},
		{"main\r\n--upload-pack=evil", false},
		{"$(whoami)", false},
		{"héllo", false},
	}
	for _, c := range cases {
		if got := validBranchName(c.name); got != c.ok {
			t.Errorf("validBranchName(%q) = %v, want %v", c.name, got, c.ok)
		}
	}
}

func TestLeaseSHARe(t *testing.T) {
	for _, ok := range []string{"abc1234", strings.Repeat("a", 40), strings.Repeat("0", 64)} {
		if !leaseSHARe.MatchString(ok) {
			t.Errorf("leaseSHARe rejected %q", ok)
		}
	}
	for _, bad := range []string{"", "abc123", "ABC1234", "abc1234 ", "abc1234\nx", "--upload-pack=x",
		strings.Repeat("a", 65), "refs/heads/main"} {
		if leaseSHARe.MatchString(bad) {
			t.Errorf("leaseSHARe accepted %q", bad)
		}
	}
}

// gitStubOpts configures the fake git below. Zero values mean "the probe fails",
// which is what most refusal tests want.
type gitStubOpts struct {
	head       string   // what `symbolic-ref --short HEAD` prints ("" => detached)
	originHead string   // what `symbolic-ref --short refs/remotes/origin/HEAD` prints
	refs       []string // remote refs `rev-parse --verify` resolves, e.g. "refs/remotes/origin/main"
	lease      string   // what rev-parse prints for refs/remotes/origin/<head> ("" => no such ref)
	failStep   string   // a first argument ("fetch"/"pull"/"push") that exits non-zero
	abortFail  bool     // `rebase --abort` itself fails
	sleep      string   // seconds each fetch/pull/push sleeps, as a shell literal
}

// gitStub writes a fake `git` that appends each invocation's argv (minus the
// leading -C DIR) to logPath and answers every probe runRebase makes.
func gitStub(t *testing.T, logPath string, o gitStubOpts) string {
	t.Helper()
	return writeStub(t, "git", `
log=`+strconv.Quote(logPath)+`
if [ "$1" = "-C" ]; then shift 2; fi
printf '%s\n' "$*" >> "$log"
head=`+strconv.Quote(o.head)+`
ohead=`+strconv.Quote(o.originHead)+`
refs=`+strconv.Quote(" "+strings.Join(o.refs, " ")+" ")+`
lease=`+strconv.Quote(o.lease)+`
fail=`+strconv.Quote(o.failStep)+`
abortfail=`+strconv.Quote(map[bool]string{true: "1"}[o.abortFail])+`
naptime=`+strconv.Quote(o.sleep)+`
case "$1" in
  symbolic-ref)
    if [ "$4" = "HEAD" ]; then [ -n "$head" ] || exit 1; echo "$head"; exit 0; fi
    [ -n "$ohead" ] || exit 1; echo "$ohead"; exit 0 ;;
  rev-parse)
    if [ -n "$lease" ] && [ "$4" = "refs/remotes/origin/$head" ]; then echo "$lease"; exit 0; fi
    case "$refs" in *" $4 "*) echo 1111111111111111111111111111111111111111; exit 0 ;; esac
    exit 1 ;;
  rebase)
    [ -z "$abortfail" ] || { echo "fatal: no rebase in progress" >&2; exit 1; }
    echo "aborted"; exit 0 ;;
esac
if [ -n "$fail" ] && [ "$1" = "$fail" ]; then echo "CONFLICT (content): merge conflict in a.txt" >&2; exit 1; fi
[ -z "$naptime" ] || sleep "$naptime"
case "$1" in
  fetch|pull|push) echo "ran git $1"; exit 0 ;;
esac
exit 1
`)
}

// gitLog returns the recorded argv lines from a gitStub log.
func gitLog(t *testing.T, logPath string) []string {
	t.Helper()
	b, err := os.ReadFile(logPath)
	if err != nil {
		return nil
	}
	var out []string
	for _, l := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		if l != "" {
			out = append(out, l)
		}
	}
	return out
}

// newRebaseCfg wires a stubbed git into an otherwise standard action config.
func newRebaseCfg(gitBin string) *actionConfig {
	cfg := newActionCfg("/bin/false", "/bin/false")
	cfg.git = gitBin
	return cfg
}

// rebaseFixture seeds one roster row and returns its cwd plus a config whose git
// is the stub described by o, and the path the stub logs argvs to.
func rebaseFixture(t *testing.T, db *sql.DB, session string, o gitStubOpts) (dir, logPath string, cfg *actionConfig) {
	t.Helper()
	dir = t.TempDir()
	insertEvent(db, Event{TS: "2026-08-12T10:00:00Z", SourceApp: "x", SessionID: session, Cwd: dir, EventType: "Stop", PayloadJSON: "{}"})
	logPath = filepath.Join(t.TempDir(), "git.log")
	return dir, logPath, newRebaseCfg(gitStub(t, logPath, o))
}

func TestHandleActionRebaseSuccess(t *testing.T) {
	db := openTestDB(t)
	_, logPath, cfg := rebaseFixture(t, db, "reb-1", gitStubOpts{
		head: "feature", originHead: "origin/main", lease: "abc1234def5678",
	})

	w := httptest.NewRecorder()
	handleAction(w, actionReq("reb-1", "rebase", false, "secret", "127.0.0.1"), db, cfg, time.Now())
	if w.Code != http.StatusOK {
		t.Fatalf("code = %d body=%s", w.Code, w.Body.String())
	}
	if got := decodeActionJSON(t, w); !got.OK {
		t.Fatalf("resp = %+v", got)
	}
	// The probes (default branch, current branch, then the LEASE — captured
	// before the fetch so no concurrent fetch can move it), then the three steps.
	// The push names its refspec explicitly and pins the lease to that SHA.
	want := []string{
		"symbolic-ref --quiet --short refs/remotes/origin/HEAD",
		"symbolic-ref --quiet --short HEAD",
		"rev-parse --verify --quiet refs/remotes/origin/feature",
		"fetch origin main",
		"pull --rebase origin main",
		"push --force-with-lease=feature:abc1234def5678 origin feature",
	}
	if !reflect.DeepEqual(gitLog(t, logPath), want) {
		t.Fatalf("git calls = %v, want %v", gitLog(t, logPath), want)
	}
	// Not a teardown: the row stays.
	if queryLatestEventType(t, db, "reb-1") == "SessionEnd" {
		t.Fatal("rebase must not end the session")
	}
}

// A branch that was never pushed has no remote-tracking ref to lease against, so
// it gets a plain push — no force at all.
func TestHandleActionRebaseNoRemoteTrackingRefPushesPlain(t *testing.T) {
	db := openTestDB(t)
	_, logPath, cfg := rebaseFixture(t, db, "reb-lease0", gitStubOpts{
		head: "feature", originHead: "origin/main",
	})
	w := httptest.NewRecorder()
	handleAction(w, actionReq("reb-lease0", "rebase", false, "secret", "127.0.0.1"), db, cfg, time.Now())
	if got := decodeActionJSON(t, w); !got.OK {
		t.Fatalf("resp = %+v", got)
	}
	last := gitLog(t, logPath)[len(gitLog(t, logPath))-1]
	if last != "push origin feature" {
		t.Fatalf("push argv = %q, want plain push origin feature", last)
	}
	for _, l := range gitLog(t, logPath) {
		if strings.Contains(l, "force") {
			t.Fatalf("forced a push with no lease: %q", l)
		}
	}
}

// A remote-tracking ref that resolves to something that is not a commit id is a
// repo we do not understand; refuse the whole action rather than build an argv
// out of it.
func TestHandleActionRebaseNonSHALeaseRefuses(t *testing.T) {
	db := openTestDB(t)
	_, logPath, cfg := rebaseFixture(t, db, "reb-lease1", gitStubOpts{
		head: "feature", originHead: "origin/main", lease: "--upload-pack=evil",
	})
	w := httptest.NewRecorder()
	handleAction(w, actionReq("reb-lease1", "rebase", false, "secret", "127.0.0.1"), db, cfg, time.Now())
	got := decodeActionJSON(t, w)
	if got.OK || !strings.Contains(got.Output, "not a commit id") {
		t.Fatalf("resp = %+v", got)
	}
	for _, l := range gitLog(t, logPath) {
		if strings.HasPrefix(l, "fetch") || strings.HasPrefix(l, "pull") || strings.HasPrefix(l, "push") {
			t.Fatalf("ran %q with an unusable lease", l)
		}
	}
}

// Neither origin/HEAD nor either conventional ref resolves: nothing to rebase onto.
func TestHandleActionRebaseUnresolvableBase(t *testing.T) {
	db := openTestDB(t)
	_, logPath, cfg := rebaseFixture(t, db, "reb-2", gitStubOpts{head: "feature"})

	w := httptest.NewRecorder()
	handleAction(w, actionReq("reb-2", "rebase", false, "secret", "127.0.0.1"), db, cfg, time.Now())
	got := decodeActionJSON(t, w)
	if got.OK || !strings.Contains(got.Output, "could not resolve the default branch") {
		t.Fatalf("resp = %+v", got)
	}
	for _, l := range gitLog(t, logPath) {
		if strings.HasPrefix(l, "fetch") || strings.HasPrefix(l, "pull") || strings.HasPrefix(l, "push") {
			t.Fatalf("ran %q with no resolvable base", l)
		}
	}
}

// Both conventional refs exist and origin/HEAD does not: guessing is exactly the
// bug (a stale origin/main in a develop-default repo), so refuse and say so —
// and say it differently from "could not resolve".
func TestHandleActionRebaseAmbiguousBaseRefuses(t *testing.T) {
	db := openTestDB(t)
	_, logPath, cfg := rebaseFixture(t, db, "reb-amb", gitStubOpts{
		head: "feature", refs: []string{"refs/remotes/origin/main", "refs/remotes/origin/master"},
	})
	w := httptest.NewRecorder()
	handleAction(w, actionReq("reb-amb", "rebase", false, "secret", "127.0.0.1"), db, cfg, time.Now())
	got := decodeActionJSON(t, w)
	if got.OK || !strings.Contains(got.Output, "refusing to guess the default branch") {
		t.Fatalf("resp = %+v", got)
	}
	if strings.Contains(got.Output, "could not resolve") {
		t.Fatalf("ambiguity reported as non-resolution: %q", got.Output)
	}
	for _, l := range gitLog(t, logPath) {
		if strings.HasPrefix(l, "fetch") || strings.HasPrefix(l, "pull") || strings.HasPrefix(l, "push") {
			t.Fatalf("ran %q with an ambiguous base", l)
		}
	}
}

// A base that resolved but failed validation is a DIFFERENT failure from one
// that never resolved, and the row must not blame detection for it.
func TestHandleActionRebaseRejectedBaseNameIsDistinct(t *testing.T) {
	db := openTestDB(t)
	_, logPath, cfg := rebaseFixture(t, db, "reb-bad", gitStubOpts{
		head: "feature", originHead: "origin/héllo",
	})
	w := httptest.NewRecorder()
	handleAction(w, actionReq("reb-bad", "rebase", false, "secret", "127.0.0.1"), db, cfg, time.Now())
	got := decodeActionJSON(t, w)
	if got.OK || !strings.Contains(got.Output, "not a name we will put on a command line") {
		t.Fatalf("resp = %+v", got)
	}
	if strings.Contains(got.Output, "could not resolve") {
		t.Fatalf("rejection reported as non-resolution: %q", got.Output)
	}
	if len(gitLog(t, logPath)) != 1 {
		t.Fatalf("ran more than the one probe: %v", gitLog(t, logPath))
	}
}

// A repo whose HEAD is the default branch must never be force-pushed.
func TestHandleActionRebaseRefusesOnDefaultBranch(t *testing.T) {
	db := openTestDB(t)
	_, logPath, cfg := rebaseFixture(t, db, "reb-3", gitStubOpts{head: "main", originHead: "origin/main"})

	w := httptest.NewRecorder()
	handleAction(w, actionReq("reb-3", "rebase", false, "secret", "127.0.0.1"), db, cfg, time.Now())
	got := decodeActionJSON(t, w)
	if got.OK || !strings.Contains(got.Output, "refusing to rebase and force-push") {
		t.Fatalf("resp = %+v", got)
	}
	for _, l := range gitLog(t, logPath) {
		if !strings.HasPrefix(l, "symbolic-ref") {
			t.Fatalf("ran %q while on the default branch", l)
		}
	}
}

// The hardcoded backstop: main/master are never rewritten from here even when
// detection resolved some OTHER branch as the base (a stale or wrong origin/HEAD).
func TestHandleActionRebaseRefusesMainMasterRegardlessOfBase(t *testing.T) {
	for _, cur := range []string{"main", "master"} {
		t.Run(cur, func(t *testing.T) {
			db := openTestDB(t)
			_, logPath, cfg := rebaseFixture(t, db, "reb-bs-"+cur, gitStubOpts{
				head: cur, originHead: "origin/develop",
			})
			w := httptest.NewRecorder()
			handleAction(w, actionReq("reb-bs-"+cur, "rebase", false, "secret", "127.0.0.1"), db, cfg, time.Now())
			got := decodeActionJSON(t, w)
			if got.OK || !strings.Contains(got.Output, "worktree is on "+cur) {
				t.Fatalf("resp = %+v", got)
			}
			for _, l := range gitLog(t, logPath) {
				if !strings.HasPrefix(l, "symbolic-ref") {
					t.Fatalf("ran %q while on %s", l, cur)
				}
			}
		})
	}
}

// A detached (or mid-rebase) work tree has no branch to push, so nothing runs.
func TestHandleActionRebaseRefusesDetachedHead(t *testing.T) {
	db := openTestDB(t)
	_, logPath, cfg := rebaseFixture(t, db, "reb-4", gitStubOpts{originHead: "origin/main"})

	w := httptest.NewRecorder()
	handleAction(w, actionReq("reb-4", "rebase", false, "secret", "127.0.0.1"), db, cfg, time.Now())
	got := decodeActionJSON(t, w)
	if got.OK || !strings.Contains(got.Output, "not on a named branch") {
		t.Fatalf("resp = %+v", got)
	}
	for _, l := range gitLog(t, logPath) {
		if !strings.HasPrefix(l, "symbolic-ref") {
			t.Fatalf("ran %q on a detached HEAD", l)
		}
	}
}

// A conflicting rebase must abort (leaving the worktree clean), report "needs
// manual rebase", and NEVER reach the force-push.
func TestHandleActionRebaseConflictAbortsAndDoesNotPush(t *testing.T) {
	db := openTestDB(t)
	_, logPath, cfg := rebaseFixture(t, db, "reb-5", gitStubOpts{
		head: "feature", originHead: "origin/main", lease: "abc1234", failStep: "pull",
	})

	w := httptest.NewRecorder()
	handleAction(w, actionReq("reb-5", "rebase", false, "secret", "127.0.0.1"), db, cfg, time.Now())
	got := decodeActionJSON(t, w)
	if got.OK {
		t.Fatalf("conflict reported ok: %+v", got)
	}
	if !strings.Contains(got.Output, "needs manual rebase") {
		t.Fatalf("output lacks the manual-rebase hint: %q", got.Output)
	}
	want := []string{
		"symbolic-ref --quiet --short refs/remotes/origin/HEAD",
		"symbolic-ref --quiet --short HEAD",
		"rev-parse --verify --quiet refs/remotes/origin/feature",
		"fetch origin main",
		"pull --rebase origin main",
		"rebase --abort",
	}
	if !reflect.DeepEqual(gitLog(t, logPath), want) {
		t.Fatalf("git calls = %v, want %v", gitLog(t, logPath), want)
	}
	if queryLatestEventType(t, db, "reb-5") == "SessionEnd" {
		t.Fatal("a failed rebase must not end the session")
	}
}

// If the abort ITSELF fails the worktree really is left mid-rebase, and the row
// must say so instead of claiming a clean abort.
func TestHandleActionRebaseFailedAbortReportedHonestly(t *testing.T) {
	db := openTestDB(t)
	_, _, cfg := rebaseFixture(t, db, "reb-abfail", gitStubOpts{
		head: "feature", originHead: "origin/main", failStep: "pull", abortFail: true,
	})
	w := httptest.NewRecorder()
	handleAction(w, actionReq("reb-abfail", "rebase", false, "secret", "127.0.0.1"), db, cfg, time.Now())
	got := decodeActionJSON(t, w)
	if got.OK {
		t.Fatalf("resp = %+v", got)
	}
	if !strings.Contains(got.Output, "may be left mid-rebase") {
		t.Fatalf("failed abort not reported: %q", got.Output)
	}
	if strings.Contains(got.Output, "was aborted — needs manual rebase") {
		t.Fatalf("claimed a clean abort that failed: %q", got.Output)
	}
}

// A failed fetch stops the sequence before any history is touched, and needs no
// abort (no rebase was started).
func TestHandleActionRebaseFetchFailureStopsEarly(t *testing.T) {
	db := openTestDB(t)
	_, logPath, cfg := rebaseFixture(t, db, "reb-6", gitStubOpts{
		head: "feature", originHead: "origin/main", failStep: "fetch",
	})

	w := httptest.NewRecorder()
	handleAction(w, actionReq("reb-6", "rebase", false, "secret", "127.0.0.1"), db, cfg, time.Now())
	if got := decodeActionJSON(t, w); got.OK {
		t.Fatalf("resp = %+v", got)
	}
	for _, l := range gitLog(t, logPath) {
		if strings.HasPrefix(l, "push") || strings.HasPrefix(l, "rebase") {
			t.Fatalf("ran %q after a failed fetch", l)
		}
	}
}

// The sequence budget bounds the WHOLE rebase, not each step: once it is spent
// the remaining steps are not started, and while it is running it caps each
// step's own timeout (min(rebaseTimeout, remaining)) so three long steps plus an
// abort cannot pin the handler for the sum of their individual ceilings.
func TestRunRebaseOverallDeadline(t *testing.T) {
	t.Run("exhausted budget starts nothing", func(t *testing.T) {
		logPath := filepath.Join(t.TempDir(), "git.log")
		git := gitStub(t, logPath, gitStubOpts{head: "feature", originHead: "origin/main"})
		res := runRebase(git, t.TempDir(), "s1", 30*time.Second, 0, time.Now())
		if !res.TimedOut || res.ExitCode == 0 {
			t.Fatalf("res = %+v", res)
		}
		if !strings.Contains(res.Output, "overall budget") {
			t.Fatalf("output does not name the budget: %q", res.Output)
		}
		for _, l := range gitLog(t, logPath) {
			if strings.HasPrefix(l, "fetch") || strings.HasPrefix(l, "pull") || strings.HasPrefix(l, "push") {
				t.Fatalf("ran %q with no budget left", l)
			}
		}
	})
	t.Run("remaining budget caps a step", func(t *testing.T) {
		logPath := filepath.Join(t.TempDir(), "git.log")
		git := gitStub(t, logPath, gitStubOpts{
			head: "feature", originHead: "origin/main", sleep: "10",
		})
		// A generous per-step timeout with a tiny sequence budget: the step must
		// be killed by the REMAINING budget, not wait out its own ceiling.
		start := time.Now()
		res := runRebase(git, t.TempDir(), "s1", 30*time.Second, 200*time.Millisecond, time.Now())
		if elapsed := time.Since(start); elapsed > 5*time.Second {
			t.Fatalf("sequence ran %s; the per-step timeout was not clamped to the budget", elapsed)
		}
		if !res.TimedOut {
			t.Fatalf("res = %+v", res)
		}
		for _, l := range gitLog(t, logPath) {
			if strings.HasPrefix(l, "pull") || strings.HasPrefix(l, "push") {
				t.Fatalf("ran %q after the budget was spent", l)
			}
		}
	})
}

// A step that outlives the per-step budget is still a timeout, and the pull step
// still gets its abort.
func TestRunRebaseStepTimeoutStillAborts(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "git.log")
	git := gitStub(t, logPath, gitStubOpts{
		head: "feature", originHead: "origin/main", sleep: "5",
	})
	// Big overall budget, tiny per-step budget: the fetch is killed, not the
	// sequence. (fetch has no abort; assert the sequence stops.)
	res := runRebase(git, t.TempDir(), "s1", 150*time.Millisecond, 30*time.Second, time.Now())
	if !res.TimedOut {
		t.Fatalf("res = %+v", res)
	}
	for _, l := range gitLog(t, logPath) {
		if strings.HasPrefix(l, "pull") || strings.HasPrefix(l, "push") {
			t.Fatalf("ran %q after a timed-out fetch", l)
		}
	}
}

func TestHandleActionRebaseConcurrent409(t *testing.T) {
	db := openTestDB(t)
	_, logPath, cfg := rebaseFixture(t, db, "reb-7", gitStubOpts{head: "feature", originHead: "origin/main"})
	cfg.inflight.acquire("reb-7")
	w := httptest.NewRecorder()
	handleAction(w, actionReq("reb-7", "rebase", false, "secret", "127.0.0.1"), db, cfg, time.Now())
	if w.Code != http.StatusConflict {
		t.Fatalf("code = %d, want 409", w.Code)
	}
	if len(gitLog(t, logPath)) != 0 {
		t.Fatalf("git ran while another action held the row: %v", gitLog(t, logPath))
	}
}

// Two DIFFERENT session rows can resolve to the same worktree (a resumed agent, a
// shared checkout). The second must 409 on the cwd lock rather than run a second
// git sequence in the same directory.
func TestHandleActionConcurrentSameCwd409(t *testing.T) {
	db := openTestDB(t)
	dir, logPath, cfg := rebaseFixture(t, db, "sess-a", gitStubOpts{head: "feature", originHead: "origin/main"})
	// A second, distinct session pointing at the SAME cwd.
	insertEvent(db, Event{TS: "2026-08-12T10:01:00Z", SourceApp: "x", SessionID: "sess-b", Cwd: dir, EventType: "Stop", PayloadJSON: "{}"})

	// sess-a's rebase is in flight: its session key AND its worktree key are held.
	cfg.inflight.acquire("sess-a")
	cfg.inflight.acquire("cwd:" + dir)

	w := httptest.NewRecorder()
	handleAction(w, actionReq("sess-b", "rebase", false, "secret", "127.0.0.1"), db, cfg, time.Now())
	if w.Code != http.StatusConflict {
		t.Fatalf("code = %d, want 409 (body %s)", w.Code, w.Body.String())
	}
	if len(gitLog(t, logPath)) != 0 {
		t.Fatalf("git ran in a worktree another action holds: %v", gitLog(t, logPath))
	}
	// The session lock taken on the way to the cwd check must not leak.
	cfg.inflight.release("cwd:" + dir)
	if !cfg.inflight.acquire("sess-b") {
		t.Fatal("sess-b's session lock leaked after the 409")
	}
}

// Every rebase attempt — including the ones that refuse — leaves an audit line,
// ungated by CLODHOPPER_DEBUG.
func TestRunRebaseAudits(t *testing.T) {
	var buf bytes.Buffer
	old := auditw
	auditw = &buf
	t.Cleanup(func() { auditw = old })

	dir := t.TempDir()
	logPath := filepath.Join(t.TempDir(), "git.log")
	git := gitStub(t, logPath, gitStubOpts{head: "feature", originHead: "origin/main", lease: "abc1234"})
	now := time.Date(2026, 8, 21, 9, 30, 0, 0, time.UTC)
	if res := runRebase(git, dir, "sess-x", 10*time.Second, 30*time.Second, now); res.ExitCode != 0 {
		t.Fatalf("res = %+v", res)
	}
	line := buf.String()
	for _, want := range []string{"audit rebase", "2026-08-21T09:30:00Z", `session="sess-x"`,
		`cwd="` + dir + `"`, `base="main"`, `cur="feature"`, `lease="abc1234"`, `outcome="pushed"`} {
		if !strings.Contains(line, want) {
			t.Errorf("audit line %q lacks %q", line, want)
		}
	}

	buf.Reset()
	git = gitStub(t, logPath, gitStubOpts{head: "main", originHead: "origin/main"})
	runRebase(git, dir, "sess-y", 10*time.Second, 30*time.Second, now)
	if !strings.Contains(buf.String(), `outcome="refused`) {
		t.Fatalf("refusal not audited: %q", buf.String())
	}
}

// The rebase action sits behind exactly the same guard chain as every other
// write action; none of them may reach git.
func TestHandleActionRebaseGuards(t *testing.T) {
	db := openTestDB(t)
	dir := t.TempDir()
	insertEvent(db, Event{TS: "2026-08-12T10:00:00Z", SourceApp: "x", SessionID: "reb-8", Cwd: dir, EventType: "Stop", PayloadJSON: "{}"})
	logPath := filepath.Join(t.TempDir(), "git.log")
	stub := gitStub(t, logPath, gitStubOpts{head: "feature", originHead: "origin/main"})

	// disabled
	cfg := newRebaseCfg(stub)
	cfg.enabled = false
	w := httptest.NewRecorder()
	handleAction(w, actionReq("reb-8", "rebase", false, "secret", "127.0.0.1"), db, cfg, time.Now())
	if w.Code != http.StatusForbidden {
		t.Fatalf("disabled: %d", w.Code)
	}
	// GET
	cfg = newRebaseCfg(stub)
	w = httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/api/action?action=rebase", nil)
	r.Host = "127.0.0.1"
	handleAction(w, r, db, cfg, time.Now())
	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("method: %d", w.Code)
	}
	// bad host
	w = httptest.NewRecorder()
	handleAction(w, actionReq("reb-8", "rebase", false, "secret", "evil.example.com"), db, cfg, time.Now())
	if w.Code != http.StatusForbidden {
		t.Fatalf("host: %d", w.Code)
	}
	// bad token
	w = httptest.NewRecorder()
	handleAction(w, actionReq("reb-8", "rebase", false, "nope", "127.0.0.1"), db, cfg, time.Now())
	if w.Code != http.StatusForbidden {
		t.Fatalf("token: %d", w.Code)
	}
	// unknown session
	w = httptest.NewRecorder()
	handleAction(w, actionReq("ghost", "rebase", false, "secret", "127.0.0.1"), db, cfg, time.Now())
	if w.Code != http.StatusNotFound {
		t.Fatalf("session: %d", w.Code)
	}
	if len(gitLog(t, logPath)) != 0 {
		t.Fatalf("git ran despite a rejected request: %v", gitLog(t, logPath))
	}
}

// nonRepoDir returns a directory where git resolves no repository. A plain
// t.TempDir() is not enough: TMPDIR can itself sit inside a checkout (it does in
// this project's worktree layout), and git would walk up and find that one. An
// unparseable .git file stops the search right here, which is exactly the
// "git says no" state these probes must fail closed on.
func nonRepoDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ".git"), []byte("not a gitfile\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

// runGit runs a real git command during test setup, failing the test on error.
func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	full := append([]string{"-C", dir,
		"-c", "user.name=t", "-c", "user.email=t@example.com",
		"-c", "init.defaultBranch=work", "-c", "commit.gpgsign=false"}, args...)
	if out, err := exec.Command("git", full...).CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

// defaultBranch against REAL repos: origin/HEAD, the two single-candidate
// fallbacks, the ambiguous both-exist case, a repo with no remote refs, and a
// non-repo.
func TestDefaultBranchRealRepos(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	newRepo := func(t *testing.T) string {
		t.Helper()
		dir := t.TempDir()
		runGit(t, dir, "init", "--quiet")
		runGit(t, dir, "commit", "--allow-empty", "--quiet", "-m", "seed")
		return dir
	}
	mustResolve := func(t *testing.T, dir, want string) {
		t.Helper()
		got, err := defaultBranch("git", dir)
		if got != want || err != nil {
			t.Fatalf("defaultBranch = %q, %v; want %q, nil", got, err, want)
		}
	}

	t.Run("origin/HEAD", func(t *testing.T) {
		dir := newRepo(t)
		runGit(t, dir, "update-ref", "refs/remotes/origin/main", "HEAD")
		runGit(t, dir, "symbolic-ref", "refs/remotes/origin/HEAD", "refs/remotes/origin/main")
		mustResolve(t, dir, "main")
	})
	// origin/HEAD wins even when both conventional refs exist — it is the
	// authoritative answer, so there is nothing ambiguous to refuse.
	t.Run("origin/HEAD beats an ambiguous fallback", func(t *testing.T) {
		dir := newRepo(t)
		runGit(t, dir, "update-ref", "refs/remotes/origin/main", "HEAD")
		runGit(t, dir, "update-ref", "refs/remotes/origin/master", "HEAD")
		runGit(t, dir, "update-ref", "refs/remotes/origin/develop", "HEAD")
		runGit(t, dir, "symbolic-ref", "refs/remotes/origin/HEAD", "refs/remotes/origin/develop")
		mustResolve(t, dir, "develop")
	})
	// An origin/HEAD pointing OUTSIDE refs/remotes/origin/ is not a remote branch
	// name; it must not be mistaken for one.
	t.Run("origin/HEAD outside refs/remotes/origin", func(t *testing.T) {
		dir := newRepo(t)
		runGit(t, dir, "symbolic-ref", "refs/remotes/origin/HEAD", "refs/heads/work")
		got, err := defaultBranch("git", dir)
		if got != "" || !errors.Is(err, errNoDefaultBranch) {
			t.Fatalf("defaultBranch = %q, %v", got, err)
		}
	})
	t.Run("main fallback", func(t *testing.T) {
		dir := newRepo(t)
		runGit(t, dir, "update-ref", "refs/remotes/origin/main", "HEAD")
		mustResolve(t, dir, "main")
	})
	t.Run("master fallback", func(t *testing.T) {
		dir := newRepo(t)
		runGit(t, dir, "update-ref", "refs/remotes/origin/master", "HEAD")
		mustResolve(t, dir, "master")
	})
	// Both present and no origin/HEAD: the answer is a guess, and guessing wrong
	// is what lets the default branch get force-pushed. Refuse instead.
	t.Run("ambiguous main and master", func(t *testing.T) {
		dir := newRepo(t)
		runGit(t, dir, "update-ref", "refs/remotes/origin/main", "HEAD")
		runGit(t, dir, "update-ref", "refs/remotes/origin/master", "HEAD")
		got, err := defaultBranch("git", dir)
		if got != "" || !errors.Is(err, errAmbiguousDefaultBranch) {
			t.Fatalf("defaultBranch = %q, %v; want ambiguity", got, err)
		}
	})
	t.Run("unresolvable", func(t *testing.T) {
		dir := newRepo(t)
		if got, err := defaultBranch("git", dir); got != "" || !errors.Is(err, errNoDefaultBranch) {
			t.Fatalf("defaultBranch = %q, %v", got, err)
		}
	})
	t.Run("not a repo", func(t *testing.T) {
		if got, err := defaultBranch("git", nonRepoDir(t)); got != "" || err == nil {
			t.Fatalf("defaultBranch = %q, %v", got, err)
		}
		if got, err := defaultBranch("git", ""); got != "" || err == nil {
			t.Fatalf("empty dir: defaultBranch = %q, %v", got, err)
		}
	})
}

// worktreeBranch reports the checked-out branch and nothing at all when HEAD is
// detached — the state the rebase guard refuses on.
func TestWorktreeBranchRealRepo(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	dir := t.TempDir()
	runGit(t, dir, "init", "--quiet")
	runGit(t, dir, "commit", "--allow-empty", "--quiet", "-m", "seed")
	if got := worktreeBranch("git", dir); got != "work" {
		t.Fatalf("worktreeBranch = %q, want work", got)
	}
	runGit(t, dir, "checkout", "--quiet", "--detach", "HEAD")
	if got := worktreeBranch("git", dir); got != "" {
		t.Fatalf("detached: worktreeBranch = %q, want empty", got)
	}
	if got := worktreeBranch("git", nonRepoDir(t)); got != "" {
		t.Fatalf("non-repo: worktreeBranch = %q", got)
	}
}
