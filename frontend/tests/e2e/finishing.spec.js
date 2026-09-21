import { test, expect } from '@playwright/test'
import AxeBuilder from '@axe-core/playwright'
import { openDashboard, nodes } from './fixtures.js'

for (const width of [320, 390, 1280]) {
  test(`comparison columns adapt at ${width}px`, async ({ context }) => {
    const page = await openDashboard(context)
    await page.setViewportSize({ width, height: 844 })
    if (width < 768) await page.getByRole('button', { name: 'Tools', exact: true }).click()
    await (width < 768 ? page.locator('#dashboard-tools') : page).getByTitle('Multi-node comparison', { exact: true }).click()
    const dialog = page.getByRole('dialog', { name: 'Multi-Node Comparison' })
    await dialog.getByRole('button', { name: /Node A/ }).click()
    await dialog.getByRole('button', { name: /Node B/ }).click()
    const cards = dialog.getByTestId('comparison-result')
    await expect(cards).toHaveCount(2)
    const columns = await cards.first().evaluate((element) => getComputedStyle(element.parentElement).gridTemplateColumns.split(' ').length)
    expect(columns).toBe(width < 640 ? 1 : 2)
    if (width < 640) {
      const first = await cards.nth(0).boundingBox()
      const second = await cards.nth(1).boundingBox()
      expect(second.y).toBeGreaterThanOrEqual(first.y + first.height)
      expect(await dialog.evaluate((element) => element.scrollWidth <= element.clientWidth)).toBe(true)
    }
    expect(page.errors).toEqual([])
  })
}

test('unavailable shared results explain expiry and return without retrying the old URL', async ({ context }) => {
  const page = await openDashboard(context, { delayedReplay: true })
  await expect.poll(() => Boolean(page.replayRoute)).toBe(true)
  await page.replayRoute.fulfill({ status: 404, body: 'not found' })
  await expect(page.getByRole('heading', { name: 'Link expired or unavailable' })).toBeVisible()
  const scan = await new AxeBuilder({ page }).withTags(['wcag2a', 'wcag2aa', 'wcag21a', 'wcag21aa']).analyze()
  expect(scan.violations).toEqual([])
  await page.getByRole('button', { name: 'Return to dashboard', exact: true }).click()
  expect(new URL(page.url()).searchParams.has('run')).toBe(false)
  await expect(page.getByRole('region', { name: 'Diagnostic output' })).toBeVisible()
  await page.reload()
  await expect(page.getByRole('heading', { name: 'Link expired or unavailable' })).not.toBeVisible()
})

test('temporary replay failures offer a retry instead of claiming expiry', async ({ context }) => {
  const page = await openDashboard(context, { delayedReplay: true })
  await expect.poll(() => Boolean(page.replayRoute)).toBe(true)
  await page.replayRoute.fulfill({ status: 503, body: 'unavailable' })
  await expect(page.getByRole('heading', { name: 'Couldn’t load this shared result' })).toBeVisible()
  await page.route('**/api/runs/saved-run', (route) => route.fulfill({ json: {
    command: 'ping', target: 'example.com', node_name: 'Synthetic', created_at: '2026-01-01T00:00:00Z',
    lines: [{ type: 'output', text: 'Recovered shared result' }],
  } }))
  await page.getByRole('button', { name: 'Try again', exact: true }).click()
  await expect(page.getByRole('region', { name: 'Diagnostic output' })).toContainText('Recovered shared result')
  await expect(page.getByRole('heading', { name: 'Couldn’t load this shared result' })).not.toBeVisible()
})

test('agent hashes are shortened in both views and full versions remain copyable', async ({ context }) => {
  const version = '0123456789abcdef0123456789abcdef01234567'
  const page = await openDashboard(context, { nodes: [{ ...nodes[0], version }] })
  await page.evaluate(() => { Object.defineProperty(navigator, 'clipboard', { configurable: true, value: { writeText: async (value) => { window.copiedVersion = value } } }) })
  await expect(page.getByText('01234567…', { exact: true })).toBeVisible()
  await page.getByRole('button', { name: 'Copy full agent version', exact: true }).click()
  expect(await page.evaluate(() => window.copiedVersion)).toBe(version)
  await page.getByTitle('Node health overview', { exact: true }).click()
  const dialog = page.getByRole('dialog', { name: 'Node health overview' })
  await expect(dialog.getByText('01234567…', { exact: true })).toBeVisible()
  await page.evaluate(() => { Object.defineProperty(navigator, 'clipboard', { configurable: true, value: undefined }) })
  await dialog.getByRole('button', { name: 'Copy full agent version', exact: true }).click()
  await expect(dialog.getByText(version, { exact: true })).toBeVisible()
  await expect(dialog.getByRole('status').filter({ hasText: 'Couldn’t copy' })).toBeVisible()
  expect(page.errors).toEqual([])
})
