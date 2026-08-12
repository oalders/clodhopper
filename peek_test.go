package main

import (
	"net/http"
	"net/http/httptest"
	"os/exec"
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
	handlePane(rec, req, peek, time.Unix(1_700_000_000, 0))
	if rec.Code != http.StatusNotFound {
		t.Errorf("unknown live pane = %d, want 404", rec.Code)
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
