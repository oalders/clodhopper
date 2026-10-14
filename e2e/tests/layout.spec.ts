import { test, expect, fontsReady } from './fixtures';

// Layout must survive the worst-case fixtures (e2e_seed_test.go) at every
// project viewport. Only these targets are checked: branch, cwd, session and
// other free text are allowed to truncate. No absolute widths are asserted, so
// a deliberate column change doesn't break the suite.
const mustFit = 'table.roster td.status, table.roster .ck-badge, table.roster td.idle, table.events td.time';

// The default board, and the debug view that adds the events table's time cells.
for (const path of ['/?refresh=0', '/?refresh=0&debug=1']) {
  test.describe(path, () => {
    test.beforeEach(async ({ page }) => {
      await page.goto(path);
      await fontsReady(page);
    });

    test('status badges and time cells are not clipped', async ({ page }) => {
      await expect(page.locator('.ck-badge', { hasText: /waiting on \d{2,} agents · running \d+h/ })).toHaveCount(1);
      const targets = page.locator(mustFit);
      expect(await targets.count()).toBeGreaterThan(0);
      const clipped = await targets.evaluateAll((els) =>
        els
          .filter((el) => el.scrollWidth > el.clientWidth)
          .map((el) => `${el.className} "${el.textContent?.trim()}": ${el.scrollWidth} > ${el.clientWidth}`),
      );
      expect(clipped).toEqual([]);
    });

    test('the page does not scroll sideways', async ({ page }) => {
      const { scrollWidth, innerWidth } = await page.evaluate(() => ({
        scrollWidth: document.documentElement.scrollWidth,
        innerWidth: window.innerWidth,
      }));
      expect(scrollWidth).toBeLessThanOrEqual(innerWidth);
    });
  });
}
