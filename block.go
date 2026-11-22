package main

import (
	"context"
	"database/sql"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"time"
)

// runBlock implements `clodhopper block`: it records that one live session
// cannot move until another lands, until an open PR in its own GitHub repo
// merges or closes (--on-pr), or while a tmux pane in its own tmux session runs
// a command (--on-pane), so the dashboard relaxes the blocked row out of the
// alert band (see applyBlocks, applyPRBlocks, applyPaneBlocks). Each side
// resolves to exactly one live session id now, so a session block lapses when
// that blocker session leaves the roster or its PR merges or closes, and a
// later session on its branch does not revive it.
func runBlock(args []string) int {
	fs := flag.NewFlagSet("block", flag.ContinueOnError)
	branch := fs.String("branch", "", "the blocked row: the live session on this git branch")
	session := fs.String("session", "", "the blocked row: the live session whose id starts with this prefix")
	onBranch := fs.String("on-branch", "", "the blocker: the live session on this git branch")
	onSession := fs.String("on-session", "", "the blocker: the live session whose id starts with this prefix")
	onPR := fs.Int("on-pr", 0, "the blocker: this open PR number in the blocked row's GitHub repo")
	onPane := fs.String("on-pane", "", "the blocker: this busy tmux pane (a tmux target) in the blocked row's tmux session")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	kinds := 0
	for _, set := range []bool{*onBranch != "" || *onSession != "", *onPR != 0, *onPane != ""} {
		if set {
			kinds++
		}
	}
	if (*branch == "" && *session == "") || kinds != 1 || *onPR < 0 {
		fmt.Fprintln(os.Stderr, "clodhopper block: need --branch or --session, and one of --on-branch/--on-session, a positive --on-pr or --on-pane")
		return 2
	}
	db, err := openDB(defaultDBPath())
	if err != nil {
		fmt.Fprintln(os.Stderr, "clodhopper block:", err)
		return 1
	}
	defer db.Close()

	blocked, err := resolveLiveSession(db, EndSelector{SessionID: *session, Branch: *branch})
	if err != nil {
		fmt.Fprintln(os.Stderr, "clodhopper block: blocked row:", err)
		return 1
	}
	if *onPane != "" {
		return blockOnPane(db, blocked, *onPane)
	}
	if *onPR > 0 {
		return blockOnPR(db, blocked, *onPR)
	}
	blocker, err := resolveLiveSession(db, EndSelector{SessionID: *onSession, Branch: *onBranch})
	if err != nil {
		fmt.Fprintln(os.Stderr, "clodhopper block: blocker:", err)
		return 1
	}
	all, err := liveSessions(db, EndSelector{})
	if err != nil {
		fmt.Fprintln(os.Stderr, "clodhopper block:", err)
		return 1
	}
	live := map[string]bool{}
	for _, l := range all {
		live[l.sess] = true
	}
	if err := addBlock(db, blocked, blocker, live, time.Now()); err != nil {
		fmt.Fprintln(os.Stderr, "clodhopper block:", err)
		return 1
	}
	fmt.Printf("blocked %s on %s\n", blocked, blocker)
	return 0
}

// blockOnPR records that blocked waits on PR n of its cwd's GitHub repo,
// refusing unless gh confirms the PR is open.
func blockOnPR(db *sql.DB, blocked string, n int) int {
	cwd, err := latestCwdForSession(db, blocked)
	if err != nil {
		fmt.Fprintln(os.Stderr, "clodhopper block:", err)
		return 1
	}
	repo := lookupRepo(cwd)
	if repo == "" {
		fmt.Fprintln(os.Stderr, "clodhopper block: the blocked row is not in a GitHub repo")
		return 1
	}
	key := repo + "#" + strconv.Itoa(n)
	switch r := lookupPRFinal(key, nil); {
	case r.PRNumber != n:
		fmt.Fprintf(os.Stderr, "clodhopper block: could not confirm %s is an open PR (is gh installed and authenticated?)\n", key)
		return 1
	case r.Done != "":
		fmt.Fprintf(os.Stderr, "clodhopper block: %s is already %s\n", key, r.Done)
		return 1
	}
	if err := addPRBlock(db, blocked, repo, n, time.Now()); err != nil {
		fmt.Fprintln(os.Stderr, "clodhopper block:", err)
		return 1
	}
	fmt.Printf("blocked %s on %s\n", blocked, key)
	return 0
}

// blockOnPane records that blocked waits on the tmux pane target (any form
// tmux accepts after -t) while it runs a command. target is its own argv
// element, never a shell token, and must resolve to a pane id that passes
// validatePaneBlock, so an agent cannot block on its own $TMUX_PANE.
func blockOnPane(db *sql.DB, blocked, target string) int {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "tmux", "display-message", "-p", "-t", target, "#{pane_id}").Output()
	pane := strings.TrimSpace(string(out))
	if err != nil || !paneIDRe.MatchString(pane) {
		fmt.Fprintf(os.Stderr, "clodhopper block: could not resolve tmux pane %q (is tmux reachable?)\n", target)
		return 1
	}
	now := time.Now()
	pid, why, err := validatePaneBlock(db, blocked, pane, now)
	if err == nil && why == "" {
		err = addPaneBlock(db, blocked, pane, pid, now)
	}
	switch {
	case err != nil:
		fmt.Fprintln(os.Stderr, "clodhopper block:", err)
		return 1
	case why != "":
		fmt.Fprintf(os.Stderr, "clodhopper block: %s: %s\n", pane, why)
		return 1
	}
	fmt.Printf("blocked %s on pane %s\n", blocked, pane)
	return 0
}

// validatePaneBlock checks, against a fresh roster, pane listing and ps read,
// that sessionID may be blocked on pane: the row is live with its own pane
// listed and matched as dropDeadPanes would, and pane passes paneBlockRefusal.
// It returns pane's root pid to store, or why it refuses; err is a DB error.
// Shared by the block-pane action and `block --on-pane`.
func validatePaneBlock(db *sql.DB, sessionID, pane string, now time.Time) (int, string, error) {
	agents, err := agentRoster(db, time.Duration(waitingRetainHours())*time.Hour, staleShell(), now)
	if err != nil {
		return 0, "", err
	}
	i := slices.IndexFunc(agents, func(a Agent) bool { return a.SessionID == sessionID })
	if i < 0 {
		return 0, "the row must be live on the roster", nil
	}
	panes := listPanes()
	if len(panes) == 0 {
		return 0, "could not list tmux panes (is tmux reachable?)", nil
	}
	row := agents[i]
	if alive, _ := paneMatchers(agents, panes); !paneIDRe.MatchString(row.TmuxPane) || !alive(row) {
		return 0, "this row has no live tmux pane", nil
	}
	if pane == row.TmuxPane {
		return 0, "that is this row's own pane", nil
	}
	p, ok := panes[pane]
	if !ok {
		return 0, "no such tmux pane", nil
	}
	taken := map[string]bool{}
	for _, a := range agents {
		taken[a.TmuxPane] = true
	}
	if why := paneBlockRefusal(panes[row.TmuxPane], pane, p, taken, paneTpgids(map[string]livePane{pane: p})); why != "" {
		return 0, why, nil
	}
	return p.pid, "", nil
}

// paneBlockRefusal says why pane id (p) cannot hold a row whose own pane is
// own, or "" if it can: it must share own's tmux session (by session_id, not
// the normalised label), be no roster row's pane (taken), and be busy per fg.
func paneBlockRefusal(own livePane, id string, p livePane, taken map[string]bool, fg map[string]int) string {
	busy, known := paneBusy(id, p, fg)
	switch {
	case p.sessionID == "" || p.sessionID != own.sessionID:
		return "that pane is not in this row's tmux session"
	case taken[id]:
		return "that pane is a roster row's own"
	case !known:
		return "could not read that pane's foreground process (is ps available?)"
	case !busy:
		return "that pane is idle at its shell"
	}
	return ""
}

// runUnblock implements `clodhopper unblock`: it drops every block on the live
// sessions matching the selector.
func runUnblock(args []string) int {
	fs := flag.NewFlagSet("unblock", flag.ContinueOnError)
	branch := fs.String("branch", "", "unblock the live sessions on this git branch")
	session := fs.String("session", "", "unblock the live session whose id starts with this prefix; must be unambiguous")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *branch == "" && *session == "" {
		fmt.Fprintln(os.Stderr, "clodhopper unblock: need at least one of --branch, --session")
		return 2
	}
	db, err := openDB(defaultDBPath())
	if err != nil {
		fmt.Fprintln(os.Stderr, "clodhopper unblock:", err)
		return 1
	}
	defer db.Close()

	targets, err := liveSessions(db, EndSelector{SessionID: *session, Branch: *branch})
	if err != nil {
		fmt.Fprintln(os.Stderr, "clodhopper unblock:", err)
		return 1
	}
	var n int64
	for _, l := range targets {
		m, err := unblockSession(db, l.sess)
		if err != nil {
			fmt.Fprintln(os.Stderr, "clodhopper unblock:", err)
			return 1
		}
		n += m
	}
	fmt.Printf("dropped %d block(s)\n", n)
	return 0
}
