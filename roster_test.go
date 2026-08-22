package main

import (
	"database/sql"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"regexp"
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
	// Each session is on a distinct branch, so each is its own singleton group;
	// group freshness therefore equals idle time and the roster orders exactly as
	// a raw idle sort would: most recently active first, longest-idle last.
	// s-busy (2m) < s-wait (4m) < s-idle (8m), regardless of status.
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
	// Each session is on its own branch, so none form a 2+ cluster: no left bar.
	for _, a := range agents {
		if a.Grouped {
			t.Errorf("session %s is a lone session on its branch; Grouped should be false", a.SessionID)
		}
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
	// sort would scatter them: a1(1), n1(2), b1(3), b2(4), n2(5), a2(8).
	// Grouped, branch fix-A wins (freshest member a1=1m < b1=3m); within each group
	// the least-idle session comes first -> a1, a2, b1, b2.
	ins(at(1), "a1", "fix-A", "Stop", "")
	ins(at(8), "a2", "fix-A", "Stop", "")
	ins(at(3), "b1", "fix-B", "Stop", "")
	ins(at(4), "b2", "fix-B", "Stop", "")
	// Two branchless sessions: must each be their own group, ordered purely by
	// idle, NOT clumped together. n1(2m) sorts between branch A and branch B, and
	// n2(5m) sorts after branch B — so branch group fix-B lands BETWEEN them,
	// proving branchless sessions are not forced into one contiguous pseudo-group.
	ins(at(2), "n1", "", "Stop", "")
	ins(at(5), "n2", "", "Stop", "")

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
	// n1 (2m) is fresher than n2 (5m), so n1 precedes n2; and they are not forced
	// adjacent as a single pseudo-group — other groups may interleave around them
	// by group freshness. Assert the idle ordering holds.
	if pos["n1"] > pos["n2"] {
		t.Errorf("branchless sessions should order by idle (n1 fresher than n2), got pos n1=%d n2=%d", pos["n1"], pos["n2"])
	}
	// They must NOT be forced into one contiguous pseudo-group: a real branch group
	// sits between them, so they are non-adjacent in the output.
	if d := pos["n1"] - pos["n2"]; d == 1 || d == -1 {
		t.Errorf("branchless sessions must not be adjacent (each is its own group); got pos n1=%d n2=%d", pos["n1"], pos["n2"])
	}
	// Concretely, at least one branch-group row sits between n1 and n2.
	lo, hi := pos["n1"], pos["n2"]
	if lo > hi {
		lo, hi = hi, lo
	}
	between := false
	for _, s := range []string{"a1", "a2", "b1", "b2"} {
		if pos[s] > lo && pos[s] < hi {
			between = true
		}
	}
	if !between {
		t.Errorf("a branch-group row should sit between the two branchless sessions; got pos %v", pos)
	}
	// Each branchless session forms its own group boundary: whatever precedes it is
	// a different group, so its GroupStart is true unless it is row 0.
	for _, s := range []string{"n1", "n2"} {
		if pos[s] > 0 && !byID[s].GroupStart {
			t.Errorf("branchless session %s is its own group; GroupStart should be true when not row 0 (pos=%d)", s, pos[s])
		}
	}

	// Grouped marks every member of a 2+ session group (drives the left accent bar).
	// Branches fix-A and fix-B each have two sessions, so all four are Grouped; the
	// branchless singletons are not.
	for _, s := range []string{"a1", "a2", "b1", "b2"} {
		if !byID[s].Grouped {
			t.Errorf("%s is in a 2-session branch group; Grouped should be true", s)
		}
	}
	for _, s := range []string{"n1", "n2"} {
		if byID[s].Grouped {
			t.Errorf("branchless singleton %s should not be Grouped", s)
		}
	}
}

// GroupKey identifies each roster row's group for the dashboard "pin order"
// toggle: same (app, branch) share a key; branchless rows are each their own
// group (distinct keys); the value is always valid hex. It must track the same
// grouping GroupStart/Grouped already express, so pinning holds clusters intact.
func TestAgentRoster_GroupKeyTracksGrouping(t *testing.T) {
	db, err := openDB(filepath.Join(t.TempDir(), "events.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	now := time.Now().UTC()
	at := func(mins int) string { return now.Add(time.Duration(-mins) * time.Minute).Format(time.RFC3339) }
	ins := func(ts, app, sess, branch string) {
		insertEvent(db, Event{TS: ts, SourceApp: app, Branch: branch, SessionID: sess, EventType: "Stop", PayloadJSON: "{}"})
	}

	// fix-A on myapp has two sessions (one group); two branchless sessions share
	// the same app but must NOT share a group.
	ins(at(1), "myapp", "a1", "fix-A")
	ins(at(2), "myapp", "a2", "fix-A")
	ins(at(3), "myapp", "n1", "")
	ins(at(4), "myapp", "n2", "")

	agents, err := agentRoster(db, 16*time.Hour, now)
	if err != nil {
		t.Fatal(err)
	}
	key := map[string]string{}
	for _, a := range agents {
		if a.GroupKey == "" {
			t.Errorf("%s has empty GroupKey", a.SessionID)
		}
		if _, err := hex.DecodeString(a.GroupKey); err != nil {
			t.Errorf("%s GroupKey %q is not valid hex: %v", a.SessionID, a.GroupKey, err)
		}
		key[a.SessionID] = a.GroupKey
	}

	// Same branch cluster shares one key (so pinning moves the block as a unit).
	if key["a1"] != key["a2"] {
		t.Errorf("same-branch sessions must share GroupKey: a1=%q a2=%q", key["a1"], key["a2"])
	}
	// Grouped rows share their key.
	if key["a1"] == key["n1"] {
		t.Errorf("branched and branchless rows must not share GroupKey")
	}
	// Two branchless sessions on the same app are distinct groups, so their keys
	// differ — else the client would clump them into one block.
	if key["n1"] == key["n2"] {
		t.Errorf("branchless sessions must have distinct GroupKeys: n1=%q n2=%q", key["n1"], key["n2"])
	}
}

// Two sessions on the SAME branch but DIFFERENT source apps must NOT group:
// worktrees are per-project, so groupKey folds SourceApp into the branched key.
// Guards that invariant against a future refactor of the key structure.
func TestAgentRoster_SameBranchDifferentAppNotGrouped(t *testing.T) {
	db, err := openDB(filepath.Join(t.TempDir(), "events.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	now := time.Now().UTC()
	at := func(mins int) string { return now.Add(time.Duration(-mins) * time.Minute).Format(time.RFC3339) }
	ins := func(ts, app, sess string) {
		insertEvent(db, Event{TS: ts, SourceApp: app, Branch: "main", SessionID: sess, EventType: "Stop", PayloadJSON: "{}"})
	}

	// Same branch ("main"), two different apps, one session each.
	ins(at(1), "app-a", "sa")
	ins(at(2), "app-b", "sb")

	agents, err := agentRoster(db, 16*time.Hour, now)
	if err != nil {
		t.Fatal(err)
	}

	byID := map[string]Agent{}
	for _, a := range agents {
		byID[a.SessionID] = a
	}
	// Each is a singleton in its own (app, branch) group: no binding bar.
	for _, s := range []string{"sa", "sb"} {
		if byID[s].Grouped {
			t.Errorf("%s shares a branch but not an app; it must not be Grouped", s)
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
	// s-bad: a slash command whose timestamp cannot be parsed. It must leave
	// LastCommandSince at 0 (an age of now.Unix(), i.e. far outside any window)
	// rather than reading as "just now" to an age-bounded consumer.
	insertEvent(db, Event{TS: "not-a-timestamp", SourceApp: "myapp", Branch: "fix-111", SessionID: "s-bad", EventType: "UserPromptSubmit", SlashCommand: "/poll-ci", PayloadJSON: "{}"})

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

// TestHandleDashboard_GroupClusterClasses verifies the grouping flags reach the
// rendered HTML as the right CSS classes — the gap between the logic (unit-tested
// in TestAgentRoster_GroupsSameBranch) and the template that consumes it. It
// mirrors the real-world case that motivated the left accent bar: a two-session
// branch cluster where one member is in an alert state and the other is idle, plus
// a separate singleton branch.
func TestHandleDashboard_GroupClusterClasses(t *testing.T) {
	db, _ := openDB(filepath.Join(t.TempDir(), "events.db"))
	defer db.Close()
	now := time.Now().UTC()
	at := func(mins int) string { return now.Add(time.Duration(-mins) * time.Minute).Format(time.RFC3339) }
	// Cluster "shared": a freshly-stopped member (waiting for you -> alert) and an
	// idle member (last tool call 30m ago, past the 5m staleness threshold).
	insertEvent(db, Event{TS: at(1), SourceApp: "myapp", Branch: "shared", SessionID: "g-hot", EventType: "Stop", Summary: "Stop", PayloadJSON: "{}"})
	insertEvent(db, Event{TS: at(30), SourceApp: "myapp", Branch: "shared", SessionID: "g-cold", EventType: "PreToolUse", ToolName: "Bash", PayloadJSON: "{}"})
	// A singleton branch, so it must NOT get the cluster bar.
	insertEvent(db, Event{TS: at(5), SourceApp: "myapp", Branch: "solo", SessionID: "g-solo", EventType: "PreToolUse", ToolName: "Bash", PayloadJSON: "{}"})

	body := getBody(t, db, "/")

	// The alert member of the cluster carries BOTH classes: the row tint and the
	// binding bar coexist (the live case where a thin divider alone read poorly).
	if !strings.Contains(body, `class="alert grouped"`) {
		t.Errorf(`expected the alert cluster member to render class="alert grouped", in:\n%s`, body)
	}
	// Its idle sibling carries the bar alone.
	if !strings.Contains(body, `class="grouped"`) {
		t.Errorf(`expected the idle cluster member to render class="grouped", in:\n%s`, body)
	}
	// Each roster row carries data-group (the hex group key) so the "pin order"
	// toggle can hold each group's rows together. The two cluster members share
	// one value; the singleton differs; there are three distinct rows total.
	groups := regexp.MustCompile(`data-group="([0-9a-f]*)"`).FindAllStringSubmatch(body, -1)
	if len(groups) != 3 {
		t.Fatalf("expected 3 roster rows carrying data-group, got %d, in:\n%s", len(groups), body)
	}
	if groups[0][1] != groups[1][1] {
		t.Errorf("the two cluster members must share one data-group, got %q and %q", groups[0][1], groups[1][1])
	}
	if groups[2][1] == groups[0][1] {
		t.Errorf("the singleton must have a distinct data-group from the cluster, got %q", groups[2][1])
	}
	// Exactly the two cluster members are marked grouped; the singleton is not.
	// Count the class-attribute terminator `grouped"` rather than the bare word, so
	// the `tr.grouped` rules in the <style> block don't inflate the tally.
	if n := strings.Count(body, `grouped"`); n != 2 {
		t.Errorf("expected exactly 2 grouped rows (the cluster), got %d, in:\n%s", n, body)
	}
	// The visual binding bar has a screen-reader equivalent: grouped rows' branch
	// cell ends in a visually-hidden " (cluster)" so assistive tech announces the
	// cluster the bar conveys visually. It is a span inside the cell, not an
	// aria-label on it, so it composes with the cell's contents (the branch name,
	// and the copy button where there is a cwd) instead of replacing them. Both
	// cluster members get it; the singleton (no bar) gets none — with three rows
	// rendered, a count of 2 is what pins that.
	if n := strings.Count(body, `<span class="ck-sr"> (cluster)</span>`); n != 2 {
		t.Errorf("expected exactly 2 cluster suffixes, got %d, in:\n%s", n, body)
	}
	if strings.Contains(body, `aria-label="shared (cluster)"`) {
		t.Errorf("cluster suffix must not be an aria-label on the cell, in:\n%s", body)
	}
	// The singleton starts a new group, so it draws the inter-cluster divider.
	// Match the class-attribute terminator `group-start"` rather than the bare
	// word, so the `tr.group-start` rules in the <style> block don't make this
	// pass vacuously (same idiom as the `grouped"` count above).
	if !strings.Contains(body, `group-start"`) {
		t.Errorf("expected a group-start divider for the singleton branch, in:\n%s", body)
	}
	// Adjacency: both "shared" branch cells sit together above the "solo" one —
	// nothing splits the cluster. (The cluster floats on its freshest member at
	// 1m < the singleton's 5m.) Scope to the roster <table> only: the same branch
	// names also appear in the filter dropdown and the Recent-events table lower on
	// the page, which would pollute a whole-document search.
	roster := body
	if i := strings.Index(roster, `<table class="roster">`); i >= 0 {
		if j := strings.Index(roster[i:], "</table>"); j >= 0 {
			roster = roster[i : i+j]
		}
	}
	// Match on the branch cell's opening tag plus its first text: a grouped cell no
	// longer ends in `>name</td>` (the visually-hidden cluster suffix follows the
	// name), while the <td> itself is now attribute-identical for every row. These
	// rows have no cwd, so no copy button intervenes either.
	soloCell := strings.Index(roster, `data-label="branch">solo`)
	lastSharedCell := strings.LastIndex(roster, `data-label="branch">shared`)
	if soloCell < 0 || lastSharedCell < 0 || soloCell < lastSharedCell {
		t.Errorf("the two shared-branch rows should be adjacent, above the singleton; order wrong in:\n%s", body)
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

	body := getBody(t, db, "/?debug=1")
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

func TestWindowOptions_DefaultAndCustom(t *testing.T) {
	// Default (0) renders as "all" and is selected.
	def := windowOptions(0)
	if def[0].Days != 0 || def[0].Label != "all" || !def[0].Selected {
		t.Errorf("window 0 should be the selected \"all\" option, got %+v", def[0])
	}
	// Singular vs plural labels.
	var got1, gotN string
	for _, o := range windowOptions(0) {
		switch o.Days {
		case 1:
			got1 = o.Label
		case 7:
			gotN = o.Label
		}
	}
	if got1 != "1 day" {
		t.Errorf("1-day label = %q, want \"1 day\"", got1)
	}
	if gotN != "7 days" {
		t.Errorf("7-day label = %q, want \"7 days\"", gotN)
	}
	// A hand-typed value outside the presets stays selectable.
	opts := windowOptions(3)
	var has3, sel3 bool
	for _, o := range opts {
		if o.Days == 3 {
			has3, sel3 = true, o.Selected
		}
	}
	if !has3 || !sel3 {
		t.Errorf("custom value 3 should be present and selected: %+v", opts)
	}
}

func TestHandleDashboard_WindowNarrowsRoster(t *testing.T) {
	db, _ := openDB(filepath.Join(t.TempDir(), "events.db"))
	defer db.Close()
	now := time.Now().UTC()
	at := func(h int) string { return now.Add(time.Duration(-h) * time.Hour).Format(time.RFC3339) }
	// A waiter idle 2h and another idle 5 days.
	insertEvent(db, Event{TS: at(2), SourceApp: "myapp", Branch: "recent-br", SessionID: "s-recent", EventType: "Stop", Summary: "Stop", PayloadJSON: "{}"})
	insertEvent(db, Event{TS: at(120), SourceApp: "myapp", Branch: "longidle-br", SessionID: "s-old", EventType: "Stop", Summary: "Stop", PayloadJSON: "{}"})

	// rosterIDs returns the set of session ids on the roster for a given URL.
	rosterIDs := func(target string) map[string]bool {
		req := httptest.NewRequest(http.MethodGet, target, nil)
		data, err := buildDashboardData(req, db, newCICache(), &peekConfig{cache: newPaneCache()}, &actionConfig{})
		if err != nil {
			t.Fatal(err)
		}
		ids := map[string]bool{}
		for _, a := range data.Agents {
			ids[a.SessionID] = true
		}
		return ids
	}

	// Default view (full cap) keeps both sessions on the roster.
	if full := rosterIDs("/"); !full["s-recent"] || !full["s-old"] {
		t.Errorf("default window should show both agents, got %v", full)
	}
	// window=1 narrows the roster to the last day: the 5-day-idle session drops,
	// the recent one stays. (The narrowing is roster-only; the events table and
	// branch filter intentionally still see every branch.)
	if narrow := rosterIDs("/?window=1"); !narrow["s-recent"] || narrow["s-old"] {
		t.Errorf("window=1 should keep only the 2h-idle agent, got %v", narrow)
	}

	// The selector renders and the chosen window round-trips as the selected option.
	body := getBody(t, db, "/?window=1")
	if !strings.Contains(body, `name="window"`) {
		t.Errorf("roster-window selector missing from controls:\n%s", body)
	}
	if !strings.Contains(body, `<option value="1" selected>1 day</option>`) {
		t.Errorf("window=1 should be the selected option:\n%s", body)
	}
}

// A window that meets or exceeds the configured cap narrows nothing, so it must
// collapse back to "all" (WindowDays 0) rather than presenting a preset that is
// a silent no-op. The default cap is 30 days, so ?window=30 is the boundary case.
func TestHandleDashboard_WindowAtCapTreatedAsAll(t *testing.T) {
	db, _ := openDB(filepath.Join(t.TempDir(), "events.db"))
	defer db.Close()
	now := time.Now().UTC()
	at := func(h int) string { return now.Add(time.Duration(-h) * time.Hour).Format(time.RFC3339) }
	insertEvent(db, Event{TS: at(2), SourceApp: "myapp", Branch: "br", SessionID: "s1", EventType: "Stop", Summary: "Stop", PayloadJSON: "{}"})

	req := httptest.NewRequest(http.MethodGet, "/?window=30", nil)
	data, err := buildDashboardData(req, db, newCICache(), &peekConfig{cache: newPaneCache()}, &actionConfig{})
	if err != nil {
		t.Fatal(err)
	}
	if data.WindowDays != 0 {
		t.Errorf("window=30 at the 30-day default cap should collapse to 0 (all), got %d", data.WindowDays)
	}

	body := getBody(t, db, "/?window=30")
	if !strings.Contains(body, `<option value="0" selected>all</option>`) {
		t.Errorf("window=30 at the cap should select the 'all' option:\n%s", body)
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

// Regression for #49: a genuinely alive agent waiting on you but idle longer
// than the old 16h cap must still appear on the roster under the default cap.
// Driven through waitingRetainHours() so it pins the default, not a literal.
func TestAgentRoster_DefaultCapKeepsLongIdleWaiter(t *testing.T) {
	db, err := openDB(filepath.Join(t.TempDir(), "events.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	now := time.Now().UTC()
	at := func(hrs int) string { return now.Add(time.Duration(-hrs) * time.Hour).Format(time.RFC3339) }
	// Waiting 20h ago: beyond the old 16h cap, well within the 720h default.
	insertEvent(db, Event{TS: at(20), SourceApp: "myapp", Branch: "b1", SessionID: "s-longidle", EventType: "Stop", Summary: "Stop", PayloadJSON: "{}"})

	capDur := time.Duration(waitingRetainHours()) * time.Hour
	agents, err := agentRoster(db, capDur, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(agents) != 1 || agents[0].SessionID != "s-longidle" {
		t.Fatalf("20h-idle live waiter should survive the default cap, got %+v", agents)
	}
	if agents[0].Status != statusWaiting {
		t.Errorf("expected long-idle waiter to stay waiting, got %q", agents[0].Status)
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

	body := getBody(t, db, "/?debug=1")
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

	body := getBody(t, db, "/?debug=1")
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

// A worktree's branch is a stable property of the session, but gitBranch is a
// best-effort shell-out that transiently returns "" under concurrent-worktree
// load (a timed-out `git symbolic-ref`). When the latest event for a session is
// one of those empty captures, the roster row must not lose the branch it has
// recorded on every other event — a last-write-wins fold would blank a live
// agent's branch intermittently. The last known non-empty branch is carried
// forward, matching how the same fold already protects slash_command and tool.
func TestRosterBranchSurvivesTransientEmptyCapture(t *testing.T) {
	db, err := openDB(testDB(t))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	now := time.Now().UTC()
	at := func(mins int) string { return now.Add(time.Duration(-mins) * time.Minute).Format(time.RFC3339) }

	// One session captures its branch on the first event, then the most recent
	// event comes back branchless (the transient git failure). The row should
	// still show "fix-3653".
	insertEvent(db, Event{TS: at(3), SourceApp: "mmir", Branch: "fix-3653", Cwd: "/w/fix-3653", SessionID: "s1", EventType: "PreToolUse", ToolName: "Bash", PayloadJSON: "{}"})
	insertEvent(db, Event{TS: at(1), SourceApp: "mmir", Branch: "", Cwd: "/w/fix-3653", SessionID: "s1", EventType: "PreToolUse", ToolName: "Bash", PayloadJSON: "{}"})

	agents, err := agentRoster(db, 16*time.Hour, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(agents) != 1 {
		t.Fatalf("want 1 roster row, got %d", len(agents))
	}
	if agents[0].Branch != "fix-3653" {
		t.Errorf("branch lost to transient empty capture: got %q, want %q", agents[0].Branch, "fix-3653")
	}
}

// When a session's branch is unknown (a detached HEAD that is not a rebase, so
// gitBranch legitimately reports ""), the roster still needs to help the operator
// find the worktree / tmux session. BranchGuess carries the cwd's basename for
// that — the last path segment, which under the worktree layout is usually the
// branch — and stays empty when the branch is known.
func TestRosterBranchGuessFromCwd(t *testing.T) {
	db, err := openDB(testDB(t))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	now := time.Now().UTC()
	ts := now.Add(-time.Minute).Format(time.RFC3339)
	// s1: branch never captured, cwd present -> guess is the basename.
	insertEvent(db, Event{TS: ts, SourceApp: "mmir", Branch: "", Cwd: "/home/olaf/.worktree/mmir/2026-08-11/fix-3655", SessionID: "s1", EventType: "PreToolUse", ToolName: "Bash", PayloadJSON: "{}"})
	// s2: branch known -> no guess, even though cwd is present.
	insertEvent(db, Event{TS: ts, SourceApp: "mmir", Branch: "fix-3652", Cwd: "/home/olaf/.worktree/mmir/2026-08-11/fix-3652", SessionID: "s2", EventType: "PreToolUse", ToolName: "Bash", PayloadJSON: "{}"})

	agents, err := agentRoster(db, 16*time.Hour, now)
	if err != nil {
		t.Fatal(err)
	}
	byID := map[string]Agent{}
	for _, a := range agents {
		byID[a.SessionID] = a
	}
	if got := byID["s1"].BranchGuess; got != "fix-3655" {
		t.Errorf("branchless session: BranchGuess = %q, want %q", got, "fix-3655")
	}
	if got := byID["s2"].BranchGuess; got != "" {
		t.Errorf("branch known: BranchGuess should be empty, got %q", got)
	}
}

// A session whose branch git never resolved (detached HEAD, not a rebase) still
// needs to be locatable, so the roster falls back to the cwd's basename, rendered
// italic via a .branch-guess span with a title that says it is a directory hint
// rather than a confirmed branch. A session with a real branch shows no such span.
func TestHandleDashboard_BranchGuessWhenBranchUnknown(t *testing.T) {
	db, _ := openDB(filepath.Join(t.TempDir(), "events.db"))
	defer db.Close()
	now := time.Now().UTC()
	recent := now.Add(-2 * time.Minute).Format(time.RFC3339)
	// Branchless session with a worktree cwd -> basename shown as a guess.
	insertEvent(db, Event{TS: recent, SourceApp: "mmir", Branch: "", Cwd: "/home/olaf/.worktree/mmir/2026-08-11/fix-3655", SessionID: "sess-detached-1", EventType: "PreToolUse", ToolName: "Bash", PayloadJSON: "{}"})
	// A session with a real branch, for contrast.
	insertEvent(db, Event{TS: recent, SourceApp: "mmir", Branch: "fix-3652", Cwd: "/home/olaf/.worktree/mmir/2026-08-11/fix-3652", SessionID: "sess-branch-1", EventType: "PreToolUse", ToolName: "Bash", PayloadJSON: "{}"})

	body := getBody(t, db, "/")
	if !strings.Contains(body, `<span class="branch-guess"`) || !strings.Contains(body, `>fix-3655</span>`) {
		t.Errorf("branchless session should render the cwd basename as an italic .branch-guess hint in:\n%s", body)
	}
	// A known branch must NOT be dressed up as a guess.
	if strings.Contains(body, `<span class="branch-guess" title="worktree directory">fix-3652`) {
		t.Errorf("a known branch should render plain, not as a .branch-guess hint in:\n%s", body)
	}
}

// TmuxSession is captured by the same best-effort tmuxContext shell-out as
// TmuxPane, so it transiently comes back "" (an unreachable or timed-out
// `tmux display-message`). When the latest event for a session carries one of
// those empty captures, the roster row must keep the name an earlier event
// recorded rather than rendering an unlabelled row — the same
// keep-the-last-non-empty rule the fold already applies to TmuxPane and Branch.
func TestRosterTmuxSessionSurvivesTransientEmptyCapture(t *testing.T) {
	db, err := openDB(testDB(t))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	now := time.Now().UTC()
	at := func(mins int) string { return now.Add(time.Duration(-mins) * time.Minute).Format(time.RFC3339) }

	insertEvent(db, Event{TS: at(3), SourceApp: "mmir", TmuxSession: "fix-113", Cwd: "/w/fix-113", SessionID: "s1", EventType: "PreToolUse", ToolName: "Bash", PayloadJSON: "{}"})
	insertEvent(db, Event{TS: at(1), SourceApp: "mmir", TmuxSession: "", Cwd: "/w/fix-113", SessionID: "s1", EventType: "PreToolUse", ToolName: "Bash", PayloadJSON: "{}"})

	agents, err := agentRoster(db, 16*time.Hour, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(agents) != 1 {
		t.Fatalf("want 1 roster row, got %d", len(agents))
	}
	if agents[0].TmuxSession != "fix-113" {
		t.Errorf("tmux session lost to transient empty capture: got %q, want %q", agents[0].TmuxSession, "fix-113")
	}
}

// LastCommandSince carries the instant of the slash-command event (not the
// session's most recent event) out of the roster, so the server layer can
// time-box a CI-watch suppression window against it.
func TestAgentRoster_LastCommandSince(t *testing.T) {
	db, err := openDB(filepath.Join(t.TempDir(), "events.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	now := time.Now().UTC()
	at := func(mins int) time.Time { return now.Add(time.Duration(-mins) * time.Minute).Truncate(time.Second) }
	ins := func(ts time.Time, sess, etype, slash string) {
		insertEvent(db, Event{TS: ts.Format(time.RFC3339), SourceApp: "myapp", Branch: "fix-111", SessionID: sess, EventType: etype, SlashCommand: slash, PayloadJSON: "{}"})
	}

	// s-cmd: the command landed at -6m; later non-command events must not move
	// LastCommandSince, and the earlier /foo must be superseded.
	ins(at(10), "s-cmd", "UserPromptSubmit", "/foo")
	ins(at(6), "s-cmd", "UserPromptSubmit", "/poll-ci")
	ins(at(2), "s-cmd", "PreToolUse", "")
	// s-none: never ran a slash command.
	ins(at(5), "s-none", "PreToolUse", "")
	// s-bad: a slash command whose timestamp cannot be parsed. It must leave
	// LastCommandSince at 0 (an age of now.Unix(), i.e. far outside any window)
	// rather than reading as "just now" to an age-bounded consumer.
	insertEvent(db, Event{TS: "not-a-timestamp", SourceApp: "myapp", Branch: "fix-111", SessionID: "s-bad", EventType: "UserPromptSubmit", SlashCommand: "/poll-ci", PayloadJSON: "{}"})

	agents, err := agentRoster(db, 16*time.Hour, now)
	if err != nil {
		t.Fatal(err)
	}
	byID := map[string]Agent{}
	for _, a := range agents {
		byID[a.SessionID] = a
	}
	if got, want := byID["s-cmd"].LastCommandSince, at(6).Unix(); got != want {
		t.Errorf("s-cmd LastCommandSince = %d, want %d (the /poll-ci event, not the latest event)", got, want)
	}
	if got := byID["s-cmd"].LastCommand; got != "/poll-ci" {
		t.Errorf("s-cmd LastCommand = %q, want /poll-ci", got)
	}
	if got := byID["s-none"].LastCommandSince; got != 0 {
		t.Errorf("s-none LastCommandSince = %d, want 0", got)
	}
	if _, ok := byID["s-bad"]; !ok {
		t.Fatalf("s-bad row missing from roster: %+v", agents)
	}
	if got := byID["s-bad"].LastCommandSince; got != 0 {
		t.Errorf("s-bad LastCommandSince = %d, want 0 (unparseable ts must not read as now)", got)
	}
}
