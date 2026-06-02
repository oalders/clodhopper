package main

import (
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSummarizeChecks(t *testing.T) {
	cases := []struct {
		name    string
		buckets []string
		want    string
	}{
		{"none", nil, ""},
		{"all pass", []string{"pass", "pass"}, "green"},
		{"pass and skip", []string{"pass", "skipping"}, "green"},
		{"any fail wins", []string{"pass", "pending", "fail"}, "failing"},
		{"cancel is failing", []string{"pass", "cancel"}, "failing"},
		{"pending holds amber", []string{"pass", "pending"}, "pending"},
	}
	for _, c := range cases {
		if got := summarizeChecks(c.buckets); got != c.want {
			t.Errorf("%s: summarizeChecks(%v) = %q, want %q", c.name, c.buckets, got, c.want)
		}
	}
}

func TestDeriveStatus(t *testing.T) {
	cases := []struct {
		event      string
		idleSecs   int
		wantLabel  string
		wantActive bool
	}{
		{"Stop", 0, statusWaiting, true},
		{"Stop", 6 * 60, statusWaiting, true}, // a finished agent stays "waiting", not idle
		{"Notification", 0, statusNeedsYou, true},
		{"PermissionRequest", 0, statusApproval, true},
		{"PreToolUse", 0, statusWorking, true},
		{"PreToolUse", staleWorkingSecs - 1, statusWorking, true}, // just under threshold
		{"PreToolUse", staleWorkingSecs, statusIdle, true},        // silent past threshold → idle
		{"SessionEnd", 0, statusEnded, false},
	}
	for _, c := range cases {
		label, _, active := deriveStatus(c.event, c.idleSecs)
		if label != c.wantLabel || active != c.wantActive {
			t.Errorf("deriveStatus(%q, %d) = (%q,%v), want (%q,%v)", c.event, c.idleSecs, label, active, c.wantLabel, c.wantActive)
		}
	}
}

func TestAgentRoster_DerivesStateAndSorts(t *testing.T) {
	db, err := openDB(filepath.Join(t.TempDir(), "events.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	now := time.Now().UTC()
	at := func(mins int) string { return now.Add(time.Duration(-mins) * time.Minute).Format(time.RFC3339) }
	ins := func(ts, sess, branch, etype, tool, summary string) {
		insertEvent(db, Event{TS: ts, SourceApp: "myapp", Branch: branch, SessionID: sess, EventType: etype, ToolName: tool, Summary: summary, PayloadJSON: "{}"})
	}

	// busy: knows its phase from a Skill event, then a later tool call.
	ins(at(10), "s-busy", "fix-2480", "PreToolUse", "Skill", "Skill: monitor-ci")
	ins(at(2), "s-busy", "fix-2480", "PreToolUse", "Bash", "Bash: gh pr checks")
	// waiting: last event is Stop, idle a while.
	ins(at(9), "s-wait", "fix-2499", "PreToolUse", "Skill", "Skill: address-gh-review")
	ins(at(4), "s-wait", "fix-2499", "Stop", "", "Stop")
	// idle: mid-flight but silent past the staleness threshold (no Stop arrived).
	ins(at(8), "s-idle", "fix-3000", "PreToolUse", "Bash", "Bash: go build")
	// ended: should be excluded.
	ins(at(3), "s-done", "fix-1111", "SessionEnd", "", "SessionEnd")

	agents, err := agentRoster(db, 16*time.Hour, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(agents) != 3 {
		t.Fatalf("want 3 live agents (ended excluded), got %d: %+v", len(agents), agents)
	}
	// Order is purely by idle time: most recently active first, longest-idle
	// last. s-busy (2m) < s-wait (4m) < s-idle (8m), regardless of status.
	if agents[0].SessionID != "s-busy" || agents[0].Status != statusWorking {
		t.Errorf("expected s-busy/working first (least idle), got %+v", agents[0])
	}
	if agents[0].Doing != "monitor-ci" {
		t.Errorf("busy agent phase should persist from skill: want monitor-ci, got %q", agents[0].Doing)
	}
	if !agents[0].DoingActive {
		t.Errorf("working agent's phase should be active (in progress), got DoingActive=false")
	}
	if agents[1].SessionID != "s-wait" || agents[1].Status != statusWaiting {
		t.Errorf("expected s-wait/waiting second, got %+v", agents[1])
	}
	if agents[1].Doing != "address-gh-review" {
		t.Errorf("waiting agent phase: want address-gh-review, got %q", agents[1].Doing)
	}
	if agents[1].DoingActive {
		t.Errorf("waiting agent's phase is the last completed thing, not active; want DoingActive=false")
	}
	if agents[2].SessionID != "s-idle" || agents[2].Status != statusIdle {
		t.Errorf("expected s-idle/idle last (longest idle), got %+v", agents[2])
	}
	if agents[0].Idle == "" {
		t.Errorf("idle label not set")
	}
}

func TestAgentRoster_StaleWorkingBecomesIdle(t *testing.T) {
	db, err := openDB(filepath.Join(t.TempDir(), "events.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	now := time.Now().UTC()
	at := func(mins int) string { return now.Add(time.Duration(-mins) * time.Minute).Format(time.RFC3339) }
	ins := func(ts, sess, etype, tool string) {
		insertEvent(db, Event{TS: ts, SourceApp: "myapp", Branch: "fix-1", SessionID: sess, EventType: etype, ToolName: tool, PayloadJSON: "{}"})
	}

	// Mid-flight (no Stop) but silent for 6 min: working has gone stale → idle.
	ins(at(6), "s-stale", "PreToolUse", "Bash")
	// Mid-flight and recent: genuinely working.
	ins(at(1), "s-busy", "PreToolUse", "Bash")

	agents, err := agentRoster(db, 30*time.Minute, now)
	if err != nil {
		t.Fatal(err)
	}
	byID := map[string]Agent{}
	for _, a := range agents {
		byID[a.SessionID] = a
	}
	if got := byID["s-stale"].Status; got != statusIdle {
		t.Errorf("silent working session should read %q, got %q", statusIdle, got)
	}
	if got := byID["s-busy"].Status; got != statusWorking {
		t.Errorf("recently-active session should read %q, got %q", statusWorking, got)
	}
}

func TestHandleDashboard_RendersAgentBoard(t *testing.T) {
	db, _ := openDB(filepath.Join(t.TempDir(), "events.db"))
	defer db.Close()
	now := time.Now().UTC()
	recent := now.Add(-2 * time.Minute).Format(time.RFC3339)
	insertEvent(db, Event{TS: now.Add(-5 * time.Minute).Format(time.RFC3339), SourceApp: "myapp", Branch: "fix-2499", SessionID: "sess-deadbeef-1", EventType: "PreToolUse", ToolName: "Skill", Summary: "Skill: address-gh-review", PayloadJSON: "{}"})
	insertEvent(db, Event{TS: recent, SourceApp: "myapp", Branch: "fix-2499", SessionID: "sess-deadbeef-1", EventType: "Stop", Summary: "Stop", PayloadJSON: "{}"})

	body := getBody(t, db, "/")
	for _, want := range []string{"Agents (live", "fix-2499", "waiting for you", "address-gh-review", `class="alert"`} {
		if !strings.Contains(body, want) {
			t.Errorf("board missing %q in:\n%s", want, body)
		}
	}
	// The session has stopped, so its phase is the last *completed* thing and
	// renders italicised rather than as work in progress.
	if !strings.Contains(body, `<em title="last completed">address-gh-review</em>`) {
		t.Errorf("a stopped agent's phase should render italicised in:\n%s", body)
	}
}

func TestHandleDashboard_RefreshConfigurable(t *testing.T) {
	db, _ := openDB(filepath.Join(t.TempDir(), "events.db"))
	defer db.Close()

	// The cadence now drives the JS poller (read from data-refresh), not a
	// meta-refresh tag.
	on := getBody(t, db, "/")
	if !strings.Contains(on, `data-refresh="5"`) {
		t.Errorf("default cadence should be 5s:\n%s", on)
	}
	custom := getBody(t, db, "/?refresh=30")
	if !strings.Contains(custom, `data-refresh="30"`) {
		t.Errorf("refresh=30 must set the poll interval:\n%s", custom)
	}
	off := getBody(t, db, "/?refresh=0")
	// A hard meta-refresh is gone entirely; off means the poller never starts.
	if strings.Contains(off, `http-equiv="refresh"`) {
		t.Errorf("the dashboard must never use a meta-refresh tag:\n%s", off)
	}
	if !strings.Contains(off, `data-refresh="0"`) {
		t.Errorf("refresh=0 must disable the poller:\n%s", off)
	}
	if !strings.Contains(off, "auto-refresh off") {
		t.Errorf("off state not shown in meta line")
	}
}

func TestRefreshOptions_IncludesCustomValue(t *testing.T) {
	opts := refreshOptions(15) // 15 is not a preset
	var has15, sel15 bool
	for _, o := range opts {
		if o.Secs == 15 {
			has15 = true
			sel15 = o.Selected
		}
	}
	if !has15 || !sel15 {
		t.Errorf("custom value 15 should be present and selected: %+v", opts)
	}
}

func TestBuildSummary_Skill(t *testing.T) {
	p := map[string]any{"tool_input": map[string]any{"skill": "fix-gh-issue"}}
	if got := buildSummary("PreToolUse", "Skill", p); got != "Skill: fix-gh-issue" {
		t.Errorf("skill summary = %q, want Skill: fix-gh-issue", got)
	}
}

func TestShortTS(t *testing.T) {
	now := time.Date(2026, 5, 31, 20, 0, 0, 0, time.UTC)
	// Same UTC day: time only, no redundant date.
	if got := shortTS("2026-05-31T14:14:28Z", now); got != "14:14:28" {
		t.Errorf("shortTS same-day = %q, want 14:14:28", got)
	}
	// An earlier day keeps MM-DD so it is not mistaken for today.
	if got := shortTS("2026-05-30T14:14:28Z", now); got != "05-30 14:14:28" {
		t.Errorf("shortTS other-day = %q, want 05-30 14:14:28", got)
	}
	if got := shortTS("not-a-time", now); got != "not-a-time" {
		t.Errorf("shortTS passthrough = %q, want not-a-time", got)
	}
}

func TestShortID(t *testing.T) {
	if got := shortID("abcdef0123456789"); got != "abcdef01" {
		t.Errorf("shortID = %q, want abcdef01", got)
	}
	if got := shortID("short"); got != "short" {
		t.Errorf("shortID passthrough = %q, want short", got)
	}
}

func TestAgentRoster_RetainsUntilCapAndRelabelsStale(t *testing.T) {
	db, err := openDB(filepath.Join(t.TempDir(), "events.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	now := time.Now().UTC()
	at := func(mins int) string { return now.Add(time.Duration(-mins) * time.Minute).Format(time.RFC3339) }
	ins := func(ts, sess, branch, etype, tool, summary string) {
		insertEvent(db, Event{TS: ts, SourceApp: "myapp", Branch: branch, SessionID: sess, EventType: etype, ToolName: tool, Summary: summary, PayloadJSON: "{}"})
	}

	// Every live session within waitingCap is kept: a worker that goes quiet is
	// relabeled idle rather than dropped, so only the cap (via the query window)
	// removes a session.
	// Waiting 90m ago: within the 2h cap -> stays, still waiting for you.
	ins(at(90), "s-wait", "b1", "Stop", "", "Stop")
	// Working 2m ago: recent -> still working.
	ins(at(2), "s-work-live", "b2", "PreToolUse", "Bash", "Bash: go test")
	// Working 45m ago: silent past staleWorkingSecs -> idle, but kept on the board.
	ins(at(45), "s-work-stale", "b3", "PreToolUse", "Bash", "Bash: go test")
	// Waiting 200m ago: beyond the 2h cap -> not even fetched -> drops.
	ins(at(200), "s-old", "b4", "Stop", "", "Stop")

	agents, err := agentRoster(db, 2*time.Hour, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(agents) != 3 {
		t.Fatalf("want the in-cap waiter plus both workers (stale relabeled, not dropped), got %d: %+v", len(agents), agents)
	}
	survived := map[string]string{}
	for _, a := range agents {
		survived[a.SessionID] = a.Status
	}
	if survived["s-wait"] != statusWaiting {
		t.Errorf("expected s-wait/waiting to survive, got %+v", agents)
	}
	if survived["s-work-live"] != statusWorking {
		t.Errorf("expected recent worker to stay working, got %+v", agents)
	}
	if survived["s-work-stale"] != statusIdle {
		t.Errorf("expected stale worker to be relabeled idle and kept, got %+v", agents)
	}
	if _, ok := survived["s-old"]; ok {
		t.Errorf("expected s-old beyond the cap to drop, got %+v", agents)
	}
}
