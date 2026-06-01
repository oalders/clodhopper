// Command clodhopper captures Claude Code lifecycle events into a local SQLite
// database and serves a read-only dashboard. See README.md for the design.
package main

import (
	"flag"
	"fmt"
	"os"
	"strconv"
)

const (
	defaultRetainDays         = 14
	defaultWaitingRetainHours = 16
	fallbackPort              = 4555
	fallbackRefresh           = 5
)

// Build metadata, overridden at release time via -ldflags -X (see .goreleaser.yaml).
var (
	version = "dev"
	commit  = "none"
	date    = "unknown"
)

// versionString is the single source of truth for --version output; kept as a
// pure function so it can be tested without capturing stdout.
func versionString() string {
	return fmt.Sprintf("clodhopper %s (commit %s, built %s)", version, commit, date)
}

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	if len(args) == 0 {
		usage()
		return 2
	}
	cmd, rest := args[0], args[1:]
	switch cmd {
	case "ingest":
		return runIngest(rest)
	case "serve":
		return runServe(rest)
	case "prune":
		return runPrune(rest)
	case "init":
		return runInit(rest)
	case "-h", "--help", "help":
		usage()
		return 0
	case "-v", "--version", "version":
		fmt.Println(versionString())
		return 0
	default:
		fmt.Fprintf(os.Stderr, "clodhopper: unknown command %q\n\n", cmd)
		usage()
		return 2
	}
}

func runPrune(args []string) int {
	fs := flag.NewFlagSet("prune", flag.ContinueOnError)
	days := fs.Int("days", retainDays(), "delete events older than this many days")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	db, err := openDB(defaultDBPath())
	if err != nil {
		fmt.Fprintln(os.Stderr, "clodhopper prune:", err)
		return 1
	}
	defer db.Close()
	n, err := pruneOld(db, *days)
	if err != nil {
		fmt.Fprintln(os.Stderr, "clodhopper prune:", err)
		return 1
	}
	fmt.Printf("pruned %d event(s) older than %d days\n", n, *days)
	return 0
}

func usage() {
	fmt.Fprint(os.Stderr, `clodhopper — Claude Code multi-agent observability

USAGE
  clodhopper ingest --source-app NAME   read one hook event (JSON on stdin), store it
  clodhopper serve [--port N] [--host H] serve the dashboard (default 127.0.0.1; --host 0.0.0.0 for container/LAN)
  clodhopper prune [--days N]           delete events older than N days
  clodhopper init [--project|--local]   wire clodhopper hooks into .claude/settings(.local).json
  clodhopper --version                  print version and build metadata

ENV
  CLODHOPPER_DB                    SQLite path (default ~/.claude/clodhopper/var/events.db)
  CLODHOPPER_RETAIN_DAYS           retention window in days (default 14)
  CLODHOPPER_DISABLED=1            make ingest a no-op
  CLODHOPPER_PORT                  dashboard port (default 4555)
  CLODHOPPER_HOST                  dashboard bind address (default 127.0.0.1)
  CLODHOPPER_REFRESH_SECS          dashboard live-update poll cadence, 0 = off (default 5)
  CLODHOPPER_WAITING_RETAIN_HOURS  hours a waiting agent stays on the roster (default 16)
  CLODHOPPER_DEBUG                 write ingest errors to stderr (otherwise silent)
`)
}

// retainDays reads CLODHOPPER_RETAIN_DAYS or returns the default.
func retainDays() int {
	if v := os.Getenv("CLODHOPPER_RETAIN_DAYS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return defaultRetainDays
}

// waitingRetainHours reads CLODHOPPER_WAITING_RETAIN_HOURS or returns the
// default. It bounds how long an agent that is waiting on you (Stop /
// Notification / PermissionRequest) stays on the roster when no SessionEnd ever
// arrives — long enough to survive a lunch or overnight gap, short enough that a
// hard-killed "zombie" session cannot linger indefinitely.
func waitingRetainHours() int {
	if v := os.Getenv("CLODHOPPER_WAITING_RETAIN_HOURS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return defaultWaitingRetainHours
}

// defaultPort reads CLODHOPPER_PORT or returns the fallback.
func defaultPort() int {
	if v := os.Getenv("CLODHOPPER_PORT"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return fallbackPort
}

// defaultHost reads CLODHOPPER_HOST or returns loopback.
func defaultHost() string {
	if v := os.Getenv("CLODHOPPER_HOST"); v != "" {
		return v
	}
	return "127.0.0.1"
}

// defaultRefreshSecs reads CLODHOPPER_REFRESH_SECS (0 disables auto-refresh) or
// returns the fallback. It is the dashboard's default cadence; the UI dropdown
// (?refresh=N) overrides it per view.
func defaultRefreshSecs() int {
	if v := os.Getenv("CLODHOPPER_REFRESH_SECS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 && n <= refreshMax {
			return n
		}
	}
	return fallbackRefresh
}
