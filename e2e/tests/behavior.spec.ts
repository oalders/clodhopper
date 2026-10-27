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
  await expect(toggle).toHaveAttribute('aria-pressed', 'true');
  expect(await storage(page, 'ch-theme')).toBe('light');
  await page.reload();
  await expect(root).toHaveAttribute('data-theme', 'light');
  await expect(toggle).toHaveAttribute('aria-pressed', 'true');

  await toggle.click();
  await expect(root).toHaveAttribute('data-theme', 'dark');
  await expect(toggle).toHaveAttribute('aria-pressed', 'false');
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

  test('controls are 44px tap targets on a phone', async ({ page }) => {
    test.skip(page.viewportSize()!.width > 640, 'the tap-target rule is phone-only');
    await page.goto('/?refresh=0&source_app=clodhopper');
    const controls = page.locator('#ck-filters select, #ck-filters input, #ck-filters button');
    await expect(controls).toHaveCount(6);
    const short = await controls.evaluateAll((els) =>
      els.filter((el) => el.getBoundingClientRect().height < 44).map((el) => el.id || el.textContent?.trim()),
    );
    expect(short).toEqual([]);
  });
});

// refresh=0 sets no poll interval, so the only /api/state request is the button's.
test('refresh CI polls once with force=1 and repaints, even with auto-refresh off', async ({ page }) => {
  await page.goto('/?refresh=0&window=7');
  let release!: () => void;
  const held = new Promise<void>((r) => { release = r; });
  let forced: URL | undefined;
  await page.route('**/api/state?*', async (route) => {
    forced = new URL(route.request().url());
    await held;
    const html = (await page.locator('#content').innerHTML()) + '<p id="forced-sentinel">forced</p>';
    return route.fulfill({ json: { signature: 'forced-sig', html } });
  });
  const btn = page.locator('#ck-ciforce');
  await btn.click();
  // Disabled while in flight, so a second click can't stack another force.
  await expect(btn).toBeDisabled();
  await expect(btn).toHaveAttribute('aria-busy', 'true');
  release();

  await expect(page.locator('#content #forced-sentinel')).toHaveText('forced');
  await expect(btn).toBeEnabled();
  await expect(btn).not.toHaveAttribute('aria-busy');
  await expect(btn).toBeFocused();
  expect(forced?.pathname).toBe('/api/state');
  expect(Object.fromEntries(forced!.searchParams)).toEqual({ refresh: '0', window: '7', force: '1' });
  // force rides only on the request, never the page URL a reload would replay.
  expect(new URL(page.url()).searchParams.has('force')).toBe(false);
});

test('a long-hung background shell escalates its row, naming the command', async ({ page }) => {
  await page.goto('/?refresh=0');
  const status = page.locator('#row-e2e-shellstuck-0000-1111-2222-333333333333 td.status');
  await expect(status).toHaveClass(/\bneeds\b/);
  const badge = status.locator('.ck-badge');
  await expect(badge).toHaveText(/^1 shell · running \d+h · stuck\?$/);
  await expect(badge).toHaveAttribute('title', /gh pr checks 1 --watch/);
});
