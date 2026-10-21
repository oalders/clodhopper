import type { Page } from '@playwright/test';
import { test, expect } from './fixtures';

const storage = (page: Page, key: string) =>
  page.evaluate((k) => localStorage.getItem(k), key);

// serve.sh's stub gh queues two seeded rows, one on a permission prompt (#219),
// ejects another (#195), and reports a fourth row's PR merged over a stale
// failing, conflicted rollup (#204).
test('queued rows fold into a collapsed merge-queue section that remembers being opened', async ({ page }) => {
  await page.goto('/?refresh=0');
  const mq = page.locator('#ck-mq');
  await expect(mq).not.toHaveAttribute('open');
  await expect(mq.locator('summary')).toHaveText(/^Merge queue \(2\) · next merge in ~\d+[smh]$/);
  await expect(mq.locator('table.mq tbody tr')).toHaveCount(2);
  // The roster is the last <section>, so .ck-card:last-of-type zeroes its margin; #ck-mq supplies the gap.
  const roster = (await page.locator('section.ck-card', { has: page.locator('table.roster') }).boundingBox())!;
  expect((await mq.boundingBox())!.y - (roster.y + roster.height)).toBeGreaterThan(0);
  // The ejected row needs you, so it stays in the main roster.
  await expect(page.locator('table.roster .cibadge.fail', { hasText: '⏏' })).toHaveCount(1);

  await mq.locator('summary').click();
  await expect(mq).toHaveAttribute('open');
  await expect.poll(() => storage(page, 'ch-mq-open')).toBe('open');
  await page.reload();
  await expect(page.locator('#ck-mq')).toHaveAttribute('open');
});

test('a queued row on a permission prompt folds into the merge queue without alerting', async ({ page }) => {
  await page.goto('/?refresh=0');
  const mq = page.locator('#ck-mq');
  const row = '#row-e2e-queued-approval-0000-1111-2222-333333333333';
  await mq.locator('summary').click();
  await expect(mq.locator(`table.mq ${row}`)).toBeVisible();
  await expect(page.locator(row)).not.toHaveClass(/\balert\b/);
  await expect(page.locator(`table.roster ${row}`)).toHaveCount(0);
  // The summary carries only its heading: no alert badge for the prompt inside.
  await expect(mq.locator('summary > *')).toHaveCount(1);
});

test('a merged PR shows a merged chip, not its stale CI', async ({ page }) => {
  await page.goto('/?refresh=0');
  const merged = page.locator('table.roster tr', { has: page.locator('.copycwd[data-cwd$="/merged"]') });
  await expect(merged.getByRole('img', { name: 'pull request merged' })).toHaveText('merged');
  await expect(merged.locator('.cibadge.fail')).toHaveCount(0);
  await expect(merged.getByRole('img', { name: 'merge conflict with base' })).toHaveCount(0);
  await expect(merged.getByRole('img', { name: 'no CI' })).toHaveCount(0);
  // Seeded "waiting for you" (a Stop); a finished PR relaxes it to background.
  await expect(merged.locator('td.status')).toHaveText(/^waiting$/);
});
