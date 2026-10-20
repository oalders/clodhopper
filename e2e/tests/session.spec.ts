import type { Page, Request } from '@playwright/test';
import { test, expect, fontsReady, abortUnstubbedExec } from './fixtures';

// The roster's session-actions column (#208): rebase as a session action, the
// lone end on a pane-less row, and the #128 invariant that arming a button
// moves nothing. Runs against serve.sh's exec server; every /api/action is
// answered here, and one a test forgot to stub is aborted and fails that test.

declare global {
  interface Window {
    __ckActionBusy: number;
  }
}

// e2e_seed_test.go gives the waiting row a tmux pane and the idle row none.
const paneSess = 'e2e-waiting-0000-1111-2222-333333333333';
const panelessSess = 'e2e-idle-0000-1111-2222-333333333333';
const group = (sess: string) => `#content tr.actrow[data-actions-row="${sess}"] .actsession`;

let unstubbed: string[] = [];

test.beforeEach(async ({ page }) => {
  unstubbed = await abortUnstubbedExec(page);
  await page.goto('/?refresh=1');
  await fontsReady(page);
});

test.afterEach(async ({ page }) => {
  // The poller keeps a /api/state fetch in flight; let the test end under it.
  await page.unrouteAll({ behavior: 'ignoreErrors' });
  expect(unstubbed, 'exec endpoint requests with no stub').toEqual([]);
});

// stubAction answers every /api/action with body (success by default) and
// returns the requests seen.
const okBody = { ok: true, exitCode: 0, timedOut: false, output: '' };
async function stubAction(page: Page, body: object = okBody): Promise<Request[]> {
  const seen: Request[] = [];
  await page.route('**/api/action', (route) => {
    seen.push(route.request());
    return route.fulfill({ json: body });
  });
  return seen;
}

// openActions opens the row's actions panel. The busy flag holds repaints off,
// so none replaces the panel between a test's clicks and its assertions.
async function openActions(page: Page, sess: string) {
  await page.evaluate(() => { window.__ckActionBusy = 1; });
  await page.locator(`#content button.actbtn[data-actions="${sess}"]`).click();
  await expect(page.locator(group(sess))).toBeVisible();
}

// TOL is how far, in px, two fractional layout coordinates may differ and
// still count as the same; every geometry check here uses it.
const TOL = 1;

// expectNear compares layout coordinates within TOL.
function expectNear(got: number, want: number, what: string) {
  expect(Math.abs(got - want), `${what}: ${got} vs ${want}`).toBeLessThanOrEqual(TOL);
}

// armRebase arms and confirms the pane row's rebase.
async function armRebase(page: Page) {
  const rebase = page.locator(`${group(paneSess)} button.pract[data-action="rebase"]`);
  await rebase.click();
  await expect(rebase).toHaveText('✓ confirm');
  await rebase.click();
  return rebase;
}

test('rebase sends /kitchen-sink:git-rebase and leaves the row in place', async ({ page }) => {
  const posts = await stubAction(page);
  await openActions(page, paneSess);
  const g = page.locator(group(paneSess));
  // The visible label is just "rebase"; screen readers get the branch and what it does.
  await expect(page.locator(`#content tr.actrow[data-actions-row="${paneSess}"]`)
    .getByRole('group', { name: 'session actions' })
    .getByRole('button', { name: /^rebase .*git-rebase/ })).toBeVisible();
  const rebase = g.locator('button.pract[data-action="rebase"]');
  await rebase.click();
  await expect(rebase).toHaveText('✓ confirm');
  await expect(g.locator('.actmsg')).toContainText('/kitchen-sink:git-rebase');
  await rebase.click();

  await expect(g.locator('.actmsg')).toHaveText('sent /kitchen-sink:git-rebase');
  expect(posts).toHaveLength(1);
  expect(posts[0].postData()).toBe(`session_id=${paneSess}&action=rebase`);
  // Unlike end, rebase does not clear the row, so its group unlocks for reuse.
  await expect(rebase).toHaveText('rebase');
  await expect(rebase).toBeEnabled();
  await expect(page.locator(`#content button.actbtn[data-actions="${paneSess}"]`)).toBeVisible();
});

const failures = [
  { name: 'a refused', body: { ok: false, exitCode: 1, timedOut: false, output: 'no pane' }, want: 'failed: no pane' },
  { name: 'a timed-out', body: { ok: false, exitCode: -1, timedOut: true, output: '' }, want: 'timed out — verify before retrying' },
];
for (const f of failures) {
  test(`${f.name} rebase reports it and unlocks for a retry`, async ({ page }) => {
    const posts = await stubAction(page, f.body);
    await openActions(page, paneSess);
    const rebase = await armRebase(page);
    await expect(page.locator(`${group(paneSess)} .actmsg`)).toHaveText(f.want);
    await expect(rebase).toHaveText('rebase');
    await expect(rebase).toBeEnabled();
    // The retry really goes out: arming and confirming again posts a second time.
    await armRebase(page);
    await expect.poll(() => posts.length).toBe(2);
    expect(posts[1].postData()).toBe(`session_id=${paneSess}&action=rebase`);
  });
}

test('a pane-less row offers only end, which posts end', async ({ page }) => {
  const posts = await stubAction(page);
  await openActions(page, panelessSess);
  const g = page.locator(group(panelessSess));
  await expect(g.locator('button.pract')).toHaveCount(1);
  await expect(g.locator('button.pract[data-action="rebase"]')).toHaveCount(0);
  await expect(g.getByRole('button', { name: /^end .*dismiss/ })).toBeVisible();
  const end = g.locator('button.pract[data-action="end"]');
  await end.click();
  await expect(end).toHaveText('✓ confirm');
  await end.click();

  await expect.poll(() => posts.length).toBe(1);
  expect(posts[0].postData()).toBe(`session_id=${panelessSess}&action=end`);
});

// expectArmingMovesNothing arms each of the row's given session actions in turn
// and checks the #128 invariant: the column keeps its width, the armed button
// stays put, and its ✓ confirm and ✕ share one line.
async function expectArmingMovesNothing(page: Page, sess: string, actions: string[]) {
  const g = page.locator(group(sess));
  const col = g.locator('xpath=ancestor::div[contains(@class, "actcol")]');
  for (const action of actions) {
    const btn = g.locator(`button.pract[data-action="${action}"]`);
    // Positions are taken relative to the column: the click may scroll the
    // page, which moves every viewport-relative box.
    const rel = async () => {
      const c = (await col.boundingBox())!;
      const b = (await btn.boundingBox())!;
      return { width: c.width, x: b.x - c.x, y: b.y - c.y };
    };
    const before = await rel();
    await btn.click();
    await expect(btn).toHaveText('✓ confirm');
    const cancel = g.locator('button.actcancel');
    await expect(cancel).toBeVisible();

    const after = await rel();
    expectNear(after.width, before.width, `${action}: column width`);
    expectNear(after.x, before.x, `${action}: armed button x`);
    expectNear(after.y, before.y, `${action}: armed button y`);
    const armed = (await btn.boundingBox())!;
    const x = (await cancel.boundingBox())!;
    expectNear(x.y + x.height / 2, armed.y + armed.height / 2, `${action}: ✕ on the confirm line`);

    await cancel.click();
    await expect(btn).not.toHaveText('✓ confirm');
  }
}

const rows = [
  { name: 'a pane row', sess: paneSess, actions: ['monitor-ci', 'rebase', 'end'] },
  { name: 'a pane-less row', sess: panelessSess, actions: ['end'] },
];

for (const r of rows) {
  test(`arming a session action moves nothing on desktop, on ${r.name}`, async ({ page }) => {
    await openActions(page, r.sess);
    await expectArmingMovesNothing(page, r.sess, r.actions);
  });

  test(`arming a session action moves nothing on a phone, where end spans the row, on ${r.name}`, async ({ page }) => {
    await page.setViewportSize({ width: 375, height: 812 });
    await openActions(page, r.sess);
    const g = page.locator(group(r.sess));
    const box = (await g.boundingBox())!;
    const end = g.locator('button.pract[data-action="end"]');
    expectNear((await end.boundingBox())!.width, box.width, 'unarmed end width');
    await expectArmingMovesNothing(page, r.sess, r.actions);
    // Armed, end gives up the span so its ✕ fits beside it (the :not(.confirm) rule).
    await end.click();
    await expect(end).toHaveText('✓ confirm');
    expect((await end.boundingBox())!.width).toBeLessThan(box.width - 1);
    await g.locator('button.actcancel').click();
    expect(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth)).toBe(true);
  });
}

// The pane-less row's narrow session column leaves room for the PR column beside it.
test('a pane-less row keeps its PR column on the session column line', async ({ page }) => {
  await openActions(page, panelessSess);
  const row = page.locator(`#content tr.actrow[data-actions-row="${panelessSess}"]`);
  const sess = (await row.locator('.actcol.sesscol').boundingBox())!;
  const pr = (await row.locator('.actcol.prcol').boundingBox())!;
  expectNear(pr.y, sess.y, 'PR column top');
});
