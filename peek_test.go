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

// paneReq builds a /api/pane GET from a given peer with a given Host header.
// Both matter: /api/pane runs the same gate the exec-backed actions run, so a
// request needs an allowed peer AND an allowed Host (httptest's defaults are
// neither).
func paneReq(remoteAddr, pane, host string) *http.Request {
	r := httptest.NewRequest(http.MethodGet, "/api/pane?pane="+pane, nil)
	r.RemoteAddr = remoteAddr
	r.Host = host
	return r
}

// handlePane refuses everything unless enabled.
func TestHandlePane_DisabledForbids(t *testing.T) {
	peek := &peekConfig{enabled: false, lines: 40, cache: newPaneCache(), bindHost: "127.0.0.1"}
	rec := httptest.NewRecorder()
	handlePane(rec, paneReq("127.0.0.1:5555", "%253", "127.0.0.1"), peek, time.Unix(0, 0))
	if rec.Code != http.StatusForbidden {
		t.Errorf("disabled peek = %d, want 403", rec.Code)
	}
}

// A malformed pane id is rejected before any tmux call.
func TestHandlePane_RejectsBadPane(t *testing.T) {
	peek := &peekConfig{enabled: true, lines: 40, cache: newPaneCache(), bindHost: "127.0.0.1"}
	for _, bad := range []string{"", "3", "-S", "%", "%3;rm", "abc"} {
		rec := httptest.NewRecorder()
		// Past the peer gate; the pane id is what's under test.
		handlePane(rec, paneReq("127.0.0.1:5555", bad, "127.0.0.1"), peek, time.Unix(0, 0))
		if rec.Code != http.StatusNotFound {
			t.Errorf("pane=%q = %d, want 404", bad, rec.Code)
		}
	}
}

// A well-formed pane that is not in the live set 404s without capturing.
func TestHandlePane_UnknownPaneNotFound(t *testing.T) {
	peek := &peekConfig{enabled: true, lines: 40, cache: newPaneCache(), bindHost: "127.0.0.1"}
	// Prime an empty live set that will not expire during the test.
	peek.cache.set = map[string]bool{}
	peek.cache.at = time.Unix(1_700_000_000, 0)
	rec := httptest.NewRecorder()
	// %99 encoded, from a peer that passes the gate.
	handlePane(rec, paneReq("127.0.0.1:5555", "%2599", "127.0.0.1"), peek, time.Unix(1_700_000_000, 0))
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
		p := &peekConfig{enabled: true, lines: 40, cache: newPaneCache(), bindHost: "127.0.0.1"}
		p.cache.set = map[string]bool{"%3": true}
		p.cache.at = time.Unix(1_700_000_000, 0)
		return p
	}
	now := time.Unix(1_700_000_000, 0)

	rec := httptest.NewRecorder()
	handlePane(rec, paneReq("192.168.1.5:1234", "%253", "127.0.0.1"), newPeek(), now)
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
	handlePane(rec, paneReq("127.0.0.1:5555", "%253", "127.0.0.1"), newPeek(), now)
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

// /api/pane runs the SAME gate the exec-backed actions run, not just the peer
// check: it is a GET (so no CSRF token is involved) that streams live transcript
// text, which makes DNS rebinding and proxying real exfiltration paths. A
// rebound Host or any forwarding header must 403 with tmux never invoked.
func TestHandlePane_HostAndProxyGated(t *testing.T) {
	dir := t.TempDir()
	log := filepath.Join(dir, "tmux.log")
	stub := filepath.Join(dir, "tmux")
	script := "#!/bin/sh\necho \"$@\" >> " + log + "\necho pane text\n"
	if err := os.WriteFile(stub, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	// PATH is REPLACED so "no log file" proves tmux truly never ran.
	t.Setenv("PATH", dir)

	newPeek := func() *peekConfig {
		p := &peekConfig{enabled: true, lines: 40, cache: newPaneCache(), bindHost: "127.0.0.1"}
		p.cache.set = map[string]bool{"%3": true}
		p.cache.at = time.Unix(1_700_000_000, 0)
		return p
	}
	now := time.Unix(1_700_000_000, 0)

	// A page on attacker.example that rebinds its name to the dashboard's address
	// is loopback as far as the peer gate is concerned; the Host allowlist is what
	// stops it.
	rec := httptest.NewRecorder()
	handlePane(rec, paneReq("127.0.0.1:5555", "%253", "evil.example.com"), newPeek(), now)
	if rec.Code != http.StatusForbidden {
		t.Errorf("rebound Host = %d, want 403", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "host not allowed") {
		t.Errorf("body = %q, want the Host reason", rec.Body.String())
	}

	for _, hdr := range []string{"X-Forwarded-For", "X-Real-IP", "Forwarded", "X-Forwarded-Host"} {
		rec = httptest.NewRecorder()
		req := paneReq("127.0.0.1:5555", "%253", "127.0.0.1")
		req.Header.Set(hdr, "203.0.113.9")
		handlePane(rec, req, newPeek(), now)
		if rec.Code != http.StatusForbidden {
			t.Errorf("%s: code = %d, want 403", hdr, rec.Code)
		}
		if !strings.Contains(rec.Body.String(), "through a proxy") {
			t.Errorf("%s: body = %q, want the proxy reason", hdr, rec.Body.String())
		}
	}
	if _, err := os.Stat(log); err == nil {
		t.Error("tmux was invoked for a rebound or proxied request")
	}

	// The control case: everything clean still serves the pane.
	rec = httptest.NewRecorder()
	handlePane(rec, paneReq("127.0.0.1:5555", "%253", "127.0.0.1"), newPeek(), now)
	if rec.Code != http.StatusOK {
		t.Fatalf("clean request = %d, want 200", rec.Code)
	}
	if _, err := os.Stat(log); err != nil {
		t.Errorf("tmux was not invoked for a clean request: %v", err)
	}
}
