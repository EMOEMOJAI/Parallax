import { test, expect } from '@playwright/test'
import AxeBuilder from '@axe-core/playwright'
import { openDashboard, chooseCommand } from './fixtures.js'

async function downloadJson(page) {
  const pending = page.waitForEvent('download')
  await page.getByTitle('Download JSON', { exact: true }).click()
  const stream = await (await pending).createReadStream()
  const chunks = []
  for await (const chunk of stream) chunks.push(chunk)
  return JSON.parse(Buffer.concat(chunks).toString())
}

async function start(page) {
  await page.getByLabel('Command target', { exact: true }).fill('example.com')
  await page.getByRole('button', { name: 'Run', exact: true }).first().click()
}

test('clearing during a run retains captured metadata for later output and exports', async ({ context }) => {
  const page = await openDashboard(context)
  await start(page)
  await page.evaluate(() => {
    const id = window.sent.findLast((message) => message.command).command.id
    window.sockets.at(-1).emit({ id, type: 'output', data: 'before clear' })
  })
  await page.getByTitle('Clear output', { exact: true }).click()
  await page.getByRole('button', { name: /^Selected node:/ }).click()
  await page.getByRole('button', { name: 'Node B, Site 2, online', exact: true }).click()
  await page.evaluate(() => {
    const id = window.sent.findLast((message) => message.command).command.id
    window.sockets.at(-1).emit({ id, type: 'output', data: 'after clear' })
    window.sockets.at(-1).emit({ id, type: 'done', data: '' })
  })
  const exported = await downloadJson(page)
  expect(exported.node.name).toBe('Node A')
  expect(exported.command).toBe('ping')
  expect(exported.target).toBe('example.com')
  expect(exported.lines.map((line) => line.text)).toContain('after clear')
  expect(exported.lines.map((line) => line.text)).not.toContain('before clear')
  await expect(page.getByTitle('Share output (creates a 24-hour permalink)', { exact: true })).toBeVisible()
})

test('share preview refuses lines the server would truncate and can recover through redaction', async ({ context }) => {
  const page = await openDashboard(context)
  await start(page)
  await page.evaluate(() => {
    const id = window.sent.findLast((message) => message.command).command.id
    window.sockets.at(-1).emit({ id, type: 'output', data: 'é'.repeat(2050) })
    window.sockets.at(-1).emit({ id, type: 'done', data: '' })
  })
  await page.getByTitle('Share output (creates a 24-hour permalink)', { exact: true }).click()
  await expect(page.getByRole('alert')).toContainText('4 KiB share limit')
  await expect(page.getByRole('button', { name: 'Create share link', exact: true })).toBeDisabled()
  await page.getByLabel('Redact exact text', { exact: true }).fill('é'.repeat(2050))
  await expect(page.getByRole('button', { name: 'Create share link', exact: true })).toBeEnabled()
  await expect(page.getByLabel('Shared content', { exact: true })).toHaveValue(/\[redacted\]/)
})

for (const action of ['Stop', 'Close', 'disconnect']) {
  test(`comparison ${action} preserves output received before its scheduled render`, async ({ context }) => {
    const page = await openDashboard(context)
    await page.getByTitle('Multi-node comparison', { exact: true }).click()
    const dialog = page.getByRole('dialog', { name: 'Multi-Node Comparison' })
    await dialog.getByRole('button', { name: /Node A/ }).click()
    await dialog.getByLabel('Comparison target', { exact: true }).fill('example.com')
    await dialog.getByRole('button', { name: 'Run All', exact: true }).click()
    await page.evaluate((action) => {
      const native = window.requestAnimationFrame
      window.requestAnimationFrame = () => 0
      const id = window.sent.findLast((message) => message.command).command.id
      const socket = window.sockets.at(-1)
      socket.emit({ id, type: 'output', data: 'received before cancellation' })
      if (action === 'disconnect') socket.close()
      else [...document.querySelectorAll('[role="dialog"] button')].find((button) => button.textContent.trim() === action).click()
      window.requestAnimationFrame = native
    }, action)
    if (action === 'Close') {
      await expect(dialog).not.toBeVisible()
      await page.getByTitle('Multi-node comparison', { exact: true }).click()
    }
    await expect(dialog.getByText('received before cancellation', { exact: true })).toBeVisible()
    await dialog.getByRole('button', { name: 'Add comparison to incident', exact: true }).click()
    await page.keyboard.press('Escape')
    await page.getByTestId('investigations').locator(':scope > summary').click()
    await page.getByRole('button', { name: 'Preview incident report', exact: true }).click()
    await expect(page.getByLabel('Incident report preview', { exact: true })).toHaveValue(/received before cancellation/)
    expect(page.errors).toEqual([])
  })
}

test('kit JSON export describes its immutable executed sequence and has no aggregate summary', async ({ context }) => {
  const page = await openDashboard(context)
  await chooseCommand(page, '🧰 diagnostic kit')
  await start(page)
  for (let i = 0; i < 3; i++) {
    await expect.poll(() => page.evaluate(() => window.sent.filter((message) => message.command).length)).toBe(i + 1)
    await page.evaluate(() => {
      const id = window.sent.findLast((message) => message.command).command.id
      window.sockets.at(-1).emit({ id, type: 'summary', data: '{"answer_count":1}' })
      window.sockets.at(-1).emit({ id, type: 'done', data: '' })
    })
  }
  const exported = await downloadJson(page)
  expect(exported.command).toBe('kit')
  expect(JSON.parse(exported.options)).toEqual([{ type: 'ping', options: 'count=5' }, { type: 'traceroute', options: '' }, { type: 'dns', options: 'type=A' }])
  expect(exported.summary).toBeNull()
})

test('enabled Run and Stop meet contrast and expose visible keyboard focus', async ({ context }) => {
  const page = await openDashboard(context)
  await page.getByLabel('Command target', { exact: true }).fill('example.com')
  const run = page.getByRole('button', { name: 'Run', exact: true }).first()
  await page.keyboard.press('Tab')
  await run.focus()
  expect(await run.evaluate((element) => parseFloat(getComputedStyle(element).outlineWidth))).toBeGreaterThanOrEqual(2)
  let scan = await new AxeBuilder({ page }).withTags(['wcag2a', 'wcag2aa', 'wcag21a', 'wcag21aa']).analyze()
  expect(scan.violations).toEqual([])
  await run.click()
  await page.mouse.move(0, 0)
  await page.keyboard.press('Tab')
  const stop = page.getByRole('button', { name: 'Stop', exact: true })
  await stop.focus()
  expect(await stop.evaluate((element) => parseFloat(getComputedStyle(element).outlineWidth))).toBeGreaterThanOrEqual(2)
  scan = await new AxeBuilder({ page }).withTags(['wcag2a', 'wcag2aa', 'wcag21a', 'wcag21aa']).analyze()
  expect(scan.violations).toEqual([])
})
