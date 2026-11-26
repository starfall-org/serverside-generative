import { defineConfig, devices } from '@playwright/test'

// The suite starts its own temporary daemon, HTTPS proxy, and fake provider.
export default defineConfig({
  testDir: './e2e',
  timeout: 60_000,
  fullyParallel: false,
  retries: 0,
  workers: 1,
  use: {
    ignoreHTTPSErrors: true,
    channel: 'chromium',
    locale: 'vi-VN',
    trace: 'retain-on-failure',
  },
  projects: [
    { name: 'chromium', use: { ...devices['Desktop Chrome'] } },
    { name: 'mobile', use: { ...devices['Pixel 7'] } },
  ],
})
