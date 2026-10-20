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

// stubAction answers every /api/action with success and returns the requests seen.
async function stubAction(page: Page): Promise<Request[]> {
  const seen: Request[] = [];
  await page.route('**/api/action', (route) => {
    seen.push(route.request());
    return route.fulfill({ json: { ok: true, exitCode: 0, timedOut: false, output: '' } });
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

test('rebase sends /kitchen-sink:git-rebase and leaves the row in place', async ({ page }) => {
  const posts = await stubAction(page);
  await openActions(page, paneSess);
  const g = page.locator(group(paneSess));
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

test('a pane-less row offers only end, which posts end', async ({ page }) => {
  const posts = await stubAction(page);
  await openActions(page, panelessSess);
  const g = page.locator(group(panelessSess));
  await expect(g.locator('button.pract')).toHaveCount(1);
  await expect(g.locator('button.pract[data-action="rebase"]')).toHaveCount(0);
  const end = g.locator('button.pract[data-action="end"]');
  await end.click();
  await expect(end).toHaveText('✓ confirm');
  await end.click();

  await expect.poll(() => posts.length).toBe(1);
  expect(posts[0].postData()).toBe(`session_id=${panelessSess}&action=end`);
});

// expectArmingMovesNothing arms each confirm-guarded session action in turn and
// checks the #128 invariant: the column keeps its width, the armed button stays
// put, and its ✓ confirm and ✕ share one line.
async function expectArmingMovesNothing(page: Page) {
  const g = page.locator(group(paneSess));
  const col = g.locator('xpath=ancestor::div[contains(@class, "actcol")]');
  for (const action of ['monitor-ci', 'rebase', 'end']) {
    const btn = g.locator(`button.pract[data-action="${action}"]`);
    const width = (await col.boundingBox())!.width;
    const before = (await btn.boundingBox())!;
    await btn.click();
    await expect(btn).toHaveText('✓ confirm');
    const cancel = g.locator('button.actcancel');
    await expect(cancel).toBeVisible();

    expect((await col.boundingBox())!.width, `${action}: column width`).toBe(width);
    const after = (await btn.boundingBox())!;
    expect([after.x, after.y], `${action}: armed button position`).toEqual([before.x, before.y]);
    const x = (await cancel.boundingBox())!;
    expect(Math.abs((x.y + x.height / 2) - (after.y + after.height / 2)), `${action}: ✕ on the confirm line`).toBeLessThanOrEqual(1);

    await cancel.click();
    await expect(btn).not.toHaveText('✓ confirm');
  }
}

test('arming a session action moves nothing on desktop', async ({ page }) => {
  await openActions(page, paneSess);
  await expectArmingMovesNothing(page);
});

test('arming a session action moves nothing on a phone, where end spans the row', async ({ page }) => {
  await page.setViewportSize({ width: 375, height: 812 });
  await openActions(page, paneSess);
  const g = page.locator(group(paneSess));
  const box = (await g.boundingBox())!;
  const end = (await g.locator('button.pract[data-action="end"]').boundingBox())!;
  expect(Math.abs(end.width - box.width)).toBeLessThanOrEqual(1);
  await expectArmingMovesNothing(page);
  expect(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth)).toBe(true);
});
