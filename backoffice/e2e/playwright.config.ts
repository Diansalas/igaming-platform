import { defineConfig } from 'playwright/test'

const results = process.env.E2E_RESULTS_DIR ?? './results'
export default defineConfig({
  testDir: './specs',
  // One worker, no parallelism: each staff factor has a replay rule (a TOTP step is usable once), and specs share one stack.
  workers: 1,
  fullyParallel: false,
  retries: 0,
  timeout: 240_000,
  expect: { timeout: 10_000 },
  reporter: [['list'], ['json', { outputFile: `${results}/playwright.json` }]],
  outputDir: `${results}/artifacts`,
  use: {
    baseURL: process.env.E2E_BASE_URL,
    headless: true,
    // Screenshots only on failure; NO traces/videos (they would capture typed passwords and bearer tokens).
    screenshot: 'only-on-failure',
    trace: 'off',
    video: 'off',
    launchOptions: { args: ['--no-sandbox'], executablePath: process.env.E2E_CHROMIUM_PATH || undefined },
  },
})
