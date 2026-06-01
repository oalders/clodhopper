package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
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

// settingsPath returns the project or local settings file under dir's .claude/.
func settingsPath(dir string, local bool) string {
	name := "settings.json"
	if local {
		name = "settings.local.json"
	}
	return filepath.Join(dir, ".claude", name)
}

// readSettings loads and parses the settings file at path. A missing or empty
// file yields an empty settings object (not an error). A present but malformed
// file is an error — we must never clobber it.
func readSettings(path string) (map[string]any, error) {
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return map[string]any{}, nil
	}
	if err != nil {
		return nil, err
	}
	if len(strings.TrimSpace(string(raw))) == 0 {
		return map[string]any{}, nil
	}
	var settings map[string]any
	if err := json.Unmarshal(raw, &settings); err != nil {
		return nil, fmt.Errorf("%s is not valid JSON: %w", path, err)
	}
	return settings, nil
}

// marshalIndentNoEscape renders v as 2-space-indented JSON without HTML-escaping,
// so shell metacharacters (>, &) in hook commands stay human-readable in the
// settings file. encoding/json escapes them by default; we don't want that here.
func marshalIndentNoEscape(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// writeSettings writes settings to path as 2-space-indented JSON, creating the
// parent .claude/ directory if needed.
func writeSettings(path string, settings map[string]any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	out, err := marshalIndentNoEscape(settings)
	if err != nil {
		return err
	}
	return os.WriteFile(path, out, 0o644)
}

// resolveSourceApp returns the source-app label: the explicit flag value if
// given, otherwise the git repo name of dir. It errors when neither is
// available, so init never silently mislabels events.
func resolveSourceApp(explicit, dir string) (string, error) {
	if explicit != "" {
		return explicit, nil
	}
	if name := gitRepoName(dir); name != "" {
		return name, nil
	}
	return "", fmt.Errorf("could not determine source-app: not in a git repo; pass --source-app NAME")
}

// initOptions are the resolved inputs to doInit, kept separate from flag parsing
// so the orchestration is testable with injected I/O and a temp dir.
type initOptions struct {
	dir       string // working directory whose .claude/ is targeted
	project   bool   // --project: write settings.json
	local     bool   // --local: write settings.local.json
	sourceApp string // --source-app override ("" = derive)
	guard     string // --guard kind ("command" | "is")
	dryRun    bool   // --dry-run: compute but don't write
}

// doInit performs the wiring described by opts, reading the prompt response from
// in and writing human output to out. It returns an error on any failure; the
// caller maps that to a non-zero exit code.
func doInit(opts initOptions, in io.Reader, out io.Writer) error {
	if opts.project && opts.local {
		return fmt.Errorf("--project and --local are mutually exclusive")
	}
	guard, err := guardPrefix(opts.guard)
	if err != nil {
		return err
	}
	sourceApp, err := resolveSourceApp(opts.sourceApp, opts.dir)
	if err != nil {
		return err
	}
	local, err := chooseLocal(opts, in, out)
	if err != nil {
		return err
	}

	path := settingsPath(opts.dir, local)
	settings, err := readSettings(path)
	if err != nil {
		return err
	}
	added, skipped, err := mergeClodhopperHooks(settings, ingestCommand(guard, sourceApp))
	if err != nil {
		return err
	}

	if opts.dryRun {
		blob, _ := marshalIndentNoEscape(settings["hooks"])
		fmt.Fprintf(out, "[dry-run] would wire %d event(s), %d already present -> %s\n", added, skipped, path)
		fmt.Fprintf(out, "%s", blob)
		return nil
	}
	if err := writeSettings(path, settings); err != nil {
		return err
	}
	fmt.Fprintf(out, "wired %d event(s), %d already present -> %s\n", added, skipped, path)
	return nil
}

// chooseLocal decides whether to target the local settings file. --project and
// --local are explicit; with neither, it prompts on out and reads p/l from in.
// A non-interactive run (empty stdin) with no flag is an error rather than a
// silent default.
func chooseLocal(opts initOptions, in io.Reader, out io.Writer) (bool, error) {
	if opts.local {
		return true, nil
	}
	if opts.project {
		return false, nil
	}
	fmt.Fprint(out, "Write clodhopper hooks to (p)roject .claude/settings.json or (l)ocal .claude/settings.local.json? [p/l] ")
	line, err := bufio.NewReader(in).ReadString('\n')
	resp := strings.ToLower(strings.TrimSpace(line))
	if resp == "" && err != nil {
		return false, fmt.Errorf("no --project/--local given and stdin is not interactive")
	}
	switch resp {
	case "p", "project":
		return false, nil
	case "l", "local":
		return true, nil
	default:
		return false, fmt.Errorf("unrecognized choice %q (want p or l)", resp)
	}
}

// runInit parses init's flags, resolves the working directory, and runs doInit
// against the real stdin/stdout. Unlike ingest, init reports errors and returns
// a non-zero exit code.
func runInit(args []string) int {
	fs := flag.NewFlagSet("init", flag.ContinueOnError)
	project := fs.Bool("project", false, "write .claude/settings.json (committed/shared)")
	local := fs.Bool("local", false, "write .claude/settings.local.json (gitignored)")
	sourceApp := fs.String("source-app", "", "source label (default: git repo name)")
	guard := fs.String("guard", "command", `binary guard: "command" (portable, default) or "is" (is there helper)`)
	dryRun := fs.Bool("dry-run", false, "print the result without writing")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	dir, err := os.Getwd()
	if err != nil {
		fmt.Fprintln(os.Stderr, "clodhopper init:", err)
		return 1
	}
	opts := initOptions{
		dir:       dir,
		project:   *project,
		local:     *local,
		sourceApp: *sourceApp,
		guard:     *guard,
		dryRun:    *dryRun,
	}
	if err := doInit(opts, os.Stdin, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "clodhopper init:", err)
		return 1
	}
	return 0
}
