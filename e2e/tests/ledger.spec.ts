import type { Page } from '@playwright/test';
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

  // On a phone the dismiss button is a 44px touch target and the page never scrolls sideways.
  await page.setViewportSize({ width: 390, height: 844 });
  expect((await dismiss(page).boundingBox())!.height).toBeGreaterThanOrEqual(44);
  expect(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth)).toBe(true);
  expect(await fits(page)).toBe(true);

  const posts = await letThrough(page);
  const response = page.waitForResponse('**/api/action');
  await dismiss(page).click();
  expect(await (await response).json()).toMatchObject({ ok: true });
  expect(posts).toEqual([`session_id=${landing}&action=dismiss`]);
  await expect(page.locator(`${row} .actmsg`)).toHaveText('dismissed — row will clear');

  // The busy flag held repaints off; released, the next poll drops the row and its tally.
  await setBusy(page, 0);
  await expect(page.locator(row)).toHaveCount(0);
  // The queued fixture keeps the section up; only its tally goes.
  await expect(page.locator('#ck-mq > summary')).toContainText('Merge queue (1)');
  await expect(page.locator('#ck-mq > summary')).not.toContainText('finished');

  await page.goto('/?refresh=1', { waitUntil: 'domcontentloaded' });
  await expect(page.locator(row)).toHaveCount(0);
});
