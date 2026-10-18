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

// runSquash opens the row's actions panel and runs squash (arm, then confirm).
// The busy flag holds repaints off until the caller releases it, so none lands
// between the two clicks or before the result is asserted.
async function runSquash(page: Page) {
  await page.evaluate(() => { window.__ckActionBusy = 1; });
  await page.locator(actBtn).click();
  const run = page.locator(`${prform} .prrun`);
  await run.click();
  await expect(run).toHaveText('✓ confirm');
  await run.click();
}

async function expectUnlocked(page: Page) {
  await expect(page.locator(`${prform} .prrun`)).toBeEnabled();
  await expect(page.locator(`${prform} .prrun`)).toHaveText('run');
  const radios = page.locator(`${prform} .prmode`);
  await expect(radios).toHaveCount(3);
  for (const r of await radios.all()) await expect(r).toBeEnabled();
}

test('a started merge reports the server text, unlocks, and keeps it across a repaint', async ({ page }) => {
  const posts = await stubAction(page, { ok: true, exitCode: 0, timedOut: false, output: started });
  await runSquash(page);

  const msg = page.locator(`${prform} .actmsg`);
  await expect(msg).toHaveText(started);
  await expectUnlocked(page);
  expect(posts).toHaveLength(1);
  expect(posts[0].postData()).toBe(`session_id=${sess}&action=squash`);

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
  await runSquash(page);

  await expect(page.locator(`${prform} .actmsg`)).toHaveText('failed: boom');
  await expectUnlocked(page);
  await page.evaluate(() => { window.__ckActionBusy = 0; });
});
