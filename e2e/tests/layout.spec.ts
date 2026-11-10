import { test, expect, fontsReady, namedSess, claudeName } from './fixtures';

// Layout must survive the worst-case fixtures (e2e_seed_test.go) at every
// project viewport. Only these targets are checked: branch, cwd and other free
// text are allowed to truncate.
// No absolute widths are asserted, so a deliberate column change doesn't break
// the suite.
const mustFit = 'table.roster td.status, table.roster .ck-badge, table.roster td.idle, table.roster td.ci, table.events td.time';

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
          const [ci, branch, status, idle] = ['td.ci', 'td.branch', 'td.status', 'td[data-label="idle"]'].map(box);
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

// In a table wider than the window (so its wrapper scrolls), a long session
// name must fit its column, not clip at a fixed 20ch reserve (#240).
test('a long roster session name is not clipped', async ({ page }, testInfo) => {
  test.skip(testInfo.project.name !== 'desktop 1440', 'the viewport is set below, so one project suffices');
  await page.setViewportSize({ width: 900, height: 900 });
  await page.goto('/?refresh=0');
  await fontsReady(page);
  const cell = page.locator('#row-e2e-approval-0000-1111-2222-333333333333 td.tmux');
  await expect(cell).toHaveText('overlong-tmux-session-name-that-scrolls-into-view');
  // th,td clip with overflow:hidden, so a cut-off name has scrollWidth > clientWidth.
  const { sw, cw } = await cell.evaluate((el) => ({ sw: el.scrollWidth, cw: el.clientWidth }));
  expect(sw).toBeLessThanOrEqual(cw);
});

// The session name is only copied, so the roster shows a tag icon, not the name; a phone, with nowhere to paste, shows neither (#242).
test('a session name is an icon on the desktop and absent on a phone', async ({ page }, testInfo) => {
  await page.goto('/?refresh=0');
  const btn = page.locator(`#row-${namedSess} td.branch .copyname`);
  await expect(page.locator(`#row-${namedSess} td.branch`)).not.toContainText(claudeName, { useInnerText: true });
  if (testInfo.project.name === 'phone 390') {
    // Present but hidden: a missing fixture must not pass for a hidden icon.
    await expect(btn).toHaveCount(1);
    await expect(btn).toBeHidden();
    return;
  }
  await expect(btn.locator('svg.tagicon')).toBeVisible();
  await expect(btn).toHaveText('', { useInnerText: true });
});

// The tag sits inside the path button so it follows a wrapped branch's last word: not below it, and not pushed to the cell's right edge (#242).
for (const hasTouch of [false, true]) {
  test.describe(hasTouch ? 'on a touch screen' : 'with a mouse', () => {
    test.use({ hasTouch });
    test('the session-name tag follows a wrapped branch\'s last word', async ({ page }, testInfo) => {
      test.skip(testInfo.project.name !== 'narrowest table 641', 'the narrowest table is where a branch wraps');
      await page.goto('/?refresh=0');
      await fontsReady(page);
      // No seeded branch is long enough to wrap, so lengthen this one's text node in place; at 641px
      // its last line is too full for the tag, so the last word must move down with it.
      const lines = await page.locator(`#row-${namedSess} td.branch .copycwd`).evaluate((b) => {
        const text = b.firstChild;
        if (!(text instanceof Text) || !text.data.trim()) throw new Error('the path button does not start with the branch text');
        text.data = 'fix-4265 tighten the roster card layout';
        const r = document.createRange();
        r.selectNodeContents(text);
        return [...r.getClientRects()].map((x) => ({ top: x.top, bottom: x.bottom, right: x.right }));
      });
      expect(lines.length, 'the lengthened branch must wrap for this test to mean anything').toBeGreaterThan(1);
      const last = lines[lines.length - 1];
      const tag = (await page.locator(`#row-${namedSess} td.branch .copyname svg.tagicon`).boundingBox())!;
      const mid = tag.y + tag.height / 2;
      expect(mid).toBeGreaterThan(last.top);
      expect(mid).toBeLessThan(last.bottom);
      // Beside the last word: the gap is the tag's margin plus the hidden ⧉ slot, well under 3em.
      const em = await page.locator(`#row-${namedSess} td.branch .copyname`).evaluate((el) => parseFloat(getComputedStyle(el).fontSize));
      expect(tag.x).toBeGreaterThanOrEqual(last.right);
      expect(tag.x - last.right).toBeLessThan(3 * em);
      // ...and inside the cell, whose overflow:hidden would otherwise clip it unseen.
      const cell = (await page.locator(`#row-${namedSess} td.branch`).boundingBox())!;
      expect(tag.x + tag.width).toBeLessThanOrEqual(cell.x + cell.width);
    });
  });
}

test.describe('on a touch screen', () => {
  test.use({ hasTouch: true });
  // The coarse-pointer padding grows the hit area around the 1em box; it must not eat the tag (#242).
  test('the session-name tag keeps its size', async ({ page }, testInfo) => {
    test.skip(testInfo.project.name === 'phone 390', 'the phone card hides the name icon');
    await page.goto('/?refresh=0');
    expect(await page.evaluate(() => matchMedia('(pointer: coarse)').matches)).toBe(true);
    const btn = page.locator(`#row-${namedSess} .copyname`);
    await btn.scrollIntoViewIfNeeded();
    const tag = (await btn.locator('svg.tagicon').boundingBox())!;
    const em = await btn.evaluate((el) => parseFloat(getComputedStyle(el).fontSize));
    // .9em whole; border-box crushed it to 1px.
    expect(tag.width).toBeCloseTo(0.9 * em, 0);
    // The padding still grows the hit area around it...
    expect((await btn.boundingBox())!.width).toBeGreaterThan(tag.width + 8);
    // ...and the cell's overflow:hidden does not clip the tag itself.
    const cell = (await page.locator(`#row-${namedSess} td.branch`).boundingBox())!;
    expect(tag.x).toBeGreaterThanOrEqual(cell.x);
    expect(tag.x + tag.width).toBeLessThanOrEqual(cell.x + cell.width);
    // A touch in the left padding (3px, inside its 6px), beside the tag, still lands on the button.
    const hit = await page.evaluate(([x, y]) => !!document.elementFromPoint(x, y)?.closest('.copyname'), [tag.x - 3, tag.y + tag.height / 2]);
    expect(hit).toBe(true);
  });
});

// The session colour chip is fixed-size; beside a long name in the capped id cell it must not shrink to a sliver (#242).
test('the id cell keeps its colour chip whole beside a session name on a phone', async ({ page }, testInfo) => {
  test.skip(testInfo.project.name !== 'phone 390', 'phone width only');
  await page.goto('/?refresh=0&debug=1');
  await fontsReady(page);
  const chip = (await page.locator(`#row-${namedSess} td.sess .chip`).boundingBox())!;
  const plain = (await page.locator('table.roster tr[id^="row-"] td.sess .chip').last().boundingBox())!;
  expect(chip.width).toBeGreaterThanOrEqual(plain.width - 0.5);
});
