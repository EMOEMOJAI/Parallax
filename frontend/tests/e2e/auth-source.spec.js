import { test } from '@playwright/test'
import assert from 'node:assert/strict'
import { openDashboard } from './fixtures.js'

test('reserved-marker API key does not crash the native WebSocket constructor', async ({
  context,
}) => {
  const p = await openDashboard(context)
  const error = await p.evaluate(async () => {
    const { openAuthedSocket } = await import('/src/lib/api.js')
    const mock = window.WebSocket
    window.WebSocket = window.NativeWebSocket
    try {
      openAuthedSocket('/ws/client', 'lg.bearer')
      return null
    } catch (e) {
      return e.message
    } finally {
      window.WebSocket = mock
    }
  })
  assert.equal(error, null)
})

test('stale credential rejection does not reopen the authentication prompt', async ({
  context,
}) => {
  const p = await openDashboard(context)
  await p.route('**/api/stale-auth', (r) => {
    p.pendingAuth = r
  })
  await p.evaluate(async () => {
    const api = await import('/src/lib/api.js')
    api.setApiKey('old')
    window.staleAuth = api.apiFetch('/api/stale-auth')
    api.setApiKey('new')
  })
  while (!p.pendingAuth) await p.waitForTimeout(10)
  await p.pendingAuth.fulfill({ status: 401, body: 'unauthorized' })
  await p.waitForTimeout(200)
  assert.equal(
    await p.getByRole('dialog', { name: 'API key required' }).count(),
    0,
  )
})
