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
  const t0 = new Date('2026-01-01T00:00:00Z').getTime();
  await page.clock.install({ time: t0 });
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
  await page.locator('#content').waitFor();
  // Frozen from here, so only runFor moves time. The jump may fire a tick, hence a baseline,
  // not an absolute count: every 5s window of runFor holds exactly one 5s tick.
  await page.clock.pauseAt(t0 + 60 * 60_000);
  const base = await polls(page);

  await page.clock.runFor(5_000);
  expect(await polls(page)).toBe(base + 1); // the interval runs while visible

  await setHidden(page, true);
  // runFor, not fastForward: it fires every tick in the window, so each hidden one is tested.
  await page.clock.runFor(20_000);
  expect(await polls(page)).toBe(base + 1);

  await setHidden(page, false);
  expect(await polls(page)).toBe(base + 2);
});
