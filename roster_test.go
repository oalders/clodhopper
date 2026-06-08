package main

import (
	"database/sql"
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
		name       string
		event      string
		notifType  string
		lastTool   string
		idleSecs   int
		wantLabel  string
		wantActive bool
	}{
		{"stop waits", "Stop", "", "", 0, statusWaiting, true},
		{"stop stays waiting not idle", "Stop", "", "", 6 * 60, statusWaiting, true},
		// Notification is overloaded; notification_type disambiguates it.
		{"permission prompt needs approval", "Notification", "permission_prompt", "", 0, statusApproval, true},
		{"AskUserQuestion prompt needs input not approval", "Notification", "permission_prompt", "AskUserQuestion", 0, statusInput, true},
		{"AskUserQuestion request needs input not approval", "PermissionRequest", "", "AskUserQuestion", 0, statusInput, true},
		{"idle reminder is not urgent", "Notification", "idle_prompt", "Read", 0, statusWaiting, true},
		{"idle reminder while parked on a wakeup", "Notification", "idle_prompt", "ScheduleWakeup", 0, statusBackground, true},
		{"unknown notification stays conservative", "Notification", "", "Bash", 0, statusNeedsYou, true},
		{"permission request needs approval", "PermissionRequest", "", "", 0, statusApproval, true},
		{"working", "PreToolUse", "", "", 0, statusWorking, true},
		{"working just under threshold", "PreToolUse", "", "", staleWorkingSecs - 1, statusWorking, true},
		{"silent past threshold goes idle", "PreToolUse", "", "", staleWorkingSecs, statusIdle, true},
		{"session end drops off", "SessionEnd", "", "", 0, statusEnded, false},
	}
	for _, c := range cases {
		label, rank, active := deriveStatus(c.event, c.notifType, c.lastTool, c.idleSecs)
		if label != c.wantLabel || active != c.wantActive {
			t.Errorf("%s: deriveStatus(%q,%q,%q,%d) = (%q,%v), want (%q,%v)", c.name, c.event, c.notifType, c.lastTool, c.idleSecs, label, active, c.wantLabel, c.wantActive)
		}
		// The parked-on-background status must never trip the alert styling
		// (StatusRank <= 1); that is the whole point of issue #31.
		if label == statusBackground && rank <= 1 {
			t.Errorf("%s: statusBackground must rank above the alert threshold, got rank %d", c.name, rank)
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

func TestAgentRoster_GroupsSameBranch(t *testing.T) {
	db, err := openDB(filepath.Join(t.TempDir(), "events.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	now := time.Now().UTC()
	at := func(mins int) string { return now.Add(time.Duration(-mins) * time.Minute).Format(time.RFC3339) }
	ins := func(ts, sess, branch, etype, tool string) {
		insertEvent(db, Event{TS: ts, SourceApp: "myapp", Branch: branch, SessionID: sess, EventType: etype, ToolName: tool, PayloadJSON: "{}"})
	}

	// Two branches, two sessions each, with INTERLEAVED idle times so a pure-idle
	// sort would scatter them: a1(1), b1(2), b2(4), a2(8).
	// Grouped, branch fix-A wins (freshest member a1=1m < b1=2m); within each group
	// the least-idle session comes first -> a1, a2, b1, b2.
	ins(at(1), "a1", "fix-A", "Stop", "")
	ins(at(8), "a2", "fix-A", "Stop", "")
	ins(at(2), "b1", "fix-B", "Stop", "")
	ins(at(4), "b2", "fix-B", "Stop", "")
	// Two branchless sessions: must each be their own group, ordered purely by
	// idle, NOT clumped together.
	ins(at(3), "n1", "", "Stop", "")
	ins(at(6), "n2", "", "Stop", "")

	agents, err := agentRoster(db, 16*time.Hour, now)
	if err != nil {
		t.Fatal(err)
	}

	pos := map[string]int{}
	for i, a := range agents {
		pos[a.SessionID] = i
	}

	// The four branch sessions keep grouped order a1, a2, b1, b2.
	branchOrder := []string{"a1", "a2", "b1", "b2"}
	for i := 1; i < len(branchOrder); i++ {
		if pos[branchOrder[i-1]] > pos[branchOrder[i]] {
			t.Errorf("branch group order wrong: %s should precede %s; got positions %v", branchOrder[i-1], branchOrder[i], pos)
		}
	}
	// a2 (same branch as a1, idle 8m) must sit right after a1, not after b1/b2,
	// proving the grouping rather than a coincidental idle order.
	if pos["a2"] != pos["a1"]+1 {
		t.Errorf("a2 should immediately follow a1 (same-branch group), got pos a1=%d a2=%d", pos["a1"], pos["a2"])
	}

	byID := map[string]Agent{}
	for _, a := range agents {
		byID[a.SessionID] = a
	}
	// GroupStart: first row of each branch group except the very first row overall.
	if byID["a1"].GroupStart {
		t.Errorf("a1 is the first row of its group; but GroupStart should reflect overall ordering (false if first row overall)")
	}
	if byID["a2"].GroupStart {
		t.Errorf("a2 is in the same group as a1, GroupStart should be false")
	}
	if !byID["b1"].GroupStart {
		t.Errorf("b1 starts a new branch group, GroupStart should be true")
	}
	if byID["b2"].GroupStart {
		t.Errorf("b2 is in the same group as b1, GroupStart should be false")
	}
	// The very first row overall must never carry GroupStart.
	if agents[0].GroupStart {
		t.Errorf("first roster row must have GroupStart=false, got %+v", agents[0])
	}

	// Branchless sessions are NOT clumped: each is its own group ordered by idle.
	// n1 (3m) is fresher than n2 (6m), so n1 precedes n2; and they are not forced
	// adjacent as a single pseudo-group — other groups may interleave around them
	// by group freshness. Assert the idle ordering holds.
	if pos["n1"] > pos["n2"] {
		t.Errorf("branchless sessions should order by idle (n1 fresher than n2), got pos n1=%d n2=%d", pos["n1"], pos["n2"])
	}
	// Each branchless session forms its own group boundary: whatever precedes it is
	// a different group, so its GroupStart is true unless it is row 0.
	for _, s := range []string{"n1", "n2"} {
		if pos[s] > 0 && !byID[s].GroupStart {
			t.Errorf("branchless session %s is its own group; GroupStart should be true when not row 0 (pos=%d)", s, pos[s])
		}
	}
}

func TestAgentRoster_LastCommandLatestPerSession(t *testing.T) {
	db, err := openDB(filepath.Join(t.TempDir(), "events.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	now := time.Now().UTC()
	at := func(mins int) string { return now.Add(time.Duration(-mins) * time.Minute).Format(time.RFC3339) }
	ins := func(ts, sess, etype, slash string) {
		insertEvent(db, Event{TS: ts, SourceApp: "myapp", Branch: "fix-45", SessionID: sess, EventType: etype, SlashCommand: slash, PayloadJSON: "{}"})
	}

	// s-cmd: ran /foo early, then a non-command event, then /bar — latest wins,
	// and the empty event in between must not clobber LastCommand.
	ins(at(10), "s-cmd", "UserPromptSubmit", "/foo")
	ins(at(8), "s-cmd", "PreToolUse", "")
	ins(at(6), "s-cmd", "UserPromptSubmit", "/bar")
	ins(at(2), "s-cmd", "PreToolUse", "")
	// s-none: never ran a slash command.
	ins(at(5), "s-none", "PreToolUse", "")

	agents, err := agentRoster(db, 16*time.Hour, now)
	if err != nil {
		t.Fatal(err)
	}
	byID := map[string]Agent{}
	for _, a := range agents {
		byID[a.SessionID] = a
	}
	if got := byID["s-cmd"].LastCommand; got != "/bar" {
		t.Errorf("s-cmd LastCommand = %q, want /bar (last non-empty wins)", got)
	}
	if got := byID["s-none"].LastCommand; got != "" {
		t.Errorf("s-none LastCommand = %q, want empty", got)
	}
}

func TestAgentRoster_NotificationTypeDisambiguates(t *testing.T) {
	db, err := openDB(filepath.Join(t.TempDir(), "events.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	now := time.Now().UTC()
	at := func(mins int) string { return now.Add(time.Duration(-mins) * time.Minute).Format(time.RFC3339) }
	ins := func(ts, sess, etype, tool, payload string) {
		insertEvent(db, Event{TS: ts, SourceApp: "myapp", Branch: "fix-31", SessionID: sess, EventType: etype, ToolName: tool, PayloadJSON: payload})
	}
	const idle = `{"notification_type":"idle_prompt"}`
	const perm = `{"notification_type":"permission_prompt"}`

	// Parked on background work: scheduled a wakeup, then got the idle reminder.
	// It will resume itself — must read as the non-urgent "waiting", not "needs you".
	ins(at(4), "s-parked", "PostToolUse", "ScheduleWakeup", "{}")
	ins(at(3), "s-parked", "Notification", "", idle)
	// Idle reminder with no parking signal: the turn genuinely ended → waiting for you.
	ins(at(6), "s-done", "PostToolUse", "Read", "{}")
	ins(at(5), "s-done", "Notification", "", idle)
	// A real permission prompt → needs approval.
	ins(at(2), "s-perm", "Notification", "", perm)
	// A Notification with no type (older Claude Code) → stays conservatively "needs you".
	ins(at(1), "s-bare", "Notification", "", "{}")

	agents, err := agentRoster(db, 16*time.Hour, now)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]Agent{}
	for _, a := range agents {
		got[a.SessionID] = a
	}
	if s := got["s-parked"]; s.Status != statusBackground || s.StatusRank <= 1 {
		t.Errorf("parked agent should be non-urgent %q (rank>1), got %q rank %d", statusBackground, s.Status, s.StatusRank)
	}
	if s := got["s-done"].Status; s != statusWaiting {
		t.Errorf("plain idle reminder should be %q, got %q", statusWaiting, s)
	}
	if s := got["s-perm"].Status; s != statusApproval {
		t.Errorf("permission prompt should be %q, got %q", statusApproval, s)
	}
	if s := got["s-bare"].Status; s != statusNeedsYou {
		t.Errorf("untyped notification should stay %q, got %q", statusNeedsYou, s)
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

func TestAgentRoster_SurfacesRebasing(t *testing.T) {
	db, err := openDB(filepath.Join(t.TempDir(), "events.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	now := time.Now().UTC()
	at := func(mins int) string { return now.Add(time.Duration(-mins) * time.Minute).Format(time.RFC3339) }
	ins := func(ts, sess, branch string, rebasing bool) {
		insertEvent(db, Event{TS: ts, SourceApp: "myapp", Branch: branch, Rebasing: rebasing, SessionID: sess, EventType: "PreToolUse", ToolName: "Bash", PayloadJSON: "{}"})
	}

	// agentRoster folds events by id ASC (insertion order), so for each session
	// the last ins() call wins — these timestamps are only for idle/sorting.
	// Mid-rebase: the latest event carries the recovered branch + rebasing signal.
	ins(at(3), "s-rebasing", "fix-7", true)
	// Was rebasing, but the latest event is back to a normal checkout — last write
	// wins, so this session is NOT flagged.
	ins(at(4), "s-finished", "fix-8", true)
	ins(at(2), "s-finished", "fix-8", false)
	// Never rebasing.
	ins(at(1), "s-normal", "fix-9", false)

	agents, err := agentRoster(db, 16*time.Hour, now)
	if err != nil {
		t.Fatal(err)
	}
	byID := map[string]Agent{}
	for _, a := range agents {
		byID[a.SessionID] = a
	}
	if a := byID["s-rebasing"]; !a.Rebasing {
		t.Errorf("mid-rebase session should surface Rebasing=true, got %+v", a)
	}
	if a := byID["s-finished"]; a.Rebasing {
		t.Errorf("session whose latest event is not rebasing should be Rebasing=false (last write wins), got %+v", a)
	}
	if a := byID["s-normal"]; a.Rebasing {
		t.Errorf("never-rebasing session should be Rebasing=false, got %+v", a)
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

func TestHandleDashboard_FlagsRebasingSession(t *testing.T) {
	db, _ := openDB(filepath.Join(t.TempDir(), "events.db"))
	defer db.Close()
	now := time.Now().UTC()
	recent := now.Add(-2 * time.Minute).Format(time.RFC3339)
	// A mid-rebase session: branch recovered, rebasing flag set.
	insertEvent(db, Event{TS: recent, SourceApp: "myapp", Branch: "fix-7", Rebasing: true, SessionID: "sess-rebasing-1", EventType: "PreToolUse", ToolName: "Bash", PayloadJSON: "{}"})
	// A normal session on a plain branch, for contrast.
	insertEvent(db, Event{TS: recent, SourceApp: "myapp", Branch: "fix-8", SessionID: "sess-normal-1", EventType: "PreToolUse", ToolName: "Bash", PayloadJSON: "{}"})

	body := getBody(t, db, "/")
	// Rebasing branch renders italicised with a 🚧 marker so it is scannable as
	// "mid-rebase, not steady state". The 🚧 carries an aria-label so screen
	// readers announce "mid-rebase", not "construction sign"; the italics are a
	// visual-only .rebase-branch class, not <em>, so they add no announcement noise.
	if !strings.Contains(body, `<span role="img" aria-label="mid-rebase" title="mid-rebase">🚧</span> <span class="rebase-branch">fix-7</span>`) {
		t.Errorf("rebasing session should render branch italic + accessible 🚧 in:\n%s", body)
	}
	// A non-rebasing branch stays plain (no .rebase-branch styling, no 🚧 on its name).
	if strings.Contains(body, `<span class="rebase-branch">fix-8</span>`) || strings.Contains(body, `aria-label="mid-rebase">🚧</span> <span class="rebase-branch">fix-8`) {
		t.Errorf("non-rebasing branch should render plain, not italic/🚧 in:\n%s", body)
	}
}

// TestHandleDashboard_FlagsRebasingEverywhere verifies the rebase marker reaches
// all three branch-rendering tables — the roster, the Activity tally, and the
// Recent events feed — not just the roster. A single mid-rebase session produces
// exactly one marker in each, so the whole dashboard reads consistently.
func TestHandleDashboard_FlagsRebasingEverywhere(t *testing.T) {
	db, _ := openDB(filepath.Join(t.TempDir(), "events.db"))
	defer db.Close()
	now := time.Now().UTC()
	recent := now.Add(-2 * time.Minute).Format(time.RFC3339)
	// One mid-rebase session and one plain one, each its own tmux session so they
	// form distinct roster rows and activity groups.
	insertEvent(db, Event{TS: recent, SourceApp: "myapp", Branch: "fix-7", Rebasing: true, SessionID: "sess-rebasing-1", TmuxSession: "tmux-r", EventType: "PreToolUse", ToolName: "Bash", PayloadJSON: "{}"})
	insertEvent(db, Event{TS: recent, SourceApp: "myapp", Branch: "fix-8", SessionID: "sess-normal-1", TmuxSession: "tmux-n", EventType: "PreToolUse", ToolName: "Bash", PayloadJSON: "{}"})

	body := getBody(t, db, "/")
	// Roster row + Activity tally + Recent events row = three markers for the one
	// rebasing branch; fix-8 contributes none.
	if n := strings.Count(body, `aria-label="mid-rebase"`); n != 3 {
		t.Errorf("want the rebase marker in all 3 tables (roster + activity + events), got %d markers in:\n%s", n, body)
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

func TestHandleDashboard_FoldsToolEvents(t *testing.T) {
	db, _ := openDB(filepath.Join(t.TempDir(), "events.db"))
	defer db.Close()
	now := time.Now().UTC().Format(time.RFC3339)
	insertEvent(db, Event{TS: now, SourceApp: "app", SessionID: "s", EventType: "PreToolUse",
		ToolName: "Bash", Summary: "Bash: go build", ToolUseID: "t1", PayloadJSON: "{}"})
	insertEvent(db, Event{TS: now, SourceApp: "app", SessionID: "s", EventType: "PostToolUse",
		ToolName: "Bash", Summary: "Bash: go build", ToolUseID: "t1",
		DurationMs: sql.NullInt64{Int64: 3100, Valid: true}, PayloadJSON: "{}"})

	body := getBody(t, db, "/")
	if !strings.Contains(body, "+3.1s") {
		t.Errorf("expected duration suffix +3.1s in:\n%s", body)
	}
	if !strings.Contains(body, "✓") {
		t.Errorf("expected success glyph in:\n%s", body)
	}
	if n := strings.Count(body, "Bash: go build"); n != 1 {
		t.Errorf("tool call should fold to ONE row, summary appeared %d times:\n%s", n, body)
	}
}

func TestHandleDashboard_FailedToolCallIsTinted(t *testing.T) {
	db, _ := openDB(filepath.Join(t.TempDir(), "events.db"))
	defer db.Close()
	now := time.Now().UTC().Format(time.RFC3339)
	insertEvent(db, Event{TS: now, SourceApp: "app", SessionID: "s", EventType: "PreToolUse",
		ToolName: "Bash", Summary: "Bash: false", ToolUseID: "tf", PayloadJSON: "{}"})
	insertEvent(db, Event{TS: now, SourceApp: "app", SessionID: "s", EventType: "PostToolUseFailure",
		ToolName: "Bash", Summary: "Bash: false", ToolUseID: "tf",
		DurationMs: sql.NullInt64{Int64: 74, Valid: true}, PayloadJSON: "{}"})

	body := getBody(t, db, "/")
	// A failed call folds to a ✗ row carrying the fail tint and its duration.
	if !strings.Contains(body, "✗") {
		t.Errorf("expected failure glyph in:\n%s", body)
	}
	if !strings.Contains(body, `class="fail"`) {
		t.Errorf("a failed tool call should render with class=\"fail\" in:\n%s", body)
	}
	if !strings.Contains(body, "+74ms") {
		t.Errorf("expected duration suffix +74ms in:\n%s", body)
	}
}
