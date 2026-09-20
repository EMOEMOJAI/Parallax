import { test } from '@playwright/test'
import assert from 'node:assert/strict'
import AxeBuilder from '@axe-core/playwright'
import { openDashboard, nodes, chooseCommand } from './fixtures.js'

test('failed command is displayed as failed', async ({ context }) => {
  const p = await openDashboard(context)
  await p.getByLabel('Command target', { exact: true }).fill('example.com')
  await p.getByRole('button', { name: 'Run', exact: true }).first().click()
  await p.evaluate(() => {
    const id = window.sent.find((x) => x.command).command.id
    window.sockets
      .at(-1)
      .emit({
        id,
        type: 'error',
        data: 'Command exited with error: exit status 1',
      })
    window.sockets
      .at(-1)
      .emit({ id, type: 'done', data: JSON.stringify({ exit_ok: false }) })
  })
  await p.getByText('✗ Command failed.', { exact: true }).waitFor()
})

test('schedule double-click submits only one mutation', async ({ context }) => {
  const p = await openDashboard(context)
  const pending = []
  await p.route('**/api/schedules', async (r) => {
    if (r.request().method() === 'POST') {
      pending.push(r)
      return
    }
    await r.fulfill({
      status: 200,
      contentType: 'application/json',
      body: '[]',
    })
  })
  await p.getByTitle('Scheduled probes', { exact: true }).click()
  await p.getByRole('button', { name: 'New schedule', exact: true }).click()
  await p.getByLabel('Target', { exact: true }).fill('example.com')
  await p.getByRole('button', { name: 'Create', exact: true }).dblclick()
  await p.waitForTimeout(100)
  assert.equal(pending.length, 1)
  for (const r of pending)
    await r.fulfill({
      status: 201,
      contentType: 'application/json',
      body: '{}',
    })
})

test('stale schedule response cannot resurrect a deleted row', async ({
  context,
}) => {
  const p = await openDashboard(context)
  let gets = 0,
    stale
  const item = {
    id: 'sc',
    node_id: 'a',
    node_name: 'Node A',
    command: 'ping',
    target: 'example.com',
    interval_sec: 300,
    enabled: true,
    last_status: 'ok',
  }
  await p.route('**/api/schedules', async (r) => {
    gets++
    if (gets === 2) {
      stale = r
      return
    }
    await r.fulfill({
      status: 200,
      contentType: 'application/json',
      body: JSON.stringify(gets === 1 ? [item] : []),
    })
  })
  await p.route('**/api/schedules/sc', (r) =>
    r.fulfill({ status: 200, body: '{}' }),
  )
  await p.getByTitle('Scheduled probes', { exact: true }).click()
  await p.getByRole('button', { name: 'Delete', exact: true }).waitFor()
  await p.getByLabel('Refresh', { exact: true }).click()
  await p.getByRole('button', { name: 'Delete', exact: true }).click()
  await p
    .getByText(
      'No schedules yet. Create one above to start probing on a timer.',
      { exact: true },
    )
    .waitFor()
  await stale.fulfill({
    status: 200,
    contentType: 'application/json',
    body: JSON.stringify([item]),
  })
  await p.waitForTimeout(200)
  assert.equal(
    await p.getByRole('button', { name: 'Delete', exact: true }).count(),
    0,
  )
})

test('terminal retains the source node when selection changes', async ({
  context,
}) => {
  const p = await openDashboard(context)
  await p.getByLabel('Command target', { exact: true }).fill('example.com')
  await p.getByRole('button', { name: 'Run', exact: true }).first().click()
  await p.evaluate(() => {
    const id = window.sent.find((x) => x.command).command.id
    window.sockets
      .at(-1)
      .emit({ id, type: 'output', data: 'OUTPUT FROM NODE A' })
    window.sockets
      .at(-1)
      .emit({ id, type: 'done', data: JSON.stringify({ exit_ok: true }) })
  })
  await p.getByLabel('Selected node: Node A at Site 1').click()
  await p.getByRole('button', { name: /Site 2 Node B/ }).click()
  await p.getByText('OUTPUT FROM NODE A', { exact: true }).waitFor()
  assert.equal(
    await p
      .locator('main span.font-mono')
      .filter({ hasText: 'Node A' })
      .count(),
    1,
  )
})

test('comparison allows long probes and cancels at the ten-minute limit', async ({
  context,
}) => {
  const p = await openDashboard(context)
  await p.getByTitle('Multi-node comparison', { exact: true }).click()
  const dialog = p.getByRole('dialog', { name: 'Multi-Node Comparison' })
  await dialog.getByRole('button', { name: /Node A/ }).click()
  await p.getByLabel('Comparison target', { exact: true }).fill('example.com')
  await dialog.getByLabel('Packets', { exact: true }).fill('100')
  await p.clock.install()
  await dialog.getByRole('button', { name: 'Run All', exact: true }).click()
  await p.evaluate(() => {
    const id = window.sent.find((x) => x.command).command.id
    window.sockets
      .at(-1)
      .emit({
        id,
        type: 'output',
        data: '64 bytes from destination: seq=60 time=2ms',
      })
  })
  await p.clock.fastForward(66000)
  assert.equal(
    await p.evaluate(() => window.sent.some((x) => x.action === 'cancel')),
    false,
  )
  await p.clock.fastForward(600000)
  assert.ok(
    await p.evaluate(() => window.sent.some((x) => x.action === 'cancel')),
  )
  await p
    .getByText('✗ Timed out — command exceeded the 10-minute limit', {
      exact: true,
    })
    .waitFor()
})

test('preset removal becomes visible on keyboard focus', async ({
  context,
}) => {
  const p = await openDashboard(context)
  await p.getByLabel('Command target', { exact: true }).fill('example.com')
  await p.getByLabel('Saved presets', { exact: true }).click()
  await p.getByRole('button', { name: '+ Save current', exact: true }).click()
  await p.mouse.move(0, 0)
  const button = p.getByRole('button', {
    name: 'Remove preset ping example.com',
    exact: true,
  })
  await p.keyboard.press('Tab')
  await button.focus()
  await p.waitForTimeout(200)
  const state = await button.evaluate((el) => ({
    active: el === document.activeElement,
    opacity: getComputedStyle(el).opacity,
  }))
  assert.deepEqual(state, { active: true, opacity: '1' })
})

test('saved traceroute replay restores the mapped route', async ({
  context,
}) => {
  const p = await openDashboard(context, { delayedReplay: true })
  await p.route('**/api/geoip/**', async (r) => {
    const ips = JSON.parse(r.request().postData()).ips
    await r.fulfill({
      status: 200,
      contentType: 'application/json',
      body: JSON.stringify(
        ips.map((query, i) => ({
          query,
          status: 'success',
          lat: 10 + i,
          lon: 20 + i,
        })),
      ),
    })
  })
  await p.replayRoute.fulfill({
    status: 200,
    contentType: 'application/json',
    body: JSON.stringify({
      command: 'traceroute',
      target: 'example.com',
      node_name: 'Saved source',
      node_location: 'Old site',
      created_at: new Date().toISOString(),
      lines: [
        { type: 'output', text: ' 1  1.1.1.1  2 ms' },
        { type: 'output', text: ' 2  8.8.8.8  3 ms' },
      ],
    }),
  })
  await p.getByText('1  1.1.1.1  2 ms', { exact: false }).waitFor()
  await p.getByTitle('Network map', { exact: true }).click()
  await p.getByRole('dialog', { name: 'Network Map', exact: true }).waitFor()
  await p.waitForTimeout(400)
  await p.getByText('2 hops mapped', { exact: true }).waitFor()
  assert.equal(await p.getByText('Route Hops', { exact: true }).count(), 1)
})

test('stale health snapshot cannot overwrite a newer refresh', async ({
  context,
}) => {
  const p = await openDashboard(context)
  const pending = []
  await p.route('**/api/nodes/health', (r) => pending.push(r))
  await p.getByTitle('Node health overview', { exact: true }).click()
  while (pending.length < 1) await p.waitForTimeout(10)
  await p.getByLabel('Refresh node health', { exact: true }).click()
  while (pending.length < 2) await p.waitForTimeout(10)
  await pending[1].fulfill({
    status: 200,
    contentType: 'application/json',
    body: JSON.stringify([{ ...nodes[0], name: 'NEW HEALTH' }]),
  })
  await p.getByText('NEW HEALTH', { exact: true }).waitFor()
  await pending[0].fulfill({
    status: 200,
    contentType: 'application/json',
    body: JSON.stringify([{ ...nodes[0], name: 'OLD HEALTH' }]),
  })
  await p.waitForTimeout(100)
  assert.equal(await p.getByText('OLD HEALTH', { exact: true }).count(), 0)
  await p.getByText('NEW HEALTH', { exact: true }).waitFor()
})

test('matrix reopen rejects the previous open snapshot', async ({
  context,
}) => {
  const p = await openDashboard(context)
  const pending = []
  await p.route('**/api/latency-matrix', (r) => pending.push(r))
  await p.getByTitle('Latency matrix', { exact: true }).click()
  while (pending.length < 1) await p.waitForTimeout(10)
  await p
    .getByRole('dialog', { name: 'Latency Matrix' })
    .getByRole('button', { name: 'Close', exact: true })
    .click()
  await p.waitForTimeout(200)
  await p.getByTitle('Latency matrix', { exact: true }).click()
  while (pending.length < 2) await p.waitForTimeout(10)
  await pending[1].fulfill({
    status: 200,
    contentType: 'application/json',
    body: JSON.stringify({
      nodes: [{ ...nodes[0], name: 'NEW MATRIX' }, nodes[1]],
      latency: {},
    }),
  })
  await p.getByText('NEW MATRIX', { exact: false }).first().waitFor()
  await pending[0].fulfill({
    status: 200,
    contentType: 'application/json',
    body: JSON.stringify({
      nodes: [{ ...nodes[0], name: 'OLD MATRIX' }, nodes[1]],
      latency: {},
    }),
  })
  await p.waitForTimeout(150)
  assert.equal(await p.getByText('OLD MATRIX', { exact: false }).count(), 0)
  await p.getByText('NEW MATRIX', { exact: false }).first().waitFor()
})

test('resubmitting the same key reconnects and refreshes HTTP data', async ({
  context,
}) => {
  const p = await openDashboard(context, { savedKey: 'same-key' })
  const beforeRequests = p.apiHeaders.length
  const before = await p.evaluate(() => {
    const ws = window.sockets.at(-1)
    ws.readyState = 3
    ws.onclose({ code: 4401 })
    return window.sockets.length
  })
  await p.getByLabel('Client API key', { exact: true }).fill('same-key')
  await p.getByRole('button', { name: 'Connect', exact: true }).click()
  await p.waitForTimeout(200)
  assert.ok((await p.evaluate(() => window.sockets.length)) > before)
  assert.ok(p.apiHeaders.length > beforeRequests)
  assert.equal(await p.getByText('Live', { exact: true }).count(), 1)
  assert.equal(
    await p.getByRole('dialog', { name: 'API key required' }).count(),
    0,
  )
})

test('diagnostic kit records failed step and aggregate failure', async ({
  context,
}) => {
  const p = await openDashboard(context)
  await chooseCommand(p, '🧰 diagnostic kit')
  await p.getByLabel('Command target', { exact: true }).fill('example.com')
  await p.getByRole('button', { name: 'Run', exact: true }).first().click()
  for (let n = 0; n < 3; n++) {
    await p.waitForFunction(
      (n) => window.sent.filter((x) => x.command).length === n + 1,
      n,
    )
    await p.evaluate((n) => {
      const id = window.sent.filter((x) => x.command)[n].command.id
      window.sockets
        .at(-1)
        .emit({ id, type: 'done', data: JSON.stringify({ exit_ok: n !== 0 }) })
    }, n)
  }
  await p.getByText('✗ Step failed.', { exact: true }).waitFor()
  await p
    .getByText('✗ Diagnostic kit finished with errors.', { exact: true })
    .waitFor()
})

test('comparison displays explicit unsuccessful completion', async ({
  context,
}) => {
  const p = await openDashboard(context)
  await p.getByTitle('Multi-node comparison', { exact: true }).click()
  const d = p.getByRole('dialog', { name: 'Multi-Node Comparison' })
  await d.getByRole('button', { name: /Node A/ }).click()
  await p.getByLabel('Comparison target', { exact: true }).fill('example.com')
  await d.getByRole('button', { name: 'Run All', exact: true }).click()
  await p.evaluate(() => {
    const id = window.sent.find((x) => x.command).command.id
    window.sockets.at(-1).emit({ id, type: 'done', data: '{"exit_ok":false}' })
  })
  await p.getByText('✗ Failed', { exact: true }).waitFor()
  assert.equal(await p.getByText('✓ Done', { exact: true }).count(), 0)
})

for (const screen of [
  'dashboard',
  'Network map',
  'Multi-node comparison',
  'Node health overview',
  'Latency matrix',
  'Scheduled probes',
  'API key required',
  'mobile dashboard',
])
  test('production a11y and runtime smoke: ' + screen, async ({ context }) => {
    const p = await openDashboard(context)
    if (screen === 'mobile dashboard')
      await p.setViewportSize({ width: 390, height: 844 })
    else if (screen === 'API key required')
      await p.evaluate(() => {
        const ws = window.sockets.at(-1)
        ws.readyState = 3
        ws.onclose({ code: 4401 })
      })
    else if (screen !== 'dashboard')
      await p.getByTitle(screen, { exact: true }).click()
    await p.waitForTimeout(350)
    const scan = await new AxeBuilder({ page: p })
      .withTags(['wcag2a', 'wcag2aa', 'wcag21a', 'wcag21aa'])
      .analyze()
    assert.deepEqual(
      scan.violations.map((v) => ({
        id: v.id,
        nodes: v.nodes.map((n) => n.target),
      })),
      [],
    )
    assert.deepEqual(p.errors, [])
    if (screen === 'mobile dashboard')
      assert.ok(
        await p.evaluate(
          () => document.documentElement.scrollWidth <= window.innerWidth,
        ),
      )
  })
