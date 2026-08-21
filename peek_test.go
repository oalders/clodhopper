package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestClampPaneLines(t *testing.T) {
	cases := []struct{ in, want int }{
		{40, 40}, {1, 1}, {2000, 2000},
		{0, paneLinesDefault}, {-5, paneLinesDefault}, {2001, paneLinesDefault},
	}
	for _, c := range cases {
		if got := clampPaneLines(c.in); got != c.want {
			t.Errorf("clampPaneLines(%d) = %d, want %d", c.in, got, c.want)
		}
	}
}

// live() memoises the pane set: it only re-lists after the TTL elapses. We can't
// fake tmux hermetically, so drive the cache directly to prove the TTL logic
// (the real list-panes call is covered by the skip-guarded integration test).
func TestPaneCache_TTL(t *testing.T) {
	c := newPaneCache()
	base := time.Unix(1_700_000_000, 0)
	c.set = map[string]bool{"%3": true}
	c.at = base
	if !c.live("%3", base.Add(paneTTL-time.Second)) {
		t.Error("within TTL: %3 should read live from the cached set")
	}
	if c.live("%9", base.Add(time.Second)) {
		t.Error("within TTL: %9 not in cached set, should be false")
	}
}

// handlePane refuses everything unless enabled.
func TestHandlePane_DisabledForbids(t *testing.T) {
	peek := &peekConfig{enabled: false, lines: 40, cache: newPaneCache()}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/pane?pane=%253", nil)
	handlePane(rec, req, peek, time.Unix(0, 0))
	if rec.Code != http.StatusForbidden {
		t.Errorf("disabled peek = %d, want 403", rec.Code)
	}
}

// A malformed pane id is rejected before any tmux call.
func TestHandlePane_RejectsBadPane(t *testing.T) {
	peek := &peekConfig{enabled: true, lines: 40, cache: newPaneCache()}
	for _, bad := range []string{"", "3", "-S", "%", "%3;rm", "abc"} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/api/pane?pane="+bad, nil)
		req.RemoteAddr = "127.0.0.1:5555" // past the peer gate; the pane id is what's under test
		handlePane(rec, req, peek, time.Unix(0, 0))
		if rec.Code != http.StatusNotFound {
			t.Errorf("pane=%q = %d, want 404", bad, rec.Code)
		}
	}
}

// A well-formed pane that is not in the live set 404s without capturing.
func TestHandlePane_UnknownPaneNotFound(t *testing.T) {
	peek := &peekConfig{enabled: true, lines: 40, cache: newPaneCache()}
	// Prime an empty live set that will not expire during the test.
	peek.cache.set = map[string]bool{}
	peek.cache.at = time.Unix(1_700_000_000, 0)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/pane?pane=%2599", nil) // %99 encoded
	req.RemoteAddr = "127.0.0.1:5555"                                       // past the peer gate
	handlePane(rec, req, peek, time.Unix(1_700_000_000, 0))
	if rec.Code != http.StatusNotFound {
		t.Errorf("unknown live pane = %d, want 404", rec.Code)
	}
}

// /api/pane execs tmux and streams live pane text, so it is peer-gated like the
// exec-backed actions: a LAN peer is refused before tmux is invoked at all, and
// a loopback peer reaches capture-pane.
func TestHandlePane_GatedOnPeer(t *testing.T) {
	dir := t.TempDir()
	log := filepath.Join(dir, "tmux.log")
	stub := filepath.Join(dir, "tmux")
	// #!/bin/sh, not /usr/bin/env: PATH below is replaced wholesale with dir, so
	// the stub must not need PATH to find its own interpreter.
	script := "#!/bin/sh\necho \"$@\" >> " + log + "\necho pane text\n"
	if err := os.WriteFile(stub, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	// PATH is REPLACED, not prefixed, so a real tmux on the host cannot be
	// reached and "the log file does not exist" means tmux truly was not run.
	t.Setenv("PATH", dir)

	newPeek := func() *peekConfig {
		p := &peekConfig{enabled: true, lines: 40, cache: newPaneCache()}
		p.cache.set = map[string]bool{"%3": true}
		p.cache.at = time.Unix(1_700_000_000, 0)
		return p
	}
	now := time.Unix(1_700_000_000, 0)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/pane?pane=%253", nil)
	req.RemoteAddr = "192.168.1.5:1234"
	handlePane(rec, req, newPeek(), now)
	if rec.Code != http.StatusForbidden {
		t.Errorf("LAN peer = %d, want 403", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "allowed network") {
		t.Errorf("body = %q, want the peer-network reason", rec.Body.String())
	}
	if _, err := os.Stat(log); err == nil {
		t.Error("tmux was invoked for a denied peer")
	}

	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/api/pane?pane=%253", nil)
	req.RemoteAddr = "127.0.0.1:5555"
	handlePane(rec, req, newPeek(), now)
	if rec.Code != http.StatusOK {
		t.Fatalf("loopback peer = %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "pane text") {
		t.Errorf("body = %q, want the captured pane text", rec.Body.String())
	}
	if _, err := os.Stat(log); err != nil {
		t.Errorf("tmux was not invoked for a loopback peer: %v", err)
	}
}

// Real tmux round-trip: list the live panes and capture one. Skipped unless the
// suite runs inside tmux with the binary present (same constraint as
// TestTmuxContext_InTmux).
func TestListAndCapturePane_InTmux(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not available")
	}
	live := listPanes()
	if len(live) == 0 {
		t.Skip("no live tmux panes (not running inside tmux)")
	}
	var pane string
	for p := range live {
		pane = p
		break
	}
	if !paneIDRe.MatchString(pane) {
		t.Fatalf("listPanes returned a non-pane-id %q", pane)
	}
	if _, ok := capturePane(pane, 5); !ok {
		t.Errorf("capturePane(%q) failed for a live pane", pane)
	}
}
