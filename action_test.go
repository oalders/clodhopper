package main

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestActionArgv(t *testing.T) {
	cases := []struct {
		action string
		force  bool
		binary string
		args   []string
		inTmux bool
		ok     bool
	}{
		{"squash", false, "merge-pr", []string{"--squash"}, true, true},
		{"squash", true, "merge-pr", []string{"--squash", "--force"}, true, true},
		{"squash-admin", false, "merge-pr", []string{"--squash", "--admin"}, true, true},
		{"squash-admin", true, "merge-pr", []string{"--squash", "--admin", "--force"}, true, true},
		{"close", false, "merge-pr", []string{"--close"}, true, true},
		{"close", true, "merge-pr", []string{"--close", "--force"}, true, true},
		{"ready", false, "gh", []string{"pr", "ready"}, false, true},
		{"ready", true, "gh", []string{"pr", "ready"}, false, true},    // force ignored
		{"teardown", true, "merge-pr", []string{"--punt"}, true, true}, // force ignored
		{"", false, "", nil, false, false},
		{"squash; rm -rf /", false, "", nil, false, false},
		{"rebase; rm -rf /", false, "", nil, false, false},
		{"REBASE", false, "", nil, false, false},
		{"--admin", false, "", nil, false, false},
	}
	for _, c := range cases {
		b, a, td, ok := actionArgv(c.action, c.force)
		if b != c.binary || td != c.inTmux || ok != c.ok || !reflect.DeepEqual(a, c.args) {
			t.Errorf("actionArgv(%q,%v) = (%q,%v,%v,%v), want (%q,%v,%v,%v)",
				c.action, c.force, b, a, td, ok, c.binary, c.args, c.inTmux, c.ok)
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

// actionReq builds a well-formed action POST from a LOOPBACK peer, which is
// what remoteAllowed requires for every action that execs. httptest's default
// RemoteAddr (192.0.2.1) is a public TEST-NET address, so it has to be set
// explicitly or every exec-backed action would 403 here. Use actionReqFrom for
// a test that wants a different peer.
func actionReq(sessionID, action string, force bool, token, host string) *http.Request {
	return actionReqFrom("127.0.0.1:54321", sessionID, action, force, token, host)
}

// actionReqFrom is actionReq with the peer address spelled out.
func actionReqFrom(remoteAddr, sessionID, action string, force bool, token, host string) *http.Request {
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
	r.RemoteAddr = remoteAddr
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

// mergeTmuxStub is a fake tmux that logs each invocation's argv (one line,
// space-joined) and answers display-message with sid, the session id a merge
// button's new-window then targets.
func mergeTmuxStub(t *testing.T, sid string) (bin, logPath string) {
	t.Helper()
	logPath = filepath.Join(t.TempDir(), "tmux.log")
	bin = writeStub(t, "tmux", `echo "$@" >> `+logPath+`
if [ "$1" = "display-message" ]; then echo '`+sid+`'; fi
exit 0`)
	return bin, logPath
}

// Regression for #188: a merge button must not run merge-pr as a subprocess
// serve would time out and kill mid merge-queue wait. It starts merge-pr in a
// new window of the row's own tmux session and returns at once, and the row is
// left to drop when merge-pr kills that session (no synthetic SessionEnd).
func TestHandleActionMergeStartsTmuxWindow(t *testing.T) {
	cases := []struct {
		action string
		force  bool
		args   string
	}{
		{"squash", false, "--squash"},
		{"close", true, "--close --force"},
	}
	for _, c := range cases {
		t.Run(c.action, func(t *testing.T) {
			db := openTestDB(t)
			dir := t.TempDir()
			insertEvent(db, Event{TS: "2026-08-12T10:00:00Z", SourceApp: "x", SessionID: "live-1", Cwd: dir, TmuxPane: "%12", EventType: "Stop", PayloadJSON: "{}"})
			mergePR := writeStub(t, "merge-pr", "exit 0")
			stub, logPath := mergeTmuxStub(t, "$4")
			cfg := newActionCfg(mergePR, "/bin/true")
			cfg.tmux = stub

			w := httptest.NewRecorder()
			handleAction(w, actionReq("live-1", c.action, c.force, "secret", "127.0.0.1"), db, cfg, time.Date(2026, 8, 12, 10, 5, 0, 0, time.UTC))
			if got := decodeActionJSON(t, w); !got.OK || got.Output != mergeStarted {
				t.Fatalf("resp = %+v", got)
			}
			got, _ := os.ReadFile(logPath)
			want := "display-message -p -t %12 #{session_id}\n" +
				"new-window -a -d -t $4:{end} -c " + dir + " -n merge-pr " + mergePR + " " + c.args +
				" ; set-option -p -t $4:{end} remain-on-exit failed\n"
			if string(got) != want {
				t.Fatalf("argv = %q, want %q", got, want)
			}
			if et := queryLatestEventType(t, db, "live-1"); et == "SessionEnd" {
				t.Fatal("starting a merge must not end the session")
			}
		})
	}
}

// teardown (#234) starts merge-pr --punt only for a ledger row whose live pane
// is still in tmux and whose CI says the snapshot's own PR merged or closed.
func TestHandleActionTeardown(t *testing.T) {
	now := time.Date(2026, 8, 12, 10, 5, 0, 0, time.UTC)
	merged := ciResult{Done: "merged", PRRepo: "o/r", PRNumber: 7}
	cases := []struct {
		name    string
		ledger  bool
		pane    string
		session string // the live pane's tmux session
		title   string // the live pane's title
		snap    string // the ledger row's TmuxSession, as dropDeadPanes labelled it
		ci      ciResult
		code    int
		starts  bool
		refusal string // the 200 refusal's text when nothing starts
	}{
		{"merged", true, "%12", "s", "", "s", merged, http.StatusOK, true, ""},
		{"session labelled by its title", true, "%12", "fix/b-live-1", "Fix it", "Fix it", merged, http.StatusOK, true, ""},
		{"pane has no session", true, "%12", "", "", "", merged, http.StatusOK, false, "belongs to another tmux session"},
		{"closed", true, "%12", "s", "", "s", ciResult{Done: "closed", PRRepo: "o/r", PRNumber: 7}, http.StatusOK, true, ""},
		{"no ledger entry", false, "%12", "s", "", "s", merged, http.StatusNotFound, false, ""},
		{"pane not live", true, "%99", "s", "", "s", merged, http.StatusOK, false, "not in tmux"},
		{"pane reused by another session", true, "%12", "other", "", "s", merged, http.StatusOK, false, "belongs to another tmux session"},
		{"PR still open", true, "%12", "s", "", "s", ciResult{State: "passing", PRRepo: "o/r", PRNumber: 7}, http.StatusConflict, false, ""},
		{"another PR merged", true, "%12", "s", "", "s", ciResult{Done: "merged", PRRepo: "o/r", PRNumber: 8}, http.StatusConflict, false, ""},
		{"same number merged in another repo", true, "%12", "s", "", "s", ciResult{Done: "merged", PRRepo: "o/other", PRNumber: 7}, http.StatusConflict, false, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			db := openTestDB(t)
			dir := t.TempDir()
			insertEvent(db, Event{TS: "2026-08-12T10:00:00Z", SourceApp: "x", SessionID: "live-1", Cwd: dir, TmuxPane: "%12", EventType: "Stop", PayloadJSON: "{}"})
			mergePR := writeStub(t, "merge-pr", "exit 0")
			stub, logPath := mergeTmuxStub(t, "$4")
			cfg := newActionCfg(mergePR, "/bin/true")
			cfg.tmux = stub
			cfg.ledger = newQueueLedger()
			if c.ledger {
				q := queuedRow("live-1")
				q.TmuxSession = c.snap
				cfg.ledger.record([]Agent{q}, now)
			}
			cfg.panes = &paneCache{set: map[string]livePane{c.pane: {session: c.session, title: c.title}}, at: now}
			cfg.ci = &cwdCache[ciResult]{m: map[string]cwdEntry[ciResult]{dir: {val: c.ci, at: now}}}

			w := httptest.NewRecorder()
			handleAction(w, actionReq("live-1", "teardown", true, "secret", "127.0.0.1"), db, cfg, now)
			if w.Code != c.code {
				t.Fatalf("code = %d %q, want %d", w.Code, w.Body.String(), c.code)
			}
			got, _ := os.ReadFile(logPath)
			if !c.starts {
				if c.code == http.StatusOK && !strings.Contains(decodeActionJSON(t, w).Output, c.refusal) {
					t.Fatalf("body = %q, want %q", w.Body.String(), c.refusal)
				}
				if len(got) != 0 {
					t.Fatalf("tmux ran: %q", got)
				}
				return
			}
			want := "display-message -p -t %12 #{session_id}\n" +
				"new-window -a -d -t $4:{end} -c " + dir + " -n merge-pr " + mergePR + " --punt" +
				" ; set-option -p -t $4:{end} remain-on-exit failed\n"
			if string(got) != want {
				t.Fatalf("argv = %q, want %q", got, want)
			}
			// merge-pr kills the session; teardown itself leaves the ledger row.
			if _, ok := cfg.ledger.snapshot("live-1"); !ok {
				t.Fatal("teardown dropped the ledger entry")
			}
		})
	}
}

// A row with no usable pane has no session to run merge-pr in: the button
// reports that and nothing runs.
func TestHandleActionMergeNeedsPane(t *testing.T) {
	db := openTestDB(t)
	dir := t.TempDir()
	insertEvent(db, Event{TS: "2026-08-12T10:00:00Z", SourceApp: "x", SessionID: "nopane", Cwd: dir, EventType: "Stop", PayloadJSON: "{}"})
	insertEvent(db, Event{TS: "2026-08-12T10:00:00Z", SourceApp: "x", SessionID: "badpane", Cwd: dir, TmuxPane: "; rm -rf /", EventType: "Stop", PayloadJSON: "{}"})
	logPath := filepath.Join(t.TempDir(), "ran.log")
	cfg := newActionCfg(logStub(t, "merge-pr", logPath), "/bin/true")
	cfg.tmux = logStub(t, "tmux", logPath)
	for _, sess := range []string{"nopane", "badpane"} {
		w := httptest.NewRecorder()
		handleAction(w, actionReq(sess, "squash", false, "secret", "127.0.0.1"), db, cfg, time.Now())
		if got := decodeActionJSON(t, w); got.OK || !strings.Contains(got.Output, "not in tmux") {
			t.Fatalf("%s: resp = %+v", sess, got)
		}
	}
	if _, err := os.Stat(logPath); err == nil {
		t.Fatal("tmux or merge-pr ran for a session with no valid pane")
	}
}

// The session id display-message prints becomes part of new-window's target, so
// anything that is not a tmux session id refuses before new-window runs.
func TestHandleActionMergeRejectsBadSessionID(t *testing.T) {
	for _, sid := range []string{"", "garbage", "$1;kill-server", "%3"} {
		t.Run(sid, func(t *testing.T) {
			db := openTestDB(t)
			insertEvent(db, Event{TS: "2026-08-12T10:00:00Z", SourceApp: "x", SessionID: "live-1", Cwd: t.TempDir(), TmuxPane: "%12", EventType: "Stop", PayloadJSON: "{}"})
			stub, logPath := mergeTmuxStub(t, sid)
			cfg := newActionCfg(writeStub(t, "merge-pr", "exit 0"), "/bin/true")
			cfg.tmux = stub
			w := httptest.NewRecorder()
			handleAction(w, actionReq("live-1", "squash", false, "secret", "127.0.0.1"), db, cfg, time.Now())
			if got := decodeActionJSON(t, w); got.OK {
				t.Fatalf("resp = %+v, want a refusal", got)
			}
			got, _ := os.ReadFile(logPath)
			if strings.Contains(string(got), "new-window") {
				t.Fatalf("new-window ran with session id %q: %q", sid, got)
			}
		})
	}
}

// tmux format-expands new-window's -c, so a cwd with "#(...)" in it would run
// a shell command inside tmux; it is refused before tmux is touched.
func TestHandleActionMergeRefusesFormatInCwd(t *testing.T) {
	db := openTestDB(t)
	dir := filepath.Join(t.TempDir(), "#(touch pwned)")
	insertEvent(db, Event{TS: "2026-08-12T10:00:00Z", SourceApp: "x", SessionID: "live-1", Cwd: dir, TmuxPane: "%12", EventType: "Stop", PayloadJSON: "{}"})
	logPath := filepath.Join(t.TempDir(), "ran.log")
	cfg := newActionCfg(logStub(t, "merge-pr", logPath), "/bin/true")
	cfg.tmux = logStub(t, "tmux", logPath)
	w := httptest.NewRecorder()
	handleAction(w, actionReq("live-1", "squash", false, "secret", "127.0.0.1"), db, cfg, time.Now())
	if got := decodeActionJSON(t, w); got.OK || !strings.Contains(got.Output, "'#'") {
		t.Fatalf("resp = %+v", got)
	}
	if _, err := os.Stat(logPath); err == nil {
		t.Fatal("tmux ran for a cwd tmux would format-expand")
	}
}

// A merge-pr that cannot be found is reported before tmux is touched, rather
// than opening a window that would only show "command not found".
func TestHandleActionMergePRNotFound(t *testing.T) {
	db := openTestDB(t)
	insertEvent(db, Event{TS: "2026-08-12T10:00:00Z", SourceApp: "x", SessionID: "live-1", Cwd: t.TempDir(), TmuxPane: "%12", EventType: "Stop", PayloadJSON: "{}"})
	logPath := filepath.Join(t.TempDir(), "ran.log")
	cfg := newActionCfg(filepath.Join(t.TempDir(), "no-such-merge-pr"), "/bin/true")
	cfg.tmux = logStub(t, "tmux", logPath)
	w := httptest.NewRecorder()
	handleAction(w, actionReq("live-1", "squash", false, "secret", "127.0.0.1"), db, cfg, time.Now())
	if got := decodeActionJSON(t, w); got.OK || !strings.Contains(got.Output, "merge-pr not found") {
		t.Fatalf("resp = %+v", got)
	}
	if _, err := os.Stat(logPath); err == nil {
		t.Fatal("tmux ran although merge-pr could not be found")
	}
}

// A tmux that cannot open the window reports its own error, not success.
func TestHandleActionMergeTmuxFailureSurfaced(t *testing.T) {
	db := openTestDB(t)
	insertEvent(db, Event{TS: "2026-08-12T10:00:00Z", SourceApp: "x", SessionID: "live-3", Cwd: t.TempDir(), TmuxPane: "%12", EventType: "Stop", PayloadJSON: "{}"})
	cfg := newActionCfg(writeStub(t, "merge-pr", "exit 0"), "/bin/true")
	cfg.tmux = writeStub(t, "tmux", `if [ "$1" = "display-message" ]; then echo '$4'; exit 0; fi
echo "create window failed" >&2; exit 1`)
	w := httptest.NewRecorder()
	handleAction(w, actionReq("live-3", "close", false, "secret", "127.0.0.1"), db, cfg, time.Now())
	if got := decodeActionJSON(t, w); got.OK || !strings.Contains(got.Output, "create window failed") {
		t.Fatalf("resp = %+v", got)
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

func TestHandleActionConcurrent409(t *testing.T) {
	db := openTestDB(t)
	dir := t.TempDir()
	insertEvent(db, Event{TS: "2026-08-12T10:00:00Z", SourceApp: "x", SessionID: "live-4", Cwd: dir, EventType: "Stop", PayloadJSON: "{}"})
	cfg := newActionCfg("/bin/true", "/bin/true")
	// Pre-acquire to simulate an in-flight action for this session.
	cfg.inflight.acquire(sidKey("live-4"))
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

// actionJSON is the JSON envelope every action handler writes.
type actionJSON struct {
	OK       bool        `json:"ok"`
	ExitCode int         `json:"exitCode"`
	TimedOut bool        `json:"timedOut"`
	Output   string      `json:"output"`
	Rows     []reportRow `json:"rows"`
}

// decodeActionJSON unpacks the JSON envelope every action handler writes.
func decodeActionJSON(t *testing.T, w *httptest.ResponseRecorder) actionJSON {
	t.Helper()
	var got actionJSON
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
	if roster, err := agentRoster(db, 16*time.Hour, 0, now); err != nil || len(roster) != 1 {
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
	roster, err := agentRoster(db, 16*time.Hour, 0, now)
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
	if !cfg.inflight.acquire(sidKey("live-1")) {
		t.Fatal("acquire failed")
	}
	defer cfg.inflight.release(sidKey("live-1"))

	w := httptest.NewRecorder()
	handleAction(w, actionReq("live-1", "end", false, "secret", "127.0.0.1"), db, cfg, now)
	if w.Code != http.StatusConflict {
		t.Fatalf("code = %d, want 409", w.Code)
	}
	if et := queryLatestEventType(t, db, "live-1"); et == "SessionEnd" {
		t.Fatal("conflicting request ended the session anyway")
	}
}

// The tmux branch takes the SAME per-session inflight lock as `end`, and the
// pattern is only as good as its weakest branch: a second monitor-ci for a
// session already running one must 409 without touching tmux, and a completed
// one must RELEASE the key it took (a mismatched release would leave the
// session wedged for the life of the process).
func TestHandleActionMonitorCIConcurrentIsConflict(t *testing.T) {
	db := openTestDB(t)
	insertEvent(db, Event{TS: "2026-08-12T10:00:00Z", SourceApp: "x", SessionID: "live-m", Cwd: t.TempDir(), TmuxPane: "%12", EventType: "Stop", PayloadJSON: "{}"})
	stub, logPath := tmuxLogStub(t)
	cfg := newActionCfg("/bin/true", "/bin/true")
	cfg.tmux = stub

	if !cfg.inflight.acquire(sidKey("live-m")) {
		t.Fatal("acquire failed")
	}
	w := httptest.NewRecorder()
	handleAction(w, actionReq("live-m", "monitor-ci", false, "secret", "127.0.0.1"), db, cfg, time.Now())
	if w.Code != http.StatusConflict {
		t.Fatalf("code = %d, want 409", w.Code)
	}
	if b, err := os.ReadFile(logPath); err == nil && len(b) > 0 {
		t.Fatalf("the conflicting request still ran tmux: %q", b)
	}
	cfg.inflight.release(sidKey("live-m"))

	// ...and the handler's own acquire/release pair must balance: the same
	// session has to be actionable again once the first run finishes.
	w = httptest.NewRecorder()
	handleAction(w, actionReq("live-m", "monitor-ci", false, "secret", "127.0.0.1"), db, cfg, time.Now())
	if w.Code != http.StatusOK {
		t.Fatalf("code = %d body=%s", w.Code, w.Body.String())
	}
	if !cfg.inflight.acquire(sidKey("live-m")) {
		t.Fatal("the tmux branch leaked its inflight key; the session is wedged")
	}
	cfg.inflight.release(sidKey("live-m"))
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

// Two DIFFERENT session rows can resolve to the same worktree (a resumed agent, a
// shared checkout). The second must 409 on the cwd lock rather than run a second
// action in the same directory.
func TestHandleActionConcurrentSameCwd409(t *testing.T) {
	db := openTestDB(t)
	dir := t.TempDir()
	insertEvent(db, Event{TS: "2026-08-12T10:00:00Z", SourceApp: "x", SessionID: "sess-a", Cwd: dir, EventType: "Stop", PayloadJSON: "{}"})
	// A second, distinct session pointing at the SAME cwd.
	insertEvent(db, Event{TS: "2026-08-12T10:01:00Z", SourceApp: "x", SessionID: "sess-b", Cwd: dir, EventType: "Stop", PayloadJSON: "{}"})
	logPath := filepath.Join(t.TempDir(), "gh.log")
	cfg := newActionCfg("/bin/true", logStub(t, "gh", logPath))

	// sess-a's action is in flight: its session key AND its worktree key are held.
	cfg.inflight.acquire(sidKey("sess-a"))
	cfg.inflight.acquire(cwdKey(dir))

	w := httptest.NewRecorder()
	handleAction(w, actionReq("sess-b", "ready", false, "secret", "127.0.0.1"), db, cfg, time.Now())
	if w.Code != http.StatusConflict {
		t.Fatalf("code = %d, want 409 (body %s)", w.Code, w.Body.String())
	}
	if got, _ := os.ReadFile(logPath); len(got) != 0 {
		t.Fatalf("gh ran in a worktree another action holds: %q", got)
	}
	// The session lock taken on the way to the cwd check must not leak.
	cfg.inflight.release(cwdKey(dir))
	if !cfg.inflight.acquire(sidKey("sess-b")) {
		t.Fatal("sess-b's session lock leaked after the 409")
	}
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

// --- network gate: remoteAllowed -------------------------------------------

func TestRemoteAllowed(t *testing.T) {
	cases := []struct {
		addr string
		want bool
	}{
		// loopback and tailnet peers may drive host commands
		{"127.0.0.1:1234", true},
		{"127.9.9.9:1234", true}, // all of 127.0.0.0/8
		{"[::1]:1234", true},
		{"100.64.0.1:1234", true},       // Tailscale CGNAT
		{"100.127.255.254:1", true},     // top of 100.64.0.0/10
		{"[fd7a:115c:a1e0::1]:1", true}, // Tailscale IPv6 ULA
		// IPv4-mapped IPv6 is classified by the IPv4 address it wraps
		{"[::ffff:127.0.0.1]:1234", true},
		{"[::ffff:192.168.1.5]:1234", false},
		// everything else is denied, LAN included
		{"192.168.1.5:1234", false},
		{"10.0.0.5:1234", false},
		{"172.16.0.5:1234", false},
		{"8.8.8.8:1234", false},
		{"[2001:db8::1]:1234", false},
		{"[fd00::1]:1234", false},   // a non-Tailscale ULA is still not the tailnet
		{"100.128.0.1:1234", false}, // just past the CGNAT block
		// a RemoteAddr with no port at all
		{"127.0.0.1", true},
		{"::1", true},
		{"192.168.1.5", false},
		// fail closed on anything unparseable
		{"", false},
		{"garbage", false},
		{"not-an-ip:1234", false},
		{"localhost:1234", false},
	}
	for _, c := range cases {
		if got := remoteAllowed(c.addr); got != c.want {
			t.Errorf("remoteAllowed(%q) = %v, want %v", c.addr, got, c.want)
		}
	}
}

// The gate keys off "does this action exec", so an action added later is
// network-gated by default rather than silently exempt.
func TestActionExecs(t *testing.T) {
	for _, a := range []string{"squash", "squash-admin", "close", "ready", "rebase", "monitor-ci", "new-monitor", "brand-new-action", ""} {
		if !actionExecs(a) {
			t.Errorf("actionExecs(%q) = false, want true", a)
		}
	}
	for _, a := range []string{"end", "block", "unblock", "dismiss"} {
		if actionExecs(a) {
			t.Errorf("actionExecs(%q) = true, want false (it runs no subprocess)", a)
		}
	}
}

// logStub is a stub binary that appends its first argument to logPath, so a
// test can assert whether it was invoked at all. It answers the two probes the
// session paths make so those paths get far enough to exec.
func logStub(t *testing.T, name, logPath string) string {
	t.Helper()
	return writeStub(t, name, `echo "$1" >> `+logPath+`
if [ "$1" = "split-window" ]; then echo "%77"; fi
exit 0`)
}

// gatedCfg wires every action binary to the same log file and seeds one session
// with a cwd and a live pane, so any exec-backed action has everything it needs
// except an allowed peer.
func gatedCfg(t *testing.T) (*sql.DB, *actionConfig, string) {
	t.Helper()
	db := openTestDB(t)
	insertEvent(db, Event{TS: "2026-08-12T10:00:00Z", SourceApp: "x", SessionID: "live-1",
		Cwd: t.TempDir(), TmuxPane: "%12", EventType: "Stop", PayloadJSON: "{}"})
	// broadcast-restart only targets a UUID session id, so seed one it will exec for.
	insertEvent(db, Event{TS: "2026-08-12T10:00:00Z", SourceApp: "x", SessionID: restartUUID,
		Cwd: t.TempDir(), TmuxPane: "%13", EventType: "Stop", PayloadJSON: "{}"})
	logPath := filepath.Join(t.TempDir(), "ran.log")
	cfg := newActionCfg(logStub(t, "merge-pr", logPath), logStub(t, "gh", logPath))
	cfg.tmux = logStub(t, "tmux", logPath)
	return db, cfg, logPath
}

// The exact hole this closes: hostAllowed validates the Host HEADER, which the
// peer controls, so a LAN host on a `--host 0.0.0.0` serve could GET the page,
// scrape the CSRF token out of the HTML, and POST with `Host: 127.0.0.1`. Only
// the peer address stops it — and nothing may exec on the way to the 403.
func TestHandleActionLANPeerBypassIsRefused(t *testing.T) {
	db, cfg, logPath := gatedCfg(t)
	w := httptest.NewRecorder()
	handleAction(w, actionReqFrom("192.168.1.5:1234", "live-1", "squash", false, "secret", "127.0.0.1"), db, cfg, time.Now())
	if w.Code != http.StatusForbidden {
		t.Fatalf("code = %d, want 403", w.Code)
	}
	if !strings.Contains(w.Body.String(), "allowed network") {
		t.Fatalf("body = %q, want the peer-network reason (not the Host-header one)", w.Body.String())
	}
	if _, err := os.Stat(logPath); err == nil {
		t.Fatal("a subprocess ran for a denied peer")
	}
}

// Every action that can run a command on the host is refused from a LAN peer
// and reaches its subprocess from loopback.
func TestHandleActionExecActionsGatedOnPeer(t *testing.T) {
	for _, action := range []string{"squash", "close", "ready", "monitor-ci", "new-monitor", "reload-plugins", "rebase", "broadcast-reload", "broadcast-restart"} {
		t.Run(action, func(t *testing.T) {
			db, cfg, logPath := gatedCfg(t)
			// Close to gatedCfg's events, so the broadcasts' fresh roster
			// still holds the seeded sessions.
			now := time.Date(2026, 8, 12, 10, 1, 0, 0, time.UTC)

			w := httptest.NewRecorder()
			handleAction(w, actionReqFrom("192.168.1.5:1234", "live-1", action, false, "secret", "127.0.0.1"), db, cfg, now)
			if w.Code != http.StatusForbidden {
				t.Fatalf("LAN peer: code = %d, want 403", w.Code)
			}
			if _, err := os.Stat(logPath); err == nil {
				t.Fatal("LAN peer: a subprocess ran")
			}

			w = httptest.NewRecorder()
			handleAction(w, actionReqFrom("127.0.0.1:54321", "live-1", action, false, "secret", "127.0.0.1"), db, cfg, now)
			if w.Code == http.StatusForbidden {
				t.Fatalf("loopback peer: code = 403, want the action to run")
			}
			if _, err := os.Stat(logPath); err != nil {
				t.Fatalf("loopback peer: no subprocess ran (%v)", err)
			}
		})
	}
}

// end is exempt: it execs nothing, it only writes a synthetic SessionEnd, so a
// peer that may not run commands can still dismiss a stale row.
func TestHandleActionEndAllowedFromLANPeer(t *testing.T) {
	db, cfg, logPath := gatedCfg(t)
	now := time.Date(2026, 8, 20, 10, 0, 0, 0, time.UTC)
	w := httptest.NewRecorder()
	handleAction(w, actionReqFrom("192.168.1.5:1234", "live-1", "end", false, "secret", "127.0.0.1"), db, cfg, now)
	if w.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200", w.Code)
	}
	if got := decodeActionJSON(t, w); !got.OK {
		t.Fatalf("got %+v, want ok", got)
	}
	if et := queryLatestEventType(t, db, "live-1"); et != "SessionEnd" {
		t.Fatalf("latest event = %q, want SessionEnd", et)
	}
	if _, err := os.Stat(logPath); err == nil {
		t.Fatal("end ran a subprocess")
	}
}

// escapedGrandchildStub writes a stub that spawns a `setsid` grandchild which
// INHERITS the stub's stdout/stderr (the os/exec pipe) and outlives the parent.
// setsid puts it in a new session, so it is NOT in the child's process group and
// survives the timeout's `kill -pgid`; it then holds the pipe's write end open.
// Without Cmd.WaitDelay, cmd.Wait() blocks on the copier goroutine for as long as
// that grandchild lives and the handler goroutine never returns its inflight locks.
func escapedGrandchildStub(t *testing.T, name string) string {
	t.Helper()
	if _, err := exec.LookPath("setsid"); err != nil {
		t.Skipf("setsid unavailable: %v", err)
	}
	return writeStub(t, name, "setsid sleep 30 &\nsleep 30\n")
}

func TestRunActionTimeoutEscapedGrandchildDoesNotHang(t *testing.T) {
	stub := escapedGrandchildStub(t, "escape")
	start := time.Now()
	done := make(chan actionResult, 1)
	go func() { done <- runAction(stub, t.TempDir(), nil, time.Second) }()
	select {
	case r := <-done:
		if elapsed := time.Since(start); elapsed > 6*time.Second {
			t.Fatalf("runAction took %s; the timeout is not enforceable", elapsed)
		}
		if !r.TimedOut || r.ExitCode != -1 {
			t.Fatalf("got %+v, want TimedOut with ExitCode -1", r)
		}
	case <-time.After(6 * time.Second):
		t.Fatal("runAction still blocked 6s after a 1s timeout: " +
			"a grandchild that escaped the process group is holding the output pipe open")
	}
}

func TestRunTmuxTimeoutEscapedGrandchildDoesNotHang(t *testing.T) {
	stub := escapedGrandchildStub(t, "escapetmux")
	start := time.Now()
	done := make(chan actionResult, 1)
	go func() {
		_, r := runTmux(stub, t.TempDir(), []string{"send-keys"}, time.Second)
		done <- r
	}()
	select {
	case r := <-done:
		if elapsed := time.Since(start); elapsed > 6*time.Second {
			t.Fatalf("runTmux took %s; the timeout is not enforceable", elapsed)
		}
		if !r.TimedOut || r.ExitCode != -1 {
			t.Fatalf("got %+v, want TimedOut with ExitCode -1", r)
		}
	case <-time.After(6 * time.Second):
		t.Fatal("runTmux still blocked 6s after a 1s timeout")
	}
}

// gitProbe gets the same treatment, and matters MORE than the other two: per its
// own comment it runs while the handler holds BOTH inflight locks, so a probe
// that blocks on a daemonising grandchild (credential cache, ssh ControlMaster)
// wedges the session and the worktree, not just one request. Its timeout is a
// fixed constant, so the ceiling here is gitProbeTimeout + waitDelay + slack,
// well under the 30s the escaped grandchild sleeps.
func TestGitProbeTimeoutEscapedGrandchildDoesNotHang(t *testing.T) {
	stub := escapedGrandchildStub(t, "escapeprobe")
	start := time.Now()
	type probeResult struct {
		out string
		ok  bool
	}
	done := make(chan probeResult, 1)
	go func() {
		out, ok := gitProbe(stub, t.TempDir(), "rev-parse", "--verify", "HEAD")
		done <- probeResult{out, ok}
	}()
	select {
	case r := <-done:
		if elapsed := time.Since(start); elapsed > gitProbeTimeout+waitDelay+4*time.Second {
			t.Fatalf("gitProbe took %s; the timeout is not enforceable", elapsed)
		}
		if r.ok {
			t.Fatalf("got (%q, true), want a failed probe so the caller fails closed", r.out)
		}
	case <-time.After(gitProbeTimeout + waitDelay + 4*time.Second):
		t.Fatal("gitProbe still blocked well past its timeout: a grandchild that " +
			"escaped the process group is holding the output pipe open while both " +
			"inflight locks are held")
	}
}

// Behind a reverse proxy every request arrives on a loopback socket, so the peer
// gate would pass for anyone who can reach the proxy. handleAction therefore
// refuses on the mere PRESENCE of a forwarding header for anything that execs.
func TestHandleActionForwardingHeaderRefused(t *testing.T) {
	for _, hdr := range []string{"X-Forwarded-For", "X-Real-IP", "Forwarded", "X-Forwarded-Host"} {
		t.Run(hdr, func(t *testing.T) {
			db, cfg, logPath := gatedCfg(t)
			r := actionReq("live-1", "squash", false, "secret", "127.0.0.1")
			r.Header.Set(hdr, "203.0.113.9")
			w := httptest.NewRecorder()
			handleAction(w, r, db, cfg, time.Now())
			if w.Code != http.StatusForbidden {
				t.Fatalf("code = %d, want 403", w.Code)
			}
			if !strings.Contains(w.Body.String(), "through a proxy") {
				t.Fatalf("body = %q, want the proxy reason (distinct from the other 403s)", w.Body.String())
			}
			if _, err := os.Stat(logPath); err == nil {
				t.Fatal("a subprocess ran for a proxied request")
			}
		})
	}
	t.Run("absent", func(t *testing.T) {
		db, cfg, logPath := gatedCfg(t)
		w := httptest.NewRecorder()
		handleAction(w, actionReq("live-1", "squash", false, "secret", "127.0.0.1"), db, cfg, time.Now())
		if w.Code != http.StatusOK {
			t.Fatalf("code = %d, want 200 without any forwarding header", w.Code)
		}
		if _, err := os.Stat(logPath); err != nil {
			t.Fatalf("no subprocess ran (%v)", err)
		}
	})
	// An EMPTY header is not a proxy marker; only a non-empty one is.
	t.Run("empty header value is ignored", func(t *testing.T) {
		db, cfg, _ := gatedCfg(t)
		r := actionReq("live-1", "squash", false, "secret", "127.0.0.1")
		r.Header.Set("X-Forwarded-For", "")
		w := httptest.NewRecorder()
		handleAction(w, r, db, cfg, time.Now())
		if w.Code != http.StatusOK {
			t.Fatalf("code = %d, want 200", w.Code)
		}
	})
}

// cwd is read from a stored hook payload and becomes exec.Command.Dir. A
// relative one would run the action against serve's own working directory, so it
// is refused before anything spawns.
func TestHandleActionRelativeCwdRefused(t *testing.T) {
	db := openTestDB(t)
	insertEvent(db, Event{TS: "2026-08-12T10:00:00Z", SourceApp: "x", SessionID: "rel-1",
		Cwd: "relative/path", EventType: "Stop", PayloadJSON: "{}"})
	logPath := filepath.Join(t.TempDir(), "ran.log")
	cfg := newActionCfg(logStub(t, "merge-pr", logPath), logStub(t, "gh", logPath))
	w := httptest.NewRecorder()
	handleAction(w, actionReq("rel-1", "squash", false, "secret", "127.0.0.1"), db, cfg, time.Now())
	if w.Code != http.StatusNotFound {
		t.Fatalf("code = %d, want 404", w.Code)
	}
	if !strings.Contains(w.Body.String(), "absolute") {
		t.Fatalf("body = %q, want the absolute-path reason", w.Body.String())
	}
	if _, err := os.Stat(logPath); err == nil {
		t.Fatal("a subprocess ran with a relative working directory")
	}
}

// The form body is bounded before it is parsed, so a client cannot make the
// handler buffer an unbounded request. An oversized body fails to parse, which
// leaves no action to dispatch and nothing execs.
func TestHandleActionOversizedBodyRejected(t *testing.T) {
	db, cfg, logPath := gatedCfg(t)
	form := url.Values{"session_id": {"live-1"}, "action": {"squash"},
		"pad": {strings.Repeat("x", 128<<10)}}
	r := httptest.NewRequest(http.MethodPost, "/api/action", strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.Header.Set("X-Clodhopper-Token", "secret")
	r.Host = "127.0.0.1"
	r.RemoteAddr = "127.0.0.1:54321"
	w := httptest.NewRecorder()
	handleAction(w, r, db, cfg, time.Now())
	if w.Code == http.StatusOK {
		t.Fatalf("code = 200, want a refusal for an oversized body")
	}
	if _, err := os.Stat(logPath); err == nil {
		t.Fatal("a subprocess ran for an oversized body")
	}
}

// The two inflight key spaces are namespaced, so a session id that literally
// reads like a worktree key cannot contend with one.
func TestInflightKeyNamespaces(t *testing.T) {
	s := newInflightSet()
	if !s.acquire(cwdKey("/w/one")) {
		t.Fatal("first worktree acquire failed")
	}
	if !s.acquire(sidKey("cwd:/w/one")) {
		t.Fatal("a session id shaped like a worktree key contended with the worktree lock")
	}
}

// ---------------------------------------------------------------------------
// review round 4
// ---------------------------------------------------------------------------

// Two spellings of one worktree must take the SAME lock, or two actions run
// concurrently in the same directory.
func TestCwdKeyNormalisesPath(t *testing.T) {
	dir := t.TempDir()
	if cwdKey(dir) != cwdKey(dir+"/") {
		t.Errorf("trailing slash took a different lock: %q vs %q", cwdKey(dir), cwdKey(dir+"/"))
	}
	if cwdKey(dir) != cwdKey(dir+"/./") {
		t.Errorf("uncleaned path took a different lock: %q", cwdKey(dir+"/./"))
	}
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(dir, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if cwdKey(link) != cwdKey(dir) {
		t.Errorf("symlinked spelling took a different lock: %q vs %q", cwdKey(link), cwdKey(dir))
	}
	// A path that does not exist still gets a (cleaned) key rather than an empty one.
	if got := cwdKey("/no/such/dir/"); got != "cwd:/no/such/dir" {
		t.Errorf("cwdKey(missing) = %q", got)
	}
}

// The forwarding-header refusal is about ATTRIBUTION, not about exec: behind a
// proxy we cannot say who the peer is, so even the exec-free "end" — the one
// write that used to slip through — is refused, and no row is dismissed.
func TestHandleActionEndRefusedThroughProxy(t *testing.T) {
	for _, hdr := range []string{"X-Forwarded-For", "X-Real-IP", "Forwarded", "X-Forwarded-Host"} {
		t.Run(hdr, func(t *testing.T) {
			db, cfg, _ := gatedCfg(t)
			now := time.Date(2026, 8, 20, 10, 0, 0, 0, time.UTC)
			r := actionReq("live-1", "end", false, "secret", "127.0.0.1")
			r.Header.Set(hdr, "203.0.113.9")
			w := httptest.NewRecorder()
			handleAction(w, r, db, cfg, now)
			if w.Code != http.StatusForbidden {
				t.Fatalf("code = %d, want 403", w.Code)
			}
			if !strings.Contains(w.Body.String(), "through a proxy") {
				t.Fatalf("body = %q, want the proxy reason", w.Body.String())
			}
			if et := queryLatestEventType(t, db, "live-1"); et == "SessionEnd" {
				t.Fatal("a proxied peer dismissed a roster row")
			}
		})
	}
}

// dismiss is exec-free like end, and refused through a proxy just the same.
func TestHandleActionDismissRefusedThroughProxy(t *testing.T) {
	db, cfg, _ := gatedCfg(t)
	cfg.ledger, _ = testLedger(nil)
	cfg.ledger.record([]Agent{queuedRow("live-1")}, ledgerNow)
	r := actionReq("live-1", "dismiss", false, "secret", "127.0.0.1")
	r.Header.Set("X-Forwarded-For", "203.0.113.9")
	w := httptest.NewRecorder()
	handleAction(w, r, db, cfg, ledgerNow)
	if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "through a proxy") {
		t.Fatalf("%d %q, want 403 with the proxy reason", w.Code, w.Body.String())
	}
	if !cfg.ledger.dismiss("live-1") {
		t.Fatal("a proxied peer dismissed a ledger row")
	}
}

// A rebound Host is refused for every action, including the exec-free one.
func TestHandleActionEndRefusedOnBadHost(t *testing.T) {
	db, cfg, _ := gatedCfg(t)
	now := time.Date(2026, 8, 20, 10, 0, 0, 0, time.UTC)
	w := httptest.NewRecorder()
	handleAction(w, actionReq("live-1", "end", false, "secret", "evil.example.com"), db, cfg, now)
	if w.Code != http.StatusForbidden {
		t.Fatalf("code = %d, want 403", w.Code)
	}
	if et := queryLatestEventType(t, db, "live-1"); et == "SessionEnd" {
		t.Fatal("a rebound Host dismissed a roster row")
	}
}

// execPeerAllowed is the one gate both endpoints run, so its three checks are
// pinned here directly.
func TestExecPeerAllowed(t *testing.T) {
	req := func(remoteAddr, host string, headers map[string]string) *http.Request {
		r := httptest.NewRequest(http.MethodGet, "/api/pane?pane=%253", nil)
		r.RemoteAddr = remoteAddr
		r.Host = host
		for k, v := range headers {
			r.Header.Set(k, v)
		}
		return r
	}
	if ok, why := execPeerAllowed(req("127.0.0.1:1", "127.0.0.1", nil), "127.0.0.1", nil, true); !ok {
		t.Fatalf("loopback + clean headers + allowed host refused: %s", why)
	}
	if ok, _ := execPeerAllowed(req("127.0.0.1:1", "evil.example.com", nil), "127.0.0.1", nil, true); ok {
		t.Error("a rebound Host was allowed")
	}
	if ok, _ := execPeerAllowed(req("192.168.1.5:1", "127.0.0.1", nil), "127.0.0.1", nil, true); ok {
		t.Error("a LAN peer was allowed")
	}
	// The peer-network half is the only part execs gates; a LAN peer is still
	// allowed for an exec-free action.
	if ok, why := execPeerAllowed(req("192.168.1.5:1", "127.0.0.1", nil), "127.0.0.1", nil, false); !ok {
		t.Errorf("a LAN peer was refused for an exec-free request: %s", why)
	}
	// The proxy refusal applies whether or not the request execs.
	for _, execs := range []bool{true, false} {
		h := map[string]string{"X-Real-IP": "203.0.113.9"}
		if ok, _ := execPeerAllowed(req("127.0.0.1:1", "127.0.0.1", h), "127.0.0.1", nil, execs); ok {
			t.Errorf("execs=%v: a proxied request was allowed", execs)
		}
	}
	// An operator-declared extra host (a magicDNS name) is accepted.
	if ok, why := execPeerAllowed(req("100.64.0.2:1", "board.tailnet.ts.net", nil),
		"100.64.0.2", []string{"board.tailnet.ts.net"}, true); !ok {
		t.Errorf("declared host refused: %s", why)
	}
}

// ---------------------------------------------------------------------------
// review round 5
// ---------------------------------------------------------------------------

// The worktree lock must be released with the SAME key it was acquired with.
// cwdKey resolves symlinks, and the merge actions run merge-pr, which REMOVES
// the worktree — so a key re-derived AFTER the run is the unresolved spelling,
// deletes nothing, and pins the resolved key for the life of the process. Every
// later request whose cwd resolves there (the agent recreating the worktree at
// the same path, say) would 409 forever.
//
// The handler is safe from this today whichever way it is spelled, because a
// deferred call's arguments are evaluated where the defer is WRITTEN, before the
// subprocess runs. That is invisible at the call site and one refactor into a
// closure away from being wrong, so the property gets a test of its own.
func TestHandleActionWorktreeLockReleasedAfterWorktreeRemoved(t *testing.T) {
	base := t.TempDir()
	realDir := filepath.Join(base, "real")
	if err := os.Mkdir(realDir, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(base, "link")
	if err := os.Symlink(realDir, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	// The point of the fixture is that the link and its target key DIFFERENTLY
	// until EvalSymlinks resolves them; if they already agreed, the release path
	// could not leak and the test would prove nothing.
	if cwdKey(link) != cwdKey(realDir) {
		t.Fatalf("fixture is not exercising symlink resolution: cwdKey(%q) = %q, cwdKey(%q) = %q",
			link, cwdKey(link), realDir, cwdKey(realDir))
	}

	db := openTestDB(t)
	insertEvent(db, Event{TS: "2026-08-12T10:00:00Z", SourceApp: "x", SessionID: "leak-1",
		Cwd: link, TmuxPane: "%1", EventType: "Stop", PayloadJSON: "{}"})
	// A second row already sitting on the RESOLVED path: this is the request the
	// leaked key would block.
	insertEvent(db, Event{TS: "2026-08-12T10:01:00Z", SourceApp: "x", SessionID: "leak-2",
		Cwd: realDir, TmuxPane: "%2", EventType: "Stop", PayloadJSON: "{}"})

	// merge-pr tears the worktree down, as the real one does; the stub tmux
	// does it inside new-window, as a merge-pr that finishes at once would.
	// The single-quoted '$1' is a literal stand-in tmux session id for
	// display-message to print, not a shell parameter.
	cfg := newActionCfg(writeStub(t, "merge-pr", "exit 0"), "/bin/true")
	cfg.tmux = writeStub(t, "tmux", `if [ "$1" = "display-message" ]; then echo '$1'; exit 0; fi
rm -rf `+strconv.Quote(link)+" "+strconv.Quote(realDir)+"; exit 0")

	w := httptest.NewRecorder()
	handleAction(w, actionReq("leak-1", "close", false, "secret", "127.0.0.1"), db, cfg, time.Now())
	if w.Code != http.StatusOK {
		t.Fatalf("first action: code = %d body = %q", w.Code, w.Body.String())
	}
	if _, err := os.Lstat(link); err == nil {
		t.Fatal("the stub did not remove the worktree; the fixture proves nothing")
	}

	w = httptest.NewRecorder()
	handleAction(w, actionReq("leak-2", "close", false, "secret", "127.0.0.1"), db, cfg, time.Now())
	if w.Code == http.StatusConflict {
		t.Fatalf("code = 409 (%q): the worktree lock leaked when the worktree was removed",
			strings.TrimSpace(w.Body.String()))
	}
}

// The 403 body names the header that was actually found, so an operator who put
// the dashboard behind a proxy can see WHICH hop is being detected. Pins the
// echoed name per header, not just that some proxy reason came back.
func TestForwardingHeaderNamesTheHeaderFound(t *testing.T) {
	for _, name := range []string{"X-Forwarded-For", "X-Real-IP", "Forwarded", "X-Forwarded-Host"} {
		h := http.Header{}
		h.Set(name, "203.0.113.9")
		if got := forwardingHeader(h); got != name {
			t.Errorf("forwardingHeader(%s) = %q, want %q", name, got, name)
		}
		// Whitespace-only is not "present".
		blank := http.Header{}
		blank.Set(name, "   ")
		if got := forwardingHeader(blank); got != "" {
			t.Errorf("forwardingHeader(%s: blank) = %q, want \"\"", name, got)
		}
	}
	if got := forwardingHeader(http.Header{}); got != "" {
		t.Errorf("forwardingHeader(none) = %q, want \"\"", got)
	}
}

// handleAction refuses a proxied request BEFORE it checks the token and before
// it reads the body. A request that is both proxied and bogus-tokened must
// therefore come back with the PROXY reason: if the early gate were dropped, the
// token check would answer first and say "bad token" instead.
func TestHandleActionProxyRefusedBeforeTokenCheck(t *testing.T) {
	db, cfg, logPath := gatedCfg(t)
	r := actionReq("live-1", "end", false, "wrong-token", "127.0.0.1")
	r.Header.Set("X-Forwarded-For", "203.0.113.9")
	w := httptest.NewRecorder()
	handleAction(w, r, db, cfg, time.Date(2026, 8, 20, 10, 0, 0, 0, time.UTC))
	if w.Code != http.StatusForbidden {
		t.Fatalf("code = %d, want 403", w.Code)
	}
	if body := w.Body.String(); !strings.Contains(body, "through a proxy") {
		t.Fatalf("body = %q, want the proxy reason (the peer gate runs before the token check)", body)
	}
	if _, err := os.Stat(logPath); err == nil {
		t.Fatal("a subprocess ran for a proxied request")
	}
	if et := queryLatestEventType(t, db, "live-1"); et == "SessionEnd" {
		t.Fatal("a proxied peer dismissed a roster row")
	}
}

func TestHandleActionSendsFixedCommandBuildsArgv(t *testing.T) {
	for action, want := range map[string]string{
		"reload-plugins": "send-keys -t %4 /reload-plugins Enter\n",
		"rebase":         "send-keys -t %4 /kitchen-sink:git-rebase Enter\n",
	} {
		t.Run(action, func(t *testing.T) {
			db := openTestDB(t)
			insertEvent(db, Event{TS: "2026-08-12T10:00:00Z", SourceApp: "x", SessionID: "rp", Cwd: t.TempDir(), TmuxPane: "%4", EventType: "Stop", PayloadJSON: "{}"})
			stub, logPath := tmuxLogStub(t)
			cfg := newActionCfg("/bin/true", "/bin/true")
			cfg.tmux = stub

			w := httptest.NewRecorder()
			// A fixed clock keeps the session on the roster: the guard refuses a
			// session that is not on it.
			handleAction(w, actionReq("rp", action, false, "secret", "127.0.0.1"), db, cfg, time.Date(2026, 8, 12, 10, 1, 0, 0, time.UTC))
			if w.Code != http.StatusOK {
				t.Fatalf("code = %d body=%s", w.Code, w.Body.String())
			}
			if got, _ := os.ReadFile(logPath); string(got) != want {
				t.Fatalf("argv = %q, want %q", got, want)
			}
		})
	}
}

func TestReloadSkipReason(t *testing.T) {
	const (
		busy   = "working — retry, or include working"
		noTmux = "not in tmux — reload manually"
	)
	cases := []struct {
		pane, status string
		includeBusy  bool
		want         string
	}{
		{"%1", statusWaiting, false, ""},
		{"%1", statusIdle, false, ""},
		{"%1", statusBackground, false, ""},
		{"%1", statusWorking, false, busy},
		{"%1", statusWorking, true, ""},
		// Enter would answer the prompt, so these are never sent, busy or not.
		{"%1", statusApproval, false, promptBlockedReason},
		{"%1", statusApproval, true, promptBlockedReason},
		{"%1", statusInput, false, promptBlockedReason},
		{"%1", statusInput, true, promptBlockedReason},
		{"%1", statusNeedsYou, false, promptBlockedReason},
		{"%1", statusNeedsYou, true, promptBlockedReason},
		// Unknown statuses fail closed.
		{"%1", statusEnded, true, "status " + strconv.Quote(statusEnded) + " — not sent"},
		{"%1", "something new", true, `status "something new" — not sent`},
		{"", statusWaiting, true, noTmux},
		{"; rm -rf /", statusWaiting, true, noTmux},
	}
	for _, c := range cases {
		got := reloadSkipReason(Agent{TmuxPane: c.pane, Status: c.status}, c.includeBusy)
		if got != c.want {
			t.Errorf("pane=%q status=%q busy=%v: reason=%q, want %q", c.pane, c.status, c.includeBusy, got, c.want)
		}
	}
}

// seedBroadcastRoster seeds one row per broadcast outcome: waiting (sent),
// working (skipped unless include_busy), prompt-blocked (never sent), no pane,
// and two sessions sharing pane %5 (only the newer one is sent to), all
// launched in the returned dir.
func seedBroadcastRoster(t *testing.T) (*sql.DB, string) {
	t.Helper()
	db := openTestDB(t)
	dir := t.TempDir()
	seed := func(ts, sess, branch, pane, etype string) {
		insertEvent(db, Event{TS: ts, SourceApp: "x", SessionID: sess, Branch: branch, Cwd: dir, TmuxPane: pane, EventType: etype, PayloadJSON: "{}"})
	}
	seed("2026-08-12T10:00:00Z", "waiting-1", "feat-a", "%1", "Stop")
	seed("2026-08-12T10:00:00Z", "working-1", "feat-b", "%2", "PreToolUse")
	seed("2026-08-12T10:00:00Z", "prompt-1", "feat-c", "%3", "PermissionRequest")
	seed("2026-08-12T10:00:00Z", "nopane-1", "feat-d", "", "Stop")
	seed("2026-08-12T09:00:00Z", "oldpane-1", "feat-e", "%5", "Stop")
	seed("2026-08-12T09:30:00Z", "newpane-1", "feat-e", "%5", "Stop")
	return db, dir
}

// runBroadcast posts broadcast-reload against db with tmux stubbed by stub and
// returns the response's ok, output and rows plus the sorted send-keys lines the
// stub logged (sorted because roster order is not part of the contract).
func runBroadcast(t *testing.T, db *sql.DB, stub, logPath string, includeBusy bool) (ok bool, report string, rows []reportRow, sent []string) {
	t.Helper()
	cfg := newActionCfg("/bin/true", "/bin/true")
	cfg.tmux = stub
	r := actionReq("", "broadcast-reload", false, "secret", "127.0.0.1")
	if includeBusy {
		// FormValue merges the query string with the body, so this is the
		// same field the dashboard posts in its form body.
		r.URL.RawQuery = "include_busy=true"
	}
	w := httptest.NewRecorder()
	handleAction(w, r, db, cfg, time.Date(2026, 8, 12, 10, 1, 0, 0, time.UTC))
	if w.Code != http.StatusOK {
		t.Fatalf("code = %d body=%s", w.Code, w.Body.String())
	}
	got, _ := os.ReadFile(logPath)
	sent = strings.Split(strings.TrimSpace(string(got)), "\n")
	slices.Sort(sent)
	d := decodeActionJSON(t, w)
	return d.OK, d.Output, d.Rows, sent
}

func reportHas(report, line string) bool {
	return slices.Contains(strings.Split(report, "\n"), line)
}

func TestHandleActionBroadcastReload(t *testing.T) {
	db, _ := seedBroadcastRoster(t)
	var audit bytes.Buffer
	old := auditw
	auditw = &audit
	t.Cleanup(func() { auditw = old })

	stub, logPath := tmuxLogStub(t)
	ok, report, _, sent := runBroadcast(t, db, stub, logPath, false)
	if !ok {
		t.Fatalf("ok = false: %q", report)
	}
	want := []string{"send-keys -t %1 /reload-plugins Enter", "send-keys -t %5 /reload-plugins Enter"}
	if !slices.Equal(sent, want) {
		t.Fatalf("default sends = %q, want %q", sent, want)
	}
	for _, line := range []string{
		"sent x feat-a " + shortID("waiting-1"),
		"skipped x feat-b " + shortID("working-1") + ": working — retry, or include working",
		"skipped x feat-c " + shortID("prompt-1") + ": " + promptBlockedReason,
		"skipped x feat-d " + shortID("nopane-1") + ": not in tmux — reload manually",
		"skipped x feat-e " + shortID("oldpane-1") + ": same pane as newer session " + shortID("newpane-1"),
		"sent x feat-e " + shortID("newpane-1"),
	} {
		if !reportHas(report, line) {
			t.Errorf("report missing %q:\n%s", line, report)
		}
	}
	if want := "audit broadcast-reload ts=2026-08-12T10:01:00Z include_busy=false sent=2 skipped=4 errors=0\n"; !strings.Contains(audit.String(), want) {
		t.Errorf("audit = %q, want it to contain %q", audit.String(), want)
	}

	stub, logPath = tmuxLogStub(t)
	_, report, _, sent = runBroadcast(t, db, stub, logPath, true)
	if !slices.Contains(sent, "send-keys -t %2 /reload-plugins Enter") || slices.Contains(sent, "send-keys -t %3 /reload-plugins Enter") {
		t.Fatalf("include_busy sends = %q, want working %%2 sent and prompt %%3 still skipped", sent)
	}
	if !reportHas(report, "sent x feat-b "+shortID("working-1")) {
		t.Errorf("include_busy report missing working send:\n%s", report)
	}
}

// One failed send fails the broadcast and is reported, but does not stop the
// remaining eligible rows from being attempted.
func TestHandleActionBroadcastReloadSendFails(t *testing.T) {
	db, dir := seedBroadcastRoster(t)
	// waiting-1 later cd'd elsewhere: its row's dir must stay the launch dir.
	insertEvent(db, Event{TS: "2026-08-12T10:00:30Z", SourceApp: "x", SessionID: "waiting-1", Branch: "feat-a", Cwd: t.TempDir(), TmuxPane: "%1", EventType: "Stop", PayloadJSON: "{}"})
	var audit bytes.Buffer
	old := auditw
	auditw = &audit
	t.Cleanup(func() { auditw = old })
	logPath := filepath.Join(t.TempDir(), "tmux.log")
	stub := writeStub(t, "tmux", `echo "$@" >> `+logPath+`
if [ "$3" = "%1" ]; then echo "no such pane TOKEN=hunter2" >&2; exit 3; fi
exit 0`)

	ok, report, rows, sent := runBroadcast(t, db, stub, logPath, false)
	if ok {
		t.Fatalf("ok = true with a failed send: %q", report)
	}
	if !strings.Contains(report, "error x feat-a "+shortID("waiting-1")+":") {
		t.Errorf("report missing the error line:\n%s", report)
	}
	// Problems first so the dashboard shows them without scrolling. Targets lead
	// with the project (SourceApp "x"); dir is each session's launch dir.
	wantRows := []reportRow{
		{Result: "error", Target: "x feat-a " + shortID("waiting-1"), Dir: dir, Reason: "no such pane TOKEN=" + redacted},
		{Result: "skipped", Target: "x feat-b " + shortID("working-1"), Dir: dir, Reason: "working — retry, or include working"},
		{Result: "skipped", Target: "x feat-c " + shortID("prompt-1"), Dir: dir, Reason: promptBlockedReason},
		{Result: "skipped", Target: "x feat-d " + shortID("nopane-1"), Dir: dir, Reason: "not in tmux — reload manually"},
		{Result: "skipped", Target: "x feat-e " + shortID("oldpane-1"), Dir: dir, Reason: "same pane as newer session " + shortID("newpane-1")},
		{Result: "sent", Target: "x feat-e " + shortID("newpane-1"), Dir: dir},
	}
	if !slices.Equal(rows, wantRows) {
		t.Errorf("rows = %q, want %q", rows, wantRows)
	}
	want := []string{"send-keys -t %1 /reload-plugins Enter", "send-keys -t %5 /reload-plugins Enter"}
	if !slices.Equal(sent, want) {
		t.Fatalf("sends = %q, want both eligible panes attempted %q", sent, want)
	}
	if !strings.Contains(audit.String(), " errors=1\n") {
		t.Errorf("audit = %q, want errors=1", audit.String())
	}
}

// A row whose tmux pane has died is not a broadcast target: it is neither sent
// to nor reported as an error, so it cannot fail the broadcast.
func TestHandleActionBroadcastReloadSkipsDeadPane(t *testing.T) {
	db := openTestDB(t)
	dir := t.TempDir()
	for _, sess := range []struct{ id, pane string }{{"alive-1", "%1"}, {"dead-1", "%80"}} {
		insertEvent(db, Event{TS: "2026-08-12T10:00:00Z", SourceApp: "x", SessionID: sess.id, Cwd: dir, TmuxPane: sess.pane, EventType: "Stop", PayloadJSON: "{}"})
	}
	stub, logPath := tmuxLogStub(t)
	cfg := newActionCfg("/bin/true", "/bin/true")
	cfg.tmux = stub
	now := time.Date(2026, 8, 12, 10, 1, 0, 0, time.UTC)
	cfg.panes = &paneCache{set: map[string]livePane{"%1": {}}, at: now}

	w := httptest.NewRecorder()
	handleAction(w, actionReq("", "broadcast-reload", false, "secret", "127.0.0.1"), db, cfg, now)
	d := decodeActionJSON(t, w)
	if !d.OK || strings.Contains(d.Output, shortID("dead-1")) {
		t.Fatalf("ok=%v report=%q, want ok with the dead pane absent", d.OK, d.Output)
	}
	if got, _ := os.ReadFile(logPath); strings.TrimSpace(string(got)) != "send-keys -t %1 /reload-plugins Enter" {
		t.Fatalf("sends = %q, want only the live pane", got)
	}
}

// Enter would answer the prompt, so an explicit per-row click is refused for a
// prompt-blocked session before tmux runs.
func TestHandleActionSendsFixedCommandRefusedOnPrompt(t *testing.T) {
	for _, action := range []string{"reload-plugins", "rebase"} {
		t.Run(action, func(t *testing.T) {
			db := openTestDB(t)
			insertEvent(db, Event{TS: "2026-08-12T10:00:00Z", SourceApp: "x", SessionID: "rp", Cwd: t.TempDir(), TmuxPane: "%4", EventType: "PermissionRequest", PayloadJSON: "{}"})
			stub, logPath := tmuxLogStub(t)
			cfg := newActionCfg("/bin/true", "/bin/true")
			cfg.tmux = stub

			w := httptest.NewRecorder()
			handleAction(w, actionReq("rp", action, false, "secret", "127.0.0.1"), db, cfg, time.Date(2026, 8, 12, 10, 1, 0, 0, time.UTC))
			if w.Code != http.StatusConflict {
				t.Fatalf("code = %d, want 409; body=%s", w.Code, w.Body.String())
			}
			// 409 alone is ambiguous with the inflight refusal; pin the prompt reason.
			if body := w.Body.String(); !strings.Contains(body, promptBlockedReason) {
				t.Fatalf("body = %q, want the prompt reason", body)
			}
			if got, _ := os.ReadFile(logPath); len(got) != 0 {
				t.Fatalf("tmux ran on a prompt-blocked session: %q", got)
			}
		})
	}
}

// After a /clear the old row lingers on the roster sharing the pane with the
// new live session. Clicking reload or rebase on the old (waiting) row must
// still be refused when the pane's current session is at a prompt: Enter lands
// on the pane, not the session.
func TestHandleActionSendsFixedCommandRefusedOnSharedPanePrompt(t *testing.T) {
	for _, action := range []string{"reload-plugins", "rebase"} {
		t.Run(action, func(t *testing.T) {
			db := openTestDB(t)
			dir := t.TempDir()
			insertEvent(db, Event{TS: "2026-08-12T09:00:00Z", SourceApp: "x", SessionID: "old", Cwd: dir, TmuxPane: "%4", EventType: "Stop", PayloadJSON: "{}"})
			insertEvent(db, Event{TS: "2026-08-12T10:00:00Z", SourceApp: "x", SessionID: "new", Cwd: dir, TmuxPane: "%4", EventType: "PermissionRequest", PayloadJSON: "{}"})
			stub, logPath := tmuxLogStub(t)
			cfg := newActionCfg("/bin/true", "/bin/true")
			cfg.tmux = stub

			w := httptest.NewRecorder()
			handleAction(w, actionReq("old", action, false, "secret", "127.0.0.1"), db, cfg, time.Date(2026, 8, 12, 10, 1, 0, 0, time.UTC))
			if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), promptBlockedReason) {
				t.Fatalf("code = %d body=%q, want 409 with the prompt reason", w.Code, w.Body.String())
			}
			if got, _ := os.ReadFile(logPath); len(got) != 0 {
				t.Fatalf("tmux ran into a pane parked on a prompt: %q", got)
			}
		})
	}
}

// Unlike the broadcast, an explicit per-row click on a WORKING session is sent
// deliberately: the user chose that row, and the command just queues.
func TestHandleActionSendsFixedCommandWhileWorking(t *testing.T) {
	for action, want := range map[string]string{
		"reload-plugins": "send-keys -t %4 /reload-plugins Enter\n",
		"rebase":         "send-keys -t %4 /kitchen-sink:git-rebase Enter\n",
	} {
		t.Run(action, func(t *testing.T) {
			db := openTestDB(t)
			insertEvent(db, Event{TS: "2026-08-12T10:00:00Z", SourceApp: "x", SessionID: "rp", Cwd: t.TempDir(), TmuxPane: "%4", EventType: "PreToolUse", PayloadJSON: "{}"})
			stub, logPath := tmuxLogStub(t)
			cfg := newActionCfg("/bin/true", "/bin/true")
			cfg.tmux = stub

			w := httptest.NewRecorder()
			handleAction(w, actionReq("rp", action, false, "secret", "127.0.0.1"), db, cfg, time.Date(2026, 8, 12, 10, 1, 0, 0, time.UTC))
			if w.Code != http.StatusOK {
				t.Fatalf("code = %d body=%s", w.Code, w.Body.String())
			}
			if got, _ := os.ReadFile(logPath); string(got) != want {
				t.Fatalf("argv = %q, want %q", got, want)
			}
		})
	}
}

// The pane lookup alone would find a valid pane for "ghost"; the guard must
// still fail closed because the session has aged off the roster, so its status
// (and so whether Enter would answer a prompt) is unknown.
func TestHandleActionSendsFixedCommandUnknownSession(t *testing.T) {
	for _, action := range []string{"reload-plugins", "rebase"} {
		t.Run(action, func(t *testing.T) {
			db := openTestDB(t)
			insertEvent(db, Event{TS: "2025-08-12T10:00:00Z", SourceApp: "x", SessionID: "ghost", Cwd: t.TempDir(), TmuxPane: "%4", EventType: "Stop", PayloadJSON: "{}"})
			stub, logPath := tmuxLogStub(t)
			cfg := newActionCfg("/bin/true", "/bin/true")
			cfg.tmux = stub

			if pane, err := latestPaneForSession(db, "ghost"); err != nil || pane != "%4" {
				t.Fatalf("latestPaneForSession = %q, %v; want %%4 so the roster check is what refuses", pane, err)
			}
			w := httptest.NewRecorder()
			handleAction(w, actionReq("ghost", action, false, "secret", "127.0.0.1"), db, cfg, time.Date(2026, 8, 12, 10, 1, 0, 0, time.UTC))
			if w.Code != http.StatusNotFound {
				t.Fatalf("code = %d, want 404; body=%s", w.Code, w.Body.String())
			}
			if got, _ := os.ReadFile(logPath); len(got) != 0 {
				t.Fatalf("tmux ran for a session not on the roster: %q", got)
			}
		})
	}
}

// restartUUID / restartUUID2 are session ids in the shape Claude Code issues,
// which broadcast-restart requires before it will put one on a command line.
const (
	restartUUID  = "0f8fad5b-d9cb-469f-a165-70867728950e"
	restartUUID2 = "7c9e6679-7425-40de-944b-e07fc1f90ae7"
)

func TestRestartSkipReason(t *testing.T) {
	dir := t.TempDir()
	cases := []struct {
		pane, status, id, cwd string
		want                  string
	}{
		{"%1", statusIdle, restartUUID, dir, ""},
		{"%1", statusWaiting, restartUUID, dir, ""},
		// Anything in flight would be lost to /exit.
		{"%1", statusWorking, restartUUID, dir, "working — its turn would be cut off"},
		{"%1", statusBackground, restartUUID, dir, "waiting on background work — it would be lost"},
		{"%1", statusSubagents, restartUUID, dir, "waiting on agents or shells — /exit would kill them"},
		{"%1", statusShellStuck, restartUUID, dir, "waiting on agents or shells — /exit would kill them"},
		{"%1", statusApproval, restartUUID, dir, promptBlockedReason},
		{"%1", statusInput, restartUUID, dir, promptBlockedReason},
		{"%1", statusNeedsYou, restartUUID, dir, promptBlockedReason},
		// Unknown statuses fail closed.
		{"%1", statusEnded, restartUUID, dir, "status " + strconv.Quote(statusEnded) + " — not restarted"},
		{"%1", "something new", restartUUID, dir, `status "something new" — not restarted`},
		{"", statusWaiting, restartUUID, dir, "not in tmux — restart manually"},
		{"; rm -rf /", statusWaiting, restartUUID, dir, "not in tmux — restart manually"},
		// The id joins the resume argv, so anything but a lowercase UUID is
		// refused before /exit could kill a session we cannot bring back.
		{"%1", statusWaiting, "live-1", dir, "session id is not a UUID — restart manually"},
		{"%1", statusWaiting, strings.ToUpper(restartUUID), dir, "session id is not a UUID — restart manually"},
		{"%1", statusWaiting, restartUUID + "; rm -rf /", dir, "session id is not a UUID — restart manually"},
		{"%1", statusWaiting, "--continue", dir, "session id is not a UUID — restart manually"},
		{"%1", statusWaiting, restartUUID, "", "no working directory recorded — restart manually"},
		{"%1", statusWaiting, restartUUID, "rel/path", "no working directory recorded — restart manually"},
	}
	for _, c := range cases {
		got := restartSkipReason(Agent{TmuxPane: c.pane, Status: c.status, SessionID: c.id, Cwd: c.cwd})
		if got != c.want {
			t.Errorf("pane=%q status=%q id=%q cwd=%q: reason=%q, want %q", c.pane, c.status, c.id, c.cwd, got, c.want)
		}
	}
}

// restartStub is a fake tmux for broadcast-restart. It logs each call with
// every argument bracketed, so a multi-word argument is visibly ONE argument,
// answers pane_current_path with path, and pane_current_command with "claude"
// until /exit and for the first busyProbes reads after it (claude still
// shutting down), then atExit — or bash once Escape has been sent — until a
// resume has been typed and afterResume once it has. capture-pane prints
// logPath+".screen", which a test writes to put text on the pane.
func restartStub(t *testing.T, busyProbes int, atExit, afterResume, path string) (bin, logPath string) {
	t.Helper()
	logPath = filepath.Join(t.TempDir(), "tmux.log")
	bin = writeStub(t, "tmux", `printf '[%s]' "$@" >> '`+logPath+`'; echo >> '`+logPath+`'
[ "$1" = capture-pane ] && { cat '`+logPath+`.screen' 2>/dev/null; exit 0; }
case "$5" in
'#{pane_current_path}') echo '`+path+`' ;;
'#{pane_current_command}')
  if grep -q -- --resume '`+logPath+`'; then echo `+afterResume+`
  elif grep -q '\[Escape\]' '`+logPath+`'; then echo bash
  elif ! grep -q '\[/exit\]' '`+logPath+`'; then echo claude
  elif [ "$(sed -n '/\[\/exit\]/,$p' '`+logPath+`' | grep -c pane_current_command)" -le `+strconv.Itoa(busyProbes)+` ]; then echo claude
  else echo `+atExit+`; fi ;;
esac
exit 0`)
	return bin, logPath
}

func TestNonoReviewOnScreen(t *testing.T) {
	const (
		title = "  [nono] Review denied paths"
		hints = "  move · Space cycle · a grant-all · d deny-all · Enter confirm · Esc cancel"
		box   = "────────────────────"
	)
	lines := func(l ...string) string { return strings.Join(l, "\n") + "\n" }
	for _, c := range []struct {
		name, screen string
		want         bool
	}{
		// claude's last frame, input box included, stays above nono's review.
		{"real layout", lines("● done", box, "> ", box, "", title, "  /home/x/.cache", hints), true},
		{"quoted above a live input box", lines(title, hints, box, "> ", box), false},
		// The phrase in claude's last frame above its box, then nono's real review
		// below: the LAST title is the one that counts.
		{"quoted in the last frame, real review below", lines(title, hints, box, "> ", box, "", title, hints), true},
		{"no Esc cancel hint", lines(box, "> ", box, title, "  /home/x/.cache"), false},
		{"in a tool-output gutter", lines("  ⎿  [nono] Review denied paths", hints), false},
		{"empty", "", false},
	} {
		if got := nonoReviewOnScreen(c.screen); got != c.want {
			t.Errorf("%s: nonoReviewOnScreen = %v, want %v", c.name, got, c.want)
		}
	}
}

// writeReviewScreen puts nono's exit-time denied-paths review on restartStub's
// pane, as captured by capture-pane.
func writeReviewScreen(t *testing.T, logPath string) {
	t.Helper()
	if err := os.WriteFile(logPath+".screen", []byte("  [nono] Review denied paths\n  Space cycle · a grant-all · d deny-all · Enter confirm · Esc cancel\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

// runRestartBroadcast seeds one waiting session (restartUUID, pane %1, in dir)
// plus one working session that must be skipped, posts broadcast-restart with
// tmux stubbed by stub, polling every millisecond for up to exitWait (zero
// probes once), and returns the decoded response and the stub's call log.
// extra events are inserted after the seed ones.
func runRestartBroadcast(t *testing.T, dir, stub, logPath string, exitWait time.Duration, extra ...Event) (d actionJSON, calls []string) {
	t.Helper()
	db := openTestDB(t)
	insertEvent(db, Event{TS: "2026-08-12T10:00:00Z", SourceApp: "x", SessionID: restartUUID, Branch: "feat-a", Cwd: dir, TmuxPane: "%1", EventType: "Stop", PayloadJSON: "{}"})
	insertEvent(db, Event{TS: "2026-08-12T10:00:00Z", SourceApp: "x", SessionID: restartUUID2, Branch: "feat-b", Cwd: dir, TmuxPane: "%2", EventType: "PreToolUse", PayloadJSON: "{}"})
	for _, e := range extra {
		insertEvent(db, e)
	}
	cfg := newActionCfg("/bin/true", "/bin/true")
	cfg.tmux = stub
	cfg.restartPoll, cfg.restartExitWait = time.Millisecond, exitWait
	w := httptest.NewRecorder()
	handleAction(w, actionReq("", "broadcast-restart", false, "secret", "127.0.0.1"), db, cfg, time.Date(2026, 8, 12, 10, 1, 0, 0, time.UTC))
	if w.Code != http.StatusOK {
		t.Fatalf("code = %d body=%s", w.Code, w.Body.String())
	}
	got, _ := os.ReadFile(logPath)
	return decodeActionJSON(t, w), strings.Split(strings.TrimSpace(string(got)), "\n")
}

func TestHandleActionBroadcastRestart(t *testing.T) {
	var audit bytes.Buffer
	old := auditw
	auditw = &audit
	t.Cleanup(func() { auditw = old })
	dir := t.TempDir()
	stub, logPath := restartStub(t, 0, "zsh", "claude", dir)

	d, calls := runRestartBroadcast(t, dir, stub, logPath, 0)
	if !d.OK {
		t.Fatalf("ok = false: %q", d.Output)
	}
	// The resume is ONE literal argument naming the exact session: never
	// --continue (picks the newest session) or --fork-session (mints a new id).
	want := []string{
		"[display-message][-p][-t][%1][#{pane_current_command}]",
		"[send-keys][-t][%1][/exit][Enter]",
		"[display-message][-p][-t][%1][#{pane_current_command}]",
		"[display-message][-p][-t][%1][#{pane_current_path}]",
		"[send-keys][-t][%1][nn --resume " + restartUUID + "][Enter]",
		"[display-message][-p][-t][%1][#{pane_current_command}]",
	}
	if !slices.Equal(calls, want) {
		t.Fatalf("tmux calls = %q, want %q", calls, want)
	}
	for _, line := range []string{
		"sent x feat-a " + shortID(restartUUID),
		"skipped x feat-b " + shortID(restartUUID2) + ": working — its turn would be cut off",
	} {
		if !reportHas(d.Output, line) {
			t.Errorf("report missing %q:\n%s", line, d.Output)
		}
	}
	if want := "audit broadcast-restart ts=2026-08-12T10:01:00Z sent=1 skipped=1 errors=0\n"; !strings.Contains(audit.String(), want) {
		t.Errorf("audit = %q, want it to contain %q", audit.String(), want)
	}
}

// A pane that never gets back to a shell (claude hung on exit, or a foreground
// command we do not recognise) is an error, and nothing more is typed into it.
func TestHandleActionBroadcastRestartNoShellSendsNoResume(t *testing.T) {
	dir := t.TempDir()
	stub, logPath := restartStub(t, 0, "claude", "claude", dir)
	d, calls := runRestartBroadcast(t, dir, stub, logPath, 0)
	if d.OK || !reportHas(d.Output, "error x feat-a "+shortID(restartUUID)+": still running 0s after /exit (background shells/agents, or the wrapper didn't exit) — nothing more sent") {
		t.Fatalf("ok=%v report=%q, want the no-shell error", d.OK, d.Output)
	}
	if want := "[send-keys][-t][%1][/exit][Enter]"; calls[1] != want || slices.ContainsFunc(calls, func(c string) bool { return strings.Contains(c, "--resume") || strings.Contains(c, "Escape") }) {
		t.Fatalf("tmux calls = %q, want /exit and no Escape or resume", calls)
	}
}

// nono can hold the pane after claude exits with its "Review denied paths"
// prompt: Escape cancels it, and the restart carries on to the resume.
func TestHandleActionBroadcastRestartCancelsNonoReview(t *testing.T) {
	dir := t.TempDir()
	stub, logPath := restartStub(t, 0, "nono", "claude", dir)
	writeReviewScreen(t, logPath)
	d, calls := runRestartBroadcast(t, dir, stub, logPath, 0)
	if !d.OK {
		t.Fatalf("ok = false: %q", d.Output)
	}
	probe := "[display-message][-p][-t][%1][#{pane_current_command}]"
	want := []string{
		probe,
		"[send-keys][-t][%1][/exit][Enter]",
		probe,                        // the exit wait: still nono
		"[capture-pane][-p][-t][%1]", // the review is on screen
		"[send-keys][-t][%1][Escape]",
		probe,
		"[display-message][-p][-t][%1][#{pane_current_path}]",
		"[send-keys][-t][%1][nn --resume " + restartUUID + "][Enter]",
		probe,
	}
	if !slices.Equal(calls, want) {
		t.Fatalf("tmux calls = %q, want %q", calls, want)
	}
}

// A failed /exit send-keys (pane gone, say) is an error and nothing more is typed.
func TestHandleActionBroadcastRestartExitSendFailsSendsNoResume(t *testing.T) {
	dir := t.TempDir()
	stub, logPath := restartStub(t, 0, "bash", "claude", dir)
	// Wrap the stub so /exit fails while every other call behaves as before.
	stub = writeStub(t, "tmux-exitfail", `printf '[%s]' "$@" | grep -q '\[/exit\]' && { printf '[%s]' "$@" >> '`+logPath+`'; echo >> '`+logPath+`'; echo "can't find pane" >&2; exit 1; }
exec '`+stub+`' "$@"`)
	d, calls := runRestartBroadcast(t, dir, stub, logPath, 0)
	if d.OK || !strings.Contains(d.Output, "error x feat-a "+shortID(restartUUID)) {
		t.Fatalf("ok=%v report=%q, want an error for the failed /exit", d.OK, d.Output)
	}
	if want := "[send-keys][-t][%1][/exit][Enter]"; len(calls) != 2 || calls[1] != want {
		t.Fatalf("tmux calls = %q, want the probe, then only the failed /exit and no resume", calls)
	}
}

// A stale row whose pane is already a plain shell gets nothing typed: /exit
// would land in the shell, and the resume would start a session nobody asked for.
func TestHandleActionBroadcastRestartAlreadyAtShellSendsNothing(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(t.TempDir(), "tmux.log")
	stub := writeStub(t, "tmux", `printf '[%s]' "$@" >> '`+logPath+`'; echo >> '`+logPath+`'; echo bash`)
	d, calls := runRestartBroadcast(t, dir, stub, logPath, 0)
	if d.OK || !reportHas(d.Output, "error x feat-a "+shortID(restartUUID)+": Claude isn't running in this pane (it's at a shell) — nothing sent") {
		t.Fatalf("ok=%v report=%q, want the already-at-shell error", d.OK, d.Output)
	}
	if want := []string{"[display-message][-p][-t][%1][#{pane_current_command}]"}; !slices.Equal(calls, want) {
		t.Fatalf("tmux calls = %q, want only the probe %q", calls, want)
	}
}

// A shell that is not in the session's launch directory is refused rather than cd'd:
// resuming there would start claude against the wrong project.
func TestHandleActionBroadcastRestartCwdMismatchSendsNoResume(t *testing.T) {
	dir := t.TempDir()
	stub, logPath := restartStub(t, 0, "bash", "claude", t.TempDir())
	d, calls := runRestartBroadcast(t, dir, stub, logPath, 0)
	if d.OK || !strings.Contains(d.Output, "error x feat-a "+shortID(restartUUID)+": exited, but the shell is in ") {
		t.Fatalf("ok=%v report=%q, want the cwd-mismatch error", d.OK, d.Output)
	}
	if slices.ContainsFunc(calls, func(c string) bool { return strings.Contains(c, "--resume") || strings.Contains(c, "cd ") }) {
		t.Fatalf("tmux calls = %q, want no resume and no cd", calls)
	}
}

// A session that later cd'd into a scratch dir is still resumed: the pane's
// shell is back in the LAUNCH dir, which is what claude --resume looks up by.
func TestHandleActionBroadcastRestartUsesLaunchCwd(t *testing.T) {
	dir := t.TempDir()
	stub, logPath := restartStub(t, 0, "bash", "claude", dir)
	cd := Event{TS: "2026-08-12T10:00:30Z", SourceApp: "x", SessionID: restartUUID, Branch: "feat-a", Cwd: t.TempDir(), TmuxPane: "%1", EventType: "Stop", PayloadJSON: "{}"}
	d, calls := runRestartBroadcast(t, dir, stub, logPath, 0, cd)
	if !d.OK || !slices.Contains(calls, "[send-keys][-t][%1][nn --resume "+restartUUID+"][Enter]") {
		t.Fatalf("ok=%v report=%q calls=%q, want the resume sent", d.OK, d.Output, calls)
	}
}

// nn runs claude under nono, so a claude that ignored /exit also reads as
// "nono": with no review on screen, Escape must not be typed into it.
func TestHandleActionBroadcastRestartNonoWithoutReviewSendsNoEscape(t *testing.T) {
	dir := t.TempDir()
	stub, logPath := restartStub(t, 0, "nono", "claude", dir)
	d, calls := runRestartBroadcast(t, dir, stub, logPath, 0)
	if d.OK || !reportHas(d.Output, "error x feat-a "+shortID(restartUUID)+": still running 0s after /exit (background shells/agents, or the wrapper didn't exit) — nothing more sent") {
		t.Fatalf("ok=%v report=%q, want the still-running error", d.OK, d.Output)
	}
	if slices.ContainsFunc(calls, func(c string) bool { return strings.Contains(c, "Escape") || strings.Contains(c, "--resume") }) {
		t.Fatalf("tmux calls = %q, want no Escape and no resume", calls)
	}
}

// Escape sent but nono's review still holds the pane: a distinct error, and
// the resume is never typed.
func TestHandleActionBroadcastRestartNonoReviewStaysOpen(t *testing.T) {
	dir := t.TempDir()
	inner, logPath := restartStub(t, 0, "nono", "claude", dir)
	// Wrap the stub so the pane keeps reporting nono after Escape too.
	stub := writeStub(t, "tmux-nonostuck", `if [ "$5" = '#{pane_current_command}' ] && grep -q '\[Escape\]' '`+logPath+`'; then printf '[%s]' "$@" >> '`+logPath+`'; echo >> '`+logPath+`'; echo nono; exit 0; fi
exec '`+inner+`' "$@"`)
	writeReviewScreen(t, logPath)
	d, calls := runRestartBroadcast(t, dir, stub, logPath, 0)
	if d.OK || !reportHas(d.Output, "error x feat-a "+shortID(restartUUID)+": nono's denied-paths review still open after Escape — nothing more sent") {
		t.Fatalf("ok=%v report=%q, want the review-still-open error", d.OK, d.Output)
	}
	if !slices.Contains(calls, "[send-keys][-t][%1][Escape]") || slices.ContainsFunc(calls, func(c string) bool { return strings.Contains(c, "--resume") }) {
		t.Fatalf("tmux calls = %q, want Escape and no resume", calls)
	}
}

// A resume typed into the shell that never takes the pane over (a broken nn
// alias, say) is reported, not counted as restarted.
func TestHandleActionBroadcastRestartStillAtShellIsError(t *testing.T) {
	dir := t.TempDir()
	stub, logPath := restartStub(t, 0, "fish", "fish", dir)
	d, calls := runRestartBroadcast(t, dir, stub, logPath, 0)
	if d.OK || !reportHas(d.Output, "error x feat-a "+shortID(restartUUID)+": exited but did not resume — check the pane") {
		t.Fatalf("ok=%v report=%q, want the did-not-resume error", d.OK, d.Output)
	}
	if !slices.Contains(calls, "[send-keys][-t][%1][nn --resume "+restartUUID+"][Enter]") {
		t.Fatalf("tmux calls = %q, want the resume sent", calls)
	}
}

// /exit takes a moment: the pane still runs claude for the first few probes.
// The restart must keep polling through them and resume once the shell is back.
func TestHandleActionBroadcastRestartPollsUntilShell(t *testing.T) {
	dir := t.TempDir()
	stub, logPath := restartStub(t, 2, "zsh", "claude", dir)
	d, calls := runRestartBroadcast(t, dir, stub, logPath, 5*time.Second)
	if !d.OK {
		t.Fatalf("ok = false: %q", d.Output)
	}
	probe := "[display-message][-p][-t][%1][#{pane_current_command}]"
	resume := slices.Index(calls, "[send-keys][-t][%1][nn --resume "+restartUUID+"][Enter]")
	if resume < 0 {
		t.Fatalf("tmux calls = %q, want the resume sent", calls)
	}
	if n := len(slices.DeleteFunc(slices.Clone(calls[:resume]), func(c string) bool { return c != probe })); n != 4 {
		t.Fatalf("probed %d times before resuming, want 4 (the pre-check, two busy, then the shell): %q", n, calls)
	}
}

// blockReq is actionReq for the block action, which also names the blocker.
func blockReq(sessionID, blockerID, token string) *http.Request {
	form := url.Values{"session_id": {sessionID}, "action": {"block"}, "blocker_session_id": {blockerID}}
	r := httptest.NewRequest(http.MethodPost, "/api/action", strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if token != "" {
		r.Header.Set("X-Clodhopper-Token", token)
	}
	r.Host = "127.0.0.1"
	r.RemoteAddr = "127.0.0.1:54321"
	return r
}

// The dashboard's block action records a block between two live rows and
// refuses a row that is not live, a self-block and a cycle with a visible
// reason; unblock drops it again. None of it execs, so the binaries are unusable.
func TestHandleActionBlockAndUnblock(t *testing.T) {
	db := openTestDB(t)
	now := time.Date(2026, 10, 20, 10, 0, 0, 0, time.UTC)
	for _, s := range []string{"a", "b"} {
		insertEvent(db, Event{TS: "2026-10-20T09:00:00Z", SourceApp: "x", SessionID: s, EventType: "Stop", PayloadJSON: "{}"})
	}
	insertEvent(db, Event{TS: "2026-10-20T09:00:00Z", SourceApp: "x", SessionID: "ended", EventType: "SessionEnd", PayloadJSON: "{}"})
	cfg := newActionCfg("/nonexistent/merge-pr", "/nonexistent/gh")
	post := func(r *http.Request) actionJSON {
		t.Helper()
		w := httptest.NewRecorder()
		handleAction(w, r, db, cfg, now)
		if w.Code != http.StatusOK {
			t.Fatalf("code = %d body=%s", w.Code, w.Body.String())
		}
		return decodeActionJSON(t, w)
	}

	for name, r := range map[string]*http.Request{
		"unknown blocker": blockReq("a", "ghost", "secret"),
		"ended blocker":   blockReq("a", "ended", "secret"),
		"unknown blocked": blockReq("ghost", "a", "secret"),
		"self":            blockReq("a", "a", "secret"),
	} {
		if got := post(r); got.OK || got.Output == "" {
			t.Errorf("%s: resp = %+v, want a non-ok result with a message", name, got)
		}
	}
	if got := post(blockReq("a", "b", "secret")); !got.OK {
		t.Fatalf("a on b: resp = %+v, want ok", got)
	}
	if got := post(blockReq("b", "a", "secret")); got.OK || !strings.Contains(got.Output, "cycle") {
		t.Errorf("cycle: resp = %+v, want refused as a cycle", got)
	}
	if blocks, _ := listBlocks(db); !slices.Equal(blocks, []Block{{"a", "b"}}) {
		t.Fatalf("blocks = %v, want only a on b", blocks)
	}
	if got := post(actionReq("a", "unblock", false, "secret", "127.0.0.1")); !got.OK {
		t.Fatalf("unblock: resp = %+v, want ok", got)
	}
	if blocks, _ := listBlocks(db); len(blocks) != 0 {
		t.Errorf("blocks after unblock = %v, want none", blocks)
	}
}

// A missing or wrong CSRF token is refused before anything is written.
func TestHandleActionBlockRejectsBadToken(t *testing.T) {
	db := openTestDB(t)
	now := time.Date(2026, 10, 20, 10, 0, 0, 0, time.UTC)
	for _, s := range []string{"a", "b"} {
		insertEvent(db, Event{TS: "2026-10-20T09:00:00Z", SourceApp: "x", SessionID: s, EventType: "Stop", PayloadJSON: "{}"})
	}
	cfg := newActionCfg("/bin/true", "/bin/true")
	for _, token := range []string{"", "wrong"} {
		w := httptest.NewRecorder()
		handleAction(w, blockReq("a", "b", token), db, cfg, now)
		if w.Code != http.StatusForbidden {
			t.Errorf("token %q: code = %d, want 403", token, w.Code)
		}
	}
	if blocks, _ := listBlocks(db); len(blocks) != 0 {
		t.Errorf("blocks = %v, want none written", blocks)
	}
}

// blockPRReq is a block-pr POST from peer with blocker_pr set.
func blockPRReq(peer, sessionID, pr string) *http.Request {
	form := url.Values{"session_id": {sessionID}, "action": {"block-pr"}, "blocker_pr": {pr}}
	r := httptest.NewRequest(http.MethodPost, "/api/action", strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.Header.Set("X-Clodhopper-Token", "secret")
	r.Host = "127.0.0.1"
	r.RemoteAddr = peer
	return r
}

// block-pr runs the full peer gate (a cache miss runs gh), refuses a bad
// number and a PR outside the row's repo's open list, and otherwise records
// the block against the server-resolved repo.
func TestHandleActionBlockPR(t *testing.T) {
	db := openTestDB(t)
	now := time.Date(2026, 10, 20, 10, 0, 0, 0, time.UTC)
	for _, sess := range []string{"a", "b", "c"} {
		insertEvent(db, Event{TS: "2026-10-20T09:00:00Z", SourceApp: "x", SessionID: sess, Cwd: "/w/" + sess, EventType: "Stop", PayloadJSON: "{}"})
	}
	cfg := newActionCfg("/nonexistent/merge-pr", "/nonexistent/gh")
	// b's cwd has no GitHub repo; c's repo cannot be listed (gh failed).
	cfg.repos = &cwdCache[string]{m: map[string]cwdEntry[string]{}, lookup: func(cwd string) string {
		return map[string]string{"/w/a": "o/r", "/w/c": "o/down"}[cwd]
	}}
	cfg.openPRs = &cwdCache[[]openPR]{m: map[string]cwdEntry[[]openPR]{}, lookup: func(repo string) []openPR {
		return map[string][]openPR{"o/r": {{Number: 7}, {Number: 9}}}[repo]
	}}
	// #9 merged since the open list was cached.
	cfg.ledger = newQueueLedger()
	cfg.ledger.finals.lookup = func(key string) ciResult {
		return map[string]ciResult{"o/r#9": {PRNumber: 9, Done: "merged"}}[key]
	}
	post := func(r *http.Request) (int, actionJSON) {
		t.Helper()
		w := httptest.NewRecorder()
		handleAction(w, r, db, cfg, now)
		if w.Code != http.StatusOK {
			return w.Code, actionJSON{}
		}
		return w.Code, decodeActionJSON(t, w)
	}
	if code, _ := post(blockPRReq("192.168.1.20:5555", "a", "7")); code != http.StatusForbidden {
		t.Errorf("LAN peer: %d, want 403", code)
	}
	for name, c := range map[string]struct{ sess, pr, want string }{
		"zero":        {"a", "0", "not a PR number"},
		"negative":    {"a", "-7", "not a PR number"},
		"garbage":     {"a", "7x", "not a PR number"},
		"not open":    {"a", "8", "PR #8 is not an open PR in o/r"},
		"not live":    {"z", "7", "the row must be live on the roster"},
		"no repo":     {"b", "7", "this row is not in a GitHub repo"},
		"list failed": {"c", "7", "could not confirm PR #7 is open in o/down"},
		"merged":      {"a", "9", "PR #9 in o/r is already merged"},
	} {
		if _, got := post(blockPRReq("127.0.0.1:5555", c.sess, c.pr)); got.OK || got.Output != c.want {
			t.Errorf("%s: resp = %+v, want refusal %q", name, got, c.want)
		}
	}
	if blocks, _ := listPRBlocks(db); len(blocks) != 0 {
		t.Fatalf("pr blocks = %v after refusals, want none", blocks)
	}
	if _, got := post(blockPRReq("127.0.0.1:5555", "a", "7")); !got.OK {
		t.Fatalf("block on #7: resp = %+v, want ok", got)
	}
	if blocks, _ := listPRBlocks(db); !slices.Equal(blocks, []PRBlock{{"a", "o/r", 7}}) {
		t.Errorf("pr blocks = %v, want a on o/r#7", blocks)
	}
}
