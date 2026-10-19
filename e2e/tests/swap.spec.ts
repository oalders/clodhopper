import type { Page } from '@playwright/test';
import { test, expect, abortUnstubbedExec } from './fixtures';

// What swapContent carries across a poll's #content repaint. Runs against
// serve.sh's exec server (the "exec desktop 1440" project), whose stub tmux
// gives the waiting row and its twin (same worktree, same pane %9001) a peek. Every /api/state answer gets a fresh signature,
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
const peekBtn = (s: string) => `#content button.peek[data-peek="${s}"]`;
const peekRow = (s: string) => `#content tr.panerow[data-peek-row="${s}"]`;
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

// later returns attr of the second of the two elements matching sel: the one a
// restore keyed on what they share (cwd or pane) would miss, landing on the first.
async function later(page: Page, sel: string, attr: string) {
  await expect(page.locator(sel)).toHaveCount(2);
  return (await page.locator(sel).nth(1).getAttribute(attr))!;
}

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
async function openPeek(page: Page, s: string) {
  await page.evaluate(() => { window.__ckActionBusy = 1; });
  await page.locator(peekBtn(s)).click();
  const cap = page.locator(`${peekRow(s)} .panecap`);
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
    const shared = '#content button.peek[data-pane="%9001"]';
    const s = await later(page, shared, 'data-peek');
    const other = (await page.locator(shared).first().getAttribute('data-peek'))!;
    const cap = await openPeek(page, s);
    await repaint(page, `${peekRow(s)} .panecap`);

    await expect(page.locator(peekRow(s))).toBeVisible();
    await expect(page.locator(peekBtn(s))).toHaveAttribute('aria-expanded', 'true');
    await expect(page.locator(peekRow(other))).toBeHidden();
    await expect(page.locator(peekBtn(other))).toHaveAttribute('aria-expanded', 'false');
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
  const s = await later(page, '#content .copycwd[data-cwd$="/worktree-waiting"]', 'data-session');
  const copy = `#content .copycwd[data-session="${s}"]`;
  await page.locator(copy).focus();
  await repaint(page, copy);
  await expect(page.locator(copy)).toBeFocused();
});

test('a busy action holds repaints off until it settles', async ({ page }) => {
  await page.evaluate(() => { window.__ckActionBusy = 1; });
  const before = await swaps(page);
  const seen = polls;
  const roster = await page.locator('#content table.roster').elementHandle();
  // Each of these answers carries a changed signature. Paced by the page's 1s poll interval, not the test.
  await expect.poll(() => polls, { timeout: 10_000 }).toBeGreaterThan(seen + 2);
  expect(await swaps(page)).toBe(before);
  expect(await roster!.evaluate((n) => n.isConnected)).toBe(true);

  await page.evaluate(() => { window.__ckActionBusy = 0; });
  await repaint(page, '#content table.roster');
});

test('the merge-queue section keeps its open or closed state across a repaint', async ({ page }) => {
  const mq = page.locator('#ck-mq');
  await mq.locator('summary').click();
  await expect(mq).toHaveAttribute('open');
  await repaint(page, '#ck-mq');
  await expect(mq).toHaveAttribute('open');

  // Closed is the fresh section's default, so this checks the close was stored, not just absent.
  await mq.locator('summary').click();
  await expect(mq).not.toHaveAttribute('open');
  await repaint(page, '#ck-mq');
  await expect(mq).not.toHaveAttribute('open');
});

test('a queued row keeps its open peek inside the merge-queue section across a repaint', async ({ page }) => {
  const q = 'e2e-queued-0000-1111-2222-333333333333';
  await page.locator('#ck-mq summary').click();
  const cap = await openPeek(page, q);
  await expect(page.locator(`#ck-mq tr.panerow[data-peek-row="${q}"]`)).toBeVisible();
  await repaint(page, `${peekRow(q)} .panecap`);

  await expect(page.locator(`#ck-mq tr.panerow[data-peek-row="${q}"]`)).toBeVisible();
  await expect(page.locator(peekBtn(q))).toHaveAttribute('aria-expanded', 'true');
  expect(await cap.textContent()).toBe(capture);
  expect(await cap.evaluate((el) => el.scrollTop)).toBe(scrolled);
  await expect(cap).toBeFocused();
});
