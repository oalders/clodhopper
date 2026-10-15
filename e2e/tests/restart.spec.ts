import type { Page, Request } from '@playwright/test';
import { test, expect, abortUnstubbedExec } from './fixtures';

// Runs only against serve.sh's --enable-merge server (the "exec desktop 1440"
// project in playwright.config.ts). Every /api/action request is answered
// here, so no test ever reaches the real handler and its tmux calls; one a
// test forgot to stub is aborted and fails that test (see beforeEach).

// The envelope writeActionResult (action.go) sends for a broadcast: any error
// row makes ok false and exitCode -1, rows arrive sorted error, skipped, sent
// (then by target), and output carries the same rows as plain lines.
const restartReport = {
  ok: false,
  exitCode: -1,
  timedOut: false,
  output: [
    'error clodhopper fix-145 1a2b3c4d: exited, but the shell is in "/tmp/x", not "/home/u/clodhopper" where the session started — resume manually',
    'skipped clodhopper main 5e6f7a8b: working — its turn would be cut off',
    'sent other-app feat-a 9c0d1e2f',
  ].join('\n'),
  rows: [
    {
      result: 'error',
      target: 'clodhopper fix-145 1a2b3c4d',
      dir: '/home/u/clodhopper',
      reason: 'exited, but the shell is in "/tmp/x", not "/home/u/clodhopper" where the session started — resume manually',
    },
    { result: 'skipped', target: 'clodhopper main 5e6f7a8b', dir: '/home/u/clodhopper-main', reason: 'working — its turn would be cut off' },
    { result: 'sent', target: 'other-app feat-a 9c0d1e2f', dir: '/home/u/other-app', reason: '' },
  ],
};

// stubActions answers every /api/action with restartReport and returns the
// requests it saw, so a test can also assert that nothing was posted.
async function stubActions(page: Page): Promise<Request[]> {
  const seen: Request[] = [];
  await page.route('**/api/action', (route) => {
    seen.push(route.request());
    return route.fulfill({ json: restartReport });
  });
  return seen;
}

// Requests to the exec endpoints that no test stub answered.
let unstubbed: string[] = [];

test.beforeEach(async ({ page }) => {
  unstubbed = await abortUnstubbedExec(page);
  await page.goto('/?refresh=0');
});

test.afterEach(() => {
  expect(unstubbed, 'exec endpoint requests with no stub').toEqual([]);
});

test('restart (all) arms on the first click, with a cancel and a caveat', async ({ page }) => {
  const posts = await stubActions(page);
  const btn = page.locator('#ck-restartall');
  const group = page.locator('.actgroup', { has: btn });
  await expect(btn).toHaveText('restart (all)');

  await btn.click();
  await expect(group).toHaveClass(/\bconfirm\b/);
  await expect(btn).toHaveClass(/\bconfirm\b/);
  await expect(btn).toHaveText('✓ confirm');
  await expect(group.getByRole('button', { name: 'cancel' })).toBeVisible();
  await expect(group.locator('.actmsg')).toContainText('restart every idle / waiting-for-you session');
  expect(posts).toHaveLength(0);
});

test('Escape disarms restart (all) and puts focus back on it', async ({ page }) => {
  const posts = await stubActions(page);
  const btn = page.locator('#ck-restartall');
  const group = page.locator('.actgroup', { has: btn });
  await btn.click();
  // From the injected cancel, which the disarm removes: the focus has to be
  // handed back, not left on <body>.
  const cancel = group.getByRole('button', { name: 'cancel' });
  await cancel.focus();

  await page.keyboard.press('Escape');
  await expect(group).not.toHaveClass(/\bconfirm\b/);
  await expect(btn).toHaveText('restart (all)');
  await expect(cancel).toHaveCount(0);
  await expect(group.locator('.actmsg')).toHaveText('');
  await expect(btn).toBeFocused();
  expect(posts).toHaveLength(0);
});

test('the cancel button disarms restart (all) and puts focus back on it', async ({ page }) => {
  const posts = await stubActions(page);
  const btn = page.locator('#ck-restartall');
  const group = page.locator('.actgroup', { has: btn });
  await btn.click();

  const cancel = group.getByRole('button', { name: 'cancel' });
  await cancel.click();
  await expect(group).not.toHaveClass(/\bconfirm\b/);
  await expect(btn).toHaveText('restart (all)');
  await expect(cancel).toHaveCount(0);
  await expect(btn).toBeFocused();
  expect(posts).toHaveLength(0);
});

test('confirming restart (all) renders the report, and clear empties it', async ({ page }) => {
  const posts = await stubActions(page);
  const btn = page.locator('#ck-restartall');
  const out = page.locator('#ck-restartout');
  const msg = page.locator('#ck-restartmsg');
  const clear = page.locator('#ck-restartclear');

  await expect(out).toHaveCSS('display', 'none'); // toBeHidden would pass on a zero-height box too
  await btn.click();
  await btn.click();
  await expect(msg).toHaveText('1 error · 1 skipped · 1 sent');
  await expect(out).toBeVisible();
  expect(posts).toHaveLength(1);
  expect(posts[0].method()).toBe('POST');
  expect(posts[0].postData()).toBe('action=broadcast-restart');
  expect(posts[0].headers()['x-clodhopper-token']).toBeTruthy();

  const table = out.getByRole('table', { name: 'restart results' });
  await expect(table.locator('th')).toHaveText(['result', 'target', 'dir', 'reason']);
  const rows = table.locator('tbody tr');
  await expect(rows).toHaveCount(3);
  for (const [i, row] of restartReport.rows.entries()) {
    const cls = ({ error: 'fail', skipped: 'alert' } as Record<string, string>)[row.result] ?? '';
    await expect(rows.nth(i)).toHaveAttribute('class', cls);
    await expect(rows.nth(i).locator('td')).toHaveText([row.result, row.target, row.dir, row.reason]);
  }
  await expect(rows.first().locator('td[data-label="dir"]')).toHaveText('/home/u/clodhopper');
  // The confirm went through fire(), which unlocks the group before posting.
  await expect(btn).toHaveText('restart (all)');
  await expect(btn).toBeEnabled();

  await expect(clear).toBeVisible();
  await clear.click();
  await expect(msg).toHaveText('');
  await expect(out.locator('table')).toHaveCount(0);
  await expect(out).toHaveCSS('display', 'none');
  await expect(out).not.toHaveAttribute('tabindex');
  await expect(clear).toBeHidden();
  await expect(btn).toBeFocused();
});

test('restart (all) is the same size as reload plugins (all)', async ({ page }) => {
  // button.pract would shrink it to row-button size without the
  // .ck-bulk button.ck-btn.pract rule.
  const fontSize = (id: string) =>
    page.locator(id).evaluate((el) => getComputedStyle(el).fontSize);
  expect(await fontSize('#ck-restartall')).toBe(await fontSize('#ck-reloadall'));
});
