package main

import (
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
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
	c.set = map[string]string{"%3": "sess"}
	c.at = base
	if !c.live("%3", base.Add(paneTTL-time.Second)) {
		t.Error("within TTL: %3 should read live from the cached set")
	}
	if c.live("%9", base.Add(time.Second)) {
		t.Error("within TTL: %9 not in cached set, should be false")
	}
}

// dropDeadPanes hides a row only on positive evidence its pane is gone, and
// hides nothing when the listing cannot be trusted to describe the roster's
// tmux server (empty, or sharing none of its live panes).
func TestDropDeadPanes(t *testing.T) {
	live := Agent{SessionID: "live", TmuxSession: "work", TmuxPane: "%1"}
	gone := Agent{SessionID: "gone", TmuxSession: "work", TmuxPane: "%2"}
	reused := Agent{SessionID: "reused", TmuxSession: "work", TmuxPane: "%3"}
	noSess := Agent{SessionID: "nosess", TmuxPane: "%3"}
	noPane := Agent{SessionID: "nopane", TmuxSession: "work"}
	cases := []struct {
		name   string
		agents []Agent
		panes  map[string]string
		want   []string
	}{
		{"pane absent is hidden", []Agent{live, gone}, map[string]string{"%1": "work"}, []string{"live"}},
		{"pane reused by another session is hidden", []Agent{live, reused}, map[string]string{"%1": "work", "%3": "other"}, []string{"live"}},
		{"row without a recorded session matches on pane alone", []Agent{live, noSess}, map[string]string{"%1": "work", "%3": "other"}, []string{"live", "nosess"}},
		{"row without a pane is kept", []Agent{live, noPane}, map[string]string{"%1": "work"}, []string{"live", "nopane"}},
		{"empty listing hides nothing", []Agent{live, gone}, map[string]string{}, []string{"live", "gone"}},
		{"listing unrelated to the roster hides nothing", []Agent{gone, reused}, map[string]string{"%3": "other", "%9": "x"}, []string{"gone", "reused"}},
	}
	for _, c := range cases {
		var got []string
		for _, a := range dropDeadPanes(slices.Clone(c.agents), c.panes) {
			got = append(got, a.SessionID)
		}
		if !slices.Equal(got, c.want) {
			t.Errorf("%s: kept %q, want %q", c.name, got, c.want)
		}
	}
}

// listPanes maps each pane id to its session name in the form ingest stores, so
// a messy real-world name compares equal to the row's recorded tmux_session.
func TestListPanes(t *testing.T) {
	dir := t.TempDir()
	messy := "\ue0a0  fix-2482     Blog post [in progress]"
	script := "#!/bin/sh\nprintf '%s\\t%s\\n' '%1' 'my-session' '%7' '" + messy + "'\n"
	if err := os.WriteFile(filepath.Join(dir, "tmux"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	want := map[string]string{"%1": "my-session", "%7": tmuxSessionLabel(messy)}
	if got := listPanes(); !maps.Equal(got, want) {
		t.Fatalf("listPanes() = %q, want %q", got, want)
	}
	if want["%7"] != "fix-2482 Blog post [in progress]" {
		t.Errorf("messy name stored as %q, want the cleaned name", want["%7"])
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
	peek.cache.set = map[string]string{}
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
		p.cache.set = map[string]string{"%3": "sess"}
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
		p.cache.set = map[string]string{"%3": "sess"}
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

// Only github.com origins resolve, and the result is spliced into an href, so
// anything outside the strict owner/repo shape must fail closed to "".
func TestParseGitHubRepo(t *testing.T) {
	cases := map[string]string{
		"git@github.com:oalders/clodhopper.git":        "oalders/clodhopper",
		"git@github.com:oalders/clodhopper":            "oalders/clodhopper",
		"ssh://git@github.com/oalders/clod.hopper.git": "oalders/clod.hopper",
		"https://github.com/oalders/clodhopper.git":    "oalders/clodhopper",
		"https://github.com/oalders/clodhopper":        "oalders/clodhopper",
		"https://gitlab.com/oalders/clodhopper.git":    "",
		"https://github.com.evil.com/o/r":              "",
		"https://github.com/oalders/clodhopper/":       "",
		"https://github.com/o/r\"><script>":            "",
		"https://github.com/o/..":                      "",
		"https://github.com/o/r/pull/1":                "",
		"":                                             "",
	}
	for in, want := range cases {
		if got := parseGitHubRepo(in); got != want {
			t.Errorf("parseGitHubRepo(%q) = %q, want %q", in, got, want)
		}
	}
}
