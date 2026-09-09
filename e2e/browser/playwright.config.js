// playwright.config.js — Task 24 (§23.4): the four-second browser route /
// CORS / CSP / noscript gate across Chromium, Firefox and WebKit.
//
// globalSetup boots the hermetic fixture (real control + real agent on
// loopback, per-hostname CONNECT-proxy DNS shim) and writes .state.json for
// the tests; globalTeardown tears everything down. Each test creates its own
// context with the loopback proxy so the namespace hostnames resolve to the
// fixture without hosts-file mutation (the Playwright analogue of the Phase 3
// chromedp --host-resolver-rules convention).

import { defineConfig, devices } from '@playwright/test';

export default defineConfig({
  testDir: '.',
  testMatch: 'relay-route.spec.js',
  globalSetup: './global-setup.mjs',
  globalTeardown: './global-teardown.mjs',
  timeout: 60_000,
  expect: { timeout: 10_000 },
  fullyParallel: false,
  workers: 1,
  retries: 0,
  reporter: [['list'], ['json', { outputFile: 'test-results/report.json' }]],
  use: {
    trace: 'retain-on-failure',
    ignoreHTTPSErrors: true,
    actionTimeout: 15_000,
    navigationTimeout: 30_000,
  },
  projects: [
    { name: 'chromium', use: { ...devices['Desktop Chrome'] } },
    { name: 'firefox', use: { ...devices['Desktop Firefox'] } },
    { name: 'webkit', use: { ...devices['Desktop Safari'] } },
  ],
});
