import type { Locator, Page } from '@playwright/test';
import { test, expect, abortUnstubbedExec } from './fixtures';

// A row stays in the merge-queue table once its PR merges, until dismissed
// (#211). serve.sh's stub gh answers the landing fixture queued on its first
// lookup (the server's readiness check) and merged on every later one. Its
// dismiss is the one /api/action let through to serve: it execs nothing.

const landing = 'e2e-landing-0000-1111-2222-333333333333';
const row = `#ck-mq #row-${landing}`;
const dismiss = (page: Page) => page.locator(row).getByRole('button', { name: /^dismiss / });
// setBusy holds repaints off (1) or releases them (0); window's typing is merge.spec.ts's.
const setBusy = (page: Page, busy: number) => page.evaluate((b) => { window.__ckActionBusy = b; }, busy);
// fits reports whether the merged chip's cell shows it unclipped.
const fits = (page: Page) => page.locator(row).getByRole('img', { name: 'pull request merged' })
  .evaluate((el) => el.closest('td')!.scrollWidth <= el.closest('td')!.clientWidth);

// The fixture is single-shot (see serve.sh): the 404 test must leave the row in
// place for the dismiss test, and a retry would find it already dismissed.
test.describe.configure({ mode: 'serial', retries: 0 });

let unstubbed: string[] = [];

test.beforeEach(async ({ page }) => {
  unstubbed = await abortUnstubbedExec(page);
  // Without force=1 the cached queued answer stands for cwdTTL (60s); force is
  // throttled server-side, hence the poll.
  await expect.poll(async () => {
    await page.request.get('/api/state?force=1');
    await page.goto('/?refresh=1', { waitUntil: 'domcontentloaded' });
    return page.locator(`${row}.done`).count();
  }, { timeout: 30_000, intervals: [1_000, 3_000, 5_000] }).toBe(1);
  await setBusy(page, 1);
  await page.locator('#ck-mq > summary').click();
});

test.afterEach(async ({ page }) => {
  await page.unrouteAll({ behavior: 'ignoreErrors' });
  expect(unstubbed, 'exec endpoint requests with no stub').toEqual([]);
});

// let through sends this test's dismiss posts to the real handler, as body when given.
async function letThrough(page: Page, body?: string): Promise<string[]> {
  const posts: string[] = [];
  await page.route('**/api/action', (route) => {
    const sent = route.request().postData() ?? '';
    if (!sent.endsWith('&action=dismiss')) return route.fallback();
    posts.push(sent);
    return route.continue(body ? { postData: body } : undefined);
  });
  return posts;
}

// A left-queue row's dismiss arms first, as the render test pins (no data-noconfirm);
// the merged landing row stands in for one by losing the attribute. Stubbed, so the row survives.
test('a left-queue dismiss arms with its caveat before it posts', async ({ page }) => {
  const posts: string[] = [];
  await page.route('**/api/action', (route) => {
    posts.push(route.request().postData() ?? '');
    return route.fulfill({ json: { ok: true, exitCode: 0, timedOut: false, output: '' } });
  });
  await dismiss(page).evaluate((el) => el.removeAttribute('data-noconfirm'));
  await dismiss(page).click();
  await expect(dismiss(page)).toHaveText('✓ confirm');
  await expect(page.locator(`${row} .actmsg`)).toHaveText(/: drop this row from the merge queue — its PR left the queue unmerged/);
  expect(posts).toEqual([]);
  await dismiss(page).click();
  await expect.poll(() => posts.length).toBe(1);
  expect(posts[0]).toBe(`session_id=${landing}&action=dismiss`);
});

// A merged row with a live pane offers tear down, armed with its caveat. Stubbed, so the row survives.
test('a merged row\'s tear down arms with its caveat before it posts', async ({ page }) => {
  const posts: string[] = [];
  await page.route('**/api/action', (route) => {
    posts.push(route.request().postData() ?? '');
    return route.fulfill({ json: { ok: true, exitCode: 0, timedOut: false, output: 'started in tmux window' } });
  });
  const teardown = page.locator(row).getByRole('button', { name: /^tear down\b/ });
  await teardown.click();
  await expect(teardown).toHaveText('✓ confirm');
  await expect(page.locator(`${row} .actmsg`)).toHaveText(/: removes worktree/);
  expect(posts).toEqual([]);
  await teardown.click();
  await expect.poll(() => posts.length).toBe(1);
  expect(posts[0]).toBe(`session_id=${landing}&action=teardown`);
  await expect(page.locator(`${row} .actmsg`)).toHaveText('started in tmux window');
});

// Must run before the dismiss test below, which consumes the single-shot row.
test('a dismiss the server refuses reports the failure', async ({ page }) => {
  await letThrough(page, 'session_id=e2e-ghost&action=dismiss');
  await dismiss(page).click();
  await expect(page.locator(`${row} .actmsg`)).toHaveText('failed: HTTP 404');
});

test('a merged PR stays in the merge-queue table until dismissed', async ({ page }) => {
  const chip = page.locator(row).getByRole('img', { name: 'pull request merged' });
  await expect(chip).toBeVisible();
  expect(await fits(page)).toBe(true);
  await expect(page.locator(row).getByRole('link', { name: 'PR #42 (opens in a new tab)' }))
    .toHaveAttribute('href', 'https://github.com/e2e/landing/pull/42');
  await expect(page.locator(`table.roster #row-${landing}`)).toHaveCount(0);
  await expect(page.locator('#ck-mq > summary')).toContainText('1 finished');

  // On a phone the dismiss and tear down buttons are 44px touch targets inside their cell, and the page never scrolls sideways.
  await page.setViewportSize({ width: 390, height: 844 });
  expect((await dismiss(page).boundingBox())!.height).toBeGreaterThanOrEqual(44);
  const teardownBtn = page.locator(row).getByRole('button', { name: /^tear down\b/ });
  expect((await teardownBtn.boundingBox())!.height).toBeGreaterThanOrEqual(44);
  // inCell checks each button sits within the actions cell and the page has no sideways scroll.
  const inCell = async (...buttons: Locator[]) => {
    const cell = (await page.locator(`${row} td.actions`).boundingBox())!;
    for (const b of buttons) {
      const box = (await b.boundingBox())!;
      expect(box.x).toBeGreaterThanOrEqual(cell.x);
      expect(box.x + box.width).toBeLessThanOrEqual(cell.x + cell.width);
    }
    expect(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth)).toBe(true);
  };
  await inCell(dismiss(page), teardownBtn);
  expect(await fits(page)).toBe(true);
  // Armed, the cell also holds the ✕ cancel; the busy flag keeps a repaint from disarming it.
  const cancel = page.locator(`${row} button.actcancel`);
  await teardownBtn.click();
  await expect(teardownBtn).toHaveText('✓ confirm');
  await inCell(dismiss(page), teardownBtn, cancel);
  await cancel.click();
  await expect(teardownBtn).toHaveText('tear down');

  const posts = await letThrough(page);
  const response = page.waitForResponse('**/api/action');
  await dismiss(page).click();
  expect(await (await response).json()).toMatchObject({ ok: true });
  expect(posts).toEqual([`session_id=${landing}&action=dismiss`]);
  await expect(page.locator(`${row} .actmsg`)).toHaveText('dismissed — row will clear');

  // The busy flag held repaints off; released, the next poll drops the row and its tally.
  await setBusy(page, 0);
  await expect(page.locator(row)).toHaveCount(0);
  // The two queued fixtures keep the section up; only its tally goes.
  await expect(page.locator('#ck-mq > summary')).toContainText('Merge queue (2)');
  await expect(page.locator('#ck-mq > summary')).not.toContainText('finished');

  await page.goto('/?refresh=1', { waitUntil: 'domcontentloaded' });
  await expect(page.locator(row)).toHaveCount(0);
});
