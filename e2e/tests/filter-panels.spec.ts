import type { Page } from '@playwright/test';
import { test, expect, abortUnstubbedExec } from './fixtures';

// The type-to-filter box against the exec server's peek and actions panels:
// a filter hides an open panel with its row and leaves it open underneath.

const sess = 'e2e-waiting-0000-1111-2222-333333333333';
const peekRow = (p: Page) => p.locator(`#content tr.panerow[data-peek-row="${sess}"]`);
const actRow = (p: Page) => p.locator(`#content tr.actrow[data-actions-row="${sess}"]`);

let unstubbed: string[] = [];

test.beforeEach(async ({ page }) => {
  unstubbed = await abortUnstubbedExec(page);
  await page.route('**/api/pane?*', (route) => route.fulfill({ contentType: 'text/plain', body: 'pane text' }));
  await page.goto('/?refresh=0');
});

test.afterEach(() => {
  expect(unstubbed, 'exec endpoint requests with no stub').toEqual([]);
});

test('an open panel hides with its row and comes back open', async ({ page }) => {
  const box = page.locator('#ck-q');
  const panels: [string, () => ReturnType<typeof peekRow>][] = [
    [`#content button.peek[data-peek="${sess}"]`, () => peekRow(page)],
    [`#content button.actbtn[data-actions="${sess}"]`, () => actRow(page)],
  ];
  for (const [btn, row] of panels) {
    await page.locator(btn).click();
    await expect(row()).toBeVisible();
    await box.fill('fix-1234');
    await expect(row()).toBeHidden();
    await box.fill('');
    await expect(row()).toBeVisible();
  }
});

test('Esc in a filled box clears it and leaves the peek open', async ({ page }) => {
  await page.locator(`#content button.peek[data-peek="${sess}"]`).click();
  await expect(peekRow(page)).toContainText('pane text');
  const box = page.locator('#ck-q');
  await box.fill('main');
  await box.press('Escape');
  await expect(box).toHaveValue('');
  await expect(peekRow(page)).toBeVisible();
  // Esc on the empty box falls through to the page, which closes the peek.
  await box.press('Escape');
  await expect(peekRow(page)).toBeHidden();
});
