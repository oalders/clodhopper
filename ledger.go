package main

import (
	"context"
	"encoding/json"
	"maps"
	"os/exec"
	"slices"
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
// It lives in memory only: serve never writes the DB for it.
type queueLedger struct {
	mu     sync.Mutex
	m      map[string]ledgerEntry // by session id
	finals *cwdCache[prFinal]     // by "owner/repo#N"; see lookupPRFinal
}

type ledgerEntry struct {
	snap Agent     // the row as last seen queued
	seen time.Time // when it was last seen queued
}

// ledgerRow is one carried-over row of the merge-queue table.
type ledgerRow struct {
	Agent
	Final     string // "merged", "closed", "open" (left the queue) or "" (session gone, state unknown)
	MergedAgo string // time since PRMergedAt ("5m ago"); "" when unknown
}

// prFinal is a ledgered PR's state looked up by number once its session is
// gone: State is "merged", "closed", "open" or "" when the lookup failed.
type prFinal struct {
	State    string
	MergedAt time.Time
}

func newQueueLedger() *queueLedger {
	return &queueLedger{m: map[string]ledgerEntry{}, finals: &cwdCache[prFinal]{m: map[string]cwdEntry[prFinal]{}, lookup: lookupPRFinal}}
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
// still on the roster shows its own CI: merged or closed moves it to the
// ledger, anything else (ejected, say) leaves it on the roster with no ledger
// row. A gone session's PR is looked up by number. now is injected for testing.
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
	var rows []ledgerRow
	for id, e := range entries {
		r := ledgerRow{Agent: e.snap}
		if a, ok := live[id]; ok {
			if a.CIDone == "" {
				continue
			}
			r = ledgerRow{Agent: a, Final: a.CIDone}
		} else {
			f := l.finals.get(e.snap.PRRepo+"#"+strconv.Itoa(e.snap.PRNumber), now)
			r.Final, r.PRMergedAt, r.LiveTmux = f.State, unixOrZero(f.MergedAt), false
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
	if len(rows) > 0 {
		// Cloned: roster is a view of the caller's agents, which must keep every row.
		roster = slices.DeleteFunc(slices.Clone(roster), func(a Agent) bool {
			return slices.ContainsFunc(rows, func(r ledgerRow) bool { return r.SessionID == a.SessionID })
		})
	}
	return roster, rows
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
	return []string{"pr", "view", strconv.Itoa(num), "--repo=" + repo, "--json", "state,mergedAt"}, true
}

// lookupPRFinal resolves a gone session's PR state through gh. Best-effort,
// like lookupCI: invalid input, a missing gh, an error or a timeout yield "".
func lookupPRFinal(key string) prFinal {
	args, ok := prFinalArgs(key)
	if !ok {
		return prFinal{}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "gh", args...).Output()
	if err != nil {
		return prFinal{}
	}
	return parsePRFinal(out)
}

// parsePRFinal turns `gh pr view --json state,mergedAt` output into a prFinal;
// anything unparseable or an unexpected state is "unknown".
func parsePRFinal(out []byte) prFinal {
	var pr struct {
		State    string    `json:"state"`
		MergedAt time.Time `json:"mergedAt"`
	}
	if err := json.Unmarshal(out, &pr); err != nil {
		return prFinal{}
	}
	switch pr.State {
	case "MERGED", "CLOSED", "OPEN":
		return prFinal{State: strings.ToLower(pr.State), MergedAt: pr.MergedAt}
	}
	return prFinal{}
}
