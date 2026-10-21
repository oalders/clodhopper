package main

import (
	"context"
	"maps"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// A ledger entry lapses ledgerTTL after its row was last seen queued; past
// ledgerMax entries the least recently seen goes first.
const (
	ledgerTTL = 24 * time.Hour
	ledgerMax = 100
)

// queueLedger remembers every row splitQueued folded into the merge queue, so
// the row can stay in that table with its PR's final state once it leaves the
// queue, its pane dies or its session ends, until dismissed or expired (#211).
// It lives in memory only, bounded by ledgerTTL and ledgerMax: serve never
// writes the DB for it.
type queueLedger struct {
	mu     sync.Mutex
	m      map[string]ledgerEntry // by session id
	finals *cwdCache[ciResult]    // by "owner/repo#N"; see lookupPRFinal
}

type ledgerEntry struct {
	snap  Agent     // the row as last seen queued
	seen  time.Time // when it was last seen queued
	final ciResult  // a gone session's PR once merged or closed: it cannot change, so it is never looked up again
}

// ledgerRow is one carried-over row of the merge-queue table.
type ledgerRow struct {
	Agent
	Final     string // see finalState
	MergedAgo string // time since PRMergedAt ("5m ago"); "" when unknown
}

func newQueueLedger() *queueLedger {
	return &queueLedger{m: map[string]ledgerEntry{}, finals: &cwdCache[ciResult]{m: map[string]cwdEntry[ciResult]{}, lookup: lookupPRFinal}}
}

// record refreshes the entry of every queued row, then applies ledgerTTL and
// ledgerMax. now is injected for testing. A nil ledger records nothing.
func (l *queueLedger) record(queued []Agent, now time.Time) {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, a := range queued {
		l.m[a.SessionID] = ledgerEntry{snap: a, seen: now}
	}
	for id, e := range l.m {
		if now.Sub(e.seen) >= ledgerTTL {
			delete(l.m, id)
		}
	}
	for len(l.m) > ledgerMax {
		oldest := ""
		for id, e := range l.m {
			if oldest == "" || e.seen.Before(l.m[oldest].seen) {
				oldest = id
			}
		}
		delete(l.m, oldest)
	}
}

// dismiss drops sessionID's entry, reporting whether there was one.
func (l *queueLedger) dismiss(sessionID string) bool {
	if l == nil {
		return false
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	_, ok := l.m[sessionID]
	delete(l.m, sessionID)
	return ok
}

// carryOver returns the ledger rows for every entry not in queued, most
// recently queued first, plus roster minus the rows they replace. A session
// still on the roster shows its own CI: merged or closed moves it to the ledger
// only if setAndForget would fold it (so pass the status from before
// demotePendingCI); anything else leaves it on the roster with no ledger row.
// A gone session's PR is looked up by number. roster is partitioned in place,
// as in splitQueued, so the caller's slice keeps every row. now is injected for testing.
func (l *queueLedger) carryOver(roster, queued []Agent, now time.Time) ([]Agent, []ledgerRow) {
	if l == nil {
		return roster, nil
	}
	l.mu.Lock()
	entries := maps.Clone(l.m) // the gh lookups below run unlocked
	l.mu.Unlock()
	for _, a := range queued {
		delete(entries, a.SessionID)
	}
	live := map[string]Agent{}
	for _, a := range roster {
		live[a.SessionID] = a
	}
	var keys []string
	for id, e := range entries {
		if _, ok := live[id]; !ok && e.final.Done == "" {
			keys = append(keys, finalKey(e.snap))
		}
	}
	finals := lookupKeys(keys, l.finals, now)
	var rows []ledgerRow
	moved := map[string]bool{}
	for id, e := range entries {
		r := ledgerRow{Agent: e.snap}
		if a, ok := live[id]; ok {
			if a.CIDone == "" || !setAndForget(a.Status) {
				continue
			}
			r = ledgerRow{Agent: a, Final: a.CIDone}
			moved[id] = true
		} else {
			f := e.final
			if f.Done == "" {
				f = finals[finalKey(e.snap)]
				l.keepFinal(id, finalKey(e.snap), f)
			}
			r.Final, r.PRMergedAt, r.CIQueuePos, r.LiveTmux = finalState(f), unixOrZero(f.MergedAt), f.QueuePos, false
		}
		if r.PRMergedAt != 0 {
			r.MergedAgo = humanizeSeconds(int(max(0, now.Unix()-r.PRMergedAt))) + " ago"
		}
		rows = append(rows, r)
	}
	sort.Slice(rows, func(i, j int) bool {
		si, sj := entries[rows[i].SessionID].seen, entries[rows[j].SessionID].seen
		if !si.Equal(sj) {
			return si.After(sj)
		}
		return rows[i].SessionID < rows[j].SessionID
	})
	var tail []Agent
	n := 0
	for _, a := range roster {
		if moved[a.SessionID] {
			tail = append(tail, a)
		} else {
			roster[n] = a
			n++
		}
	}
	copy(roster[n:], tail)
	return roster[:n], rows
}

// LedgerTally counts the ledger rows for the merge-queue heading, apart from
// the "left queue" alerts, which must show while the section is folded.
func (d dashboardData) LedgerTally() (t struct{ Finished, Left int }) {
	for _, r := range d.LedgerRows {
		if r.Final == "open" {
			t.Left++
		} else {
			t.Finished++
		}
	}
	return t
}

// keepFinal stores f, key's state, on id's entry once it is terminal (merged
// or closed), unless the entry was re-recorded for another PR meanwhile.
func (l *queueLedger) keepFinal(id, key string, f ciResult) {
	if f.Done == "" {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if e, ok := l.m[id]; ok && finalKey(e.snap) == key {
		e.final = f
		l.m[id] = e
	}
}

func finalKey(a Agent) string {
	return a.PRRepo + "#" + strconv.Itoa(a.PRNumber)
}

// finalState names a gone session's PR for its ledger row: "merged" or
// "closed"; "queued" while still in the merge queue; "open" once it left the
// queue unmerged; "" when the lookup failed (no number came back).
func finalState(r ciResult) string {
	switch {
	case r.Done != "":
		return r.Done
	case r.PRNumber == 0:
		return ""
	case r.Queue != "":
		return "queued"
	}
	return "open"
}

// prFinalArgs is the gh argv that looks up key ("owner/repo#N"), or false when
// either half fails validation, so nothing unchecked reaches gh. --repo= keeps
// the value from ever parsing as a flag.
func prFinalArgs(key string) ([]string, bool) {
	repo, n, _ := strings.Cut(key, "#")
	num, err := strconv.Atoi(n)
	if err != nil || num <= 0 || repo == "" || parseGitHubRepo("https://github.com/"+repo) != repo {
		return nil, false
	}
	return []string{"pr", "view", strconv.Itoa(num), "--repo=" + repo, "--json", "id,number,state,mergedAt"}, true
}

// lookupPRFinal resolves a gone session's PR through gh, an open one's merge
// queue entry too, as lookupCI does. Best-effort, like lookupCI: invalid input,
// a missing gh, an error or a timeout yield the zero ciResult.
func lookupPRFinal(key string) ciResult {
	args, ok := prFinalArgs(key)
	if !ok {
		return ciResult{}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "gh", args...).Output()
	if err != nil {
		return ciResult{}
	}
	r, id := parsePRView(out)
	return withMergeQueue(ctx, "", r, id)
}
