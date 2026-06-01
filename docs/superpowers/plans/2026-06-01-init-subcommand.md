# `clodhopper init` Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add a `clodhopper init` subcommand that idempotently wires a project's `.claude/settings.json` (or `settings.local.json`) with clodhopper ingest hooks for all 12 Claude Code lifecycle events.

**Architecture:** One new file `init.go` holding small, single-purpose functions: pure builders for the guard/command strings, a pure `mergeClodhopperHooks` that mutates an in-memory settings map, thin git/file/prompt wrappers, and a `doInit` orchestrator with injected I/O so it's testable against a temp dir. `main.go` gains one dispatch case. The never-fail/exit-0 invariant of `ingest` does **not** apply here — `init` reports errors to stderr and returns non-zero.

**Tech Stack:** Go standard library only (`encoding/json`, `flag`, `os/exec`, `bufio`). No new dependencies. Tests use the standard `testing` package, `t.TempDir()`, and the existing `gitInitOnBranch` helper from `branch_test.go` (same package).

**Spec:** `docs/superpowers/specs/2026-05-31-init-subcommand-design.md`

---

## File Structure

- **Create `init.go`** — the entire feature:
  - `clodhopperEvents` — canonical 12-event slice (single source of truth).
  - `guardPrefix(kind) (string, error)` + `ingestCommand(guard, app) string` — pure string builders.
  - `hookGroup(command) map[string]any` + `mergeClodhopperHooks(settings, command) (added, skipped int, err error)` + `hasClodhopperHook([]any) bool` — pure merge core.
  - `gitRepoName(dir) string` — best-effort git side-effect (mirrors `gitBranch`).
  - `settingsPath(dir, local) string`, `readSettings(path)`, `writeSettings(path, settings)` — file layer.
  - `resolveSourceApp(explicit, dir) (string, error)` — label resolution.
  - `initOptions`, `doInit(opts, in, out) error`, `chooseLocal(opts, in, out) (bool, error)` — orchestration with injected I/O.
  - `runInit(args) int` — flag parsing + exit-code mapping.
- **Modify `main.go`** — add `case "init":` to the dispatch in `run()` and one line to `usage()`.
- **Create `init_test.go`** — unit + integration tests for all of the above.
- **Modify `README.md`** — make `init` the primary "Wiring a project" path; demote the manual block.

Tasks are ordered so each only depends on functions defined in earlier tasks.

---

### Task 1: Guard and command string builders

**Files:**
- Create: `init.go`
- Test: `init_test.go`

- [ ] **Step 1: Write the failing tests**

Create `init_test.go`:

```go
package main

import "testing"

// A realistic command string used across merge/orchestration tests. It MUST
// contain "clodhopper ingest" so the idempotency check recognizes it.
const testCmd = "command -v clodhopper >/dev/null 2>&1 && clodhopper ingest --source-app x 2>/dev/null || true"

func TestGuardPrefix(t *testing.T) {
	cmd, err := guardPrefix("command")
	if err != nil || cmd != "command -v clodhopper >/dev/null 2>&1" {
		t.Fatalf("command: got %q, err %v", cmd, err)
	}
	is, err := guardPrefix("is")
	if err != nil || is != "is there clodhopper" {
		t.Fatalf("is: got %q, err %v", is, err)
	}
	if _, err := guardPrefix("nope"); err == nil {
		t.Error("invalid guard: want error, got nil")
	}
}

func TestIngestCommand(t *testing.T) {
	got := ingestCommand("command -v clodhopper >/dev/null 2>&1", "mmir")
	want := "command -v clodhopper >/dev/null 2>&1 && clodhopper ingest --source-app mmir 2>/dev/null || true"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test -run 'TestGuardPrefix|TestIngestCommand' ./...`
Expected: FAIL — `undefined: guardPrefix`, `undefined: ingestCommand`.

- [ ] **Step 3: Write minimal implementation**

Create `init.go`:

```go
package main

import "fmt"

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
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test -run 'TestGuardPrefix|TestIngestCommand' ./...`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add init.go init_test.go
git commit -m "feat: clodhopper init guard and command builders"
```

---

### Task 2: Canonical event list and `mergeClodhopperHooks`

**Files:**
- Modify: `init.go`
- Test: `init_test.go`

- [ ] **Step 1: Write the failing tests**

Append to `init_test.go`:

```go
func TestMergeClodhopperHooks_Empty(t *testing.T) {
	settings := map[string]any{}
	added, skipped, err := mergeClodhopperHooks(settings, testCmd)
	if err != nil {
		t.Fatal(err)
	}
	if added != 12 || skipped != 0 {
		t.Fatalf("added=%d skipped=%d, want 12/0", added, skipped)
	}
	hooks := settings["hooks"].(map[string]any)
	for _, ev := range clodhopperEvents {
		groups, ok := hooks[ev].([]any)
		if !ok || len(groups) != 1 {
			t.Fatalf("%s: not wired (%v)", ev, hooks[ev])
		}
	}
}

func TestMergeClodhopperHooks_PreservesForeign(t *testing.T) {
	foreign := map[string]any{"hooks": []any{map[string]any{"type": "command", "command": "echo hi"}}}
	settings := map[string]any{"hooks": map[string]any{"PreToolUse": []any{foreign}}}
	added, _, err := mergeClodhopperHooks(settings, testCmd)
	if err != nil {
		t.Fatal(err)
	}
	groups := settings["hooks"].(map[string]any)["PreToolUse"].([]any)
	if len(groups) != 2 {
		t.Fatalf("PreToolUse groups = %d, want 2 (foreign + clodhopper)", len(groups))
	}
	if added != 12 {
		t.Fatalf("added=%d, want 12 (foreign hook is not clodhopper)", added)
	}
}

func TestMergeClodhopperHooks_Idempotent(t *testing.T) {
	settings := map[string]any{}
	if _, _, err := mergeClodhopperHooks(settings, testCmd); err != nil {
		t.Fatal(err)
	}
	added, skipped, err := mergeClodhopperHooks(settings, testCmd)
	if err != nil {
		t.Fatal(err)
	}
	if added != 0 || skipped != 12 {
		t.Fatalf("second pass added=%d skipped=%d, want 0/12", added, skipped)
	}
}

func TestMergeClodhopperHooks_WrongEventType(t *testing.T) {
	settings := map[string]any{"hooks": map[string]any{"PreToolUse": "garbage"}}
	if _, _, err := mergeClodhopperHooks(settings, testCmd); err == nil {
		t.Error("want error for wrong-typed event value, got nil")
	}
	if settings["hooks"].(map[string]any)["PreToolUse"] != "garbage" {
		t.Error("settings mutated despite error")
	}
}

func TestMergeClodhopperHooks_WrongHooksType(t *testing.T) {
	settings := map[string]any{"hooks": "garbage"}
	if _, _, err := mergeClodhopperHooks(settings, testCmd); err == nil {
		t.Error("want error for non-object hooks, got nil")
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test -run TestMergeClodhopperHooks ./...`
Expected: FAIL — `undefined: mergeClodhopperHooks`, `undefined: clodhopperEvents`.

- [ ] **Step 3: Write minimal implementation**

Add to `init.go` (add `"strings"` to the import block, making it `import ( "fmt"; "strings" )`):

```go
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
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test -run TestMergeClodhopperHooks ./...`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add init.go init_test.go
git commit -m "feat: idempotent clodhopper hook merge for 12 events"
```

---

### Task 3: `gitRepoName` helper

**Files:**
- Modify: `init.go`
- Test: `init_test.go`

- [ ] **Step 1: Write the failing tests**

Append to `init_test.go` (the `gitInitOnBranch` helper already exists in `branch_test.go`, same package — reuse it; `filepath` is needed in the import block):

```go
func TestGitRepoName(t *testing.T) {
	dir := t.TempDir()
	gitInitOnBranch(t, dir, "main")
	want := filepath.Base(dir)
	if got := gitRepoName(dir); got != want {
		t.Errorf("gitRepoName = %q, want %q", got, want)
	}
}

func TestGitRepoName_EmptyAndNonRepo(t *testing.T) {
	if got := gitRepoName(""); got != "" {
		t.Errorf("empty: want \"\", got %q", got)
	}
	if got := gitRepoName("/nonexistent/path/xyzzy"); got != "" {
		t.Errorf("nonexistent: want \"\", got %q", got)
	}
}
```

Add `"path/filepath"` to the test file's imports (change `import "testing"` to a block):

```go
import (
	"path/filepath"
	"testing"
)
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test -run TestGitRepoName ./...`
Expected: FAIL — `undefined: gitRepoName`.

- [ ] **Step 3: Write minimal implementation**

Add to `init.go` and expand its import block to `import ( "context"; "fmt"; "os/exec"; "path/filepath"; "strings"; "time" )`:

```go
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
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test -run TestGitRepoName ./...`
Expected: PASS (skips if `git` is unavailable).

- [ ] **Step 5: Commit**

```bash
git add init.go init_test.go
git commit -m "feat: gitRepoName best-effort helper"
```

---

### Task 4: Settings path and file read/write

**Files:**
- Modify: `init.go`
- Test: `init_test.go`

- [ ] **Step 1: Write the failing tests**

Append to `init_test.go` (add `"os"` to the test import block):

```go
func TestSettingsPath(t *testing.T) {
	if got := settingsPath("/x", false); got != "/x/.claude/settings.json" {
		t.Errorf("project: %q", got)
	}
	if got := settingsPath("/x", true); got != "/x/.claude/settings.local.json" {
		t.Errorf("local: %q", got)
	}
}

func TestReadSettings_Missing(t *testing.T) {
	got, err := readSettings(filepath.Join(t.TempDir(), "nope.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Errorf("want empty, got %v", got)
	}
}

func TestReadSettings_Invalid(t *testing.T) {
	p := filepath.Join(t.TempDir(), "s.json")
	if err := os.WriteFile(p, []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := readSettings(p); err == nil {
		t.Error("want error for invalid JSON, got nil")
	}
}

func TestWriteReadSettings_RoundTrip(t *testing.T) {
	p := filepath.Join(t.TempDir(), ".claude", "settings.json")
	if err := writeSettings(p, map[string]any{"model": "opus"}); err != nil {
		t.Fatal(err)
	}
	got, err := readSettings(p)
	if err != nil {
		t.Fatal(err)
	}
	if got["model"] != "opus" {
		t.Errorf("round-trip lost data: %v", got)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test -run 'TestSettingsPath|TestReadSettings|TestWriteReadSettings' ./...`
Expected: FAIL — `undefined: settingsPath`, `undefined: readSettings`, `undefined: writeSettings`.

- [ ] **Step 3: Write minimal implementation**

Add to `init.go` and expand its import block to add `"encoding/json"`, `"errors"`, and `"os"` (full block: `import ( "context"; "encoding/json"; "errors"; "fmt"; "os"; "os/exec"; "path/filepath"; "strings"; "time" )`):

```go
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

// writeSettings writes settings to path as 2-space-indented JSON, creating the
// parent .claude/ directory if needed.
func writeSettings(path string, settings map[string]any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	out, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return err
	}
	out = append(out, '\n')
	return os.WriteFile(path, out, 0o644)
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test -run 'TestSettingsPath|TestReadSettings|TestWriteReadSettings' ./...`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add init.go init_test.go
git commit -m "feat: settings path resolution and JSON read/write"
```

---

### Task 5: `resolveSourceApp`

**Files:**
- Modify: `init.go`
- Test: `init_test.go`

- [ ] **Step 1: Write the failing tests**

Append to `init_test.go`:

```go
func TestResolveSourceApp_Explicit(t *testing.T) {
	got, err := resolveSourceApp("mmir", "/nonexistent")
	if err != nil || got != "mmir" {
		t.Fatalf("got %q err %v", got, err)
	}
}

func TestResolveSourceApp_FromRepo(t *testing.T) {
	dir := t.TempDir()
	gitInitOnBranch(t, dir, "main")
	got, err := resolveSourceApp("", dir)
	if err != nil {
		t.Fatal(err)
	}
	if got != filepath.Base(dir) {
		t.Errorf("got %q, want %q", got, filepath.Base(dir))
	}
}

func TestResolveSourceApp_Error(t *testing.T) {
	if _, err := resolveSourceApp("", "/nonexistent/path/xyzzy"); err == nil {
		t.Error("want error when no flag and not a repo, got nil")
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test -run TestResolveSourceApp ./...`
Expected: FAIL — `undefined: resolveSourceApp`.

- [ ] **Step 3: Write minimal implementation**

Add to `init.go`:

```go
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
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test -run TestResolveSourceApp ./...`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add init.go init_test.go
git commit -m "feat: source-app resolution with repo-name fallback"
```

---

### Task 6: `doInit` orchestration and target prompt

**Files:**
- Modify: `init.go`
- Test: `init_test.go`

- [ ] **Step 1: Write the failing tests**

Append to `init_test.go` (add `"bytes"`, `"io"`, and `"strings"` to the test import block):

```go
func TestDoInit_WritesProjectSettings(t *testing.T) {
	dir := t.TempDir()
	var out bytes.Buffer
	opts := initOptions{dir: dir, project: true, sourceApp: "mmir", guard: "command"}
	if err := doInit(opts, strings.NewReader(""), &out); err != nil {
		t.Fatal(err)
	}
	settings, err := readSettings(settingsPath(dir, false))
	if err != nil {
		t.Fatal(err)
	}
	hooks := settings["hooks"].(map[string]any)
	if len(hooks) != 12 {
		t.Fatalf("want 12 events, got %d", len(hooks))
	}
	g := hooks["PreToolUse"].([]any)[0].(map[string]any)["hooks"].([]any)[0].(map[string]any)
	if !strings.Contains(g["command"].(string), "command -v clodhopper") {
		t.Errorf("guard not applied: %v", g["command"])
	}
	if !strings.Contains(g["command"].(string), "--source-app mmir") {
		t.Errorf("source-app not applied: %v", g["command"])
	}
	if !strings.Contains(out.String(), "wired 12 event(s)") {
		t.Errorf("summary: %q", out.String())
	}
}

func TestDoInit_DryRunWritesNothing(t *testing.T) {
	dir := t.TempDir()
	var out bytes.Buffer
	opts := initOptions{dir: dir, project: true, sourceApp: "x", guard: "command", dryRun: true}
	if err := doInit(opts, strings.NewReader(""), &out); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(settingsPath(dir, false)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("dry-run wrote a file: %v", err)
	}
	if !strings.Contains(out.String(), "[dry-run]") {
		t.Errorf("missing dry-run notice: %q", out.String())
	}
}

func TestDoInit_BothFlagsError(t *testing.T) {
	opts := initOptions{dir: t.TempDir(), project: true, local: true, sourceApp: "x", guard: "command"}
	if err := doInit(opts, strings.NewReader(""), io.Discard); err == nil {
		t.Error("want error for both --project and --local")
	}
}

func TestDoInit_PromptLocal(t *testing.T) {
	dir := t.TempDir()
	var out bytes.Buffer
	opts := initOptions{dir: dir, sourceApp: "x", guard: "command"}
	if err := doInit(opts, strings.NewReader("l\n"), &out); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(settingsPath(dir, true)); err != nil {
		t.Errorf("local settings not written: %v", err)
	}
}

func TestDoInit_NonInteractiveNoFlagError(t *testing.T) {
	opts := initOptions{dir: t.TempDir(), sourceApp: "x", guard: "command"}
	if err := doInit(opts, strings.NewReader(""), io.Discard); err == nil {
		t.Error("want error when no flag and empty stdin")
	}
}

func TestDoInit_InvalidGuardError(t *testing.T) {
	opts := initOptions{dir: t.TempDir(), project: true, sourceApp: "x", guard: "nope"}
	if err := doInit(opts, strings.NewReader(""), io.Discard); err == nil {
		t.Error("want error for invalid guard")
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test -run TestDoInit ./...`
Expected: FAIL — `undefined: initOptions`, `undefined: doInit`.

- [ ] **Step 3: Write minimal implementation**

Add to `init.go` and add `"bufio"` and `"io"` to its import block (full block now: `import ( "bufio"; "context"; "encoding/json"; "errors"; "fmt"; "io"; "os"; "os/exec"; "path/filepath"; "strings"; "time" )`):

```go
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
		blob, _ := json.MarshalIndent(settings["hooks"], "", "  ")
		fmt.Fprintf(out, "[dry-run] would wire %d event(s), %d already present -> %s\n", added, skipped, path)
		fmt.Fprintf(out, "%s\n", blob)
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
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test -run TestDoInit ./...`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add init.go init_test.go
git commit -m "feat: doInit orchestration with target prompt and dry-run"
```

---

### Task 7: `runInit`, flag parsing, and dispatch

**Files:**
- Modify: `init.go`
- Modify: `main.go:41-58` (the `switch` in `run()`) and `main.go:82-89` (`usage()`)
- Test: `init_test.go`

- [ ] **Step 1: Write the failing test**

Append to `init_test.go`:

```go
func TestRun_InitBothFlagsExitsNonZero(t *testing.T) {
	// Both flags fail in doInit before any I/O, so this is safe regardless of cwd.
	if code := run([]string{"init", "--project", "--local", "--source-app", "x"}); code == 0 {
		t.Error("want non-zero exit for --project --local")
	}
}

func TestRun_InitInvalidGuardExitsNonZero(t *testing.T) {
	if code := run([]string{"init", "--project", "--source-app", "x", "--guard", "nope"}); code == 0 {
		t.Error("want non-zero exit for invalid --guard")
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test -run TestRun_Init ./...`
Expected: FAIL — `init` is an unknown command (exit 2 currently, but `runInit` is undefined so it won't compile): `undefined: runInit`.

- [ ] **Step 3: Write minimal implementation**

Add to `init.go` (uses `flag` and `os`, already imported):

```go
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
```

Add `"flag"` to `init.go`'s import block (full block: `import ( "bufio"; "context"; "encoding/json"; "errors"; "flag"; "fmt"; "io"; "os"; "os/exec"; "path/filepath"; "strings"; "time" )`).

In `main.go`, add the dispatch case after the `prune` case (around `main.go:47`):

```go
	case "prune":
		return runPrune(rest)
	case "init":
		return runInit(rest)
```

In `main.go`, add a usage line to the `USAGE` block in `usage()` (after the `prune` line):

```go
  clodhopper prune [--days N]           delete events older than N days
  clodhopper init [--project|--local]   wire clodhopper hooks into .claude/settings(.local).json
  clodhopper --version                  print version and build metadata
```

- [ ] **Step 4: Run tests + vet + build to verify**

Run: `go test ./... && go vet ./... && go build ./...`
Expected: all PASS; build succeeds.

- [ ] **Step 5: Commit**

```bash
git add init.go main.go init_test.go
git commit -m "feat: wire clodhopper init into the CLI dispatch"
```

---

### Task 8: Manual smoke test and README

**Files:**
- Modify: `README.md:86-103` (the "Wiring a project" section)

- [ ] **Step 1: Manual smoke test (dry-run, then real, in a throwaway dir)**

Run:

```bash
go build -o /tmp/clodhopper ./... && cd "$(mktemp -d)" && git init -q && \
  /tmp/clodhopper init --project --source-app demo --dry-run && \
  /tmp/clodhopper init --project --source-app demo && \
  cat .claude/settings.json && \
  /tmp/clodhopper init --project --source-app demo   # second run: idempotent
```

Expected: the dry-run prints a `[dry-run]` summary and the hooks block but writes nothing; the second invocation prints `wired 12 event(s), 0 already present -> .claude/.../settings.json` and writes the file; `cat` shows all 12 events each with the `command -v clodhopper ... clodhopper ingest --source-app demo ...` command and `"timeout": 5`; the final invocation prints `wired 0 event(s), 12 already present`.

- [ ] **Step 2: Update the README "Wiring a project" section**

Replace the body of `## Wiring a project` (currently `README.md:86-103`) with:

````markdown
## Wiring a project

The easiest way is `clodhopper init`, run from the project's root. It writes the
ingest hooks for every Claude Code lifecycle event into the project's settings,
idempotently (safe to re-run):

```bash
clodhopper init --project                 # -> .claude/settings.json (committed)
clodhopper init --local                   # -> .claude/settings.local.json (gitignored)
clodhopper init --project --dry-run       # preview without writing
```

With neither `--project` nor `--local` it prompts you to choose. `--source-app`
defaults to the git repo name; pass `--source-app NAME` to override (required
outside a git repo). The generated command is guarded so environments without the
binary simply no-op; `--guard command` (default) uses the portable
`command -v clodhopper` check, `--guard is` uses the `is there clodhopper` helper.

`init` writes 2-space-indented JSON; the first run on an existing committed
settings file will re-sort its keys (a one-time noisy diff) — preview with
`--dry-run`.

Or, by hand: add hooks to the project's `.claude/settings.json` (or
`settings.local.json`). Guard on the binary existing so environments without
`clodhopper` simply no-op:

```json
{
  "hooks": {
    "PreToolUse": [
      { "hooks": [ { "type": "command",
        "command": "command -v clodhopper >/dev/null 2>&1 && clodhopper ingest --source-app myproject || true" } ] }
    ]
  }
}
```

`source_app` is a static label per project; finer grouping (worktree, session)
comes from the `cwd` / `session_id` already in each hook payload.
````

- [ ] **Step 3: Verify the build is still clean**

Run: `go build ./... && go vet ./...`
Expected: success (README-only change, but confirm nothing else drifted).

- [ ] **Step 4: Commit**

```bash
git add README.md
git commit -m "docs: document clodhopper init as the primary wiring path"
```

---

## Self-Review

**Spec coverage:**
- `init` subcommand + flag set (`--project`/`--local`/`--source-app`/`--guard`/`--dry-run`) → Tasks 1, 6, 7. ✓
- Target selection incl. interactive prompt + non-interactive error → Task 6 (`chooseLocal`). ✓
- `--guard` default `command`, `is` variant, invalid errors → Task 1 + Task 6/7. ✓
- source-app from repo name, error outside repo → Tasks 3, 5. ✓
- 12 canonical events as one slice → Task 2 (`clodhopperEvents`). ✓
- Matcher-less group, `timeout: 5` → Task 2 (`hookGroup`). ✓
- Idempotent merge, preserve foreign hooks, skip already-wired → Task 2. ✓
- Fail closed on wrong-typed `hooks`/event values → Task 2. ✓
- Missing file → empty; invalid JSON → error; never clobber → Task 4. ✓
- 2-space `MarshalIndent`, key re-sort caveat → Task 4 + README (Task 8). ✓
- `--dry-run` writes nothing, prints block → Task 6. ✓
- Summary wording incl. "already present" → Task 6 + smoke test (Task 8). ✓
- Dispatch + usage in `main.go` → Task 7. ✓
- README demotes manual block → Task 8. ✓
- Testability via pure functions + injected I/O → every task. ✓

**Placeholder scan:** No TBD/TODO; every code step shows complete code. ✓

**Type consistency:** `mergeClodhopperHooks(settings, command) (added, skipped int, err error)`, `guardPrefix(kind) (string, error)`, `ingestCommand(guard, sourceApp)`, `gitRepoName(dir)`, `settingsPath(dir, local)`, `readSettings(path)`, `writeSettings(path, settings)`, `resolveSourceApp(explicit, dir)`, `chooseLocal(opts, in, out)`, `doInit(opts, in, out)`, `runInit(args)` — names and signatures used consistently across tasks and tests. `testCmd` (defined Task 1) contains `clodhopper ingest`, which the idempotency test in Task 2 relies on. ✓
