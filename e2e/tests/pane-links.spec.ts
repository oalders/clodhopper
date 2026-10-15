import type { Page } from '@playwright/test';
import { test, expect } from './fixtures';

// Render a capture into a detached <pre> and report its links and text.
const render = (page: Page, text: string, repo: string) =>
  page.evaluate(
    ([t, r]) => {
      const pre = document.createElement('pre');
      (window as any).__ckRenderCapture(pre, t, r);
      return {
        links: Array.from(pre.querySelectorAll('a')).map((a) => [a.textContent, a.href]),
        text: pre.textContent,
      };
    },
    [text, repo],
  );

test('links bare #N and footer PR #N to the repo, leaving other text alone', async ({ page }) => {
  await page.goto('/?refresh=0');

  const bare = '  - #1235 fixes the CI failure.';
  expect(await render(page, bare, 'o/r')).toEqual({
    links: [['#1235', 'https://github.com/o/r/issues/1235']],
    text: bare,
  });

  const footer = 'main · PR #12 · ctx 40%';
  expect(await render(page, footer, 'o/r')).toEqual({
    links: [['PR #12', 'https://github.com/o/r/pull/12']],
    text: footer,
  });

  const mixed = 'main · PR #12 · ctx 40%\n- #1235 and #0, see PR #7.';
  expect(await render(page, mixed, 'o/r')).toEqual({
    links: [
      ['PR #12', 'https://github.com/o/r/pull/12'],
      ['#1235', 'https://github.com/o/r/issues/1235'],
      ['#7', 'https://github.com/o/r/issues/7'],
    ],
    text: mixed,
  });

  for (const t of ['abc#12', '#12abc', 'foo/#12', '#0', '&#12;']) {
    expect((await render(page, t, 'o/r')).links, t).toEqual([]);
  }
  for (const repo of ['', 'not a repo']) {
    expect((await render(page, '#5', repo)).links, repo).toEqual([]);
  }
});
