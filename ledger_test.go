package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

var ledgerNow = time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)

// testLedger is a ledger whose final-state lookup answers from finals and logs
// each key it was asked for, so no test reaches gh.
func testLedger(finals map[string]ciResult) (*queueLedger, *[]string) {
	l := newQueueLedger()
	var asked []string
	var mu sync.Mutex // gone sessions are looked up concurrently
	l.finals.lookup = func(key string) ciResult {
		mu.Lock()
		defer mu.Unlock()
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
	merged := Agent{SessionID: "s1", Status: statusWaiting, CIDone: "merged", PRRepo: "o/r", PRNumber: 7, PRMergedAt: ledgerNow.Add(-5 * time.Minute).Unix(), LiveTmux: true}
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

// A live merged row that is busy or needs you stays on the roster with its
// merged chip, so no prompt is hidden.
func TestQueueLedgerKeepsBusyMergedRowOnRoster(t *testing.T) {
	l, _ := testLedger(nil)
	l.record([]Agent{queuedRow("s1")}, ledgerNow)
	working := Agent{SessionID: "s1", Status: statusWorking, CIDone: "merged"}
	roster, rows := l.carryOver([]Agent{working}, nil, ledgerNow)
	if len(roster) != 1 || len(rows) != 0 {
		t.Errorf("roster = %+v rows = %+v, want the row on the roster only", roster, rows)
	}
}

// A row queued again is not a ledger row, and its entry is refreshed so it
// expires ledgerTTL after this latest sighting.
func TestQueueLedgerRequeued(t *testing.T) {
	l, _ := testLedger(nil)
	a := queuedRow("s1")
	l.record([]Agent{a}, ledgerNow)
	l.record(nil, ledgerNow.Add(time.Hour))
	later := ledgerNow.Add(2 * time.Hour)
	l.record([]Agent{a}, later)
	if _, rows := l.carryOver(nil, []Agent{a}, later); len(rows) != 0 {
		t.Fatalf("re-queued row carried over: %+v", rows)
	}
	l.record(nil, ledgerNow.Add(ledgerTTL))
	if _, rows := l.carryOver(nil, nil, later); len(rows) != 1 {
		t.Errorf("re-queued entry expired by its first sighting: rows = %+v", rows)
	}
}

// A gone session renders from its snapshot, with its PR's state looked up by number.
func TestQueueLedgerLooksUpGoneSession(t *testing.T) {
	l, asked := testLedger(map[string]ciResult{"o/r#7": {Done: "merged", PRNumber: 7, MergedAt: ledgerNow.Add(-2 * time.Hour)}})
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

// A gone session's merged PR cannot change, so it is looked up once, even
// after the lookup cache's TTL; a still-open one is looked up again.
func TestQueueLedgerKeepsTerminalFinal(t *testing.T) {
	l, asked := testLedger(map[string]ciResult{"o/r#7": {Done: "merged", PRNumber: 7}, "o/r#8": {PRNumber: 8}})
	open := queuedRow("s2")
	open.PRNumber = 8
	l.record([]Agent{queuedRow("s1"), open}, ledgerNow)
	l.carryOver(nil, nil, ledgerNow)
	_, rows := l.carryOver(nil, nil, ledgerNow.Add(2*cwdTTL))
	if len(rows) != 2 || rows[0].Final != "merged" {
		t.Fatalf("rows = %+v, want s1 merged and s2", rows)
	}
	slices.Sort(*asked)
	if !slices.Equal(*asked, []string{"o/r#7", "o/r#8", "o/r#8"}) {
		t.Errorf("looked up %v, want o/r#7 once and o/r#8 twice", *asked)
	}
}

// A live row still open (ejected, say) stays on the roster only, and its entry
// is kept, so it shows once the session is gone.
func TestQueueLedgerLeavesOpenLiveRowOnRoster(t *testing.T) {
	l, _ := testLedger(map[string]ciResult{"o/r#7": {PRNumber: 7}})
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
		t.Errorf("%d rows (%.3v…); want %d, newest first, without s000", len(ids), ids, ledgerMax)
	}
}

// A final looked up for one PR is not kept on an entry re-recorded for another.
func TestQueueLedgerKeepFinalChecksPR(t *testing.T) {
	l, _ := testLedger(nil)
	l.record([]Agent{queuedRow("s1")}, ledgerNow)
	l.keepFinal("s1", "o/r#8", ciResult{Done: "merged"})
	if e := l.m["s1"]; e.final.Done != "" {
		t.Errorf("kept %+v for o/r#8 on an o/r#7 entry", e.final)
	}
}

// Through the real build, a live merged queued row that is waiting moves to
// the ledger, while one that needs you stays on the roster: carryOver must see
// the status from before demotePendingCI relaxes it.
func TestBuildDashboardDataLedgersMergedRow(t *testing.T) {
	db := openTestDB(t)
	now := time.Now()
	ts := now.UTC().Format(time.RFC3339)
	insertEvent(db, Event{TS: ts, SourceApp: "x", SessionID: "w", Cwd: "/w", EventType: "Stop", PayloadJSON: "{}"})
	insertEvent(db, Event{TS: ts, SourceApp: "x", SessionID: "n", Cwd: "/n", EventType: "Notification", PayloadJSON: "{}"})
	// Stamped an hour ahead so cwdTTL cannot lapse mid-test and gh is never run.
	ci := newCICache()
	for _, cwd := range []string{"/w", "/n"} {
		ci.m[cwd] = cwdEntry[ciResult]{val: ciResult{Done: "merged"}, at: now.Add(time.Hour)}
	}
	l, _ := testLedger(nil)
	l.record([]Agent{queuedRow("w"), queuedRow("n")}, now)
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.RemoteAddr = "127.0.0.1:5555"
	r.Host = "127.0.0.1:4555"
	data, err := buildDashboardData(r, db, ci, &peekConfig{cache: newPaneCache()}, &actionConfig{ledger: l})
	if err != nil {
		t.Fatal(err)
	}
	var roster []string
	for _, a := range data.Agents {
		roster = append(roster, a.SessionID)
	}
	if !slices.Equal(roster, []string{"n"}) || !slices.Equal(ledgerIDs(data.LedgerRows), []string{"w"}) {
		t.Errorf("roster %v, ledger %v; want [n] and [w]", roster, ledgerIDs(data.LedgerRows))
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
	if args, ok := prFinalArgs("o-1/r.x_y#42"); !ok || strings.Join(args, " ") != "pr view 42 --repo=o-1/r.x_y --json id,number,state,mergedAt" {
		t.Errorf("valid key: %v %v", args, ok)
	}
	for _, key := range []string{"#7", "o/r#0", "o/r#-1", "o/r#x", "o/r", "o/r/x#7", "o/..#7", "o r/x#7", "o/r.git#7"} {
		if args, ok := prFinalArgs(key); ok {
			t.Errorf("%q accepted as %v", key, args)
		}
	}
}

// A gone session's PR folds to its row state: an open PR still in the merge
// queue is "queued", with its position; one out of it "left" ("open").
func TestFinalStateFold(t *testing.T) {
	merged, _ := parsePRView([]byte(`{"number":7,"state":"MERGED","mergedAt":"2026-10-21T11:00:00Z"}`))
	closed, _ := parsePRView([]byte(`{"number":7,"state":"CLOSED"}`))
	open, id := parsePRView([]byte(`{"id":"PR_1","number":7,"state":"OPEN"}`))
	queued := applyMergeQueue(open, []byte(`{"data":{"node":{"mergeQueueEntry":{"state":"QUEUED","position":3}}}}`), ledgerNow)
	left := applyMergeQueue(open, []byte(`{"data":{"node":{"mergeQueueEntry":null}}}`), ledgerNow)
	unknown, _ := parsePRView([]byte(`not json`))
	if id != "PR_1" {
		t.Errorf("open PR id = %q, want PR_1 for the queue lookup", id)
	}
	for _, c := range []struct {
		r    ciResult
		want string
	}{{merged, "merged"}, {closed, "closed"}, {queued, "queued"}, {left, "open"}, {unknown, ""}} {
		if got := finalState(c.r); got != c.want {
			t.Errorf("finalState(%+v) = %q, want %q", c.r, got, c.want)
		}
	}
	if queued.QueuePos != 3 {
		t.Errorf("queued position = %d, want 3", queued.QueuePos)
	}
}

// ledgerRowHTML is the <tr> of session id's row inside the merge-queue section.
func ledgerRowHTML(t *testing.T, html, id string) string {
	t.Helper()
	_, mq, ok := strings.Cut(html, `id="ck-mq"`)
	if !ok {
		t.Fatal("no merge-queue section")
	}
	_, row, ok := strings.Cut(mq, `id="row-`+id+`"`)
	if !ok {
		t.Fatalf("no row %q in the merge-queue section", id)
	}
	tr := mq[:len(mq)-len(row)]
	tr = tr[strings.LastIndex(tr, "<tr"):]
	row, _, _ = strings.Cut(row, "</tr>")
	return tr + row
}

// Ledger rows render in the merge-queue table with their state, the PR link and
// a dismiss button; a PR still open but out of the queue is an alert.
func TestDashboardRendersLedgerRows(t *testing.T) {
	merged := ledgerRow{Agent: Agent{SessionID: "m", Branch: "bm", PRRepo: "o/r", PRNumber: 7}, Final: "merged", MergedAgo: "5m ago"}
	left := ledgerRow{Agent: Agent{SessionID: "l", Branch: "bl"}, Final: "open"}
	queued := ledgerRow{Agent: Agent{SessionID: "q", Branch: "bq", CIQueuePos: 2}, Final: "queued"}
	gone := ledgerRow{Agent: Agent{SessionID: "g", Branch: "bg"}}
	html := renderDashboard(t, dashboardData{LedgerRows: []ledgerRow{merged, left, queued, gone}, ExecEnabled: true, CSRFToken: "tok"})
	if !strings.Contains(html, `<span class="mqleft">1 left queue</span>`) {
		t.Error("heading does not show the left-queue alert")
	}
	for id, want := range map[string][]string{
		"m": {`class="done"`, `aria-label="pull request merged"`, "5m ago", `href="https://github.com/o/r/pull/7"`, `data-action="dismiss"`},
		"l": {`class="alert"`, "left queue"},
		"q": {"still queued · position 2 · session gone"},
		"g": {"gone — state unknown"},
	} {
		row := ledgerRowHTML(t, html, id)
		for _, w := range want {
			if !strings.Contains(row, w) {
				t.Errorf("row %s: missing %q in %s", id, w, row)
			}
		}
	}
	// Dismissing the only alert for a PR out of the queue is armed and confirmed.
	if row := ledgerRowHTML(t, html, "l"); strings.Contains(row, "data-noconfirm") {
		t.Errorf("left-queue row dismisses without confirm: %s", row)
	}
	if row := ledgerRowHTML(t, html, "m"); !strings.Contains(row, `data-noconfirm="true"`) {
		t.Errorf("merged row asks to confirm its dismiss: %s", row)
	}
}

// Only merged or closed rows are finished; a still-queued or unknown one is neither that nor an alert.
func TestLedgerTally(t *testing.T) {
	var d dashboardData
	for _, f := range []string{"merged", "closed", "open", "queued", ""} {
		d.LedgerRows = append(d.LedgerRows, ledgerRow{Final: f})
	}
	if got := d.LedgerTally(); got.Finished != 2 || got.Left != 1 {
		t.Errorf("tally = %+v, want 2 finished, 1 left", got)
	}
}
