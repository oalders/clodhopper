package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"
)

// runIngest reads a single Claude Code hook event as JSON on stdin, scrubs it,
// and writes one row to the database. It is designed to NEVER fail a tool call:
// any error results in exit code 0, and output goes to stderr only when
// CLODHOPPER_DEBUG is set. Capture is skipped entirely when CLODHOPPER_DISABLED=1.
func runIngest(args []string) int {
	fs := flag.NewFlagSet("ingest", flag.ContinueOnError)
	sourceApp := fs.String("source-app", "unknown", "logical source name for this project")
	if err := fs.Parse(args); err != nil {
		debugf("parse args: %v", err)
		return 0
	}

	if os.Getenv("CLODHOPPER_DISABLED") == "1" {
		return 0
	}

	raw, err := io.ReadAll(os.Stdin)
	if err != nil {
		debugf("read stdin: %v", err)
		return 0
	}
	if len(strings.TrimSpace(string(raw))) == 0 {
		return 0
	}

	ev := buildEvent(raw, *sourceApp)

	db, err := openDB(defaultDBPath())
	if err != nil {
		debugf("open db: %v", err)
		return 0
	}
	defer db.Close()

	if err := insertEvent(db, ev); err != nil {
		debugf("insert: %v", err)
		return 0
	}
	maybePrune(db, retainDays())
	return 0
}

// buildEvent extracts the fields we care about from a hook payload and returns
// a fully scrubbed Event. It tolerates missing or malformed fields.
func buildEvent(raw []byte, sourceApp string) Event {
	var p map[string]any
	_ = json.Unmarshal(raw, &p) // p stays nil on error; getters handle that

	cwd := str(p, "cwd")
	ev := Event{
		TS:          time.Now().UTC().Format(time.RFC3339),
		SourceApp:   sourceApp,
		Branch:      gitBranch(cwd),
		Cwd:         cwd,
		TmuxSession: tmuxSession(),
		SessionID:   str(p, "session_id"),
		EventType:   str(p, "hook_event_name"),
		ToolName:    str(p, "tool_name"),
		PayloadJSON: scrubPayload(raw),
	}
	if ev.EventType == "" {
		ev.EventType = "Unknown"
	}
	ev.Summary = buildSummary(ev.EventType, ev.ToolName, p)
	return ev
}

// buildSummary produces a short, scrubbed human-readable line. It uses a small
// per-tool allowlist so only low-risk fields contribute to the summary.
func buildSummary(eventType, tool string, p map[string]any) string {
	input, _ := p["tool_input"].(map[string]any)
	switch tool {
	case "Bash":
		if cmd := str(input, "command"); cmd != "" {
			return truncate(scrubString("Bash: "+cmd), 160)
		}
	case "Read", "Edit", "Write", "NotebookEdit":
		if fp := str(input, "file_path"); fp != "" {
			return truncate(scrubString(tool+": "+fp), 160)
		}
	case "Skill":
		if name := str(input, "skill"); name != "" {
			return truncate(scrubString("Skill: "+name), 160)
		}
	case "Grep":
		if pat := str(input, "pattern"); pat != "" {
			return truncate(scrubString("Grep: "+pat), 160)
		}
	case "Glob":
		if pat := str(input, "pattern"); pat != "" {
			return truncate(scrubString("Glob: "+pat), 160)
		}
	}
	if tool != "" {
		return eventType + ": " + tool
	}
	if eventType == "UserPromptSubmit" {
		prompt := str(p, "prompt")
		summary := fmt.Sprintf("UserPromptSubmit (%d chars)", len([]rune(prompt)))
		if prompt != "" {
			summary += ": " + truncate(scrubString(prompt), promptPreviewLen)
		}
		return summary
	}
	return eventType
}

// gitBranch returns the current branch of the git work tree containing dir, or
// "" if dir is empty, not a repo, detached, or git is unavailable. It is
// deliberately best-effort with a tight timeout: capture must never block or
// fail on it. Works for linked worktrees, which is the whole point — each
// concurrent worktree resolves to its own branch.
func gitBranch(dir string) string {
	if dir == "" {
		return ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", "-C", dir, "symbolic-ref", "--quiet", "--short", "HEAD")
	out, err := cmd.Output()
	if err != nil {
		return "" // not a repo, detached HEAD, timeout, or git missing
	}
	return strings.TrimSpace(string(out))
}

// tmuxSession returns the name of the tmux session the current process is in, or
// "" when not inside tmux, on any error, or if it times out. Like gitBranch it is
// deliberately best-effort: capture must never block or fail a tool call. The
// $TMUX guard avoids spawning tmux (and its stderr noise) outside a session;
// `display-message -p '#S'` resolves the current pane's session via $TMUX, so no
// `-t` target is needed. The name is user-chosen free text, so it is scrubbed and
// truncated to honour the scrub layer's fail-closed bias.
func tmuxSession() string {
	if os.Getenv("TMUX") == "" {
		return ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "tmux", "display-message", "-p", "#S").Output()
	if err != nil {
		return ""
	}
	return truncate(scrubString(strings.TrimSpace(string(out))), maxFieldLen)
}

// str safely reads a string field from a decoded JSON map.
func str(m map[string]any, key string) string {
	if m == nil {
		return ""
	}
	if v, ok := m[key].(string); ok {
		return v
	}
	return ""
}

func debugf(format string, a ...any) {
	if os.Getenv("CLODHOPPER_DEBUG") != "" {
		fmt.Fprintf(os.Stderr, "clodhopper: "+format+"\n", a...)
	}
}
