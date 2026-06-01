package main

import (
	"flag"
	"fmt"
	"os"
	"time"
)

// runEnd implements `clodhopper end`: it marks the live sessions matching a
// selector as ended by writing a synthetic SessionEnd, so agents that were
// hard-killed (tmux kill-session, kill -9, crash, sleep) drop off the dashboard
// roster immediately instead of lingering until the waiting-retention cap. A
// teardown script knows the branch or worktree path it is tearing down, not
// Claude's session_id, so those are the natural selectors. At least one selector
// is required so the command can never end every session by accident.
func runEnd(args []string) int {
	fs := flag.NewFlagSet("end", flag.ContinueOnError)
	branch := fs.String("branch", "", "end live sessions on this git branch")
	cwd := fs.String("cwd", "", "end live sessions whose latest event has this cwd")
	session := fs.String("session", "", "end this exact session id")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *branch == "" && *cwd == "" && *session == "" {
		fmt.Fprintln(os.Stderr, "clodhopper end: need at least one of --branch, --cwd, --session")
		return 2
	}
	db, err := openDB(defaultDBPath())
	if err != nil {
		fmt.Fprintln(os.Stderr, "clodhopper end:", err)
		return 1
	}
	defer db.Close()

	n, err := endSessions(db, EndSelector{SessionID: *session, Branch: *branch, Cwd: *cwd}, time.Now())
	if err != nil {
		fmt.Fprintln(os.Stderr, "clodhopper end:", err)
		return 1
	}
	fmt.Printf("ended %d live session(s)\n", n)
	return 0
}
