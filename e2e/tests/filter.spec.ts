import type { Page } from '@playwright/test';
import { test, expect, repaintEveryPoll, repaint } from './fixtures';

// The type-to-filter box over the roster and merge queue, and the source_app /
// branch dropdowns that narrow them server-side (#216).

const rosterRows = '#content table.roster > tbody > tr[data-group]';
const appB = 'a-second-project-with-a-long-source-app-name';
// The page's card-layout breakpoint (max-width: 640px).
const phone = (page: Page) => page.viewportSize()!.width <= 640;

const ui = (page: Page) => ({
  box: page.locator('#ck-q'),
  count: page.locator('#ck-qcount'),
  rows: page.locator(rosterRows),
  shown: page.locator(`${rosterRows}:visible`),
});

async function filterFor(page: Page, term: string, url = '/?refresh=0') {
  await page.goto(url);
  const u = ui(page);
  await u.box.fill(term);
  return u;
}

test('typing narrows the roster, fixes the dividers, and Esc clears it', async ({ page }) => {
  await page.goto('/?refresh=0');
  const { box, count, rows, shown } = ui(page);
  const total = await rows.count();
  await page.keyboard.press('/');
  await expect(box).toBeFocused();
  // fix-1234/5/6 are three single-row groups: only the first visible one has no divider.
  await page.keyboard.type('  FIX-123 ');
  await expect(shown).toHaveCount(3);
  await expect(shown.first()).not.toHaveClass(/group-start/);
  await expect(shown.nth(1)).toHaveClass(/group-start/);
  // The fourth match is the queued row's "blocked on fix-1235" chip, folded in the merge queue.
  await expect(count).toHaveText(/^4 of \d+$/);
  expect(new URL(page.url()).hash).toBe('#q=FIX-123');

  await page.keyboard.press('Escape');
  await expect(box).toHaveValue('');
  await expect(box).toBeFocused();
  await expect(shown).toHaveCount(total);
  await expect(count).toHaveText('');
  expect(new URL(page.url()).hash).toBe('');
  // Esc on the empty box keeps focus there (filter-panels.spec.ts: it still closes a peek).
  await page.keyboard.press('Escape');
  await expect(box).toBeFocused();
});

test('the clear button shows only with a term, clears it and refocuses the box', async ({ page }) => {
  await page.goto('/?refresh=0');
  const { box, shown, rows } = ui(page);
  const clear = page.locator('#ck-qclear');
  await expect(clear).toBeHidden();
  await box.fill('fix-123');
  await expect(clear).toBeVisible();
  await clear.click();
  await expect(box).toHaveValue('');
  await expect(box).toBeFocused();
  await expect(clear).toBeHidden();
  await expect(shown).toHaveCount(await rows.count());
  expect(new URL(page.url()).hash).toBe('');
  // Esc clears a typed term and hides the button too.
  await page.keyboard.type('fix');
  await expect(clear).toBeVisible();
  await page.keyboard.press('Escape');
  await expect(clear).toBeHidden();
});

test('whitespace alone is no filter', async ({ page }) => {
  const { count, rows, shown } = await filterFor(page, '   ');
  await expect(shown).toHaveCount(await rows.count());
  await expect(count).toHaveText('');
  expect(new URL(page.url()).hash).toBe('');
});

test('an empty roster shows neither a count nor the no-match line', async ({ page }) => {
  await page.goto('/?refresh=0&source_app=no-such-app#q=x');
  await expect(page.locator('#ck-q')).toHaveValue('x');
  await expect(page.locator('#ck-qcount')).toHaveText('');
  await expect(page.locator('#ck-qnone')).toBeHidden();
});

test('hiding the first row of a group moves its divider; clearing restores it', async ({ page }) => {
  // The approval row leads the clodhopper/main group, and "waiting" misses it.
  const { box } = await filterFor(page, 'waiting');
  const lead = page.locator('#row-e2e-approval-0000-1111-2222-333333333333');
  const group = (await lead.getAttribute('data-group'))!;
  const next = page.locator(`#row-e2e-approval-0000-1111-2222-333333333333 ~ tr[data-group="${group}"]`).first();
  await expect(lead).toBeHidden();
  await expect(next).toBeVisible();
  await expect(next).toHaveClass(/group-start/);

  await box.fill('');
  await expect(lead).toHaveClass(/group-start/);
  await expect(next).not.toHaveClass(/group-start/);
});

test('a term found only in a title attribute matches', async ({ page }) => {
  const { shown } = await filterFor(page, 'last completed');
  await expect(shown).not.toHaveCount(0);
  await expect(shown.filter({ hasNot: page.locator('em[title="last completed"]') })).toHaveCount(0);
});

test('the last row left showing draws no bottom border', async ({ page }) => {
  test.skip(phone(page), 'the card layout strips row borders');
  // The idle row is the roster's last; fix-123 hides it and leaves the fix-1235 row last.
  const { shown } = await filterFor(page, 'fix-123');
  await expect(page.locator('#row-e2e-idle-0000-1111-2222-333333333333')).toBeHidden();
  await expect(shown.last().locator('td').first()).toHaveCSS('border-bottom-width', '0px');
  await expect(shown.first().locator('td').first()).not.toHaveCSS('border-bottom-width', '0px');
});

test('a CI badge label matches, and no match shows the empty line', async ({ page }) => {
  // The ejected row shows only ⏏; its aria-label carries the words.
  const { box, shown } = await filterFor(page, 'removed from merge queue');
  await expect(shown).toHaveCount(1);
  await expect(shown.locator('.cibadge.fail')).toHaveText('⏏');

  await box.fill('zzz <b>');
  await expect(page.locator('#ck-qnone')).toHaveText("no rows match 'zzz <b>'");
  await expect(page.locator('#content table.roster')).toBeHidden();
  // The no-match line says it all, and the emptied card is just its header.
  await expect(page.locator('#ck-qcount')).toHaveText('');
  const head = page.locator('section.ck-card > .ck-cardhead', { hasText: 'Agents' });
  await expect(head).toHaveCSS('border-bottom-width', '0px');
});

test('the folded merge queue shows its match count', async ({ page }) => {
  const { box } = await filterFor(page, 'merge queue position');
  const summary = page.locator('#ck-mq > summary');
  await expect(summary).toContainText('· 2 matches');
  await box.fill('merge queue position 2');
  await expect(summary).toContainText('· 1 match');
  await expect(summary).not.toContainText('matches');
  await expect(page.locator('#ck-mq')).not.toHaveAttribute('open');
  // The tag drops the h2's tight heading tracking.
  await expect(summary.locator('.ck-qmq')).toHaveCSS('letter-spacing', 'normal');
  await summary.click();
  await expect(summary).not.toContainText('match');
  const queued = page.locator('#ck-mq table.mq > tbody > tr[id^="row-"]');
  await expect(queued).toHaveCount(2);
  await expect(queued.filter({ visible: true })).toHaveCount(1);
  await summary.click();
  await expect(summary).toContainText('· 1 match');
});

test('the next-merge ETA stays ahead of the fold\'s match tag', async ({ page }) => {
  // Position 1 is the next row to merge.
  await filterFor(page, 'merge queue position 1');
  await expect(page.locator('#ck-mq > summary')).toHaveText(/next merge .* · 1 match$/);
});

test('an open fold keeps the filter across a repaint', async ({ page }) => {
  await repaintEveryPoll(page);
  await page.goto('/?refresh=1');
  const mq = page.locator('#ck-mq');
  await mq.locator('summary').click();
  await ui(page).box.fill('merge queue position 2');
  const queued = mq.locator('table.mq > tbody > tr[id^="row-"]');
  await expect(queued.filter({ visible: true })).toHaveCount(1);
  await repaint(page, '#ck-mq');
  await expect(mq).toHaveAttribute('open');
  await expect(queued.filter({ visible: true })).toHaveCount(1);
  await expect(queued.filter({ visible: true })).toContainText('merge queue position 2');
  await expect(mq.locator('summary')).not.toContainText('match');
});

test('/ only focuses the box from outside a text field', async ({ page }) => {
  await page.goto('/?refresh=0&debug=1');
  const { box } = ui(page);
  await box.click();
  await page.keyboard.type('a/b');
  await expect(box).toHaveValue('a/b');

  await page.locator('#ck-filtersbtn').click();
  const et = page.locator('#event_type');
  await et.click();
  await page.keyboard.type('x/');
  await expect(et).toBeFocused();
  await expect(et).toHaveValue('x/');
});

test('Ctrl+/ and a composing / leave the box alone', async ({ page }) => {
  await page.goto('/?refresh=0');
  const { box } = ui(page);
  const slash = (isComposing: boolean) =>
    page.evaluate((isComposing) => document.body.dispatchEvent(new KeyboardEvent('keydown', { key: '/', isComposing, bubbles: true })), isComposing);
  // The same synthetic keydown, not composing, is the shortcut, so the negatives below mean something.
  await slash(false);
  await expect(box).toBeFocused();
  await box.blur();
  await page.keyboard.press('Control+/');
  await expect(box).not.toBeFocused();
  // An IME's composition keystrokes are not the shortcut.
  await slash(true);
  await expect(box).not.toBeFocused();
});

test('#q= round-trips odd characters through a reload', async ({ page }) => {
  const { box } = await filterFor(page, 'a b&c/d');
  expect(new URL(page.url()).hash).toBe('#q=a%20b%26c%2Fd');
  await page.reload();
  await expect(box).toHaveValue('a b&c/d');
  await expect(page.locator('#ck-qclear')).toBeVisible();
});

test('#q= survives a poll repaint', async ({ page }) => {
  await repaintEveryPoll(page);
  await page.goto('/?refresh=1#q=fix-123');
  const { box, count, shown } = ui(page);
  await expect(box).toHaveValue('fix-123');
  await expect(shown).toHaveCount(3);

  await repaint(page, '#content table.roster');
  await expect(shown).toHaveCount(3);
  await expect(shown.first()).not.toHaveClass(/group-start/);
  await expect(shown.nth(1)).toHaveClass(/group-start/);
  await expect(shown.nth(2)).toHaveClass(/group-start/);
  await expect(count).toHaveText(/^4 of \d+$/);
  await page.unrouteAll({ behavior: 'ignoreErrors' });
});

test('the source_app and branch dropdowns narrow the roster and keep #q=', async ({ page }) => {
  await page.goto('/?refresh=0');
  const { box, rows, shown } = ui(page);
  await page.locator('#ck-filtersbtn').click();
  // event_type only filters Recent events, which only debug renders; nor is debug=1 carried.
  await expect(page.locator('#event_type')).toHaveCount(0);
  await expect(page.locator('#ck-filters input[name="debug"]')).toHaveCount(0);

  await page.locator('#source_app').selectOption(appB);
  await page.waitForURL(/source_app=/);
  const apps = await page.locator(`${rosterRows} td[data-label="app"]`).allTextContents();
  expect(apps.length).toBeGreaterThan(0);
  expect(new Set(apps)).toEqual(new Set([appB]));

  await box.fill('fix-123');
  await expect(shown).toHaveCount(3);
  // Options come from the live roster, so a seeded branch is on offer.
  await page.locator('#branch').selectOption('fix-1234');
  await page.waitForURL(/branch=fix-1234/);
  expect(new URL(page.url()).hash).toBe('#q=fix-123');
  await expect(box).toHaveValue('fix-123');
  await expect(rows).toHaveCount(1);
  await expect(shown).toHaveCount(1);
  await expect(page.locator('#branch')).toHaveValue('fix-1234');
});

test('the box shares its row without overflowing', async ({ page }) => {
  const width = page.viewportSize()!.width;
  const { box } = await filterFor(page, 'x'.repeat(60));
  for (const el of [box, page.locator('#ck-qclear')]) {
    const b = (await el.boundingBox())!;
    expect(b.x).toBeGreaterThanOrEqual(0);
    expect(b.x + b.width).toBeLessThanOrEqual(width);
  }
  // The unbroken term is echoed in the no-match line, which must wrap.
  await expect(page.locator('#ck-qnone')).toBeVisible();
  expect(await page.evaluate(() => document.documentElement.scrollWidth)).toBeLessThanOrEqual(width);
});

test('on a phone the box is 16px and its clear button a 44px target', async ({ page }) => {
  test.skip(!phone(page), 'phone layout only');
  // Under 16px, iOS zooms the page in on focus.
  const { box } = await filterFor(page, 'x');
  await expect(box).toHaveCSS('font-size', '16px');
  const b = (await page.locator('#ck-qclear').boundingBox())!;
  expect(Math.min(b.width, b.height)).toBeGreaterThanOrEqual(44);
});

test('pin order and dividers hold under a filter across a repaint', async ({ page }) => {
  test.skip(phone(page), 'the sort headers are table-only');
  // Every poll repaints, in server order.
  await repaintEveryPoll(page);
  await page.goto('/?refresh=1');
  const { box, shown } = ui(page);
  const app = (row: ReturnType<Page['locator']>) => row.locator('td[data-label="app"]');
  // Server order leads with clodhopper; pinned app-ascending leads with appB.
  await expect(app(shown.first())).toHaveText('clodhopper');
  await page.locator('#ck-pin').click();
  await page.locator('#content th[data-sort="app"] button.sortbtn').click();
  await box.fill('waiting');
  await expect(app(shown.first())).toHaveText(appB);
  const ids = () => shown.evaluateAll((rows) => rows.map((r) => r.id));
  const frozen = await ids();

  await repaint(page, '#content table.roster');
  expect(await ids()).toEqual(frozen);
  // Each visible row starts a divider exactly when its group differs from the visible row above.
  const groups = await shown.evaluateAll((rows) => new Set(rows.map((r) => r.dataset.group)).size);
  expect(groups).toBeGreaterThan(1);
  const wrong = await shown.evaluateAll((rows) =>
    rows.filter((r, i) => r.classList.contains('group-start') !== (i > 0 && r.dataset.group !== rows[i - 1].dataset.group)).map((r) => r.id),
  );
  expect(wrong).toEqual([]);
  await page.unrouteAll({ behavior: 'ignoreErrors' });
});

test('submitting the filters under debug keeps debug and event_type', async ({ page }) => {
  await page.goto('/?refresh=0&debug=1');
  // Drop the flag the page just stored, so only the form itself can carry debug=1.
  await page.evaluate(() => localStorage.removeItem('ch-debug'));
  await page.locator('#ck-filtersbtn').click();
  await page.locator('#event_type').fill('Stop');
  await page.locator('#event_type').press('Enter');
  await page.waitForURL(/event_type=Stop/);
  expect(new URL(page.url()).searchParams.get('debug')).toBe('1');
  await expect(page.locator('#event_type')).toHaveValue('Stop');
});
