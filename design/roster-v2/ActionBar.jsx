import React from 'react';

/**
 * ActionBar — the roster's per-row actions, in two clusters that work
 * differently because they are different kinds of act.
 *
 * `session` acts on the agent in front of you and is immediate: one button per
 * act (a /clear-and-watch, a fresh watcher), confirm-armed when it destroys
 * context.
 *
 * `pr` is a form, not a row of buttons. "squash" and "squash --admin" are the
 * same act with a modifier, so you pick the act, tick the modifiers, read the
 * command you are about to run, and press one submit. Nothing fires on the way
 * there — the submit is the only thing that acts.
 */

export const SESSION_ACTIONS = [
  { id: 'monitor-ci', label: 'monitor ci', title: '/clear then /monitor-ci in the running session', confirm: true, needsLive: true },
  { id: 'new-monitor', label: '+ watcher', title: 'start a new nono session in its own tmux session running /monitor-ci' },
];

export const PR_MODES = [
  { id: 'squash', label: 'squash', cmd: 'merge-pr --squash', modifiers: true },
  { id: 'close', label: 'close', cmd: 'merge-pr --close' },
  { id: 'ready', label: 'ready', cmd: 'gh pr ready' },
];

const EYEBROW = {
  fontSize: 'var(--fs-2xs)', letterSpacing: 'var(--tracking-wide)',
  textTransform: 'uppercase', color: 'var(--text-3)',
};

function btn({ stacked, armed, primary, disabled, full }) {
  return {
    fontFamily: 'var(--font-mono)',
    fontSize: stacked ? 'var(--fs-sm)' : 'var(--fs-xs)',
    lineHeight: 1.2,
    padding: stacked ? '11px 12px' : '4px 10px',
    minHeight: stacked ? 44 : 26,
    width: full ? '100%' : 'auto',
    borderRadius: 'var(--radius-sm)',
    border: '1px solid ' + (armed ? 'var(--fail)' : primary ? 'transparent' : 'var(--border-strong)'),
    background: armed ? 'var(--fail-bg)' : primary ? 'var(--brand)' : 'var(--surface-2)',
    color: armed ? 'var(--fail)' : primary ? 'var(--text-invert)' : 'var(--text-1)',
    opacity: disabled ? 0.45 : 1,
    cursor: disabled ? 'not-allowed' : 'pointer',
    whiteSpace: 'nowrap',
    transition: 'background var(--dur-fast) var(--ease), border-color var(--dur-fast) var(--ease)',
  };
}

function Check({ label, title, checked, disabled, stacked, onChange }) {
  return (
    <label title={title} style={{
      display: 'inline-flex', alignItems: 'center', gap: 6,
      fontSize: stacked ? 'var(--fs-sm)' : 'var(--fs-xs)',
      color: disabled ? 'var(--text-3)' : 'var(--text-2)',
      opacity: disabled ? 0.5 : 1,
      minHeight: stacked ? 44 : 0, cursor: disabled ? 'not-allowed' : 'pointer',
    }}>
      <input type="checkbox" checked={checked} disabled={disabled}
        onChange={(e) => onChange(e.target.checked)}
        style={{ margin: 0, width: stacked ? 18 : 13, height: stacked ? 18 : 13, accentColor: 'var(--brand)' }} />
      {label}
    </label>
  );
}

export function ActionBar({
  sessionActions = SESSION_ACTIONS,
  layout = 'inline',
  live = true,
  busy = false,
  message = '',
  onRun,
}) {
  const stacked = layout === 'stacked';
  const [armed, setArmed] = React.useState(null);   // session action id, or 'pr'
  const [mode, setMode] = React.useState('squash');
  const [admin, setAdmin] = React.useState(false);
  const [force, setForce] = React.useState(false);

  const chosen = PR_MODES.find((m) => m.id === mode);
  const command = chosen.cmd + (chosen.modifiers && admin ? ' --admin' : '') + (chosen.modifiers && force ? ' --force' : '');

  const clickSession = (a) => {
    if (busy) return;
    if (a.confirm && armed !== a.id) { setArmed(a.id); return; }
    setArmed(null);
    onRun?.(a.id, {});
  };
  const submitPr = () => {
    if (busy) return;
    if (armed !== 'pr') { setArmed('pr'); return; }
    setArmed(null);
    onRun?.(mode, { admin: chosen.modifiers && admin, force: chosen.modifiers && force });
  };

  const usable = sessionActions.filter((a) => !a.needsLive || live);

  return (
    <div style={{ display: 'flex', flexDirection: stacked ? 'column' : 'row', flexWrap: 'wrap', alignItems: stacked ? 'stretch' : 'flex-start', gap: stacked ? 14 : 24, minWidth: 0 }}>
      <div style={{ display: 'flex', flexDirection: 'column', gap: 6 }}>
        <span style={EYEBROW}>session</span>
        <div style={{ display: stacked ? 'grid' : 'flex', gridTemplateColumns: stacked ? 'repeat(2, minmax(0, 1fr))' : undefined, gap: 8 }}>
          {usable.map((a) => (
            <React.Fragment key={a.id}>
              <button type="button" title={a.title} disabled={busy} onClick={() => clickSession(a)}
                style={btn({ stacked, armed: armed === a.id, disabled: busy, full: stacked })}>
                {armed === a.id ? 'confirm?' : a.label}
              </button>
              {armed === a.id && (
                <button type="button" aria-label="cancel" onClick={() => setArmed(null)}
                  style={btn({ stacked, full: stacked })}>✕</button>
              )}
            </React.Fragment>
          ))}
        </div>
      </div>

      <div style={{ display: 'flex', flexDirection: 'column', gap: 6, minWidth: 0, flex: stacked ? undefined : '1 1 320px' }}>
        <span style={EYEBROW}>pull request</span>
        <div style={{ display: 'flex', flexWrap: 'wrap', alignItems: 'center', gap: 8 }}>
          <div role="radiogroup" aria-label="pull request action" style={{
            display: stacked ? 'grid' : 'inline-flex',
            gridTemplateColumns: stacked ? 'repeat(3, minmax(0, 1fr))' : undefined,
            border: '1px solid var(--border-strong)', borderRadius: 'var(--radius-sm)', overflow: 'hidden',
          }}>
            {PR_MODES.map((m) => (
              <button key={m.id} type="button" role="radio" aria-checked={mode === m.id} title={m.cmd}
                disabled={busy}
                onClick={() => { setMode(m.id); setArmed(null); }}
                style={{
                  ...btn({ stacked, disabled: busy }),
                  border: 0, borderRadius: 0, width: '100%',
                  background: mode === m.id ? 'var(--brand-soft)' : 'transparent',
                  color: mode === m.id ? 'var(--brand)' : 'var(--text-2)',
                  fontWeight: mode === m.id ? 600 : 400,
                }}>
                {m.label}
              </button>
            ))}
          </div>
          <Check label="--admin" title="bypass branch protection" stacked={stacked}
            checked={admin} disabled={busy || !chosen.modifiers} onChange={(v) => { setAdmin(v); setArmed(null); }} />
          <Check label="--force" title="merge even if the worktree has uncommitted changes" stacked={stacked}
            checked={force} disabled={busy || !chosen.modifiers} onChange={(v) => { setForce(v); setArmed(null); }} />
        </div>

        <div style={{ display: 'flex', flexWrap: 'wrap', alignItems: 'center', gap: 8 }}>
          <code style={{
            fontSize: 'var(--fs-xs)', color: 'var(--text-3)', whiteSpace: 'nowrap',
            overflow: 'hidden', textOverflow: 'ellipsis', minWidth: 0, flex: '1 1 auto',
          }}>{command}</code>
          <button type="button" disabled={busy} onClick={submitPr}
            style={{ ...btn({ stacked, armed: armed === 'pr', primary: armed !== 'pr', disabled: busy, full: stacked }), flex: stacked ? undefined : 'none' }}>
            {armed === 'pr' ? 'confirm — run it' : 'run'}
          </button>
          {armed === 'pr' && (
            <button type="button" onClick={() => setArmed(null)} style={btn({ stacked, full: stacked })}>cancel</button>
          )}
        </div>

        {message && (
          <span aria-live="polite" style={{ fontSize: 'var(--fs-2xs)', color: 'var(--text-3)', whiteSpace: 'pre-wrap', wordBreak: 'break-word' }}>{message}</span>
        )}
      </div>
    </div>
  );
}
