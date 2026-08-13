// Roster — the "Agents (live + waiting on you)" board, and the only section
// that matters day to day.
//
// Two real layouts, not one layout squeezed: >640px keeps the dense table
// (columns are how you scan a board of agents); ≤640px renders a prioritised
// card — status and branch lead, doing follows, and the identity fields (app,
// id, session) drop to one muted footer line. Everything that acts — peek and
// actions — is a proper 44px control that expands a panel inside the card
// rather than a 10px button wedged into a cell.
const ROSTER_DS = window.ClodhopperDesignSystem_ca5794;
const { StatusBadge, CIDot, SessionChip, BranchLabel, CopyPath, PeekButton, PanePeek, ActionBar } = ROSTER_DS;

const ALERT_STATUS = new Set(['waiting for you', 'needs you', 'needs approval', 'needs input']);

function useNarrow() {
  const q = '(max-width: 640px)';
  // data-force-narrow lets the phone-width preview page show the card layout
  // inside a wide catalog frame, where a media query would see the frame.
  const forced = document.documentElement.hasAttribute('data-force-narrow');
  const [narrow, setNarrow] = React.useState(() => forced || window.matchMedia(q).matches);
  React.useEffect(() => {
    if (forced) return;
    const mq = window.matchMedia(q);
    const on = () => setNarrow(mq.matches);
    mq.addEventListener('change', on);
    return () => mq.removeEventListener('change', on);
  }, []);
  return narrow;
}

// Stand-in for the server round-trip: the kit reports what would have been run.
function outcomeText(id, agent, opts) {
  const mods = (opts.admin ? ' --admin' : '') + (opts.force ? ' --force' : '');
  switch (id) {
    case 'monitor-ci': return '↳ sent /clear then /monitor-ci to ' + agent.tmux;
    case 'new-monitor': return '↳ started nono in tmux session ' + agent.tmux + '-ci running /monitor-ci';
    case 'squash': return '↳ merge-pr --squash' + mods;
    case 'close': return '↳ merge-pr --close' + mods;
    case 'ready': return '↳ gh pr ready';
    default: return '';
  }
}

function useRosterPanels() {
  const [open, setOpen] = React.useState({});
  const [msg, setMsg] = React.useState({});
  return {
    open, msg,
    toggle: (id, kind) => setOpen((o) => ({ ...o, [id]: o[id] === kind ? null : kind })),
    close: (id) => setOpen((o) => ({ ...o, [id]: null })),
    run: (agent) => (actionId, opts) => setMsg((m) => ({ ...m, [agent.sessionId]: outcomeText(actionId, agent, opts) })),
  };
}

function Doing({ a }) {
  return (
    <React.Fragment>
      {a.doingActive ? <span>{a.doing}</span> : <em className="ck-em" title="last completed">{a.doing}</em>}
      {a.lastCommand && a.lastCommand !== ('/' + a.doing) && (
        <div className="ck-lastcmd" title="last slash command">↳ {a.lastCommand}</div>
      )}
    </React.Fragment>
  );
}

function rowClass(a) {
  return [
    ALERT_STATUS.has(a.status) ? 'alert' : '',
    a.groupStart ? 'group-start' : '',
    a.grouped ? 'grouped' : '',
  ].filter(Boolean).join(' ');
}

/* ── ≤640px: one card per agent ─────────────────────────────────────────── */
function AgentCard({ a, panels }) {
  const state = panels.open[a.sessionId];
  return (
    <article className={'ck-agent ' + rowClass(a)}>
      <div className="ck-agent-top">
        <StatusBadge status={a.status} />
        <span className="ck-agent-idle" title="idle">{a.idle}</span>
      </div>
      <div className="ck-agent-branch">
        <CIDot status={a.ci} />
        <BranchLabel branch={a.branch} rebasing={a.rebasing} />
      </div>
      <div className="ck-agent-doing"><Doing a={a} /></div>
      <div className="ck-agent-foot">
        <span>{a.app}</span>
        <SessionChip id={a.sessionId} color={a.color} />
        <span className="ck-agent-tmux">{a.tmux || '—'}</span>
      </div>
      <div className="ck-agent-controls">
        {a.live && (
          <PeekButton touch open={state === 'peek'} onClick={() => panels.toggle(a.sessionId, 'peek')} />
        )}
        <button type="button" className="ck-actbtn" aria-expanded={state === 'actions'}
          onClick={() => panels.toggle(a.sessionId, 'actions')}>
          actions {state === 'actions' ? '⌃' : '⌄'}
        </button>
      </div>
      {state === 'peek' && (
        <PanePeek wrap session={a.tmux} capture={window.CLOD_CAPTURE}
          onClose={() => panels.close(a.sessionId)}
          style={{ margin: '10px -12px -10px', borderRadius: '0 0 var(--radius) var(--radius)' }} />
      )}
      {state === 'actions' && (
        <div className="ck-agent-actions">
          <ActionBar layout="stacked" live={!!a.live} message={panels.msg[a.sessionId]} onRun={panels.run(a)} />
        </div>
      )}
    </article>
  );
}

/* ── >640px: the dense table ────────────────────────────────────────────── */
function AgentRow({ a, panels }) {
  const state = panels.open[a.sessionId];
  return (
    <React.Fragment>
      <tr className={rowClass(a)}>
        <td className="ck-ci" data-label="CI"><CIDot status={a.ci} /></td>
        <td className="ck-branch" data-label="branch">
          <span className="ck-branchcell">
            <BranchLabel branch={a.branch} rebasing={a.rebasing} />
            {a.cwd && <CopyPath path={a.cwd} className="ck-copy" />}
          </span>
        </td>
        <td data-label="app" className="ck-app">{a.app}</td>
        <td data-label="status"><StatusBadge status={a.status} /></td>
        <td className="ck-doing" data-label="doing"><Doing a={a} /></td>
        <td className="ck-idle" data-label="idle">{a.idle}</td>
        <td className="ck-sess" data-label="id"><SessionChip id={a.sessionId} color={a.color} /></td>
        <td className="ck-name" data-label="session">
          <span className="ck-sessname">{a.tmux || '—'}</span>
          {a.live && <PeekButton open={state === 'peek'} onClick={() => panels.toggle(a.sessionId, 'peek')} />}
        </td>
        <td className="ck-actions" data-label="actions">
          <button type="button" className="ck-actbtn" aria-expanded={state === 'actions'}
            title="squash · admin · close · ready · monitor ci"
            onClick={() => panels.toggle(a.sessionId, 'actions')}>⋯ actions</button>
        </td>
      </tr>
      {state && (
        <tr className="ck-panelrow">
          <td colSpan={9}>
            {state === 'peek'
              ? <PanePeek session={a.tmux} capture={window.CLOD_CAPTURE} onClose={() => panels.close(a.sessionId)} />
              : (
                <div className="ck-panelpad">
                  <ActionBar live={!!a.live} message={panels.msg[a.sessionId]} onRun={panels.run(a)} />
                </div>
              )}
          </td>
        </tr>
      )}
    </React.Fragment>
  );
}

function Roster({ agents }) {
  const narrow = useNarrow();
  const panels = useRosterPanels();

  if (narrow) {
    return <div className="ck-agents">{agents.map((a) => <AgentCard key={a.sessionId} a={a} panels={panels} />)}</div>;
  }

  return (
    <div className="ck-tablewrap">
      <table className="ck-table ck-roster">
        <colgroup>
          <col style={{ width: '4ch' }} /><col style={{ width: '24ch' }} />
          <col style={{ width: '13ch' }} /><col style={{ width: '17ch' }} />
          <col style={{ width: '30ch' }} /><col style={{ width: '6ch' }} />
          <col style={{ width: '12ch' }} /><col /><col style={{ width: '13ch' }} />
        </colgroup>
        <thead>
          <tr>
            <th className="ck-ci">CI</th><th>branch</th><th>app</th><th>status</th>
            <th>doing</th><th>idle</th><th>id</th><th>session</th><th>actions</th>
          </tr>
        </thead>
        <tbody>
          {agents.map((a) => <AgentRow key={a.sessionId} a={a} panels={panels} />)}
        </tbody>
      </table>
    </div>
  );
}

Object.assign(window, { Roster });
