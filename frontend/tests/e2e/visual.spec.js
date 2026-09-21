import { test, expect } from '@playwright/test'
import { openDashboard } from './fixtures.js'

// Baselines use the pinned Linux Playwright container locally and in CI.
// All data is synthetic; no live agents, API keys, or external resources.
for (const screen of ['login', 'desktop', 'mobile', 'mobile-tools', 'mobile-output', 'share-preview', 'mobile-comparison', 'expired-link']) {
  test(`visual: ${screen}`, async ({ context }) => {
    const page = await openDashboard(context, { authRequired: screen === 'login', savedKey: screen === 'login' ? undefined : 'visual-fixture-key', delayedReplay: screen === 'expired-link' })
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
    if (screen === 'mobile-comparison') {
      await page.getByRole('button', { name: 'Tools', exact: true }).click()
      await page.locator('#dashboard-tools').getByTitle('Multi-node comparison', { exact: true }).click()
      const dialog = page.getByRole('dialog', { name: 'Multi-Node Comparison' })
      await dialog.getByRole('button', { name: /Node A/ }).click()
      await dialog.getByRole('button', { name: /Node B/ }).click()
      await expect(dialog).toHaveAttribute('data-state', 'open')
    }
    if (screen === 'expired-link') {
      await expect.poll(() => Boolean(page.replayRoute)).toBe(true)
      await page.replayRoute.fulfill({ status: 404, body: 'not found' })
      await expect(page.getByRole('heading', { name: 'Link expired or unavailable' })).toBeVisible()
    }
    await page.evaluate(() => document.fonts.ready)
    await expect(page).toHaveScreenshot(`${screen}.png`, { fullPage: true, animations: 'disabled', maxDiffPixelRatio: 0.001 })
    expect(page.errors).toEqual([])
  })
}
