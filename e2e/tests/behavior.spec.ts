import type { Page } from '@playwright/test';
import { test, expect } from './fixtures';

const storage = (page: Page, key: string) =>
  page.evaluate((k) => localStorage.getItem(k), key);

test('theme toggle flips data-theme and persists across reload', async ({ page }) => {
  await page.goto('/?refresh=0');
  const root = page.locator('html');
  const toggle = page.locator('#ck-theme');
  // The page ships dark; the toggle flips to light and back.
  await expect(root).toHaveAttribute('data-theme', 'dark');

  await toggle.click();
  await expect(root).toHaveAttribute('data-theme', 'light');
  expect(await storage(page, 'ch-theme')).toBe('light');
  await page.reload();
  await expect(root).toHaveAttribute('data-theme', 'light');
  await expect(toggle).toHaveAttribute('aria-pressed', 'true');

  await toggle.click();
  await expect(root).toHaveAttribute('data-theme', 'dark');
  await page.reload();
  await expect(root).toHaveAttribute('data-theme', 'dark');
  await expect(toggle).toHaveAttribute('aria-pressed', 'false');
});

test.describe('filter strip', () => {
  test('is closed with no badge when no filter is set', async ({ page }) => {
    await page.goto('/?refresh=0');
    await expect(page.locator('#ck-filters')).toBeHidden();
    await expect(page.locator('#ck-filtersbtn')).toHaveAttribute('aria-expanded', 'false');
    await expect(page.locator('#ck-fbadge')).toBeHidden();
  });

  test('opens and counts active filters when the URL carries them', async ({ page }) => {
    await page.goto('/?refresh=0&source_app=clodhopper&branch=main');
    await expect(page.locator('#ck-filters')).toBeVisible();
    await expect(page.locator('#ck-filtersbtn')).toHaveAttribute('aria-expanded', 'true');
    await expect(page.locator('#ck-fbadge')).toHaveText('2');
  });

  test('remembers open/closed across reloads, over the URL default', async ({ page }) => {
    await page.goto('/?refresh=0');
    await page.locator('#ck-filtersbtn').click();
    await expect(page.locator('#ck-filters')).toBeVisible();
    expect(await storage(page, 'ch-filters-open')).toBe('open');
    await page.reload();
    await expect(page.locator('#ck-filters')).toBeVisible();

    await page.locator('#ck-filtersbtn').click();
    expect(await storage(page, 'ch-filters-open')).toBe('closed');
    // A filtered URL would open it by default; the saved choice wins.
    await page.goto('/?refresh=0&source_app=clodhopper');
    await expect(page.locator('#ck-filters')).toBeHidden();
    await expect(page.locator('#ck-fbadge')).toHaveText('1');
  });
});
