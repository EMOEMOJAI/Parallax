import { test, expect } from '@playwright/test'
import { openDashboard } from './fixtures.js'

// Baselines use the pinned Linux Playwright container locally and in CI.
// All data is synthetic; no live agents, API keys, or external resources.
for (const screen of ['login', 'desktop', 'mobile', 'mobile-tools', 'mobile-output', 'share-preview']) {
  test(`visual: ${screen}`, async ({ context }) => {
    const page = await openDashboard(context, { authRequired: screen === 'login', savedKey: screen === 'login' ? undefined : 'visual-fixture-key' })
    if (screen.startsWith('mobile')) await page.setViewportSize({ width: 390, height: 844 })
    if (screen === 'login') await expect(page.getByRole('dialog')).toHaveAttribute('data-state', 'open')
    if (screen === 'mobile-tools') {
      await page.getByRole('button', { name: 'Tools', exact: true }).click()
      await expect(page.locator('#dashboard-tools')).toHaveAttribute('data-state', 'open')
    }
    if (screen === 'mobile-output' || screen === 'share-preview') {
      await page.getByLabel('Command target', { exact: true }).fill('example.com')
      await page.getByRole('button', { name: 'Run', exact: true }).first().click()
      await page.evaluate(() => {
        const id = window.sent.find((message) => message.command).command.id
        window.sockets.at(-1).emit({ id, type: 'output', data: 'Reply from 192.0.2.1: time=12 ms' })
        window.sockets.at(-1).emit({ id, type: 'done', data: '{"exit_ok":true}' })
      })
      await expect(page.getByRole('region', { name: 'Diagnostic output' })).toContainText('time=12 ms')
      if (screen === 'share-preview') {
        await page.getByTitle('Share output (creates a 24-hour permalink)', { exact: true }).click()
        await expect(page.getByRole('dialog', { name: 'Share preview' })).toBeVisible()
      }
    }
    await page.evaluate(() => document.fonts.ready)
    await expect(page).toHaveScreenshot(`${screen}.png`, { fullPage: true, animations: 'disabled', maxDiffPixelRatio: 0.001 })
    expect(page.errors).toEqual([])
  })
}
