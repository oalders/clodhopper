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

    // A long app name in the card footer must not widen the CI track and squeeze the branch.
    test('card branch sits beside CI and status clears idle on a phone', async ({ page }) => {
      test.skip(page.viewportSize()!.width > 640, 'the roster is only a card grid on a phone');
      const cards = page.locator('table.roster tbody tr:not(.panerow):not(.actrow)');
      expect(await cards.count()).toBeGreaterThan(0);
      const bad = await cards.evaluateAll((rows) =>
        rows.flatMap((row) => {
          // The contents' extent, not the cell's: a grid cell stretches to fill its track.
          const box = (sel: string) => {
            const el = row.querySelector(sel);
            if (!el) return undefined;
            const r = document.createRange();
            r.selectNodeContents(el);
            return r.getBoundingClientRect();
          };
          const [ci, branch, status, idle] = ['td.ci', 'td.branch', 'td.status', 'td.idle:not(.status)'].map(box);
          const name = row.querySelector('td.branch')?.textContent?.trim();
          const out: string[] = [];
          if (ci && branch && branch.left - ci.right > 12) out.push(`"${name}": branch starts ${branch.left - ci.right}px after CI`);
          if (status && idle && status.right > idle.left) out.push(`"${name}": status overlaps idle by ${status.right - idle.left}px`);
          return out;
        }),
      );
      expect(bad).toEqual([]);
    });
  });
}
