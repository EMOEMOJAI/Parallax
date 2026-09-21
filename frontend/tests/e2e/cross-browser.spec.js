import { test, expect } from '@playwright/test'
import { openDashboard, chooseCommand } from './fixtures.js'

// Keep the native browser WebSocket constructor. Playwright supplies a local,
// deterministic peer; no production deployment or external network is used.
test('login, native socket output and sign-out work across engines', async ({ context }) => {
  let sockets = 0
  await context.routeWebSocket('**/ws/client', (socket) => {
    sockets++
    socket.onMessage((raw) => {
      const message = JSON.parse(raw)
      if (!message.command) return
      const id = message.command.id
      socket.send(JSON.stringify({ id, type: 'output', data: 'Synthetic streamed output: 日本語 <script>text only</script>' }))
      socket.send(JSON.stringify({ id, type: 'summary', data: '{"avg_ms":2,"loss_pct":0}' }))
      socket.send(JSON.stringify({ id, type: 'done', data: '{"exit_ok":true}' }))
    })
  })
  const page = await openDashboard(context, { nativeSocket: true, authRequired: true })
  await page.getByLabel('Client API key', { exact: true }).fill('fixture-client-key')
  await page.getByRole('button', { name: 'Connect', exact: true }).click()
  await expect(page.getByRole('dialog', { name: 'API key required' })).not.toBeVisible()
  await page.getByLabel('Command target', { exact: true }).fill('example.com')
  await page.getByRole('button', { name: 'Run', exact: true }).first().click()
  const output = page.getByRole('region', { name: 'Diagnostic output' })
  await expect(output).toContainText('日本語 <script>text only</script>')
  await expect(output).toContainText('Command completed.')
  expect(await output.locator('script').count()).toBe(0)
  expect(sockets).toBeGreaterThan(0)
  await page.getByRole('button', { name: 'Sign out', exact: true }).click()
  await expect(page.getByRole('dialog', { name: 'API key required' })).toBeVisible()
  expect(page.errors).toEqual([])
})

test('dialogs, keyboard focus and PTY output work across engines', async ({ context }) => {
  const page = await openDashboard(context)
  const trigger = page.getByTitle('Scheduled probes', { exact: true })
  await trigger.focus()
  await trigger.press('Enter')
  await expect(page.getByRole('dialog', { name: 'Scheduled probes', exact: true })).toBeVisible()
  await page.keyboard.press('Escape')
  await expect(trigger).toBeFocused()
  await chooseCommand(page, '💻 shell')
  await page.getByRole('button', { name: 'Run', exact: true }).first().click()
  const dialog = page.getByRole('dialog', { name: 'Shell on Node A', exact: true })
  await expect(dialog).toBeVisible()
  await expect.poll(() => page.evaluate(() => window.sent.some((m) => m.action === 'shell_start'))).toBe(true)
  await page.evaluate(() => {
    const id = window.sent.find((m) => m.action === 'shell_start').id
    window.sockets.at(-1).emit({ id, type: 'shell_output', data: 'synthetic-terminal-output\r\n' })
  })
  await expect(dialog.locator('.xterm-rows')).toContainText('synthetic-terminal-output')
  await dialog.getByRole('button', { name: 'Close shell', exact: true }).click()
  await expect(dialog).not.toBeVisible()
  expect(page.errors).toEqual([])
})

test('history deletion is synchronized between browser tabs', async ({ context }) => {
  const first = await openDashboard(context)
  await first.evaluate(() => localStorage.setItem('lg-cmd-history', JSON.stringify([{ type: 'ping', target: 'old.example' }])))
  const second = await openDashboard(context)
  const input = second.getByLabel('Command target', { exact: true })
  await input.press('ArrowUp')
  await expect(input).toHaveValue('old.example')
  await second.getByTitle('Command history (↑/↓ in input)', { exact: true }).click()
  await expect(second.getByText('Recent Commands', { exact: true })).toBeVisible()
  await first.evaluate(() => localStorage.removeItem('lg-cmd-history'))
  await expect(second.getByText('Recent Commands', { exact: true })).not.toBeVisible()
  await input.fill('new.example')
  await second.getByRole('button', { name: 'Run', exact: true }).first().click()
  expect(await second.evaluate(() => JSON.parse(localStorage.getItem('lg-cmd-history')).map((entry) => entry.target))).toEqual(['new.example'])
  expect(second.errors).toEqual([])
})
