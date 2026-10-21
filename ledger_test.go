package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"
)

var ledgerNow = time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)

// testLedger is a ledger whose final-state lookup answers from finals and logs
// each key it was asked for, so no test reaches gh.
func testLedger(finals map[string]prFinal) (*queueLedger, *[]string) {
	l := newQueueLedger()
	var asked []string
	l.finals.lookup = func(key string) prFinal {
		asked = append(asked, key)
		return finals[key]
	}
	return l, &asked
}

func queuedRow(id string) Agent {
	return Agent{SessionID: id, Branch: "b-" + id, CIQueue: "queued", CIQueuePos: 1, PRRepo: "o/r", PRNumber: 7, LiveTmux: true}
}

func ledgerIDs(rows []ledgerRow) []string {
	var ids []string
	for _, r := range rows {
		ids = append(ids, r.SessionID)
	}
	return ids
}

// A row is ledgered only while it is queued, and shows only once it is not.
func TestQueueLedgerRecordsQueuedRows(t *testing.T) {
	l, _ := testLedger(nil)
	a := queuedRow("s1")
	l.record([]Agent{a}, ledgerNow)
	if _, rows := l.carryOver(nil, []Agent{a}, ledgerNow); len(rows) != 0 {
		t.Fatalf("still-queued row carried over: %+v", rows)
	}
	l.record(nil, ledgerNow.Add(time.Minute))
	if _, rows := l.carryOver(nil, nil, ledgerNow.Add(time.Minute)); !slices.Equal(ledgerIDs(rows), []string{"s1"}) {
		t.Fatalf("rows = %v, want [s1]", ledgerIDs(rows))
	}
}

// A live row that left the queue because its PR merged moves from the roster to
// the ledger, with its own CI's state; no lookup runs.
func TestQueueLedgerMovesLiveMergedRowOffRoster(t *testing.T) {
	l, asked := testLedger(nil)
	l.record([]Agent{queuedRow("s1")}, ledgerNow)
	merged := Agent{SessionID: "s1", CIDone: "merged", PRRepo: "o/r", PRNumber: 7, PRMergedAt: ledgerNow.Add(-5 * time.Minute).Unix(), LiveTmux: true}
	other := Agent{SessionID: "s2"}
	roster, rows := l.carryOver([]Agent{other, merged}, nil, ledgerNow)
	if len(roster) != 1 || roster[0].SessionID != "s2" {
		t.Errorf("roster = %+v, want only s2", roster)
	}
	if len(rows) != 1 || rows[0].Final != "merged" || rows[0].MergedAgo != "5m ago" || !rows[0].LiveTmux {
		t.Errorf("rows = %+v, want s1 merged 5m ago, still live", rows)
	}
	if len(*asked) != 0 {
		t.Errorf("looked up %v for a live row", *asked)
	}
}

// A gone session renders from its snapshot, with its PR's state looked up by number.
func TestQueueLedgerLooksUpGoneSession(t *testing.T) {
	l, asked := testLedger(map[string]prFinal{"o/r#7": {State: "merged", MergedAt: ledgerNow.Add(-2 * time.Hour)}})
	l.record([]Agent{queuedRow("s1")}, ledgerNow)
	_, rows := l.carryOver(nil, nil, ledgerNow)
	if len(rows) != 1 {
		t.Fatalf("rows = %+v, want 1", rows)
	}
	r := rows[0]
	if r.Branch != "b-s1" || r.Final != "merged" || r.MergedAgo != "2h ago" || r.LiveTmux {
		t.Errorf("row = %+v, want b-s1 merged 2h ago with no live pane", r)
	}
	if !slices.Equal(*asked, []string{"o/r#7"}) {
		t.Errorf("looked up %v, want [o/r#7]", *asked)
	}
}

// A live row still open (ejected, say) stays on the roster only, and its entry
// is kept, so it shows once the session is gone.
func TestQueueLedgerLeavesOpenLiveRowOnRoster(t *testing.T) {
	l, _ := testLedger(map[string]prFinal{"o/r#7": {State: "open"}})
	l.record([]Agent{queuedRow("s1")}, ledgerNow)
	ejected := Agent{SessionID: "s1", CI: "ejected"}
	roster, rows := l.carryOver([]Agent{ejected}, nil, ledgerNow)
	if len(roster) != 1 || len(rows) != 0 {
		t.Fatalf("roster = %+v rows = %+v, want the row on the roster only", roster, rows)
	}
	if _, rows = l.carryOver(nil, nil, ledgerNow); len(rows) != 1 || rows[0].Final != "open" {
		t.Errorf("after the session went: rows = %+v, want s1 open", rows)
	}
}

// An entry lapses ledgerTTL after its row was last seen queued.
func TestQueueLedgerExpires(t *testing.T) {
	l, _ := testLedger(nil)
	l.record([]Agent{queuedRow("s1")}, ledgerNow)
	l.record(nil, ledgerNow.Add(ledgerTTL-time.Second))
	if _, rows := l.carryOver(nil, nil, ledgerNow); len(rows) != 1 {
		t.Fatalf("expired early: rows = %+v", rows)
	}
	l.record(nil, ledgerNow.Add(ledgerTTL))
	if _, rows := l.carryOver(nil, nil, ledgerNow); len(rows) != 0 {
		t.Errorf("not expired: rows = %+v", rows)
	}
}

// Past ledgerMax entries the least recently seen queued is evicted.
func TestQueueLedgerEvictsLeastRecentlySeen(t *testing.T) {
	l, _ := testLedger(nil)
	for i := range ledgerMax + 1 {
		l.record([]Agent{queuedRow(fmt.Sprintf("s%03d", i))}, ledgerNow.Add(time.Duration(i)*time.Second))
	}
	_, rows := l.carryOver(nil, nil, ledgerNow)
	if ids := ledgerIDs(rows); len(ids) != ledgerMax || slices.Contains(ids, "s000") || ids[0] != fmt.Sprintf("s%03d", ledgerMax) {
		t.Errorf("%d rows, first %q; want %d, newest first, without s000", len(ids), ids[0], ledgerMax)
	}
}

// Dismissing drops the entry, and reports whether there was one.
func TestQueueLedgerDismiss(t *testing.T) {
	l, _ := testLedger(nil)
	l.record([]Agent{queuedRow("s1")}, ledgerNow)
	if !l.dismiss("s1") || l.dismiss("s1") {
		t.Fatal("dismiss did not report the entry once")
	}
	if _, rows := l.carryOver(nil, nil, ledgerNow); len(rows) != 0 {
		t.Errorf("rows = %+v after dismiss", rows)
	}
}

// The dismiss action goes through handleAction's token check, 404s on an
// unknown id, and otherwise drops only the ledger entry.
func TestHandleActionDismiss(t *testing.T) {
	db := openTestDB(t)
	cfg := newActionCfg("/nonexistent/merge-pr", "/nonexistent/gh")
	cfg.ledger, _ = testLedger(nil)
	cfg.ledger.record([]Agent{queuedRow("s1")}, ledgerNow)

	w := httptest.NewRecorder()
	handleAction(w, actionReq("s1", "dismiss", false, "wrong", "127.0.0.1"), db, cfg, ledgerNow)
	if w.Code != http.StatusForbidden {
		t.Errorf("bad token: %d, want 403", w.Code)
	}
	w = httptest.NewRecorder()
	handleAction(w, actionReq("ghost", "dismiss", false, "secret", "127.0.0.1"), db, cfg, ledgerNow)
	if w.Code != http.StatusNotFound {
		t.Errorf("unknown id: %d, want 404", w.Code)
	}
	w = httptest.NewRecorder()
	handleAction(w, actionReq("s1", "dismiss", false, "secret", "127.0.0.1"), db, cfg, ledgerNow)
	if got := decodeActionJSON(t, w); w.Code != http.StatusOK || !got.OK {
		t.Fatalf("dismiss: %d %+v, want ok", w.Code, got)
	}
	if _, rows := cfg.ledger.carryOver(nil, nil, ledgerNow); len(rows) != 0 {
		t.Errorf("rows = %+v after dismiss", rows)
	}
}

// Only a valid repo and a positive number reach gh, always as --repo=.
func TestPRFinalArgs(t *testing.T) {
	if args, ok := prFinalArgs("o-1/r.x_y#42"); !ok || strings.Join(args, " ") != "pr view 42 --repo=o-1/r.x_y --json state,mergedAt" {
		t.Errorf("valid key: %v %v", args, ok)
	}
	for _, key := range []string{"#7", "o/r#0", "o/r#-1", "o/r#x", "o/r", "o/r/x#7", "o/..#7", "o r/x#7", "o/r.git#7"} {
		if args, ok := prFinalArgs(key); ok {
			t.Errorf("%q accepted as %v", key, args)
		}
	}
}

func TestParsePRFinal(t *testing.T) {
	at := time.Date(2026, 10, 21, 11, 0, 0, 0, time.UTC)
	cases := map[string]prFinal{
		`{"state":"MERGED","mergedAt":"2026-10-21T11:00:00Z"}`: {State: "merged", MergedAt: at},
		`{"state":"CLOSED","mergedAt":null}`:                   {State: "closed"},
		`{"state":"OPEN","mergedAt":null}`:                     {State: "open"},
		`{"state":"DRAFTY"}`:                                   {},
		`not json`:                                             {},
	}
	for out, want := range cases {
		if got := parsePRFinal([]byte(out)); got != want {
			t.Errorf("%s: %+v, want %+v", out, got, want)
		}
	}
}

// Ledger rows render in the merge-queue table with their state, the PR link and
// a dismiss button; a PR still open but out of the queue is an alert.
func TestDashboardRendersLedgerRows(t *testing.T) {
	merged := ledgerRow{Agent: Agent{SessionID: "m", Branch: "bm", PRRepo: "o/r", PRNumber: 7}, Final: "merged", MergedAgo: "5m ago"}
	left := ledgerRow{Agent: Agent{SessionID: "l", Branch: "bl"}, Final: "open"}
	gone := ledgerRow{Agent: Agent{SessionID: "g", Branch: "bg"}}
	html := renderDashboard(t, dashboardData{LedgerRows: []ledgerRow{merged, left, gone}, ExecEnabled: true, CSRFToken: "tok"})
	for _, want := range []string{
		"Merge queue (0) · 3 left",
		`class="done"`, `aria-label="pull request merged"`, "merged 5m ago", `href="https://github.com/o/r/pull/7"`,
		`class="alert"`, "left queue",
		"gone — state unknown",
		`data-action="dismiss"`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("missing %q", want)
		}
	}
}
