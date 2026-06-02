package main

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

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

func TestMergeClodhopperHooks_Empty(t *testing.T) {
	settings := map[string]any{}
	added, skipped, err := mergeClodhopperHooks(settings, testCmd)
	if err != nil {
		t.Fatal(err)
	}
	if added != len(clodhopperEvents) || skipped != 0 {
		t.Fatalf("added=%d skipped=%d, want %d/0", added, skipped, len(clodhopperEvents))
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
	if added != len(clodhopperEvents) {
		t.Fatalf("added=%d, want %d (foreign hook is not clodhopper)", added, len(clodhopperEvents))
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
	if added != 0 || skipped != len(clodhopperEvents) {
		t.Fatalf("second pass added=%d skipped=%d, want 0/%d", added, skipped, len(clodhopperEvents))
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
	if settings["hooks"].(map[string]any)["SessionStart"] != nil {
		t.Error("preceding event was mutated despite error")
	}
}

func TestMergeClodhopperHooks_WrongHooksType(t *testing.T) {
	settings := map[string]any{"hooks": "garbage"}
	if _, _, err := mergeClodhopperHooks(settings, testCmd); err == nil {
		t.Error("want error for non-object hooks, got nil")
	}
	if settings["hooks"] != "garbage" {
		t.Error("settings mutated despite error")
	}
}

func TestGitRepoName(t *testing.T) {
	dir := t.TempDir()
	gitInitOnBranch(t, dir, "main")
	want := filepath.Base(dir)
	if got := gitRepoName(dir); got != want {
		t.Errorf("gitRepoName = %q, want %q", got, want)
	}
}

// In a linked worktree, the worktree folder is often named after its branch.
// gitRepoName must still return the main repo's name so the source-app label
// stays distinct from the branch.
func TestGitRepoName_Worktree(t *testing.T) {
	base := t.TempDir()
	repo := filepath.Join(base, "myrepo")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	gitInitOnBranch(t, repo, "main")
	// Name the worktree dir after its branch to reproduce the confusing case.
	wt := filepath.Join(base, "feature-x")
	gitAddWorktree(t, repo, wt, "feature-x")
	if got := gitRepoName(wt); got != "myrepo" {
		t.Errorf("gitRepoName(worktree) = %q, want %q", got, "myrepo")
	}
	// --git-common-dir is location-independent, so a subdir of the worktree
	// resolves to the same main repo name.
	sub := filepath.Join(wt, "pkg", "sub")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	if got := gitRepoName(sub); got != "myrepo" {
		t.Errorf("gitRepoName(worktree subdir) = %q, want %q", got, "myrepo")
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

func TestReadSettings_Empty(t *testing.T) {
	p := filepath.Join(t.TempDir(), "empty.json")
	if err := os.WriteFile(p, []byte("   \n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := readSettings(p)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Errorf("want empty map, got %v", got)
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

func TestWriteSettings_NoHTMLEscaping(t *testing.T) {
	p := filepath.Join(t.TempDir(), ".claude", "settings.json")
	settings := map[string]any{}
	if _, _, err := mergeClodhopperHooks(settings, testCmd); err != nil {
		t.Fatal(err)
	}
	if err := writeSettings(p, settings); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	s := string(raw)
	if strings.Contains(s, `\u003e`) || strings.Contains(s, `\u0026`) {
		t.Errorf("settings.json contains HTML-escaped metacharacters:\n%s", s)
	}
	if !strings.Contains(s, ">/dev/null") || !strings.Contains(s, "&& clodhopper ingest") {
		t.Errorf("settings.json does not contain clean shell command:\n%s", s)
	}
}

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

func TestDoInit_MalformedFileNotClobbered(t *testing.T) {
	dir := t.TempDir()
	path := settingsPath(dir, false)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	const bad = "{ this is not valid json"
	if err := os.WriteFile(path, []byte(bad), 0o644); err != nil {
		t.Fatal(err)
	}
	opts := initOptions{dir: dir, project: true, sourceApp: "x", guard: "command"}
	if err := doInit(opts, strings.NewReader(""), io.Discard); err == nil {
		t.Fatal("want error for malformed settings file, got nil")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != bad {
		t.Errorf("malformed file was modified:\n got: %q\nwant: %q", string(raw), bad)
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

func TestRun_InitDispatchesToRunInit(t *testing.T) {
	// A success (exit 0) path is unreachable via run()'s unknown-command default
	// (which returns 2), so this proves "init" actually dispatches to runInit.
	// --dry-run writes nothing; --project + --source-app need no prompt or git.
	t.Chdir(t.TempDir())
	if code := run([]string{"init", "--project", "--source-app", "demo", "--dry-run"}); code != 0 {
		t.Errorf("run init --dry-run = %d, want 0", code)
	}
}
