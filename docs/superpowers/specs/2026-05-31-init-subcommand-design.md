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
clodhopper init [--project | --local] [--source-app NAME] [--guard is|command]
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
| `--guard is` (default) | Guard prefix `is there clodhopper`. |
| `--guard command` | Guard prefix `command -v clodhopper >/dev/null 2>&1`. |

`--guard` accepts only `is` or `command`; any other value is an error. It defaults
to `is`.

### Interactive prompt

When neither `--project` nor `--local` is given, prompt on stdin/stderr, e.g.:

```
Write clodhopper hooks to (p)roject .claude/settings.json or (l)ocal .claude/settings.local.json? [p/l]
```

`p` → project file, `l` → local file. Unrecognized input re-prompts or errors
(implementation detail; either is acceptable). When run non-interactively (no TTY
/ empty stdin) with no flag, error and tell the user to pass `--project` or
`--local`.

## What it writes

For each of the 10 lifecycle events:

```
SessionStart, SessionEnd, UserPromptSubmit, PreToolUse, Stop, Notification,
PostToolUseFailure, PermissionRequest, SubagentStart, SubagentStop
```

`init` adds this matcher group to the event's array:

```json
{ "hooks": [ { "type": "command", "timeout": 5,
  "command": "<GUARD> && clodhopper ingest --source-app <NAME> 2>/dev/null || true" } ] }
```

Where `<GUARD>` is:

- `--guard is` (default): `is there clodhopper`
- `--guard command`: `command -v clodhopper >/dev/null 2>&1`

So the default command string is:

```
is there clodhopper && clodhopper ingest --source-app NAME 2>/dev/null || true
```

and the `command` variant is:

```
command -v clodhopper >/dev/null 2>&1 && clodhopper ingest --source-app NAME 2>/dev/null || true
```

`timeout: 5` and the matcher-less group shape match the user's reference diff.

> Note on guards: `is there clodhopper` depends on the user's personal `is there`
> helper. That is fine for `--local`, but a `--project` file is committed and
> teammates without that helper would see hook errors. `--guard command` exists
> so the committed case can opt into the portable POSIX form. The default stays
> `is` per the user's preference.

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
2. For each of the 10 events:
   - Event key missing under `hooks` → add the clodhopper matcher group.
   - Event already contains a hook whose `command` string contains
     `clodhopper ingest` → **skip** (leave as-is, even if its source-app or guard
     differ; re-running never duplicates a clodhopper hook).
   - Event contains other, non-clodhopper hooks → **append** the clodhopper group,
     preserving the existing entries.
3. Write the file back with `encoding/json` `MarshalIndent` (2-space indent).
   - Known effect: Go marshals map keys alphabetically, so an existing
     `settings.json` will be re-sorted / reformatted on write. Acceptable;
     settings files are plain JSON without comments.
4. Print a summary, e.g.:
   `wired 7 event(s), 3 already present -> .claude/settings.json`

### Testable core

The merge is factored into a pure function operating on in-memory data, e.g.:

```go
// mergeClodhopperHooks adds the clodhopper ingest hook for each lifecycle event
// to settings["hooks"], skipping events that already have one. Returns counts.
func mergeClodhopperHooks(settings map[string]any, command string) (added, skipped int)
```

`command` is the fully-built command string (guard + ingest + source-app). This
function touches neither disk, git, nor stdin, so it is unit-tested directly.

## Error handling

All of these print to stderr and exit non-zero:

- Unknown flag / unparseable args.
- Both `--project` and `--local`.
- Invalid `--guard` value.
- No `--source-app` and not in a git repo.
- Target file present but not valid JSON.
- Unwritable target file / uncreatable `.claude/` directory.
- No flag given and no interactive TTY to prompt.

## Testing

`init_test.go`:

- Merge into empty/missing settings → all 10 events added.
- Merge preserving a foreign (non-clodhopper) hook on an event → existing kept,
  clodhopper appended.
- Idempotent re-run → second pass adds 0, skips events already wired.
- Guard variants (`is` vs `command`) produce the expected `command` string.
- Both-flags error path.
- source-app-required error path (no `--source-app`, not a repo).

Git, disk I/O, and the interactive prompt stay as thin wrappers around the pure
merge function so tests need none of them.

## Files touched

- `main.go` — add `case "init":` dispatch and a usage line.
- `init.go` (new) — `runInit`, flag parsing, prompt, source-app derivation,
  file read/write, and the pure `mergeClodhopperHooks`.
- `init_test.go` (new) — tests above.
- `README.md` — document `clodhopper init` under "Wiring a project".
