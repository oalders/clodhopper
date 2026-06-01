package main

import (
	"context"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// guardPrefix returns the shell guard that precedes the clodhopper invocation
// for the given --guard kind. "command" is the portable POSIX form (the
// default, safe in committed files); "is" uses the personal `is there` helper.
func guardPrefix(kind string) (string, error) {
	switch kind {
	case "command":
		return "command -v clodhopper >/dev/null 2>&1", nil
	case "is":
		return "is there clodhopper", nil
	default:
		return "", fmt.Errorf(`invalid --guard %q (want "command" or "is")`, kind)
	}
}

// ingestCommand builds the full hook command string from a guard prefix and a
// source-app label.
func ingestCommand(guard, sourceApp string) string {
	return guard + " && clodhopper ingest --source-app " + sourceApp + " 2>/dev/null || true"
}

// clodhopperEvents is the canonical set of Claude Code lifecycle events that
// `init` wires up. Keeping it as one slice means the merge logic and the docs
// read the same source of truth.
var clodhopperEvents = []string{
	"SessionStart", "SessionEnd", "UserPromptSubmit",
	"PreToolUse", "PostToolUse", "PostToolUseFailure",
	"Stop", "Notification", "PermissionRequest",
	"SubagentStart", "SubagentStop", "PreCompact",
}

// hookGroup returns one matcher-less hook group ({"hooks":[{...}]}) for command.
// Matcher-less means match-all, which is what we want for blanket observability.
func hookGroup(command string) map[string]any {
	return map[string]any{
		"hooks": []any{
			map[string]any{
				"type":    "command",
				"timeout": 5,
				"command": command,
			},
		},
	}
}

// mergeClodhopperHooks adds the clodhopper ingest hook (command) for each event
// in clodhopperEvents to settings["hooks"], idempotently. It returns how many
// events were newly wired (added) and how many already had a clodhopper hook
// (skipped). It never overwrites foreign hooks. If settings["hooks"] or any
// event value we touch is present but the wrong JSON type, it returns an error
// and leaves settings unmodified (fail closed — never clobber hand-edited
// config).
func mergeClodhopperHooks(settings map[string]any, command string) (added, skipped int, err error) {
	var hooks map[string]any
	switch h := settings["hooks"].(type) {
	case nil:
		hooks = map[string]any{}
	case map[string]any:
		hooks = h
	default:
		return 0, 0, fmt.Errorf("settings hooks is not an object")
	}

	// Validate every event value before mutating any, so a type error on a later
	// event can't leave earlier ones half-applied.
	for _, ev := range clodhopperEvents {
		switch hooks[ev].(type) {
		case nil, []any:
			// ok: missing or already an array
		default:
			return 0, 0, fmt.Errorf("settings hooks.%s is not an array", ev)
		}
	}

	for _, ev := range clodhopperEvents {
		groups, _ := hooks[ev].([]any)
		if hasClodhopperHook(groups) {
			skipped++
			continue
		}
		hooks[ev] = append(groups, hookGroup(command))
		added++
	}
	settings["hooks"] = hooks
	return added, skipped, nil
}

// gitRepoName returns the basename of the git work-tree root containing dir, or
// "" if dir is empty, not a repo, or git is unavailable. Like gitBranch it is
// deliberately best-effort with a tight timeout.
func gitRepoName(dir string) string {
	if dir == "" {
		return ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", "-C", dir, "rev-parse", "--show-toplevel")
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	top := strings.TrimSpace(string(out))
	if top == "" {
		return ""
	}
	return filepath.Base(top)
}

// hasClodhopperHook reports whether any hook command in groups already invokes
// `clodhopper ingest`. Malformed entries are skipped, not errored on — we only
// fail closed on the top-level event-value type.
func hasClodhopperHook(groups []any) bool {
	for _, g := range groups {
		gm, ok := g.(map[string]any)
		if !ok {
			continue
		}
		hs, ok := gm["hooks"].([]any)
		if !ok {
			continue
		}
		for _, h := range hs {
			hm, ok := h.(map[string]any)
			if !ok {
				continue
			}
			if cmd, ok := hm["command"].(string); ok && strings.Contains(cmd, "clodhopper ingest") {
				return true
			}
		}
	}
	return false
}
