import { defineConfig } from '@playwright/test';

// @playwright/test is pinned to exactly 1.57.0 (package.json), which runs
// chromium / chromium-headless-shell revision 1200. Local runs use the
// read-only host browser bundle (~/.cache/ms-playwright), so before bumping the
// pin, refresh that bundle on the host with installer/playwright-mcp.sh (it
// lives in the user's installer setup, not in this repo).

// The nono sandbox opens only 9323–9342 for e2e work; 9323 is the HTML report.
const port = 9324;

export default defineConfig({
  testDir: './tests',
  forbidOnly: !!process.env.CI,
  reporter: [['list'], ['html', { open: 'never' }]],
  use: { baseURL: `http://127.0.0.1:${port}`, browserName: 'chromium' },
  projects: [
    { name: 'desktop 1440', use: { viewport: { width: 1440, height: 900 } } },
    // Narrowest table layout: dashboard.html's min-width: 641px breakpoint; 640 and below is cards.
    { name: 'narrowest table 641', use: { viewport: { width: 641, height: 900 } } },
    { name: 'phone 390', use: { viewport: { width: 390, height: 844 } } },
  ],
  webServer: {
    command: `./serve.sh ${port}`,
    url: `http://127.0.0.1:${port}/`,
    reuseExistingServer: false,
    timeout: 120_000,
    // SIGTERM (not the default SIGKILL) so serve.sh's trap removes its temp dir.
    gracefulShutdown: { signal: 'SIGTERM', timeout: 5_000 },
  },
});
