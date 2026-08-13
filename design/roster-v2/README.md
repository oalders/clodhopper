# Design reference — roster v2

These files are a **local, checked-in copy** of the design handoff for the
roster-v2 work (mobile card + row actions + debug-gated diagnostics), pulled
from the `clodhopper Design System` project on claude.ai/design so the source of
truth lives next to the code and future changes don't have to guess.

**The rendered component source is the source of truth for appearance, not the
prose.** Where `HANDOFF.md` (prose) and the reference-kit component code
disagree, the component code wins — and both are kept here so the disagreement
is visible rather than lost.

| File | What it is |
|---|---|
| `HANDOFF.md` | The prose handoff (`design_handoff_roster_v2/README.md` upstream). The four-commit plan and the CSS-only §1 grid. |
| `ActionBar.jsx` | The per-row actions cluster — **the authoritative appearance/behaviour** of the session + PR clusters. Fully inline-styled. The PR mode picker is a **segmented toggle of `<button role="radio">`**, not `<input type="radio">`. |
| `Roster.jsx` | The roster board: the >640px table (`AgentRow`) and the ≤640px card (`AgentCard`), including where peek/actions live at each tier. |
| `index.html` | The kit entry point; the `ck-*` CSS (`.ck-actbtn`, `.ck-agent-controls`, panel backgrounds, etc.). |

Upstream project: `clodhopper Design System`
(`ca5794e7-49e0-44e5-8ea6-c5a1bc6526bb`), path `ui_kits/dashboard/` and
`components/core/`. Re-sync with the `DesignSync` tool / `/design-sync`.

## Class-name mapping (kit → shipped template)

The kit uses `ck-`-prefixed classes; `templates/dashboard.html` uses the
original names. Key equivalences:

- `td.ck-doing` → `td.summary`, `.ck-lastcmd` → `.lastcmd`
- `.ck-actbtn` → `button.actbtn`, `tr.ck-panelrow` → `tr.actrow` / `tr.panerow`
- `.ck-agent*` (card) → the `@media (max-width: 640px)` grid on `table.roster`
