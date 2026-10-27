import type { Page } from '@playwright/test';
import { test, expect } from './fixtures';

// The type-to-filter box over the roster and merge queue, and the source_app /
// branch dropdowns that narrow them server-side (#216).

declare global {
  interface Window {
    __swaps: number;
  }
}

const rosterRows = '#content table.roster > tbody > tr[data-group]';
const appB = 'a-second-project-with-a-long-source-app-name';

const ui = (page: Page) => ({
  box: page.locator('#ck-q'),
  count: page.locator('#ck-qcount'),
  rows: page.locator(rosterRows),
  shown: page.locator(`${rosterRows}:visible`),
});

async function filterFor(page: Page, term: string, url = '/?refresh=0') {
  await page.goto(url);
  const u = ui(page);
  await u.box.fill(term);
  return u;
}

test('typing narrows the roster, fixes the dividers, and Esc clears it', async ({ page }) => {
  await page.goto('/?refresh=0');
  const { box, count, rows, shown } = ui(page);
  const total = await rows.count();
  await page.keyboard.press('/');
  await expect(box).toBeFocused();
  // fix-1234/5/6 are three single-row groups: only the first visible one has no divider.
  await page.keyboard.type('  FIX-123 ');
  await expect(shown).toHaveCount(3);
  await expect(shown.first()).not.toHaveClass(/group-start/);
  await expect(shown.nth(1)).toHaveClass(/group-start/);
  // The fourth match is the queued row's "blocked on fix-1235" chip, folded in the merge queue.
  await expect(count).toHaveText(/^4 of \d+$/);
  expect(new URL(page.url()).hash).toBe('#q=FIX-123');

  await page.keyboard.press('Escape');
  await expect(box).toHaveValue('');
  await expect(box).toBeFocused();
  await expect(shown).toHaveCount(total);
  await expect(count).toHaveText('');
  expect(new URL(page.url()).hash).toBe('');
  // Esc on the empty box steps out of it.
  await page.keyboard.press('Escape');
  await expect(box).not.toBeFocused();
});

test('whitespace alone is no filter', async ({ page }) => {
  const { count, rows, shown } = await filterFor(page, '   ');
  await expect(shown).toHaveCount(await rows.count());
  await expect(count).toHaveText('');
  expect(new URL(page.url()).hash).toBe('');
});

test('an empty roster shows neither a count nor the no-match line', async ({ page }) => {
  await page.goto('/?refresh=0&source_app=no-such-app#q=x');
  await expect(page.locator('#ck-q')).toHaveValue('x');
  await expect(page.locator('#ck-qcount')).toHaveText('');
  await expect(page.locator('#ck-qnone')).toBeHidden();
});

test('hiding the first row of a group moves its divider; clearing restores it', async ({ page }) => {
  // Rows 2-7 are one clodhopper/main group led by the approval row (2), which "waiting" misses.
  const { box, rows } = await filterFor(page, 'waiting');
  await expect(rows.nth(1)).toBeVisible();
  await expect(rows.nth(2)).toBeHidden();
  await expect(rows.nth(3)).toHaveClass(/group-start/);

  await box.fill('');
  await expect(rows.nth(2)).toHaveClass(/group-start/);
  await expect(rows.nth(3)).not.toHaveClass(/group-start/);
});

test('a CI badge label matches, and no match shows the empty line', async ({ page }) => {
  // The ejected row shows only ⏏; its aria-label carries the words.
  const { box, shown } = await filterFor(page, 'removed from merge queue');
  await expect(shown).toHaveCount(1);
  await expect(shown.locator('.cibadge.fail')).toHaveText('⏏');

  await box.fill('zzz <b>');
  await expect(page.locator('#ck-qnone')).toHaveText("no rows match 'zzz <b>'");
  await expect(page.locator('#content table.roster')).toBeHidden();
});

test('the folded merge queue shows its match count', async ({ page }) => {
  const { box } = await filterFor(page, 'merge queue position');
  const summary = page.locator('#ck-mq > summary');
  await expect(summary).toContainText('· 2 matches');
  await box.fill('merge queue position 2');
  await expect(summary).toContainText('· 1 match');
  await expect(summary).not.toContainText('matches');
  await expect(page.locator('#ck-mq')).not.toHaveAttribute('open');
  await summary.click();
  await expect(summary).not.toContainText('match');
  const queued = page.locator('#ck-mq table.mq > tbody > tr[id^="row-"]');
  await expect(queued).toHaveCount(2);
  await expect(queued.filter({ visible: true })).toHaveCount(1);
});

test('/ only focuses the box from outside a text field', async ({ page }) => {
  await page.goto('/?refresh=0&debug=1');
  const { box } = ui(page);
  await box.click();
  await page.keyboard.type('a/b');
  await expect(box).toHaveValue('a/b');

  await page.locator('#ck-filtersbtn').click();
  const et = page.locator('#event_type');
  await et.click();
  await page.keyboard.type('x/');
  await expect(et).toBeFocused();
  await expect(et).toHaveValue('x/');
});

test('#q= round-trips odd characters through a reload', async ({ page }) => {
  const { box } = await filterFor(page, 'a b&c/d');
  expect(new URL(page.url()).hash).toBe('#q=a%20b%26c%2Fd');
  await page.reload();
  await expect(box).toHaveValue('a b&c/d');
});

test('#q= survives a poll repaint', async ({ page }) => {
  let polls = 0;
  // A fresh signature per poll forces every one-second poll to repaint.
  await page.route('**/api/state?*', async (route) => {
    const response = await route.fetch();
    const json = await response.json();
    return route.fulfill({ response, json: { ...json, signature: `filter-${++polls}` } });
  });
  await page.goto('/?refresh=1#q=fix-123');
  const { box, count, rows, shown } = ui(page);
  await expect(box).toHaveValue('fix-123');
  await expect(shown).toHaveCount(3);

  // Stamp the live board; the stamp is gone once a later repaint replaces it.
  await rows.first().evaluate((n) => n.setAttribute('data-old', '1'));
  await expect(page.locator('#content [data-old]')).toHaveCount(0);
  await expect(shown).toHaveCount(3);
  await expect(shown.first()).not.toHaveClass(/group-start/);
  await expect(shown.nth(1)).toHaveClass(/group-start/);
  await expect(shown.nth(2)).toHaveClass(/group-start/);
  await expect(count).toHaveText(/^4 of \d+$/);
  await page.unrouteAll({ behavior: 'ignoreErrors' });
});

test('the source_app and branch dropdowns narrow the roster and keep #q=', async ({ page }) => {
  await page.goto('/?refresh=0');
  const { box, rows, shown } = ui(page);
  await page.locator('#ck-filtersbtn').click();
  // event_type only filters Recent events, which only debug renders.
  await expect(page.locator('#event_type')).toHaveCount(0);

  await page.locator('#source_app').selectOption(appB);
  await page.waitForURL(/source_app=/);
  const apps = await page.locator(`${rosterRows} td[data-label="app"]`).allTextContents();
  expect(apps.length).toBeGreaterThan(0);
  expect(new Set(apps)).toEqual(new Set([appB]));

  await box.fill('fix-123');
  await expect(shown).toHaveCount(3);
  // Options come from the live roster, so a seeded branch is on offer.
  await page.locator('#branch').selectOption('fix-1234');
  await page.waitForURL(/branch=fix-1234/);
  expect(new URL(page.url()).hash).toBe('#q=fix-123');
  await expect(box).toHaveValue('fix-123');
  await expect(rows).toHaveCount(1);
  await expect(shown).toHaveCount(1);
  await expect(page.locator('#branch')).toHaveValue('fix-1234');
});

test('the box shares a phone-width row without overflowing', async ({ page }) => {
  const width = page.viewportSize()!.width;
  test.skip(width > 400, 'phone width only');
  const { box } = await filterFor(page, 'x'.repeat(60));
  const b = (await box.boundingBox())!;
  expect(b.x).toBeGreaterThanOrEqual(0);
  expect(b.x + b.width).toBeLessThanOrEqual(width);
  // The unbroken term is echoed in the no-match line, which must wrap.
  await expect(page.locator('#ck-qnone')).toBeVisible();
  expect(await page.evaluate(() => document.documentElement.scrollWidth)).toBeLessThanOrEqual(width);
});

test('pin order and dividers hold under a filter across a repaint', async ({ page }) => {
  test.skip(page.viewportSize()!.width <= 640, 'the sort headers are table-only');
  let polls = 0;
  // A fresh signature per poll forces every one-second poll to repaint in server order.
  await page.route('**/api/state?*', async (route) => {
    const response = await route.fetch();
    const json = await response.json();
    return route.fulfill({ response, json: { ...json, signature: `pin-${++polls}` } });
  });
  await page.addInitScript(() => {
    window.__swaps = 0;
    document.addEventListener('ch:contentswap', () => window.__swaps++);
  });
  await page.goto('/?refresh=1');
  const { box, shown } = ui(page);
  const app = (row: ReturnType<Page['locator']>) => row.locator('td[data-label="app"]');
  // Server order leads with clodhopper; pinned app-ascending leads with appB.
  await expect(app(shown.first())).toHaveText('clodhopper');
  await page.locator('#ck-pin').click();
  await page.locator('#content th[data-sort="app"] button.sortbtn').click();
  await box.fill('waiting');
  await expect(app(shown.first())).toHaveText(appB);
  const ids = () => shown.evaluateAll((rows) => rows.map((r) => r.id));
  const frozen = await ids();

  const before = await page.evaluate(() => window.__swaps);
  await expect.poll(() => page.evaluate(() => window.__swaps)).toBeGreaterThan(before);
  expect(await ids()).toEqual(frozen);
  // Each visible row starts a divider exactly when its group differs from the visible row above.
  const wrong = await shown.evaluateAll((rows) =>
    rows.filter((r, i) => r.classList.contains('group-start') !== (i > 0 && r.dataset.group !== rows[i - 1].dataset.group)).map((r) => r.id),
  );
  expect(wrong).toEqual([]);
  await page.unrouteAll({ behavior: 'ignoreErrors' });
});

test('submitting the filters under debug keeps debug and event_type', async ({ page }) => {
  await page.goto('/?refresh=0&debug=1');
  // Drop the flag the page just stored, so only the form itself can carry debug=1.
  await page.evaluate(() => localStorage.removeItem('ch-debug'));
  await page.locator('#ck-filtersbtn').click();
  await page.locator('#event_type').fill('Stop');
  await page.locator('#event_type').press('Enter');
  await page.waitForURL(/event_type=Stop/);
  expect(new URL(page.url()).searchParams.get('debug')).toBe('1');
  await expect(page.locator('#event_type')).toHaveValue('Stop');
});
