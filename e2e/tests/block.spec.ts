import type { Page, Request } from '@playwright/test';
import { test, expect, abortUnstubbedExec } from './fixtures';

// The blocked-on chip and the row panel's block picker (#210), against
// serve.sh's exec server; e2e_seed_test.go blocks fix-1234 on fix-1235,
// fix-1236 on the queued row, and the queued row on fix-1235.

const blocked = 'e2e-blocked-0000-1111-2222-333333333333';
const blocker = 'e2e-blocker-0000-1111-2222-333333333333';
const queueBlocked = 'e2e-queueblocked-0000-1111-2222-333333333333';
const queued = 'e2e-queued-0000-1111-2222-333333333333';
const chip = `#row-${blocked} .blockchip`;
const panel = (sess: string) => `#content tr.actrow[data-actions-row="${sess}"] .blockform`;

let unstubbed: string[] = [];

test.beforeEach(async ({ page }) => {
  unstubbed = await abortUnstubbedExec(page);
  await page.goto('/');
});

test.afterEach(async ({ page }) => {
  await page.unrouteAll({ behavior: 'ignoreErrors' });
  expect(unstubbed, 'exec endpoint requests with no stub').toEqual([]);
});

async function stubAction(page: Page): Promise<Request[]> {
  const seen: Request[] = [];
  await page.route('**/api/action', (route) => {
    seen.push(route.request());
    return route.fulfill({ json: { ok: true, output: '' } });
  });
  return seen;
}

test('a blocked row is relaxed and its chip names the blocker', async ({ page }) => {
  await expect(page.locator(`#row-${blocked} td.status`)).toHaveClass(/\bbackground\b/);
  await expect(page.locator(`#row-${blocked} .ck-badge`)).toHaveText('blocked');
  await expect(page.locator(chip).getByRole('button', { name: 'blocked on fix-1235' })).toBeVisible();
});

test('the chip ✕ sends unblock for the blocked row', async ({ page }) => {
  const posts = await stubAction(page);
  await page.evaluate(() => { (window as unknown as { __ckActionBusy: number }).__ckActionBusy = 1; });
  await page.locator(chip).getByRole('button', { name: 'unblock fix-1234 from fix-1235' }).click();
  await expect(page.locator(`${chip} .actmsg`)).toHaveText('unblocked — row will update');
  expect(posts).toHaveLength(1);
  expect(posts[0].postData()).toBe(`session_id=${blocked}&action=unblock`);
});

test('the chip label jumps to the blocker row', async ({ page }) => {
  await page.locator(chip).getByRole('button', { name: 'blocked on fix-1235' }).click();
  const row = page.locator(`#row-${blocker}`);
  await expect(row).toBeFocused();
  await expect(row).toHaveClass(/\bjumped\b/);
});

test('the row panel blocks on the picked row', async ({ page }) => {
  const posts = await stubAction(page);
  await page.evaluate(() => { (window as unknown as { __ckActionBusy: number }).__ckActionBusy = 1; });
  await page.locator(`#content button.actbtn[data-actions="${blocker}"]`).click();
  const form = page.locator(panel(blocker));
  await expect(form.locator(`option[value="${blocker}"]`)).toHaveCount(0);
  await expect(form.locator(`option[value="${queued}"]`)).toHaveCount(1);
  await form.getByRole('combobox', { name: 'block this row on' }).selectOption(blocked);
  await form.getByRole('button', { name: 'block fix-1235 on the selected row' }).click();
  await expect(form.locator('.actmsg')).toHaveText('blocked — row will update');
  expect(posts).toHaveLength(1);
  expect(posts[0].postData()).toBe(`session_id=${blocker}&action=block&blocker_session_id=${blocked}`);
});

test('the chip label opens the merge-queue fold to reach a queued blocker', async ({ page }) => {
  // The fold remembers being opened; start from its collapsed default.
  await page.evaluate(() => localStorage.clear());
  await page.reload();
  const mq = page.locator('#ck-mq');
  await expect(mq).not.toHaveAttribute('open');
  await page.locator(`#row-${queueBlocked} .blockjump`).click();
  await expect(mq).toHaveAttribute('open');
  const row = page.locator(`#row-${queued}`);
  await expect(row).toBeFocused();
  await expect(row).toHaveClass(/\bjumped\b/);
});

test('the ✕ on a blocked row folded into the merge queue sends unblock', async ({ page }) => {
  const posts = await stubAction(page);
  await page.evaluate(() => { (window as unknown as { __ckActionBusy: number }).__ckActionBusy = 1; });
  await page.locator('#ck-mq > summary').click();
  const qchip = page.locator(`#row-${queued} .blockchip`);
  await expect(qchip.getByRole('button', { name: 'blocked on fix-1235' })).toBeVisible();
  await qchip.getByRole('button', { name: 'unblock main from fix-1235' }).click();
  await expect(qchip.locator('.actmsg')).toHaveText('unblocked — row will update');
  expect(posts).toHaveLength(1);
  expect(posts[0].postData()).toBe(`session_id=${queued}&action=unblock`);
});

test('a poll repaint keeps the picker choice and its focus', async ({ page }) => {
  let polls = 0;
  await page.route('**/api/state?*', async (route) => {
    const response = await route.fetch();
    const json = await response.json();
    return route.fulfill({ response, json: { ...json, signature: `block-${++polls}` } });
  });
  await page.goto('/?refresh=1');
  await page.locator(`#content button.actbtn[data-actions="${blocker}"]`).click();
  const pick = page.locator(`${panel(blocker)} .blockpick`);
  const first = await pick.inputValue();
  // The untouched picker sits on its first option, which is not the one chosen below.
  expect(first).toBe(await pick.locator('option').first().getAttribute('value'));
  expect(first).not.toBe(queued);
  await pick.selectOption(queued);
  await pick.focus();
  // Tag the live node so the assertions below can only pass on its repainted replacement.
  await pick.evaluate((el) => el.setAttribute('data-old', '1'));
  await expect(page.locator('.blockpick[data-old]')).toHaveCount(0);
  await expect(pick).toHaveValue(queued);
  await expect(pick).toBeFocused();
});

test('the chip label in the merge-queue fold jumps back to the roster blocker', async ({ page }) => {
  await page.locator('#ck-mq > summary').click();
  await page.locator(`#row-${queued} .blockchip`).getByRole('button', { name: 'blocked on fix-1235' }).click();
  const row = page.locator(`#row-${blocker}`);
  await expect(row).toBeFocused();
  await expect(row).toHaveClass(/\bjumped\b/);
});

test('a picker whose chosen blocker left the board falls back to an offered row', async ({ page }) => {
  await page.goto('/?refresh=1');
  await page.locator(`#content button.actbtn[data-actions="${blocker}"]`).click();
  const pick = page.locator(`${panel(blocker)} .blockpick`);
  await pick.selectOption(queued);
  const gone = new RegExp(`<option value="${queued}"[^>]*>[^<]*</option>`, 'g');
  let polls = 0;
  await page.route(/\/api\/state(\?|$)/, async (route) => {
    const response = await route.fetch();
    const json = await response.json();
    return route.fulfill({ response, json: { ...json, html: json.html.replace(gone, ''), signature: `gone-${++polls}` } });
  });
  await pick.evaluate((el) => el.setAttribute('data-old', '1'));
  await expect(page.locator('.blockpick[data-old]')).toHaveCount(0);
  await expect(pick.locator(`option[value="${queued}"]`)).toHaveCount(0);
  const value = await pick.inputValue();
  expect(value).not.toBe('');
  expect(value).not.toBe(queued);
});
