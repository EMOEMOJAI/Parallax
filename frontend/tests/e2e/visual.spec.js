import { test, expect } from '@playwright/test'
import { openDashboard } from './fixtures.js'

// Baselines use the pinned Linux Playwright container locally and in CI.
// All data is synthetic; no live agents, API keys, or external resources.
for (const screen of ['login', 'desktop', 'mobile', 'mobile-tools']) {
  test(`visual: ${screen}`, async ({ context }) => {
    const page = await openDashboard(context, { authRequired: screen === 'login', savedKey: screen === 'login' ? undefined : 'visual-fixture-key' })
    if (screen.startsWith('mobile')) await page.setViewportSize({ width: 390, height: 844 })
    if (screen === 'login') await expect(page.getByRole('dialog')).toHaveAttribute('data-state', 'open')
    if (screen === 'mobile-tools') {
      await page.getByRole('button', { name: 'Tools', exact: true }).click()
      await expect(page.locator('#dashboard-tools')).toHaveAttribute('data-state', 'open')
    }
    await page.evaluate(() => document.fonts.ready)
    await expect(page).toHaveScreenshot(`${screen}.png`, { fullPage: true, animations: 'disabled', maxDiffPixelRatio: 0.001 })
    expect(page.errors).toEqual([])
  })
}
