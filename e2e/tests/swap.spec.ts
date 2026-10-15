import type { Page } from '@playwright/test';
import { test, expect, abortUnstubbedExec } from './fixtures';

// What swapContent carries across a poll's #content repaint. Runs against
// serve.sh's exec server (the "exec desktop 1440" project), whose stub tmux
// gives the waiting row a peek. Every /api/state answer gets a fresh signature,
// so each one-second poll repaints; the page counts repaints off the
// ch:contentswap event swapContent fires, and since swapContent is synchronous
// a repaint is finished by the time the count moves.

declare global {
  interface Window {
    __swaps: number;
    __ckActionBusy: number;
  }
}

const sess = 'e2e-waiting-0000-1111-2222-333333333333';
const peekBtn = `#content button.peek[data-peek="${sess}"]`;
const peekRow = `#content tr.panerow[data-peek-row="${sess}"]`;
const capture = Array.from({ length: 200 }, (_, i) => `line ${i}`).join('\n');
// Mid-capture: neither the top nor the bottom openPane() lands on.
const scrolled = 500;

let unstubbed: string[] = [];
let polls = 0;

test.beforeEach(async ({ page }) => {
  unstubbed = await abortUnstubbedExec(page);
  polls = 0;
  await page.route('**/api/pane?*', (route) => route.fulfill({ contentType: 'text/plain', body: capture }));
  await page.route('**/api/state?*', async (route) => {
    const response = await route.fetch();
    const json = await response.json();
    return route.fulfill({ response, json: { ...json, signature: `swap-${++polls}` } });
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

const swaps = (page: Page) => page.evaluate(() => window.__swaps);

// repaint waits for the next swap and checks it really replaced el.
async function repaint(page: Page, el: string) {
  const old = await page.locator(el).elementHandle();
  const before = await swaps(page);
  await expect.poll(() => swaps(page)).toBeGreaterThan(before);
  expect(await old!.evaluate((n) => n.isConnected)).toBe(false);
}

// openPeek opens the peek, scrolls its capture mid-way and focuses it. The busy
// flag holds polls off meanwhile: a repaint before the fetch lands would carry
// the "loading…" placeholder over instead of the capture.
async function openPeek(page: Page) {
  await page.evaluate(() => { window.__ckActionBusy = 1; });
  await page.locator(peekBtn).click();
  const cap = page.locator(`${peekRow} .panecap`);
  await expect(cap).toContainText('line 199');
  await cap.evaluate((el, top) => { el.scrollTop = top; }, scrolled);
  await cap.focus();
  await page.evaluate(() => { window.__ckActionBusy = 0; });
  return cap;
}

for (const pin of [false, true]) {
  test(`an open peek keeps its capture, scroll and focus across a repaint (pin order ${pin ? 'on' : 'off'})`, async ({ page }) => {
    if (pin) {
      // Pinning re-appends every row after the swap, which resets a capsule's scroll (#99).
      await page.locator('#ck-pin').click();
      await expect(page.locator('#ck-pin')).toHaveAttribute('aria-pressed', 'true');
    }
    const cap = await openPeek(page);
    await repaint(page, `${peekRow} .panecap`);

    await expect(page.locator(peekRow)).toBeVisible();
    await expect(page.locator(peekBtn)).toHaveAttribute('aria-expanded', 'true');
    expect(await cap.textContent()).toBe(capture);
    expect(await cap.evaluate((el) => el.scrollTop)).toBe(scrolled);
    await expect(cap).toBeFocused();
  });
}

test('an open actions panel stays open across a repaint', async ({ page }) => {
  const btn = page.locator(`#content button.actbtn[data-actions="${sess}"]`);
  await btn.click();
  await expect(btn).toHaveAttribute('aria-expanded', 'true');
  await repaint(page, `#content button.actbtn[data-actions="${sess}"]`);

  await expect(page.locator(`#content tr.actrow[data-actions-row="${sess}"]`)).toBeVisible();
  await expect(btn).toHaveAttribute('aria-expanded', 'true');
});

test('a focused copy button is focused again after a repaint', async ({ page }) => {
  const copy = `#content .copycwd[data-session="${sess}"]`;
  await page.locator(copy).focus();
  await repaint(page, copy);
  await expect(page.locator(copy)).toBeFocused();
});

test('a busy action holds repaints off until it settles', async ({ page }) => {
  await page.evaluate(() => { window.__ckActionBusy = 1; });
  const before = await swaps(page);
  const seen = polls;
  const roster = await page.locator('#content table.roster').elementHandle();
  // Each of these answers carries a changed signature.
  await expect.poll(() => polls).toBeGreaterThan(seen + 2);
  expect(await swaps(page)).toBe(before);
  expect(await roster!.evaluate((n) => n.isConnected)).toBe(true);

  await page.evaluate(() => { window.__ckActionBusy = 0; });
  await repaint(page, '#content table.roster');
});
