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

test('the native search clear empties the filter', async ({ page }) => {
  const { box, count, rows, shown } = await filterFor(page, 'fix-123');
  await expect(shown).toHaveCount(3);
  // appearance:none must not have removed the cancel button just inside the 9px right padding.
  const b = (await box.boundingBox())!;
  await page.mouse.click(b.x + b.width - 16, b.y + b.height / 2);
  await expect(box).toHaveValue('');
  await expect(shown).toHaveCount(await rows.count());
  await expect(count).toHaveText('');
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
  await page.addInitScript(() => {
    window.__swaps = 0;
    document.addEventListener('ch:contentswap', () => window.__swaps++);
  });
  await page.goto('/?refresh=1#q=fix-123');
  const { box, count, rows, shown } = ui(page);
  await expect(box).toHaveValue('fix-123');
  await expect(shown).toHaveCount(3);

  const old = await rows.first().elementHandle();
  await expect.poll(() => page.evaluate(() => window.__swaps)).toBeGreaterThan(0);
  expect(await old!.evaluate((n) => n.isConnected)).toBe(false);
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
  test.skip(page.viewportSize()!.width > 400, 'phone width only');
  await page.goto('/?refresh=0');
  const b = (await ui(page).box.boundingBox())!;
  expect(b.x).toBeGreaterThanOrEqual(0);
  expect(b.x + b.width).toBeLessThanOrEqual(390);
  expect(await page.evaluate(() => document.documentElement.scrollWidth)).toBeLessThanOrEqual(390);
});
