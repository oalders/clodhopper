import type { Page, Request } from '@playwright/test';
import { test, expect, abortUnstubbedExec } from './fixtures';

// The PR form's result line for a merge (#188). Runs against serve.sh's exec
// server (the "exec desktop 1440" project); every /api/action is answered
// here, and one a test forgot to stub is aborted and fails that test. Every
// /api/state answer gets a fresh signature, so each one-second poll repaints;
// repaints are counted off the ch:contentswap event swapContent fires.

declare global {
  interface Window {
    __swaps: number;
    __ckActionBusy: number;
  }
}

const sess = 'e2e-waiting-0000-1111-2222-333333333333';
const actBtn = `#content button.actbtn[data-actions="${sess}"]`;
const prform = `#content tr.actrow[data-actions-row="${sess}"] .prform`;
const started = 'merge started in tmux window merge-pr';

let unstubbed: string[] = [];

test.beforeEach(async ({ page }) => {
  unstubbed = await abortUnstubbedExec(page);
  let polls = 0;
  await page.route('**/api/state?*', async (route) => {
    const response = await route.fetch();
    const json = await response.json();
    return route.fulfill({ response, json: { ...json, signature: `merge-${++polls}` } });
  });
  await page.addInitScript(() => {
    window.__swaps = 0;
    document.addEventListener('ch:contentswap', () => window.__swaps++);
  });
  await page.goto('/?refresh=1');
});

test.afterEach(async ({ page }) => {
  // The poller keeps a /api/state fetch in flight; let the test end under it.
  await page.unrouteAll({ behavior: 'ignoreErrors' });
  expect(unstubbed, 'exec endpoint requests with no stub').toEqual([]);
});

// stubAction answers every /api/action with body and returns the requests seen.
async function stubAction(page: Page, body: object): Promise<Request[]> {
  const seen: Request[] = [];
  await page.route('**/api/action', (route) => {
    seen.push(route.request());
    return route.fulfill({ json: body });
  });
  return seen;
}

// runMerge opens the row's actions panel, lets choose() pick the mode and
// modifiers, and runs it (arm, then confirm). The busy flag holds repaints off
// until the caller releases it, so none lands between the clicks or before the
// result is asserted.
async function runMerge(page: Page, choose: (page: Page) => Promise<void> = async () => {}) {
  await page.evaluate(() => { window.__ckActionBusy = 1; });
  await page.locator(actBtn).click();
  await choose(page);
  const run = page.locator(`${prform} .prrun`);
  await run.click();
  await expect(run).toHaveText('✓ confirm');
  await run.click();
}

// expectUnlocked checks the form is usable again. --admin and --force apply
// to squash only, so syncForm greys them in any other mode.
async function expectUnlocked(page: Page, squash = true) {
  await expect(page.locator(`${prform} .prrun`)).toBeEnabled();
  await expect(page.locator(`${prform} .prrun`)).toHaveText('run');
  const radios = page.locator(`${prform} .prmode`);
  await expect(radios).toHaveCount(3);
  for (const r of await radios.all()) await expect(r).toBeEnabled();
  for (const cls of ['.practadmin', '.practforce']) {
    const box = page.locator(`${prform} ${cls}`);
    if (await box.count() === 0) continue;
    if (squash) await expect(box).toBeEnabled();
    else await expect(box).toBeDisabled();
  }
}

const cases: { name: string; want: string; squash: boolean; choose?: (page: Page) => Promise<void> }[] = [
  { name: 'squash', want: 'squash', squash: true },
  {
    name: 'close', want: 'close', squash: false,
    choose: (page) => page.locator(`${prform} .prmode[data-mode="close"]`).click(),
  },
  {
    name: 'squash --admin', want: 'squash-admin', squash: true,
    choose: (page) => page.locator(`${prform} .practadmin`).check(),
  },
];

for (const c of cases) {
  test(`a started ${c.name} posts ${c.want}, reports the server text, and unlocks`, async ({ page }) => {
    const posts = await stubAction(page, { ok: true, exitCode: 0, timedOut: false, output: started });
    await runMerge(page, c.choose);

    await expect(page.locator(`${prform} .actmsg`)).toHaveText(started);
    await expectUnlocked(page, c.squash);
    expect(posts).toHaveLength(1);
    expect(posts[0].postData()).toBe(`session_id=${sess}&action=${c.want}`);
  });
}

test('a started merge keeps its server text across a repaint', async ({ page }) => {
  const posts = await stubAction(page, { ok: true, exitCode: 0, timedOut: false, output: started });
  await runMerge(page);

  const msg = page.locator(`${prform} .actmsg`);
  await expect(msg).toHaveText(started);
  expect(posts).toHaveLength(1);

  // Release the hold-off and wait for a repaint that really replaced the line.
  const old = await msg.elementHandle();
  const before = await page.evaluate(() => window.__swaps);
  await page.evaluate(() => { window.__ckActionBusy = 0; });
  await expect.poll(() => page.evaluate(() => window.__swaps)).toBeGreaterThan(before);
  expect(await old!.evaluate((n) => n.isConnected)).toBe(false);

  await expect(msg).toHaveText(started);
  await expectUnlocked(page);
});

test('a failed merge reports the failure and unlocks', async ({ page }) => {
  await stubAction(page, { ok: false, exitCode: -1, timedOut: false, output: 'boom' });
  await runMerge(page);

  await expect(page.locator(`${prform} .actmsg`)).toHaveText('failed: boom');
  await expectUnlocked(page);
});

// The merge-queue section's end button reuses the roster's two-step confirm (#195).
test('end in the merge-queue section posts end for the queued session', async ({ page }) => {
  const posts = await stubAction(page, { ok: true, exitCode: 0, timedOut: false, output: '' });
  await page.evaluate(() => { window.__ckActionBusy = 1; });
  await page.locator('#ck-mq summary').click();
  const end = page.locator('#ck-mq button.pract[data-action="end"]');
  // An empty result line must not open a second flex line under the controls. Not
  // toBeHidden: Playwright already counts an empty zero-height box as hidden.
  await expect(page.locator('#ck-mq .actmsg')).toHaveCSS('display', 'none');
  await end.click();
  await expect(end).toHaveText('✓ confirm');
  await end.click();

  await expect.poll(() => posts.length).toBe(1);
  expect(posts[0].postData()).toBe('session_id=e2e-queued-0000-1111-2222-333333333333&action=end');
  const msg = page.locator('#ck-mq .actmsg');
  await expect(msg).toHaveText('dismissed — row will clear');
  // The fixed-width actions cell wraps the result onto its own line under ⤢, inside the cell.
  const cell = (await page.locator('#ck-mq td.actions').boundingBox())!;
  const peek = (await page.locator('#ck-mq button.peek').boundingBox())!;
  const box = (await msg.boundingBox())!;
  expect(box.x).toBeLessThanOrEqual(peek.x);
  expect(box.y).toBeGreaterThanOrEqual(peek.y + peek.height);
  expect(box.x + box.width).toBeLessThanOrEqual(cell.x + cell.width);
});

// The actions col is fixed and the session col flexes, not the other way round (#199).
test('the merge-queue session name and its peek + end controls are not clipped', async ({ page }) => {
  await page.evaluate(() => { window.__ckActionBusy = 1; });
  await page.locator('#ck-mq summary').click();
  const name = page.locator('#ck-mq td.tmux');
  await expect(name).toHaveText('seeded-merge-queue-session-name');
  const clipped = (el: Element) => el.scrollWidth > el.clientWidth;
  expect(await name.evaluate(clipped)).toBe(false);
  const actions = page.locator('#ck-mq td.actions');
  await expect(actions.locator('button.peek')).toBeVisible();
  const end = actions.locator('button.pract[data-action="end"]');
  await expect(end).toBeVisible();
  expect(await actions.evaluate(clipped)).toBe(false);
  // Armed, the cell also holds "✓ confirm" and its ✕ cancel; the busy flag keeps a repaint from disarming it.
  await end.click();
  await expect(end).toHaveText('✓ confirm');
  await expect(actions.locator('button.actcancel')).toBeVisible();
  expect(await actions.evaluate(clipped)).toBe(false);
  // The cell wraps, so also pin ⤢, confirm and ✕ to one line: a too-narrow col would wrap, not clip.
  const mid = async (sel: string) => { const b = (await actions.locator(sel).boundingBox())!; return b.y + b.height / 2; };
  const line = await mid('button.peek');
  for (const sel of ['button.pract[data-action="end"]', 'button.actcancel']) {
    expect(Math.abs((await mid(sel)) - line)).toBeLessThanOrEqual(1);
  }
  await actions.locator('button.actcancel').click();
  await expect(end).toHaveText('end');
});

// On a phone the queue card's peek is a 44px touch target and the card never scrolls sideways.
test('the merge-queue actions cell fits a phone', async ({ page }) => {
  await page.setViewportSize({ width: 390, height: 844 });
  await page.locator('#ck-mq summary').click();
  const peek = (await page.locator('#ck-mq td.actions button.peek').boundingBox())!;
  expect(peek.height).toBeGreaterThanOrEqual(44);
  const end = (await page.locator('#ck-mq td.actions button.pract[data-action="end"]').boundingBox())!;
  expect(end.height).toBeGreaterThanOrEqual(44);
  expect(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth)).toBe(true);
});
