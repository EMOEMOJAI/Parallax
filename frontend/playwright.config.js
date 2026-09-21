import { defineConfig } from '@playwright/test'

export default defineConfig({
  testDir: './tests/e2e',
  fullyParallel: true,
  forbidOnly: !!process.env.CI,
  retries: 0,
  workers: process.env.CI ? 2 : undefined,
  timeout: 30000,
  expect: { timeout: 5000 },
  reporter: [['list'], ['html', { open: 'never' }]],
  use: {
    browserName: 'chromium',
    trace: 'retain-on-failure',
    screenshot: 'only-on-failure',
  },
  projects: [
    {
      name: 'production',
      testIgnore: ['**/auth-source.spec.js', '**/visual.spec.js'],
      use: { baseURL: 'http://127.0.0.1:5199' },
    },
    {
      name: 'visual',
      testMatch: '**/visual.spec.js',
      snapshotPathTemplate: '{testDir}/visual-snapshots/{arg}{ext}',
      use: {
        baseURL: 'http://127.0.0.1:5199',
        viewport: { width: 1280, height: 900 },
        deviceScaleFactor: 1,
        colorScheme: 'dark',
        reducedMotion: 'reduce',
        locale: 'en-US',
        timezoneId: 'UTC',
      },
    },
    {
      // These two races call the API module directly, so use Vite's module server.
      name: 'source-auth',
      testMatch: '**/auth-source.spec.js',
      use: { baseURL: 'http://127.0.0.1:5200' },
    },
  ],
  webServer: [
    {
      command: 'npm run preview -- --host 127.0.0.1 --port 5199 --strictPort',
      url: 'http://127.0.0.1:5199',
      reuseExistingServer: false,
    },
    {
      command: 'npm run dev -- --host 127.0.0.1 --port 5200 --strictPort',
      url: 'http://127.0.0.1:5200',
      reuseExistingServer: false,
    },
  ],
})
