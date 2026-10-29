package main

import (
	"database/sql"
	"flag"
	"fmt"
	"os"
	"strconv"
	"time"
)

// runBlock implements `clodhopper block`: it records that one live session
// cannot move until another lands, or until an open PR in its own GitHub repo
// merges or closes (--on-pr), so the dashboard relaxes the blocked row out of
// the alert band (see applyBlocks, applyPRBlocks). Each side resolves to
// exactly one live session id now, so a session block lapses when that blocker
// session leaves the roster and a later session on its branch does not revive it.
func runBlock(args []string) int {
	fs := flag.NewFlagSet("block", flag.ContinueOnError)
	branch := fs.String("branch", "", "the blocked row: the live session on this git branch")
	session := fs.String("session", "", "the blocked row: the live session whose id starts with this prefix")
	onBranch := fs.String("on-branch", "", "the blocker: the live session on this git branch")
	onSession := fs.String("on-session", "", "the blocker: the live session whose id starts with this prefix")
	onPR := fs.Int("on-pr", 0, "the blocker: this open PR number in the blocked row's GitHub repo")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	onRow := *onBranch != "" || *onSession != ""
	if (*branch == "" && *session == "") || onRow == (*onPR != 0) || *onPR < 0 {
		fmt.Fprintln(os.Stderr, "clodhopper block: need --branch or --session, and one of --on-branch/--on-session or a positive --on-pr")
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
	switch r := lookupPRFinal(key); {
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
