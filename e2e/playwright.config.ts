import { defineConfig } from '@playwright/test';

// @playwright/test is pinned to exactly 1.57.0 (package.json), which runs
// chromium / chromium-headless-shell revision 1200. Local runs use the
// read-only host browser bundle (~/.cache/ms-playwright), so before bumping the
// pin, refresh that bundle on the host with installer/playwright-mcp.sh (it
// lives in the user's installer setup, not in this repo).

// The nono sandbox opens only 9323–9342 for e2e work; 9323 is the HTML report.
const port = 9324;
// restart.spec.ts and swap.spec.ts need the --enable-merge (and --pane-peek)
// markup, so they get their own server rather than turning the write path on
// for every other spec.
const execPort = 9325;
// Anchored: testMatch/testIgnore regexes run against the absolute file path.
const execSpec = /(^|\/)(restart|swap)\.spec\.ts$/;

const server = (p: number, flags = '') => ({
  command: `./serve.sh ${p}${flags}`,
  url: `http://127.0.0.1:${p}/`,
  reuseExistingServer: false,
  // Both servers compile cgo sqlite at once, which can outlast 120s on a cold CI runner.
  timeout: 300_000,
  // SIGTERM (not the default SIGKILL) so serve.sh's trap removes its temp dir.
  gracefulShutdown: { signal: 'SIGTERM' as const, timeout: 5_000 },
});

export default defineConfig({
  testDir: './tests',
  forbidOnly: !!process.env.CI,
  reporter: [['list'], ['html', { open: 'never' }]],
  use: { baseURL: `http://127.0.0.1:${port}`, browserName: 'chromium' },
  projects: [
    { name: 'desktop 1440', testIgnore: execSpec, use: { viewport: { width: 1440, height: 900 } } },
    // Narrowest table layout: dashboard.html's min-width: 641px breakpoint; 640 and below is cards.
    { name: 'narrowest table 641', testIgnore: execSpec, use: { viewport: { width: 641, height: 900 } } },
    { name: 'phone 390', testIgnore: execSpec, use: { viewport: { width: 390, height: 844 } } },
    {
      name: 'exec desktop 1440',
      testMatch: execSpec,
      use: { baseURL: `http://127.0.0.1:${execPort}`, viewport: { width: 1440, height: 900 } },
    },
  ],
  webServer: [server(port), server(execPort, ' --enable-merge')],
});
