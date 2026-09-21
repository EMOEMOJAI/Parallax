import { test, expect } from '@playwright/test'
import { openDashboard } from './fixtures.js'

test('clearing browser storage in another tab resets presets and kits', async ({ context }) => {
  const first = await openDashboard(context)
  await first.evaluate(() => {
    localStorage.setItem('lookingGlass.presets', JSON.stringify([{ id: 'saved', type: 'ping', target: 'example.com', options: '', label: 'Fixture' }]))
    localStorage.setItem('lg-kit', JSON.stringify([{ type: 'tcp', options: '' }]))
  })
  const second = await openDashboard(context)
  await second.getByRole('button', { name: 'Saved presets', exact: true }).click()
  await expect(second.getByText('Presets (1/10)', { exact: true })).toBeVisible()
  await expect(second.getByText('Diagnostic kit (1/8)', { exact: true })).toBeVisible()
  await first.evaluate(() => localStorage.clear())
  await expect(second.getByText('Presets (0/10)', { exact: true })).toBeVisible()
  await expect(second.getByText('Diagnostic kit (3/8)', { exact: true })).toBeVisible()
  expect(second.errors).toEqual([])
})

test('focus wraps around a pending disabled schedule fieldset', async ({ context }) => {
  const page = await openDashboard(context)
  let pending
  await page.route('**/api/schedules', (route) => {
    if (route.request().method() === 'POST') { pending = route; return }
    return route.fulfill({ contentType: 'application/json', body: '[]' })
  })
  await page.getByTitle('Scheduled probes', { exact: true }).click()
  const dialog = page.getByRole('dialog', { name: 'Scheduled probes', exact: true })
  await dialog.getByRole('button', { name: 'New schedule' }).click()
  await dialog.getByLabel('Target', { exact: true }).fill('example.com')
  await dialog.getByRole('button', { name: 'Create', exact: true }).click()
  await expect(dialog.getByRole('button', { name: 'Create', exact: true })).toBeDisabled()
  await dialog.getByRole('button', { name: 'Close', exact: true }).focus()
  await page.keyboard.press('Tab')
  await expect(dialog.getByRole('button', { name: 'Refresh', exact: true })).toBeFocused()
  await page.keyboard.press('Shift+Tab')
  await expect(dialog.getByRole('button', { name: 'Close', exact: true })).toBeFocused()
  await expect.poll(() => Boolean(pending)).toBe(true)
  await pending.fulfill({ status: 201, contentType: 'application/json', body: '{}' })
  expect(page.errors).toEqual([])
})

test('comparison drops invisible offline selections while idle', async ({ context }) => {
  const page = await openDashboard(context)
  await page.getByTitle('Multi-node comparison', { exact: true }).click()
  const dialog = page.getByRole('dialog', { name: 'Multi-Node Comparison', exact: true })
  const node = dialog.getByRole('button', { name: /Node A/ })
  await node.click()
  await expect(node).toHaveAttribute('aria-pressed', 'true')
  await dialog.getByLabel('Comparison target', { exact: true }).fill('example.com')
  await expect(dialog.getByRole('button', { name: 'Run All', exact: true })).toBeEnabled()
  await page.evaluate(() => window.sockets[0].emit({ type: 'node_status', node_id: 'a', online: false }))
  await expect(node).toHaveCount(0)
  await expect(dialog.getByRole('button', { name: 'Run All', exact: true })).toBeDisabled()
  expect(page.errors).toEqual([])
})

for (const operation of ['remove', 'clear']) {
  test(`history deletion in another tab stays deleted after ${operation}`, async ({ context }) => {
    const first = await openDashboard(context)
    await first.evaluate(() => localStorage.setItem('lg-cmd-history', JSON.stringify([{ type: 'ping', target: 'old.example' }])))
    const second = await openDashboard(context)
    const input = second.getByLabel('Command target', { exact: true })
    await input.press('ArrowUp')
    await expect(input).toHaveValue('old.example')
    await second.getByTitle('Command history (↑/↓ in input)', { exact: true }).click()
    await expect(second.getByText('Recent Commands', { exact: true })).toBeVisible()
    await first.evaluate((operation) => operation === 'clear' ? localStorage.clear() : localStorage.removeItem('lg-cmd-history'), operation)
    await expect(second.getByText('Recent Commands', { exact: true })).not.toBeVisible()
    await input.fill('new.example')
    await input.press('ArrowUp')
    await expect(input).not.toHaveValue('old.example')
    await input.fill('new.example')
    await second.getByRole('button', { name: 'Run', exact: true }).first().click()
    const stored = await second.evaluate(() => JSON.parse(localStorage.getItem('lg-cmd-history')))
    expect(stored.map((entry) => entry.target)).toEqual(['new.example'])
    expect(second.errors).toEqual([])
  })
}
