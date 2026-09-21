import { test, expect } from '@playwright/test'
import AxeBuilder from '@axe-core/playwright'
import { openDashboard, nodes } from './fixtures.js'

for (const remember of [false, true]) {
  test(`login ${remember ? 'remembers the browser' : 'lasts only for this tab'} and sign-out closes the session`, async ({ context }) => {
    const page = await openDashboard(context, { authRequired: true })
    const prompt = page.getByRole('dialog', { name: 'API key required' })
    await expect(prompt).toBeVisible()
    await page.getByLabel('Client API key', { exact: true }).fill('fixture-client-key')
    await page.getByLabel('Remember this browser').setChecked(remember)
    await page.getByRole('button', { name: 'Connect', exact: true }).click()
    await expect(prompt).not.toBeVisible()
    await expect.poll(() => page.evaluate(() => window.sockets.at(-1).protocols)).toEqual(['lg.bearer', 'fixture-client-key'])
    const readStorage = () => page.evaluate(() => ({
      local: localStorage.getItem('lg-client-key'),
      session: sessionStorage.getItem('lg-client-key'),
    }))
    expect(await readStorage()).toEqual({ local: remember ? 'fixture-client-key' : null, session: remember ? null : 'fixture-client-key' })
    await page.reload()
    await expect(page.getByRole('button', { name: 'Sign out', exact: true })).toBeVisible()
    await expect(prompt).not.toBeVisible()
    // Remounting must dispose of active diagnostic panels and their sockets.
    await page.getByTitle('Node health overview', { exact: true }).click()
    await expect(page.getByRole('dialog', { name: 'Node health overview' })).toBeVisible()
    // Close the panel to reach the header as a real user would.
    await page.keyboard.press('Escape')
    await expect(page.getByRole('dialog', { name: 'Node health overview' })).not.toBeVisible()
    await page.evaluate(() => { window.oldSocket = window.sockets.at(-1) })
    await page.getByRole('button', { name: 'Sign out', exact: true }).click()
    await expect(prompt).toBeVisible()
    expect(await readStorage()).toEqual({ local: null, session: null })
    await expect.poll(() => page.evaluate(() => window.oldSocket.readyState)).toBe(3)
    await expect.poll(() => page.evaluate(() => window.sockets.at(-1).protocols || [])).toEqual([])
    expect(page.errors).toEqual([])
  })
}

test('failed inventory loads have a retry action and empty inventories have setup guidance', async ({ context }) => {
  const page = await openDashboard(context, { nodes: [] })
  await page.getByRole('button', { name: 'Select a node', exact: true }).click()
  await expect(page.getByText('No agents connected', { exact: true })).toBeVisible()
  await expect(page.getByText('Connect an agent to start running diagnostics.')).toBeVisible()
  let fail = true
  await page.route('**/api/nodes', (route) => route.fulfill({
    status: fail ? 503 : 200, contentType: 'application/json', body: fail ? '{}' : JSON.stringify(nodes),
  }))
  await page.getByRole('button', { name: 'Retry', exact: true }).click()
  await expect(page.getByRole('alert')).toContainText('Couldn’t load agents')
  fail = false
  await page.keyboard.press('Escape')
  await page.getByRole('alert').getByRole('button', { name: 'Retry', exact: true }).click()
  await expect(page.getByRole('alert')).not.toBeVisible()
  await expect(page.getByRole('button', { name: /^Selected node: Node A/ })).toBeVisible()
  expect(page.errors).toEqual([])
})

test('mobile tools have labels, touch targets and keyboard dismissal', async ({ context }) => {
  const page = await openDashboard(context)
  await page.setViewportSize({ width: 390, height: 844 })
  const trigger = page.getByRole('button', { name: 'Tools', exact: true })
  await trigger.click()
  const panel = page.locator('#dashboard-tools')
  await expect(panel).toBeVisible()
  for (const button of await panel.getByRole('button').all()) {
    await expect.poll(async () => (await button.boundingBox()).height).toBeGreaterThanOrEqual(44)
  }
  const scan = await new AxeBuilder({ page }).withTags(['wcag2a', 'wcag2aa', 'wcag21a', 'wcag21aa']).analyze()
  expect(scan.violations).toEqual([])
  await page.keyboard.press('Tab')
  await page.keyboard.press('Escape')
  await expect(trigger).toBeFocused()
  await expect(panel).not.toBeVisible()
  await trigger.click()
  await panel.getByRole('button', { name: 'Node health overview', exact: true }).click()
  await expect(page.getByRole('dialog', { name: 'Node health overview' })).toBeVisible()
  expect(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth)).toBe(true)
  expect(page.errors).toEqual([])
})

test('node picker groups status and supports keyboard selection of filtered nodes', async ({ context }) => {
  const fleet = Array.from({ length: 8 }, (_, index) => ({ ...nodes[0], id: `node-${index}`, name: `Agent ${index}`, location: `Site ${index}`, online: index % 2 === 1 }))
  const page = await openDashboard(context, { nodes: fleet })
  await page.getByRole('button', { name: /^Selected node:/ }).click()
  await expect(page.getByText('Online', { exact: true })).toBeVisible()
  await expect(page.getByText('Offline', { exact: true })).toBeVisible()
  const search = page.getByRole('textbox', { name: 'Search nodes' })
  await expect(search).toBeFocused()
  await search.press('ArrowDown')
  await expect(page.getByRole('button', { name: 'Agent 1, Site 1, online', exact: true })).toBeFocused()
  await page.keyboard.press('ArrowDown')
  await expect(page.getByRole('button', { name: 'Agent 3, Site 3, online', exact: true })).toBeFocused()
  await page.keyboard.press('End')
  await expect(page.getByRole('button', { name: 'Agent 6, Site 6, offline', exact: true })).toBeFocused()
  await search.fill('Agent 5')
  await search.press('ArrowDown')
  await page.keyboard.press('Enter')
  await expect(page.getByRole('button', { name: 'Selected node: Agent 5 at Site 5', exact: true })).toBeFocused()
  expect(page.errors).toEqual([])
})
