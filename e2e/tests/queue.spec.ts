import type { Page } from '@playwright/test';
import { test, expect } from './fixtures';

const storage = (page: Page, key: string) =>
  page.evaluate((k) => localStorage.getItem(k), key);

// serve.sh's stub gh queues one seeded row and ejects another (#195).
test('queued rows fold into a collapsed merge-queue section that remembers being opened', async ({ page }) => {
  await page.goto('/?refresh=0');
  const mq = page.locator('#ck-mq');
  await expect(mq).not.toHaveAttribute('open');
  await expect(mq.locator('summary')).toHaveText(/^Merge queue \(1\) · next merge in ~\d+[smh]$/);
  await expect(mq.locator('table.mq tbody tr')).toHaveCount(1);
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
