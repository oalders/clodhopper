// Command clodhopper captures Claude Code lifecycle events into a local SQLite
// database and serves a read-only dashboard. See README.md for the design.
package main

import (
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

const (
	defaultRetainDays         = 14
	defaultWaitingRetainHours = 720 // 30 days; "don't auto-drop" (bounded in practice by retainDays) — evict via `clodhopper end`
	defaultStaleShell         = 2 * time.Hour
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
	case "end":
		return runEnd(rest)
	case "block":
		return runBlock(rest)
	case "unblock":
		return runUnblock(rest)
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
  clodhopper COMMAND [FLAGS]      run 'clodhopper COMMAND -h' for that command's flags

COMMANDS
  ingest    read one hook event (JSON on stdin), scrub it, store it
    --source-app NAME     logical source name for this project (default "unknown")

  serve     serve the dashboard (read-only unless --enable-merge)
    --port N              port to listen on (default 4555, or CLODHOPPER_PORT)
    --host H              address to bind (default 127.0.0.1; 0.0.0.0 for container/LAN)
    --tailscale           bind this machine's Tailscale IPv4; cannot be combined with --host
    --allow-public        allow binding a public IP (UNSAFE: no auth or TLS); or CLODHOPPER_ALLOW_PUBLIC=1
    --pane-peek           stream live tmux pane content to the dashboard (trusted networks only)
    --pane-lines N        lines shown in a pane peek, 1..2000 (default 40)
    --enable-merge        turn on the roster's PR- and session-action buttons; these write to
                          your repos and are accepted only from loopback and Tailscale peers,
                          whatever --host says. Never run behind a proxy or port forward

  prune     delete events older than the retention window
    --days N              retention window in days (default 14, or CLODHOPPER_RETAIN_DAYS)

  init      wire clodhopper hooks into a project's .claude settings
    --project             write .claude/settings.json (committed/shared)
    --local               write .claude/settings.local.json (gitignored)
    --source-app NAME     source label (default: git repo name)
    --guard KIND          binary guard: "command" (portable, default) or "is"
    --dry-run             print the result without writing

  end       mark matching live sessions ended (give exactly one selector)
    --branch B            sessions on this git branch
    --cwd D               sessions whose latest event has this cwd
    --session PREFIX      the session whose id starts with PREFIX (the roster fragment)

  block     relax a row out of the alert band until its blocker leaves the roster
    --branch B | --session PREFIX          the blocked session
    --on-branch B | --on-session PREFIX    the session it waits on

  unblock   drop every block on the matching live sessions
    --branch B | --session PREFIX

  --version print version and build metadata

ENV
  CLODHOPPER_DB                    SQLite path (default ~/.claude/clodhopper/var/events.db)
  CLODHOPPER_RETAIN_DAYS           retention window in days (default 14)
  CLODHOPPER_DISABLED=1            stop ingest capturing events (transcript stamps still print)
  CLODHOPPER_STAMPS=0              turn off the finished/waiting/message time stamps in the transcript
  CLODHOPPER_STAMP_CONTEXT=1       also tell the model the local time each prompt was sent
  CLODHOPPER_PORT                  dashboard port (default 4555)
  CLODHOPPER_HOST                  dashboard bind address (default 127.0.0.1)
  CLODHOPPER_ALLOW_PUBLIC=1        let serve bind a public IP (same as --allow-public)
  CLODHOPPER_REFRESH_SECS          dashboard live-update poll cadence, 0 = off (default 5)
  CLODHOPPER_WAITING_RETAIN_HOURS  hours a waiting agent stays on the roster (default 720 = 30 days; evict via 'clodhopper end')
  CLODHOPPER_STALE_SHELL           how long a background shell runs, main agent quiet, before its row reads "shell stuck?" (default 2h; 0 = off)
  CLODHOPPER_ALLOWED_HOSTS         comma-separated extra Host header values accepted by --enable-merge's endpoint (e.g. a Tailscale magicDNS name)
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
// arrives. The default (720h / 30 days) is deliberately generous: a genuinely
// alive agent left idle for days — overnight, a weekend, a multi-day pause — is
// exactly the one the roster is most useful for, so it should not silently age
// out. Hard-killed "zombie" sessions are reaped explicitly via `clodhopper end`
// (which writes a synthetic SessionEnd), not by this timeout — a short cap could
// not tell a zombie from a long-idle live agent and evicted both. In practice the
// roster window is also bounded by retainDays: pruned events drop off regardless,
// so this cap only matters up to the event-retention horizon.
func waitingRetainHours() int {
	if v := os.Getenv("CLODHOPPER_WAITING_RETAIN_HOURS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return defaultWaitingRetainHours
}

// staleShell reads CLODHOPPER_STALE_SHELL (a Go duration, e.g. "2h") or returns
// the default; 0 disables. It is how long a row's oldest background shell may run,
// with its main agent silent as long, before the roster calls it "shell stuck?"
// rather than quietly waiting on it: a `gh pr checks --watch` on a check that
// never finishes would otherwise hold the row at background rank forever.
func staleShell() time.Duration {
	if v := os.Getenv("CLODHOPPER_STALE_SHELL"); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d >= 0 {
			return d
		}
	}
	return defaultStaleShell
}

// allowedHosts reads CLODHOPPER_ALLOWED_HOSTS (comma-separated) — extra Host
// header values accepted on every route (see newServeMux), e.g. a Tailscale
// magicDNS name. Loopback and the bind host are always allowed without listing.
func allowedHosts() []string {
	v := os.Getenv("CLODHOPPER_ALLOWED_HOSTS")
	if v == "" {
		return nil
	}
	var out []string
	for _, s := range strings.Split(v, ",") {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
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

// stampsEnabled reads CLODHOPPER_STAMPS; only "0" turns the transcript stamps off.
func stampsEnabled() bool {
	return os.Getenv("CLODHOPPER_STAMPS") != "0"
}

// stampContext reads CLODHOPPER_STAMP_CONTEXT; "1" opts in to telling the model
// when each prompt was sent (it costs context tokens, so it is off by default).
func stampContext() bool {
	return os.Getenv("CLODHOPPER_STAMP_CONTEXT") == "1"
}

// allowPublic reads CLODHOPPER_ALLOW_PUBLIC; "1" opts in to binding a public IP.
func allowPublic() bool {
	return os.Getenv("CLODHOPPER_ALLOW_PUBLIC") == "1"
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
