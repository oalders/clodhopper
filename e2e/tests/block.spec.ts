import type { Page, Request } from '@playwright/test';
import { test, expect, abortUnstubbedExec } from './fixtures';

// The blocked-on chip and the row panel's block picker (#210), against
// serve.sh's exec server; e2e_seed_test.go blocks fix-1234 on fix-1235.

const blocked = 'e2e-blocked-0000-1111-2222-333333333333';
const blocker = 'e2e-blocker-0000-1111-2222-333333333333';
const chip = `#row-${blocked} .blockchip`;

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
  const form = page.locator(`#content tr.actrow[data-actions-row="${blocker}"] .blockform`);
  await expect(form.locator(`option[value="${blocker}"]`)).toHaveCount(0);
  await form.getByRole('combobox', { name: 'block this row on' }).selectOption(blocked);
  await form.getByRole('button', { name: 'block fix-1235 on the selected row' }).click();
  await expect(form.locator('.actmsg')).toHaveText('blocked — row will update');
  expect(posts).toHaveLength(1);
  expect(posts[0].postData()).toBe(`session_id=${blocker}&action=block&blocker_session_id=${blocked}`);
});
