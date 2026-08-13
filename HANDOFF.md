# Handoff: roster v2 — mobile card, row actions, debug-gated diagnostics

Target: `templates/dashboard.html` (the Go `html/template`). Reference design:
`ui_kits/dashboard/index.html` in the design system (narrow the window past
640px to see the card layout). Class-name mapping between the two:
`td.summary` → `td.ck-doing`, `.lastcmd` → `.ck-lastcmd`.

Four changes, roughly in order of payoff. 1 and 2 are CSS-only. 3 and 4 need
small markup/Go changes.

---

## 1. The ≤640px roster becomes a prioritised card (CSS only)

Today every roster cell is a wrapping `label value` chip, so nine fields of
equal weight compete and the row reads as soup. Give the card a hierarchy
instead: **status + idle**, then **CI + branch**, then **doing**, then the
identity fields as one muted footer line, then actions.

The markup already carries everything needed (`data-label`, the cell classes),
so this is a grid placement. Add inside the existing `@media (max-width: 640px)`
block, **after** the generic `tr { display: flex … }` rules so it wins for the
roster only:

```css
      /* The roster is not a table on a phone; it is a card with a hierarchy.
         Grid placement rather than source order: status leads, the identity
         fields (app / id / session) fall to one muted footer line. */
      table.roster tbody tr:not(.panerow) {
        display: grid;
        grid-template-columns: auto minmax(0, 1fr) auto;
        align-items: baseline;
        gap: 8px;
      }
      table.roster td.status   { grid-area: 1 / 1 / auto / 3; }
      table.roster td.idle     { grid-area: 1 / 3; justify-self: end; }
      table.roster td.ci       { grid-area: 2 / 1; }
      table.roster td.branch   { grid-area: 2 / 2 / auto / 4; font-size: var(--fs-md); }
      table.roster td.summary  { grid-area: 3 / 1 / auto / 4; }
      table.roster td[data-label="app"] { grid-area: 4 / 1; }
      table.roster td.sess     { grid-area: 4 / 2; }
      table.roster td.namecell { grid-area: 4 / 3; justify-content: flex-end; }
      table.roster td.actions  { grid-area: 5 / 1 / auto / 4; }
      /* The footer line is context, not content. */
      table.roster td[data-label="app"],
      table.roster td.sess,
      table.roster td.namecell { color: var(--text-3); font-size: var(--fs-xs); }
      /* Column names are noise once the layout says what each field is; only
         the idle timer still needs naming. */
      table.roster td::before { content: none; }
      table.roster td.idle::before { content: attr(data-label); }
      /* Copy-to-clipboard has no utility on a phone — there is nowhere to paste
         a path. Drop the affordance rather than shrink it. */
      table.roster td.branch .copyicon { display: none; }
      table.roster td.branch .copycwd { cursor: default; }
```

## 2. Real tap targets for peek and the PR actions (CSS only)

`button.pract` is 10px text with 2px padding, and `button.peek` is a bare `⤢`
with `all: unset`. Both are well under the 44px guidance. Still inside the
`@media (max-width: 640px)` block:

```css
      /* Peek becomes a real control on touch. */      table.roster td.namecell button.peek {
        min-height: 44px; padding: 0 12px;
        border: 1px solid var(--border-strong); border-radius: var(--radius-sm);
        background: var(--surface-2);
      }
      /* The action cluster becomes a two-column grid of full-size buttons,
         with force and the result line spanning it. */
      td.actions .actgroup { display: grid; grid-template-columns: 1fr 1fr; gap: 8px; width: 100%; }
      td.actions button.pract { min-height: 44px; font-size: var(--fs-sm); border-radius: var(--radius-sm); }
      td.actions .actforce { grid-column: 1 / -1; min-height: 44px; font-size: var(--fs-sm); }
      td.actions .actforce input { width: 18px; height: 18px; }
      td.actions .actmsg { grid-column: 1 / -1; }
```

Desktop equivalent (outside the media query): bump `button.pract` to
`font-size: var(--fs-xs); padding: 3px 8px; min-height: 24px;` — 10px text in a
13px table is smaller than it needs to be even with a mouse.

> **Better, if you want the markup change:** put the whole cluster behind one
> `⋯ actions` toggle per row that expands a panel row beneath (exactly like the
> existing `tr.panerow`), so six buttons and a checkbox are not painted on every
> row of a quiet board. That is what the reference kit does, on both tiers.

## 3. Two new session actions, and a PR form instead of four buttons

Two clusters. The `session` cluster is buttons that act on their own:

| Action | Runs | Confirm |
|---|---|---|
| `monitor ci` | `/clear` then `/monitor-ci` in the session's running nono pane | yes — it drops the agent's context |
| `+ watcher` | a new nono session in its own tmux session running `/monitor-ci` | no |

The `pr` cluster becomes a small form. `squash` and `squash --admin` are the
same act with a modifier, so today's four peer buttons plus a stray `force`
checkbox read as four unrelated things with no submit. Instead: a three-way
choice (`squash` · `close` · `ready`), `--admin` and `--force` as modifiers
enabled only for `squash`, a line printing the exact command
(`merge-pr --squash --admin`), and one **run** button that arms the existing
confirm step. Nothing acts until run is pressed twice.

Markup, alongside the existing `.pract` buttons (offer `monitor-ci` only when
`.LiveTmux`, the same gate the peek button uses):

```html
<button type="button" class="pract" data-action="monitor-ci"
        title="/clear then /monitor-ci in the running session">monitor ci</button>
<button type="button" class="pract" data-action="new-monitor"
        title="start a new nono session in its own tmux session running /monitor-ci">+ watcher</button>
```

Both route through the same `.actgroup` confirm/busy machinery already in the
page; `monitor-ci` should carry the confirm step, `new-monitor` should not
(nothing is destroyed by spawning a watcher).

## 4. Activity and Recent events become debug-only

Nobody reads them in normal use, and on a phone they are most of the scroll.
Gate both sections behind a `debug` flag:

- Go side: a `Debug bool` on the view data, set from `?debug=1`.
- Template: wrap the two `<section class="ck-card">` blocks in `{{ if .Debug }}`.
- Header: a `debug` button beside `filters` / theme, persisting to
  `localStorage['ch-debug']` and reloading with `?debug=1` (same shape as the
  existing filter-strip toggle).

The roster is then the whole page by default, which is what the board is for.

## 5. Scroll the peek to the bottom when it opens

`pre.panecap` already scrolls (`overflow: auto; max-height: 40vh`) but opens at
the top, which is the oldest part of the capture. In `openPane()`, after the
text is set: `pre.scrollTop = pre.scrollHeight;` — and do the same in the
poll-restore path in `swapContent()` unless a saved scroll position is being
restored.

---

## Verify

1. ≤640px: each agent reads status → branch → doing → muted footer, and every
   control is at least 44px. No `label value` chip soup.
2. 641–960px and >960px: unchanged apart from the larger `pract` buttons.
3. `debug` off: only the Agents card renders. On: all three, as before.
