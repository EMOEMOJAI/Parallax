import { test, expect } from '@playwright/test'
import AxeBuilder from '@axe-core/playwright'
import { openDashboard, nodes } from './fixtures.js'

async function runOutput(page, count = 80, finish = true) {
  await page.getByLabel('Command target', { exact: true }).fill('example.com')
  await page.getByRole('button', { name: 'Run', exact: true }).first().click()
  await page.evaluate(({ count, finish }) => {
    const id = window.sent.findLast((message) => message.command).command.id
    for (let i = 0; i < count; i++) window.sockets.at(-1).emit({ id, type: 'output', data: `example.com synthetic line ${i}` })
    if (finish) window.sockets.at(-1).emit({ id, type: 'done', data: '{"exit_ok":true}' })
  }, { count, finish })
}

test('node choice survives refresh and missing nodes fall back to an available node', async ({ context }) => {
  const page = await openDashboard(context)
  await page.getByRole('button', { name: /^Selected node:/ }).click()
  await page.getByRole('button', { name: 'Node B, Site 2, online', exact: true }).click()
  await page.reload()
  await expect(page.getByRole('button', { name: 'Selected node: Node B at Site 2' })).toBeVisible()
  await page.route('**/api/nodes', (route) => route.fulfill({ json: [nodes[0]] }))
  await page.reload()
  await expect(page.getByRole('button', { name: 'Selected node: Node A at Site 1' })).toBeVisible()
})

test('output search, pause and jump controls preserve streaming text', async ({ context }) => {
  const page = await openDashboard(context)
  await runOutput(page, 80, false)
  const output = page.getByRole('region', { name: 'Diagnostic output' })
  await page.getByRole('button', { name: 'Pause auto-scroll', exact: true }).click()
  await output.evaluate((element) => { element.scrollTop = 0 })
  await page.evaluate(() => {
    const id = window.sent.findLast((message) => message.command).command.id
    window.sockets.at(-1).emit({ id, type: 'output', data: 'new streaming line' })
  })
  await expect(output).toContainText('new streaming line')
  expect(await output.evaluate((element) => element.scrollTop)).toBe(0)
  await page.getByRole('searchbox', { name: 'Search output' }).fill('synthetic line 79')
  await expect(output).toContainText('synthetic line 79')
  await expect(output).not.toContainText('synthetic line 78')
  await page.getByRole('button', { name: 'Jump to latest', exact: true }).click()
  await expect(page.getByRole('searchbox')).toHaveValue('')
  await expect(output).toContainText('synthetic line 78')
  await expect.poll(() => output.evaluate((element) => element.scrollHeight - element.clientHeight - element.scrollTop)).toBeLessThan(40)
})

test('share preview sends only reviewed redacted snapshot and offers manual clipboard fallback', async ({ context }) => {
  const page = await openDashboard(context)
  await page.evaluate(() => { Object.defineProperty(navigator, 'clipboard', { configurable: true, value: { writeText: async () => { throw new Error('denied') } } }) })
  const posts = []
  await page.route('**/api/runs', (route) => { posts.push(route.request().postDataJSON()); return route.fulfill({ json: { id: 'synthetic-run' } }) })
  await runOutput(page, 2, false)
  await page.getByTitle('Copy output', { exact: true }).click()
  await expect(page.getByRole('status').filter({ hasText: 'Couldn’t copy output' })).toBeVisible()
  await page.getByTitle('Share output (creates a 24-hour permalink)', { exact: true }).click()
  const dialog = page.getByRole('dialog', { name: 'Share preview' })
  expect(posts).toEqual([])
  await page.evaluate(() => {
    const id = window.sent.findLast((message) => message.command).command.id
    window.sockets.at(-1).emit({ id, type: 'output', data: 'arrived after preview opened' })
  })
  expect(await page.getByLabel('Shared content', { exact: true }).inputValue()).not.toContain('arrived after preview opened')
  await page.getByLabel('Redact exact text', { exact: true }).fill('example.com\nNode A')
  const reviewed = await page.getByLabel('Shared content', { exact: true }).inputValue()
  expect(reviewed).not.toContain('example.com')
  expect(reviewed).not.toContain('Node A')
  const scan = await new AxeBuilder({ page }).withTags(['wcag2a', 'wcag2aa', 'wcag21a', 'wcag21aa']).analyze()
  expect(scan.violations).toEqual([])
  await page.getByRole('button', { name: 'Create share link', exact: true }).click()
  await expect(page.getByLabel('Share link', { exact: true })).toHaveValue(/run=synthetic-run$/)
  expect(posts).toHaveLength(1)
  expect(posts[0].target).toBe('[redacted]')
  expect(posts[0].node_name).toBe('[redacted]')
  expect(posts[0].lines.map((line) => line.text).join('\n')).toBe(reviewed.split('\nOutput\n')[1])
  expect(JSON.stringify(posts[0])).not.toContain('arrived after preview opened')
  await expect(dialog.getByRole('alert')).toContainText('copy it manually')
  await page.keyboard.press('Escape')
  await expect(dialog).not.toBeVisible()
  await expect(page.getByTitle('Share output (creates a 24-hour permalink)', { exact: true })).toBeFocused()
  expect(page.errors).toEqual([])
})

test('JSON download retains source metadata and mobile output actions are touch sized', async ({ context }) => {
  const page = await openDashboard(context)
  await runOutput(page, 2)
  await page.getByRole('button', { name: /^Selected node:/ }).click()
  await page.getByRole('button', { name: 'Node B, Site 2, online', exact: true }).click()
  const downloaded = page.waitForEvent('download')
  await page.getByTitle('Download JSON', { exact: true }).click()
  const download = await downloaded
  const stream = await download.createReadStream()
  const chunks = []
  for await (const chunk of stream) chunks.push(chunk)
  const exported = JSON.parse(Buffer.concat(chunks).toString())
  expect(exported.node.name).toBe('Node A')
  expect(exported.target).toBe('example.com')
  expect(exported.started_at).toMatch(/^\d{4}-/)
  expect(exported.lines.every((line) => !('_id' in line))).toBe(true)
  await page.setViewportSize({ width: 390, height: 844 })
  for (const title of ['Copy output', 'Download output', 'Download JSON', 'Clear output', 'Share output (creates a 24-hour permalink)']) {
    const box = await page.getByTitle(title, { exact: true }).boundingBox()
    expect(box.width).toBeGreaterThanOrEqual(44)
    expect(box.height).toBeGreaterThanOrEqual(44)
  }
  expect(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth)).toBe(true)
})

test('comparison CSV keeps the executed command when controls change', async ({ context }) => {
  const page = await openDashboard(context)
  await page.getByTitle('Multi-node comparison', { exact: true }).click()
  const dialog = page.getByRole('dialog', { name: 'Multi-Node Comparison' })
  await dialog.getByRole('button', { name: /Node A/ }).click()
  await page.getByLabel('Comparison target', { exact: true }).fill('example.com')
  await dialog.getByRole('button', { name: 'Run All', exact: true }).click()
  await page.evaluate(() => {
    const id = window.sent.findLast((message) => message.command).command.id
    window.sockets.at(-1).emit({ id, type: 'output', data: 'original output' })
    window.sockets.at(-1).emit({ id, type: 'done', data: '{"exit_ok":true}' })
  })
  await expect(dialog.getByText('original output')).toBeVisible()
  await page.getByLabel('Comparison target', { exact: true }).fill('changed.example')
  const downloaded = page.waitForEvent('download')
  await dialog.getByRole('button', { name: 'Download CSV', exact: true }).click()
  const stream = await (await downloaded).createReadStream()
  const chunks = []
  for await (const chunk of stream) chunks.push(chunk)
  const csv = Buffer.concat(chunks).toString()
  expect(csv).toContain('example.com')
  expect(csv).toContain('original output')
  expect(csv).not.toContain('changed.example')
})

test('clipboard failure for node addresses is visible', async ({ context }) => {
  const page = await openDashboard(context)
  await page.evaluate(() => { Object.defineProperty(navigator, 'clipboard', { configurable: true, value: undefined }) })
  await page.getByRole('button', { name: 'Copy IPv4 address', exact: true }).click()
  await expect(page.getByRole('status').filter({ hasText: 'Couldn’t copy. Select the address' })).toBeVisible()
  expect(page.errors).toEqual([])
})

test('expired authentication closes the preview and releases the dashboard', async ({ context }) => {
  const page = await openDashboard(context)
  await runOutput(page, 1)
  await page.getByTitle('Share output (creates a 24-hour permalink)', { exact: true }).click()
  await page.route('**/api/runs', (route) => route.fulfill({ status: 401, body: 'unauthorized' }))
  await page.getByRole('button', { name: 'Create share link', exact: true }).click()
  await expect(page.getByRole('dialog', { name: 'Share preview' })).not.toBeVisible()
  await expect(page.getByRole('dialog', { name: 'API key required' })).toBeVisible()
  expect(await page.locator('#root').evaluate((element) => element.inert)).toBe(false)
  await expect(page.getByLabel('Client API key', { exact: true })).toBeFocused()
})
