package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
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
	branch, rebasing := gitBranch(cwd)
	ev := Event{
		TS:          time.Now().UTC().Format(time.RFC3339),
		SourceApp:   sourceApp,
		Branch:      branch,
		Rebasing:    rebasing,
		Cwd:         cwd,
		TmuxSession: tmuxSession(),
		SessionID:   str(p, "session_id"),
		EventType:   str(p, "hook_event_name"),
		ToolName:    str(p, "tool_name"),
		PayloadJSON: scrubPayload(raw),
	}
	ev.ToolUseID = str(p, "tool_use_id")
	if ms, ok := numField(p, "duration_ms"); ok {
		ev.DurationMs = sql.NullInt64{Int64: ms, Valid: true}
	}
	if ev.EventType == "" {
		ev.EventType = "Unknown"
	}
	ev.SlashCommand = slashCommand(ev.EventType, str(p, "prompt"))
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

// slashCommand extracts the slash command a user invoked from a UserPromptSubmit
// prompt, or "" for any other event type or a prompt that is not a command. Only
// the first whitespace-delimited token is kept — arguments are never retained, in
// keeping with "never persist chat content". A bare "/" is not a command. The
// token is scrubbed and truncated to honour the scrub layer's fail-closed bias,
// consistent with tmuxSession and the prompt summary.
func slashCommand(eventType, prompt string) string {
	if eventType != "UserPromptSubmit" {
		return ""
	}
	prompt = strings.TrimSpace(prompt)
	if !strings.HasPrefix(prompt, "/") {
		return ""
	}
	token := strings.Fields(prompt)[0]
	if token == "/" {
		return ""
	}
	return truncate(scrubString(token), maxFieldLen)
}

// gitBranch returns the current branch of the git work tree containing dir and
// whether that work tree is mid-rebase. It returns ("", false) if dir is empty,
// not a repo, or git is unavailable. During a rebase HEAD is detached, so the
// normal symbolic-ref lookup fails; we then recover the branch being rebased
// from git's rebase state and report rebasing=true. It is deliberately
// best-effort with a tight timeout: capture must never block or fail on it.
// Works for linked worktrees, which is the whole point — each concurrent
// worktree resolves to its own branch (and its own rebase state).
func gitBranch(dir string) (branch string, rebasing bool) {
	if dir == "" {
		return "", false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", "-C", dir, "symbolic-ref", "--quiet", "--short", "HEAD")
	out, err := cmd.Output()
	if err == nil {
		return strings.TrimSpace(string(out)), false
	}
	// symbolic-ref fails on a detached HEAD, which is exactly the state during a
	// rebase. Recover the underlying branch if a rebase is in progress.
	if b := rebaseBranch(dir); b != "" {
		return b, true
	}
	return "", false // not a repo, plain detached HEAD, timeout, or git missing
}

// rebaseBranch recovers the name of the branch being rebased in the work tree at
// dir, or "" if no rebase is in progress (or on any error). git records the
// original branch ref in rebase-merge/head-name (interactive / merge-based
// rebase) or rebase-apply/head-name (am-based rebase); each file holds a single
// ref like "refs/heads/my-branch". rev-parse --git-path resolves those paths per
// work tree, so a linked worktree's own rebase state is found correctly. The
// file's absence is how we detect "no rebase" — rev-parse always prints a path,
// existing or not — so the ReadFile failure is the real signal, not an error.
// Best-effort with a single tight timeout shared across both lookups, like the
// rest of the capture path. Only a local-branch ref (refs/heads/…) is recovered;
// a rebase begun from an already-detached HEAD stores the literal "detached
// HEAD" in head-name, which we fail closed on (return "") rather than record as
// a bogus branch.
func rebaseBranch(dir string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	for _, sub := range []string{"rebase-merge/head-name", "rebase-apply/head-name"} {
		out, err := exec.CommandContext(ctx, "git", "-C", dir, "rev-parse", "--git-path", sub).Output()
		if err != nil {
			continue
		}
		path := strings.TrimSpace(string(out))
		if path == "" {
			continue
		}
		// --git-path prints relative to the command's cwd (dir, via -C).
		if !filepath.IsAbs(path) {
			path = filepath.Join(dir, path)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			continue // no rebase of this kind in progress, or unreadable
		}
		if name, ok := strings.CutPrefix(strings.TrimSpace(string(data)), "refs/heads/"); ok && name != "" {
			return name
		}
	}
	return ""
}

// tmuxSession returns the name of the tmux session the current process is in, or
// "" when not inside tmux, on any error, or if it times out. Like gitBranch it is
// deliberately best-effort: capture must never block or fail a tool call. The
// $TMUX guard avoids spawning tmux (and its stderr noise) outside a session;
// `display-message -p '#S'` resolves the current pane's session via $TMUX, so no
// `-t` target is needed. The name is user-chosen free text, so it is cleaned
// (unrenderable glyphs and padding stripped), scrubbed, and truncated to honour
// the scrub layer's fail-closed bias.
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
	return truncate(scrubString(cleanSessionName(string(out))), maxFieldLen)
}

// cleanSessionName makes a raw tmux session name safe and tidy for the web
// dashboard. tmux names routinely carry Nerd Font / devicon glyphs (Unicode
// Private Use Area) that a browser without that font draws as a tofu box, plus
// control characters and alignment padding. These glyphs show up as decorative
// prefixes and separators (e.g. "<icon>  branch  <icon> name"), so when any are
// present the meaningful name is whatever follows the LAST one — we keep only
// that tail. Remaining control characters are dropped and whitespace runs are
// collapsed to single spaces (trimming the ends). Ordinary text and real emoji
// are kept. A blank or all-glyph name becomes "".
func cleanSessionName(s string) string {
	if i := afterLastPUA(s); i >= 0 {
		s = s[i:]
	}
	cleaned := strings.Map(func(r rune) rune {
		switch {
		case isPUA(r):
			return -1 // Nerd Font / devicon glyph — unrenderable tofu in a browser
		case unicode.IsControl(r) && !unicode.IsSpace(r):
			return -1 // other control chars; whitespace is left for Fields to collapse
		default:
			return r
		}
	}, s)
	return strings.Join(strings.Fields(cleaned), " ")
}

// afterLastPUA returns the byte index just past the last Private Use Area glyph
// in s, or -1 if s contains none. Slicing s at this index yields the text that
// follows the final decorative icon/separator.
func afterLastPUA(s string) int {
	idx := -1
	for i, r := range s {
		if isPUA(r) {
			idx = i + utf8.RuneLen(r)
		}
	}
	return idx
}

// isPUA reports whether r lies in a Unicode Private Use Area, where Nerd Fonts
// and devicon icon sets place their glyphs.
func isPUA(r rune) bool {
	return (r >= 0xE000 && r <= 0xF8FF) || // BMP PUA
		(r >= 0xF0000 && r <= 0xFFFFD) || // Plane 15 PUA-A
		(r >= 0x100000 && r <= 0x10FFFD) // Plane 16 PUA-B
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

// numField reads a JSON number field (decoded as float64 by encoding/json) from
// a map as an int64, reporting whether it was present and numeric. Used for
// duration_ms, which str() cannot read.
func numField(m map[string]any, key string) (int64, bool) {
	if m == nil {
		return 0, false
	}
	if v, ok := m[key].(float64); ok {
		return int64(v), true
	}
	return 0, false
}

func debugf(format string, a ...any) {
	if os.Getenv("CLODHOPPER_DEBUG") != "" {
		fmt.Fprintf(os.Stderr, "clodhopper: "+format+"\n", a...)
	}
}
