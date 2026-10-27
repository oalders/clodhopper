import { test as base, expect, type Page } from '@playwright/test';

// The clip checks have ~2px to spare, so every run must measure with the same
// face. JetBrains Mono (the page's first-choice font) comes from
// @fontsource/jetbrains-mono, never from the network or the host's installed
// fonts: the faces are registered before any page script runs, and an
// @font-face face shadows an installed font of the same family name.
const faces = [
  ...[400, 500, 600, 700, 800].map((weight) => ({ weight, style: 'normal' })),
  { weight: 400, style: 'italic' },
];
// A stand-in host for the font files; the route below serves them locally.
const fontHost = 'https://fonts.e2e.invalid/';

export const test = base.extend({
  context: async ({ context, baseURL }, use) => {
    await context.route('**/*', (route) => {
      const url = route.request().url();
      if (url.startsWith(fontHost)) {
        return route.fulfill({
          path: require.resolve(`@fontsource/jetbrains-mono/files/${url.slice(fontHost.length)}`),
          contentType: 'font/woff2',
        });
      }
      // Everything off the local server (e.g. a Google Fonts @import) is aborted.
      return url.startsWith(`${baseURL}/`) ? route.continue() : route.abort();
    });
    await context.addInitScript(
      ({ faces, fontHost }) => {
        for (const { weight, style } of faces) {
          const src = `url(${fontHost}jetbrains-mono-latin-${weight}-${style}.woff2)`;
          const face = new FontFace('JetBrains Mono', src, { weight: String(weight), style });
          document.fonts.add(face);
          face.load();
        }
      },
      { faces, fontHost },
    );
    await use(context);
  },
});

// fontsReady waits until every JetBrains Mono face has loaded, and fails the
// test if any did not, so nothing is measured in a fallback font.
export async function fontsReady(page: Page) {
  const statuses = await page.evaluate(async () => {
    await document.fonts.ready;
    // Newer Chromium reports the family quoted ("JetBrains Mono").
    return [...document.fonts].filter((f) => f.family.replace(/"/g, '') === 'JetBrains Mono').map((f) => f.status);
  });
  expect(statuses).toEqual(faces.map(() => 'loaded'));
}

export { expect };

// abortUnstubbedExec aborts every /api/action and /api/pane request that no
// test stub answered, so none reaches the exec server's real handlers, and
// returns their URLs for an afterEach to assert empty. Register it before a
// test's own stubs: Playwright tries the newest matching route first.
export async function abortUnstubbedExec(page: Page): Promise<string[]> {
  const aborted: string[] = [];
  await page.route(
    (url) => url.pathname === '/api/action' || url.pathname === '/api/pane',
    (route) => {
      aborted.push(route.request().url());
      return route.abort();
    },
  );
  return aborted;
}

declare global {
  interface Window {
    __swaps: number;
  }
}

// repaintEveryPoll gives every /api/state answer a fresh signature, so each
// one-second poll repaints, and counts repaints off the ch:contentswap event
// swapContent fires (it is synchronous, so a counted repaint is finished). Call
// it before page.goto; it returns a getter for how many polls have been answered.
export async function repaintEveryPoll(page: Page): Promise<() => number> {
  let polls = 0;
  await page.route('**/api/state?*', async (route) => {
    const response = await route.fetch();
    const json = await response.json();
    return route.fulfill({ response, json: { ...json, signature: `repaint-${++polls}` } });
  });
  await page.addInitScript(() => {
    window.__swaps = 0;
    document.addEventListener('ch:contentswap', () => window.__swaps++);
  });
  return () => polls;
}

export const swaps = (page: Page) => page.evaluate(() => window.__swaps);

// repaint waits for the next repaint and checks it really replaced el, which
// must select a single element.
export async function repaint(page: Page, el: string) {
  const old = await page.locator(el).elementHandle();
  const before = await swaps(page);
  await expect.poll(() => swaps(page)).toBeGreaterThan(before);
  expect(await old!.evaluate((n) => n.isConnected)).toBe(false);
  await old!.dispose();
}
