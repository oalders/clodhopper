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
    return [...document.fonts].filter((f) => f.family === 'JetBrains Mono').map((f) => f.status);
  });
  expect(statuses).toEqual(faces.map(() => 'loaded'));
}

export { expect };
