import type { Page } from '@playwright/test';
import { test, expect } from './fixtures';

// The type-to-filter box over the roster and merge queue, and the source_app /
// branch dropdowns that narrow them server-side (#216).

declare global {
  interface Window {
    __swaps: number;
  }
}

const box = '#ck-q';
const rosterRows = '#content table.roster > tbody > tr[data-group]';
const shown = (page: Page) => page.locator(`${rosterRows}:visible`);
const appB = 'a-second-project-with-a-long-source-app-name';

test('typing narrows the roster, fixes the dividers, and Esc clears it', async ({ page }) => {
  await page.goto('/?refresh=0');
  const total = await page.locator(rosterRows).count();
  await page.keyboard.press('/');
  await expect(page.locator(box)).toBeFocused();
  // fix-1234/5/6 are three single-row groups: only the first visible one has no divider.
  await page.keyboard.type('FIX-123');
  await expect(shown(page)).toHaveCount(3);
  await expect(shown(page).first()).not.toHaveClass(/group-start/);
  await expect(shown(page).nth(1)).toHaveClass(/group-start/);
  // The fourth match is the queued row's "blocked on fix-1235" chip, folded in the merge queue.
  await expect(page.locator('#ck-qcount')).toHaveText(/^4 of \d+$/);
  expect(new URL(page.url()).hash).toBe('#q=FIX-123');

  await page.keyboard.press('Escape');
  await expect(page.locator(box)).toHaveValue('');
  await expect(shown(page)).toHaveCount(total);
  await expect(page.locator('#ck-qcount')).toHaveText('');
  expect(new URL(page.url()).hash).toBe('');
});

test('a CI badge label matches, and no match shows the empty line', async ({ page }) => {
  await page.goto('/?refresh=0');
  // The ejected row shows only ⏏; its aria-label carries the words.
  await page.locator(box).fill('removed from merge queue');
  await expect(shown(page)).toHaveCount(1);
  await expect(shown(page).locator('.cibadge.fail')).toHaveText('⏏');

  await page.locator(box).fill('zzz <b>');
  await expect(page.locator('#ck-qnone')).toHaveText("no rows match 'zzz <b>'");
  await expect(page.locator('#content table.roster')).toBeHidden();
});

test('the folded merge queue shows its match count', async ({ page }) => {
  await page.goto('/?refresh=0');
  const summary = page.locator('#ck-mq > summary');
  await page.locator(box).fill('merge queue position');
  await expect(summary).toContainText('· 2 matches');
  await expect(page.locator('#ck-mq')).not.toHaveAttribute('open');
  await summary.click();
  await expect(summary).not.toContainText('matches');
});

test('#q= survives a reload and a poll repaint', async ({ page }) => {
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
  await expect(page.locator(box)).toHaveValue('fix-123');
  await expect(shown(page)).toHaveCount(3);

  const old = await page.locator(rosterRows).first().elementHandle();
  await expect.poll(() => page.evaluate(() => window.__swaps)).toBeGreaterThan(0);
  expect(await old!.evaluate((n) => n.isConnected)).toBe(false);
  await expect(shown(page)).toHaveCount(3);
  await page.unrouteAll({ behavior: 'ignoreErrors' });
});

test('the source_app and branch dropdowns narrow the roster', async ({ page }) => {
  await page.goto('/?refresh=0');
  await page.locator('#ck-filtersbtn').click();
  // event_type only filters Recent events, which only debug renders.
  await expect(page.locator('#event_type')).toHaveCount(0);

  await page.locator('#source_app').selectOption(appB);
  await page.waitForURL(/source_app=/);
  const apps = await page.locator(`${rosterRows} td[data-label="app"]`).allTextContents();
  expect(apps.length).toBeGreaterThan(0);
  expect(new Set(apps)).toEqual(new Set([appB]));

  // Options come from the live roster, so a seeded branch is on offer.
  await page.locator('#branch').selectOption('fix-1234');
  await page.waitForURL(/branch=fix-1234/);
  await expect(page.locator(rosterRows)).toHaveCount(1);
  await expect(page.locator('#branch')).toHaveValue('fix-1234');
});
