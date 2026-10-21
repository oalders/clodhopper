import { test, expect, abortUnstubbedExec } from './fixtures';

// A row stays in the merge-queue table once its PR merges, until dismissed
// (#211). serve.sh's stub gh answers the landing fixture queued on its first
// lookup (the server's readiness check) and merged on every later one. Its
// dismiss is the one /api/action let through to serve: it execs nothing.

const landing = 'e2e-landing-0000-1111-2222-333333333333';
const row = `#ck-mq #row-${landing}`;

let unstubbed: string[] = [];

test.beforeEach(async ({ page }) => {
  unstubbed = await abortUnstubbedExec(page);
});

test.afterEach(async ({ page }) => {
  await page.unrouteAll({ behavior: 'ignoreErrors' });
  expect(unstubbed, 'exec endpoint requests with no stub').toEqual([]);
});

test('a merged PR stays in the merge-queue table until dismissed', async ({ page }) => {
  // force=1 expires the CI cache (throttled server-side), so the merged answer lands.
  await expect.poll(async () => {
    await page.request.get('/api/state?force=1');
    await page.goto('/?refresh=1', { waitUntil: 'domcontentloaded' });
    return page.locator(`${row}.done`).count();
  }, { timeout: 30_000, intervals: [1_000, 3_000, 5_000] }).toBe(1);

  await page.evaluate(() => { (window as unknown as { __ckActionBusy: number }).__ckActionBusy = 1; });
  await page.locator('#ck-mq > summary').click();
  const chip = page.locator(row).getByRole('img', { name: 'pull request merged' });
  await expect(chip).toBeVisible();
  expect(await chip.evaluate((el) => el.closest('td')!.scrollWidth <= el.closest('td')!.clientWidth)).toBe(true);
  await expect(page.locator(row).getByRole('link', { name: 'PR #42 (opens in a new tab)' }))
    .toHaveAttribute('href', 'https://github.com/e2e/landing/pull/42');
  await expect(page.locator(`table.roster #row-${landing}`)).toHaveCount(0);

  // Dismiss execs nothing, so it alone goes through to the real handler.
  const posts: string[] = [];
  await page.route('**/api/action', (route) => {
    const body = route.request().postData() ?? '';
    if (!body.endsWith('&action=dismiss')) return route.fallback();
    posts.push(body);
    return route.continue();
  });
  const response = page.waitForResponse('**/api/action');
  await page.locator(row).getByRole('button', { name: /^dismiss / }).click();
  expect(await (await response).json()).toMatchObject({ ok: true });
  expect(posts).toEqual([`session_id=${landing}&action=dismiss`]);
  await expect(page.locator(`${row} .actmsg`)).toHaveText('dismissed — row will clear');

  await page.goto('/?refresh=1', { waitUntil: 'domcontentloaded' });
  await expect(page.locator(`#ck-mq #row-${landing}`)).toHaveCount(0);
});
