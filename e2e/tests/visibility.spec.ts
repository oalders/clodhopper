import type { Page } from '@playwright/test';
import { test, expect } from './fixtures';

declare global {
  interface Window {
    __polls: number;
  }
}

// Chromium cannot hide a headless tab on cue, so the test fakes the flags the poller reads.
const setHidden = (page: Page, hidden: boolean) =>
  page.evaluate((h) => {
    Object.defineProperty(document, 'hidden', { configurable: true, get: () => h });
    Object.defineProperty(document, 'visibilityState', { configurable: true, get: () => (h ? 'hidden' : 'visible') });
    document.dispatchEvent(new Event('visibilitychange'));
  }, hidden);

const polls = (page: Page) => page.evaluate(() => window.__polls);

// A hidden tab skips its polls, each of which can run gh (#244), and polls once the moment it is shown again.
test('a hidden tab stops polling and polls once on return', async ({ page }) => {
  await page.clock.install();
  // Counted in the page as fetch is called, so a stray poll cannot land after an assertion.
  await page.addInitScript(() => {
    window.__polls = 0;
    const orig = window.fetch;
    window.fetch = (input, init) => {
      if (String(input).startsWith('/api/state')) window.__polls++;
      return orig(input, init);
    };
  });
  await page.goto('/?refresh=5');

  await page.clock.runFor(5_000);
  expect(await polls(page)).toBe(1); // the interval runs while visible

  await setHidden(page, true);
  await page.clock.runFor(20_000);
  expect(await polls(page)).toBe(1);

  await setHidden(page, false);
  expect(await polls(page)).toBe(2);
});
