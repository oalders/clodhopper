# `clodhopper init` — design

## Goal

Automate wiring a project into clodhopper. Today a user copies a `hooks` block
into a project's `.claude/settings.json` by hand (see README "Wiring a
project"). `clodhopper init` writes that block for them: one command that adds a
clodhopper ingest hook to every relevant Claude Code lifecycle event, idempotently
and without clobbering existing settings.

## Command

A new subcommand, dispatched in `main.go` alongside `ingest`/`serve`/`prune` and
implemented in a new `init.go`:

```
clodhopper init [--project | --local] [--source-app NAME] [--guard is|command] [--dry-run]
```

`init` operates on the **current working directory**: it reads/writes
`<cwd>/.claude/settings.json` or `<cwd>/.claude/settings.local.json`, creating
`.claude/` if it does not exist.

Unlike `ingest`, `init` is an ordinary CLI command. The "ingest must never fail a
tool call / always exit 0" invariant does **not** apply here: `init` prints
diagnostics to stderr and returns a non-zero exit code on error.

### Flags

| Flag | Effect |
|---|---|
| `--project` | Write to `.claude/settings.json` (committed / shared). |
| `--local` | Write to `.claude/settings.local.json` (gitignored / personal). |
| neither `--project` nor `--local` | Interactively prompt the user to choose project or local. |
| both `--project` and `--local` | Error (mutually exclusive). |
| `--source-app NAME` | Label passed to `clodhopper ingest --source-app`. Optional; derived when omitted (see "source-app derivation"). |
| `--guard command` (default) | Guard prefix `command -v clodhopper >/dev/null 2>&1`. |
| `--guard is` | Guard prefix `is there clodhopper`. |
| `--dry-run` | Compute and print the result (summary + resulting `hooks` block) but write nothing. |

`--guard` accepts only `command` or `is`; any other value is an error. It defaults
to `command` — the portable POSIX form, safe for committed (`--project`) files and
for teammates without the personal `is there` helper. Pass `--guard is` to use the
`is there clodhopper` helper.

### Interactive prompt

When neither `--project` nor `--local` is given, prompt on stdin/stderr, e.g.:

```
Write clodhopper hooks to (p)roject .claude/settings.json or (l)ocal .claude/settings.local.json? [p/l]
```

`p` → project file, `l` → local file. Unrecognized input re-prompts or errors
(implementation detail; either is acceptable). To avoid adding a TTY-detection
dependency (the project's only dependency is `go-sqlite3`), non-interactive
handling is by I/O, not terminal detection: when neither flag is given and
reading the prompt response from stdin yields no input (immediate EOF), error and
tell the user to pass `--project` or `--local`.

## What it writes

The 12 lifecycle events (canonical list, defined as a single package-level slice
in `init.go` so the merge function and any docs read the same source):

```
SessionStart, SessionEnd, UserPromptSubmit,
PreToolUse, PostToolUse, PostToolUseFailure,
Stop, Notification, PermissionRequest,
SubagentStart, SubagentStop, PreCompact
```

`init` adds this matcher group to the event's array:

```json
{ "hooks": [ { "type": "command", "timeout": 5,
  "command": "<GUARD> && clodhopper ingest --source-app <NAME> 2>/dev/null || true" } ] }
```

Where `<GUARD>` is:

- `--guard command` (default): `command -v clodhopper >/dev/null 2>&1`
- `--guard is`: `is there clodhopper`

So the default command string is:

```
command -v clodhopper >/dev/null 2>&1 && clodhopper ingest --source-app NAME 2>/dev/null || true
```

and the `is` variant is:

```
is there clodhopper && clodhopper ingest --source-app NAME 2>/dev/null || true
```

`timeout: 5` and the matcher-less group shape match the user's reference diff. A
matcher-less group (no `"matcher"` key) is a match-all group; for tool events
(`PreToolUse`/`PostToolUse`) that means "every tool", which is what we want for
blanket observability. This is consistent across all 12 events, so the same group
shape is used for each.

> Note on guards: `command -v clodhopper >/dev/null 2>&1` is the portable POSIX
> default and is safe in a committed (`--project`) file. `--guard is` switches to
> `is there clodhopper`, which depends on the user's personal `is there` helper —
> fine for `--local`, but a `--project` file is committed and teammates without
> that helper would see hook errors. The default stays `command` so the common
> committed case is correct without thought.

## source-app derivation

- If `--source-app NAME` is given, use it verbatim (works anywhere).
- Otherwise derive from the git repo name: the basename of
  `git rev-parse --show-toplevel`, run with a tight timeout following the existing
  `gitBranch` side-effect pattern.
- If derivation fails (not in a git repo, `git` missing/timeout) **and** no
  `--source-app` was given → error: tell the user to pass `--source-app`
  explicitly.

## Merge behavior (idempotent)

1. Read the target file.
   - Absent → start from an empty settings object `{}`.
   - Present but not valid JSON → error; never clobber.
   - Present and valid → parse into `map[string]any`. Only the `hooks` key is
     modified; every other key is preserved untouched.
2. For each of the 12 events:
   - Event key missing under `hooks` → add the clodhopper matcher group.
   - Event already contains a hook whose `command` string contains
     `clodhopper ingest` → **skip** (leave as-is, even if its source-app or guard
     differ; re-running never duplicates a clodhopper hook).
   - Event contains other, non-clodhopper hooks → **append** the clodhopper group,
     preserving the existing entries.
3. **Type safety / fail closed.** The merge assumes `hooks` is a JSON object and
   each event value is an array. If the existing file violates this (e.g.
   `"hooks": "x"`, or `"PreToolUse": "garbage"` rather than an array), the merge
   does not coerce or overwrite — it errors and writes nothing. This preserves the
   codebase's fail-closed bias (never destroy hand-edited config).
4. Write the file back with `encoding/json` `MarshalIndent` (2-space indent).
   - Known effect: Go marshals map keys alphabetically, so an existing
     `settings.json` will be re-sorted / reformatted on write — a one-time noisy
     git diff for committed files. `--dry-run` lets the user preview this before
     it happens; the README calls it out.
   - With `--dry-run`, steps 1–3 run as normal but this write is skipped; the
     resulting `hooks` block and the summary are printed instead.
5. Print a summary, e.g.:
   `wired 7 event(s), 3 already present -> .claude/settings.json`
   - "already present" explicitly includes events wired by hand with a *different*
     guard or source-app — `init` reports them as present and leaves them
     untouched rather than rewriting. The summary wording makes this visible so a
     stale hand-written source-app isn't silently assumed to be updated.

### Testable core

The merge is factored into a pure function operating on in-memory data, e.g.:

```go
// mergeClodhopperHooks adds the clodhopper ingest hook for each event in the
// canonical list to settings["hooks"], skipping events that already have one.
// Returns counts, or an error if the existing hooks structure is wrong-typed.
func mergeClodhopperHooks(settings map[string]any, command string) (added, skipped int, err error)
```

`command` is the fully-built command string (guard + ingest + source-app); the
event list it iterates is the single package-level slice (see "What it writes").
This function touches neither disk, git, nor stdin, so it is unit-tested directly.

## Error handling

All of these print to stderr and exit non-zero:

- Unknown flag / unparseable args.
- Both `--project` and `--local`.
- Invalid `--guard` value.
- No `--source-app` and not in a git repo.
- Target file present but not valid JSON.
- Target file's `hooks`/event values are present but wrong-typed (fail closed).
- Unwritable target file / uncreatable `.claude/` directory.
- No flag given and stdin yields no prompt response (non-interactive).

## Testing

`init_test.go`:

- Merge into empty/missing settings → all 12 events added.
- Merge preserving a foreign (non-clodhopper) hook on an event → existing kept,
  clodhopper appended.
- Idempotent re-run → second pass adds 0, skips events already wired.
- Wrong-typed existing `hooks`/event value → merge returns an error, file untouched.
- Guard variants (`is` vs `command`) produce the expected `command` string.
- `--dry-run` → file on disk is unchanged; summary/block printed.
- Both-flags error path.
- source-app-required error path (no `--source-app`, not a repo).

Git, disk I/O, and the interactive prompt stay as thin wrappers around the pure
merge function so tests need none of them.

## Files touched

- `main.go` — add `case "init":` dispatch and a usage line.
- `init.go` (new) — `runInit`, flag parsing, prompt, source-app derivation,
  file read/write, and the pure `mergeClodhopperHooks`.
- `init_test.go` (new) — tests above.
- `README.md` — make `clodhopper init` the **primary** path under "Wiring a
  project"; demote the existing manual JSON block to "or, by hand". Both now use
  the same `command -v` guard; the manual block omits `timeout` whereas `init`
  writes `timeout: 5`. Also note that `init`'s first run on a committed file
  reorders keys (preview with `--dry-run`).
