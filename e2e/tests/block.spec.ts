import type { Page, Request } from '@playwright/test';
import { test, expect, abortUnstubbedExec, repaintEveryPoll, repaint, swaps } from './fixtures';

// The blocked-on chip and the row panel's block picker (#210), against
// serve.sh's exec server; e2e_seed_test.go blocks fix-1234 and fix-2537 on fix-1235,
// fix-1236 on the queued row, the queued row on fix-1235, and draft-conflict
// on its repo's open PR #101.

const blocked = 'e2e-blocked-0000-1111-2222-333333333333';
const blocker = 'e2e-blocker-0000-1111-2222-333333333333';
const blockedAsk = 'e2e-blockedask-0000-1111-2222-333333333333';
const queueBlocked = 'e2e-queueblocked-0000-1111-2222-333333333333';
const queued = 'e2e-queued-0000-1111-2222-333333333333';
const draft = 'e2e-draft-conflict-0000-1111-2222-333333333333';
const chip = `#row-${blocked} .blockchip`;
const prChip = `#row-${draft} .blockchip`;
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
  await expect(page.locator(chip).getByRole('button', { name: 'blocked on fix-1235' })).toHaveText('fix-1235');
});

test('blocked rows nest right after their blocker, an alerting one still alerting', async ({ page }) => {
  const after = (id: string) => page.locator(`#row-${id} ~ tr[data-group]`).first();
  await expect(after(blocker)).toHaveAttribute('id', `row-${blocked}`);
  await expect(after(blocked)).toHaveAttribute('id', `row-${blockedAsk}`);
  await expect(page.locator(`#row-${blocked}`)).toHaveClass(/\bnested\b/);
  await expect(page.locator(`#row-${blockedAsk}`)).toHaveClass(/\balert\b/);
  await expect(page.locator(`#row-${blockedAsk}`)).toHaveClass(/\bnested\b/);
  await expect(page.locator(`#row-${blocked}`)).toHaveClass(/\bnest-1\b/);
  await expect(page.locator(`#row-${blocked} .nestmark`)).toHaveText('└─');
  await expect(page.locator(`#row-${blocker}`)).not.toHaveClass(/\bnested\b/);
  await expect(page.locator(`#row-${blocker} .nestmark`)).toHaveCount(0);
});

test('on a phone a nested card is inset from its blocker\'s, connector hidden, no sideways scroll', async ({ page }) => {
  await page.setViewportSize({ width: 375, height: 812 });
  await expect(page.locator(`#row-${blocked} .nestmark`)).toBeHidden();
  const x = async (id: string) => (await page.locator(`#row-${id}`).boundingBox())!.x;
  expect(await x(blocked)).toBeGreaterThan(await x(blocker));
  const { scrollWidth, innerWidth } = await page.evaluate(() => ({ scrollWidth: document.documentElement.scrollWidth, innerWidth: window.innerWidth }));
  expect(scrollWidth).toBeLessThanOrEqual(innerWidth);
});

test('a nested row\'s status cell keeps its status colour', async ({ page }) => {
  const want = await page.evaluate(() => {
    const probe = document.body.appendChild(document.createElement('span'));
    probe.style.color = 'var(--background)';
    const c = getComputedStyle(probe).color;
    probe.remove();
    return c;
  });
  await expect(page.locator(`#row-${blocked}`)).toHaveClass(/\bnested\b/);
  await expect(page.locator(`#row-${blocked} td.status`)).toHaveCSS('color', want);
});

test('a branch filter that drops the blocker leaves the child un-nested', async ({ page }) => {
  await page.goto('/?branch=fix-1234');
  await expect(page.locator(`#row-${blocked}`)).toBeVisible();
  await expect(page.locator(`#row-${blocker}`)).toHaveCount(0);
  await expect(page.locator(`#row-${blocked}`)).not.toHaveClass(/\bnested\b/);
});

test('under pin order and a status sort, either way, blocked rows ride in their blocker\'s group', async ({ page }) => {
  await repaintEveryPoll(page);
  await page.goto('/?refresh=1');
  await page.locator('#ck-pin').click();
  await expect(page.locator('#ck-pin')).toHaveAttribute('aria-pressed', 'true');
  // Alone, the needs-you child would sort to the top and the background one to the bottom.
  const ids = [blocker, blocked, blockedAsk];
  const run = () => page.locator('#content table.roster > tbody > tr[data-group]').evaluateAll((rows, ids) => {
    const at = rows.findIndex((r) => r.id === `row-${ids[0]}`);
    return rows.slice(at, at + ids.length).map((r) => [r.id, r.getAttribute('data-group')]);
  }, ids);
  const g = await page.locator(`#row-${blocker}`).getAttribute('data-group');
  const want = ids.map((id) => [`row-${id}`, g]);
  const sortBtn = page.locator('#content th[data-sort="status"] button.sortbtn');
  for (const dir of ['ascending', 'descending']) {
    await sortBtn.click();
    await expect(page.locator('#content th[data-sort="status"]')).toHaveAttribute('aria-sort', dir);
    expect(await run()).toEqual(want);
    await repaint(page, '#content table.roster');
    expect(await run()).toEqual(want);
  }
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

test('the picker leads with the row\'s own app even when it sorts last', async ({ page }) => {
  await page.evaluate(() => { (window as unknown as { __ckActionBusy: number }).__ckActionBusy = 1; });
  const working = 'e2e-working-0000-1111-2222-333333333333';
  await page.locator(`#content button.actbtn[data-actions="${working}"]`).click();
  const groups = page.locator(`${panel(working)} optgroup`);
  await expect(groups.first()).toHaveAttribute('label', 'clodhopper');
  await expect(groups.nth(1)).toHaveAttribute('label', 'a-second-project-with-a-long-source-app-name');
});

test('the row panel blocks on the picked row', async ({ page }) => {
  const posts = await stubAction(page);
  await page.evaluate(() => { (window as unknown as { __ckActionBusy: number }).__ckActionBusy = 1; });
  await page.locator(`#content button.actbtn[data-actions="${blocker}"]`).click();
  const form = page.locator(panel(blocker));
  await expect(form.locator(`option[value="${blocker}"]`)).toHaveCount(0);
  await expect(form.locator(`option[value="${queued}"]`)).toHaveCount(1);
  // Grouped by app, the row's own app first.
  await expect(form.locator('optgroup').first()).toHaveAttribute('label', 'a-second-project-with-a-long-source-app-name');
  // "app · branch", or the worktree dir for a branchless row; never the session id.
  await expect(form.locator(`option[value="${blocked}"]`)).toHaveText('a-second-project-with-a-long-source-app-name · fix-1234');
  await expect(form.locator('option[value="e2e-idle-0000-1111-2222-333333333333"]'))
    .toHaveText('a-second-project-with-a-long-source-app-name · a-very-long-worktree-directory-name-standing-in-for-the-branch');
  await form.getByRole('combobox', { name: 'this row is blocked on' }).selectOption(blocked);
  await form.getByRole('button', { name: 'mark fix-1235 as blocked on the selected row' }).click();
  await expect(form.locator('.actmsg')).toHaveText('marked blocked — row will update');
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
  await qchip.getByRole('button', { name: 'unblock mq-queued from fix-1235' }).click();
  await expect(qchip.locator('.actmsg')).toHaveText('unblocked — row will update');
  expect(posts).toHaveLength(1);
  expect(posts[0].postData()).toBe(`session_id=${queued}&action=unblock`);
});

test('a focused picker holds repaints off, and the choice survives the one after', async ({ page }) => {
  const polls = await repaintEveryPoll(page);
  await page.goto('/?refresh=1');
  await page.locator(`#content button.actbtn[data-actions="${blocker}"]`).click();
  const pick = page.locator(`${panel(blocker)} .blockpick`);
  const first = await pick.inputValue();
  // The untouched picker sits on its first option, which is not the one chosen below.
  expect(first).toBe(await pick.locator('option').first().getAttribute('value'));
  expect(first).not.toBe(queued);
  await pick.selectOption(queued);
  await pick.focus();
  // A repaint would replace the <select> and shut its open dropdown, so none lands while it has focus.
  const held = await pick.elementHandle();
  const before = await swaps(page);
  const answered = polls();
  await expect.poll(polls).toBeGreaterThan(answered + 1);
  expect(await swaps(page)).toBe(before);
  expect(await held!.evaluate((n) => n.isConnected)).toBe(true);
  await held!.dispose();
  await pick.blur();
  await repaint(page, `${panel(blocker)} .blockpick`);
  await expect(pick).toHaveValue(queued);
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
  // Stamped by hand, not repaint(): that needs repaintEveryPoll's swap counter, and this test supplies its own route.
  // A stamp on a node a repaint already detached would let the wait pass with no repaint.
  expect(await pick.evaluate((el) => { el.setAttribute('data-old', '1'); return el.isConnected; })).toBe(true);
  await expect(page.locator('.blockpick[data-old]')).toHaveCount(0);
  await expect(pick.locator(`option[value="${queued}"]`)).toHaveCount(0);
  const value = await pick.inputValue();
  expect(value).not.toBe('');
  expect(value).not.toBe(queued);
});

test('the row panel blocks on a picked open PR', async ({ page }) => {
  const posts = await stubAction(page);
  await page.evaluate(() => { (window as unknown as { __ckActionBusy: number }).__ckActionBusy = 1; });
  // serve.sh gives this row a GitHub origin whose repo has PRs #101 and #102 open.
  await page.locator(`#content button.actbtn[data-actions="${draft}"]`).click();
  const form = page.locator(panel(draft));
  await expect(form.locator('optgroup[label^="open PRs"]')).toHaveAttribute('label', 'open PRs · e2e/draft-conflict');
  await expect(form.locator('option[value="pr:101"]')).toHaveText('#101 Land the parser');
  await form.getByRole('combobox', { name: 'this row is blocked on' }).selectOption('pr:101');
  await form.getByRole('button', { name: 'mark mq-draft-conflict as blocked on the selected row' }).click();
  await expect(form.locator('.actmsg')).toHaveText('marked blocked — row will update');
  expect(posts).toHaveLength(1);
  expect(posts[0].postData()).toBe(`session_id=${draft}&action=block-pr&blocker_pr=101`);
});

test('a PR block relaxes the row and its chip links to the PR', async ({ page }) => {
  await expect(page.locator(`#row-${draft} .ck-badge`)).toHaveText('blocked');
  const link = page.locator(prChip).getByRole('link', { name: 'PR #101' });
  await expect(link).toHaveText('PR #101');
  await expect(link).toHaveAttribute('href', 'https://github.com/e2e/draft-conflict/pull/101');
  await expect(link).toHaveAttribute('target', '_blank');
  await expect(link).toHaveAttribute('rel', /noopener/);
  await expect(link).toHaveAttribute('title', 'Land the parser by alice');
  await expect(page.locator(prChip).locator('.blockjump')).toHaveCount(0);
});

test('the PR chip ✕ sends unblock for the blocked row', async ({ page }) => {
  const posts = await stubAction(page);
  await page.evaluate(() => { (window as unknown as { __ckActionBusy: number }).__ckActionBusy = 1; });
  await page.locator(prChip).getByRole('button', { name: 'unblock mq-draft-conflict from PR #101' }).click();
  await expect(page.locator(prChip).locator('.actmsg')).toHaveText('unblocked — row will update');
  expect(posts).toHaveLength(1);
  expect(posts[0].postData()).toBe(`session_id=${draft}&action=unblock`);
});
