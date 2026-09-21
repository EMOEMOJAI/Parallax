import { test, expect } from '@playwright/test'
import { openDashboard, chooseCommand } from './fixtures.js'

const surfaces = [
  ['Node health overview', 'Node Health Overview', 'Close'],
  ['Latency matrix', 'Latency Matrix', 'Close'],
  ['Multi-node comparison', 'Multi-Node Comparison', 'Close'],
  ['Scheduled probes', 'Scheduled probes', 'Close'],
  ['Network map', 'Network Map', 'Close network map'],
]

for (const [title, name, close] of surfaces) {
  test(`modal exits release focus while retaining the visual surface: ${name}`, async ({ context }) => {
    const page = await openDashboard(context)
    // A nondefault duration catches stale JS constants and seconds parsing.
    await page.addStyleTag({ content: ':root { --modal-close-dur: 0.6s; }' })
    const trigger = page.getByTitle(title, { exact: true })
    await trigger.click()
    const dialog = page.locator(`.t-modal[aria-label="${name}"]`)
    await expect(dialog).toHaveClass(/is-open/)
    await dialog.getByRole('button', { name: close, exact: true }).click()
    await expect(dialog).toHaveAttribute('inert', '')
    await expect(page.getByRole('dialog', { name, exact: true })).toHaveCount(0)
    await expect(trigger).toBeFocused()
    await page.waitForTimeout(220)
    await expect(dialog).toHaveCount(1)
    await expect(dialog).toHaveCount(0, { timeout: 2000 })
    expect(page.errors).toEqual([])
  })
}

test('dropdown reopening cancels stale cleanup and outside clicks retain focus', async ({ context }) => {
  const page = await openDashboard(context)
  await page.addStyleTag({ content: ':root { --dropdown-close-dur: 600ms; }' })
  const trigger = page.getByRole('button', { name: 'Saved presets', exact: true })
  await trigger.click()
  const menu = page.locator('.t-dropdown')
  await expect(menu).toHaveClass(/is-open/)
  await menu.getByTitle('Restore the default kit (ping, traceroute, dns type=A)').focus()
  await page.keyboard.press('Escape')
  await expect(trigger).toBeFocused()
  await expect(menu).toHaveAttribute('inert', '')
  await page.keyboard.press('Enter')
  await expect(menu).toHaveClass(/is-open/)
  await page.waitForTimeout(700)
  await expect(menu).toBeVisible()
  await expect(menu).not.toHaveAttribute('inert')
  await page.getByLabel('Command target', { exact: true }).click()
  await expect(page.getByLabel('Command target', { exact: true })).toBeFocused()
  await expect(menu).toHaveCount(0)
  expect(page.errors).toEqual([])
})

test('a slow lazy map chunk still starts with an entrance', async ({ context }) => {
  const page = await openDashboard(context)
  await page.route('**/assets/GeoMap-*.js', async (route) => {
    await new Promise((resolve) => setTimeout(resolve, 400))
    await route.continue()
  })
  await page.evaluate(() => {
    window.firstMapState = null
    const observer = new MutationObserver(() => {
      const map = document.querySelector('.t-modal[aria-label="Network Map"]')
      if (!map) return
      window.firstMapState = { phase: map.dataset.state, opacity: getComputedStyle(map).opacity }
      observer.disconnect()
    })
    observer.observe(document.body, { childList: true, subtree: true })
  })
  await page.getByTitle('Network map', { exact: true }).click()
  await expect(page.locator('.t-modal')).toHaveClass(/is-open/)
  expect(await page.evaluate(() => window.firstMapState)).toEqual({ phase: 'enter', opacity: '0' })
  expect(page.errors).toEqual([])
})

test('command popovers use anchored entrances and dismiss cleanly', async ({ context }) => {
  const page = await openDashboard(context)
  await page.getByLabel('Command target', { exact: true }).fill('example.com')
  await page.getByRole('button', { name: 'Run', exact: true }).first().click()
  await page.evaluate(() => {
    const id = window.sent.find((event) => event.command).command.id
    window.sockets.at(-1).emit({ id, type: 'done', data: '{"exit_ok":true}' })
  })
  const triggers = [
    page.getByRole('button', { name: /^Selected node:/ }),
    page.getByRole('button', { name: /^Command type:/ }),
    page.getByRole('button', { name: 'Saved presets', exact: true }),
    page.getByTitle('Command history (↑/↓ in input)', { exact: true }),
    page.getByLabel('ping options', { exact: true }),
    page.getByLabel('IP version', { exact: true }),
  ]
  for (const trigger of triggers) {
    await trigger.click()
    const menu = page.locator('.t-dropdown')
    await expect(menu).toHaveClass(/is-open/)
    await expect(menu).toHaveAttribute('data-origin', /top-(left|right)/)
    await page.keyboard.press('Escape')
    await expect(menu).toHaveCount(0)
  }
  await chooseCommand(page, '🔍 dns lookup')
  await expect(page.locator('.t-dropdown')).toHaveCount(0)
  await page.getByLabel('DNS record type', { exact: true }).click()
  await expect(page.locator('.t-dropdown')).toHaveClass(/is-open/)
  await page.keyboard.press('Escape')
  await expect(page.locator('.t-dropdown')).toHaveCount(0)
  expect(page.errors).toEqual([])
})

test('reduced motion skips waits and reacts during an exit', async ({ context }) => {
  const page = await openDashboard(context)
  await page.addStyleTag({ content: ':root { --modal-close-dur: 5s; --dropdown-close-dur: 5s; }' })
  await page.getByTitle('Node health overview', { exact: true }).click()
  await expect(page.locator('.t-modal')).toHaveClass(/is-open/)
  await page.getByRole('dialog').getByRole('button', { name: 'Close', exact: true }).click()
  await expect(page.locator('.t-modal')).toHaveAttribute('inert', '')
  await page.emulateMedia({ reducedMotion: 'reduce' })
  await expect(page.locator('.t-modal')).toHaveCount(0, { timeout: 500 })
  await page.getByRole('button', { name: 'Saved presets', exact: true }).click()
  await expect(page.locator('.t-dropdown')).toHaveClass(/is-open/)
  await expect(page.locator('.t-dropdown')).toHaveCSS('transition-duration', '0s')
  await page.keyboard.press('Escape')
  await expect(page.locator('.t-dropdown')).toHaveCount(0, { timeout: 500 })
  expect(await page.evaluate(() => document.getAnimations().filter((animation) => animation.playState === 'running').length)).toBe(0)
  await page.emulateMedia({ reducedMotion: 'no-preference' })
  await page.getByTitle('Node health overview', { exact: true }).click()
  await expect(page.locator('.t-modal')).toHaveClass(/is-open/)
  expect(page.errors).toEqual([])
})

test('clipboard icons and share notices retain the latest feedback', async ({ context }) => {
  await context.grantPermissions(['clipboard-read', 'clipboard-write'])
  const page = await openDashboard(context)
  await page.route('**/api/runs', (route) => route.fulfill({ json: { id: 'example-run' } }))
  await page.getByLabel('Command target', { exact: true }).fill('example.com')
  await page.getByRole('button', { name: 'Run', exact: true }).first().click()
  await page.evaluate(() => {
    const id = window.sent.find((event) => event.command).command.id
    window.sockets.at(-1).emit({ id, type: 'output', data: 'Synthetic test output' })
    window.sockets.at(-1).emit({ id, type: 'done', data: '{"exit_ok":true}' })
  })
  const copy = page.getByTitle('Copy output', { exact: true })
  await copy.click()
  await expect(copy.locator('.t-icon-swap')).toHaveAttribute('data-state', 'b')
  await page.waitForTimeout(1200)
  await copy.click()
  await page.waitForTimeout(1000)
  await expect(copy.locator('.t-icon-swap')).toHaveAttribute('data-state', 'b')
  const share = page.getByTitle('Share output (creates a 24-hour permalink)', { exact: true })
  await share.click()
  await page.getByRole('button', { name: 'Create share link', exact: true }).click()
  await page.getByLabel('Share link', { exact: true }).waitFor()
  await page.getByRole('button', { name: 'Close preview', exact: true }).click()
  await expect(page.getByRole('status')).toHaveText('Link copied to clipboard')
  await expect(page.locator('.t-toast')).toHaveClass(/is-open/)
  await page.waitForTimeout(2200)
  await share.click()
  await page.getByRole('button', { name: 'Create share link', exact: true }).click()
  await page.getByLabel('Share link', { exact: true }).waitFor()
  await page.getByRole('button', { name: 'Close preview', exact: true }).click()
  await page.waitForTimeout(2100)
  await expect(page.locator('.t-toast')).toHaveClass(/is-open/)
  await expect(page.locator('.t-toast')).toHaveCount(0, { timeout: 3000 })
  expect(page.errors).toEqual([])
})

test('terminal resizing fits the final box without restarting its session', async ({ context }) => {
  const page = await openDashboard(context)
  await chooseCommand(page, '💻 shell')
  await page.getByRole('button', { name: 'Run', exact: true }).first().click()
  const dialog = page.getByRole('dialog', { name: 'Shell on Node A', exact: true })
  await expect(dialog).toBeVisible()
  await page.waitForFunction(() => window.sent.some((event) => event.action === 'shell_start'))
  const initial = await page.evaluate(() => window.sent.find((event) => event.action === 'shell_start'))
  await dialog.getByRole('button', { name: 'Maximize terminal', exact: true }).click()
  await expect(dialog).toHaveCSS('width', `${page.viewportSize().width}px`)
  await page.waitForFunction((cols) => window.sent.some((event) => event.input?.action === 'resize' && event.input.cols > cols), initial.cols)
  await page.emulateMedia({ reducedMotion: 'reduce' })
  await dialog.getByRole('button', { name: 'Restore terminal size', exact: true }).click()
  await page.waitForFunction((cols) => window.sent.filter((event) => event.input?.action === 'resize').at(-1)?.input.cols === cols, initial.cols)
  expect(await page.evaluate(() => window.sent.filter((event) => event.action === 'shell_start').length)).toBe(1)
  await dialog.getByRole('button', { name: 'Close shell', exact: true }).click()
  await expect(page.getByRole('dialog', { name: 'Shell on Node A', exact: true })).toHaveCount(0)
  expect(page.errors).toEqual([])
})
