import test from 'node:test'
import assert from 'node:assert/strict'

let nextModule = 0
const freshApi = () => import(`./api.js?test=${++nextModule}`)

for (const readable of [false, true]) {
  test(`saved API key authenticates requests when storage ${readable ? 'is read-only' : 'is blocked'}`, async (t) => {
    t.mock.method(globalThis, 'fetch', async (_path, init) => ({ status: 200, authorization: init.headers.get('Authorization') }))
    const previous = Object.getOwnPropertyDescriptor(globalThis, 'localStorage')
    Object.defineProperty(globalThis, 'localStorage', { configurable: true, value: {
      getItem() { if (!readable) throw new Error('blocked'); return 'old-key' },
      setItem() { throw new Error('blocked') },
      removeItem() { throw new Error('blocked') },
    } })
    t.after(() => {
      if (previous) Object.defineProperty(globalThis, 'localStorage', previous)
      else delete globalThis.localStorage
    })
    const api = await freshApi()
    api.setApiKey('new-key')
    assert.equal((await api.apiFetch('/api/nodes')).authorization, 'Bearer new-key')
    api.clearApiKey()
    assert.equal((await api.apiFetch('/api/nodes')).authorization, null)
  })
}

test('token-safe socket credentials stay out of the URL', async () => {
  const api = await freshApi()
  assert.deepEqual(api.wsAuth('test-key'), { protocols: ['lg.bearer', 'test-key'], query: '' })
  assert.deepEqual(api.wsAuth('key with spaces'), { protocols: [], query: '?key=key%20with%20spaces' })
})

test('reserved bearer marker uses the compatible query transport', async () => {
  const api = await freshApi()
  assert.deepEqual(api.wsAuth('lg.bearer'), { protocols: [], query: '?key=lg.bearer' })
})

test('stale authentication failures cannot reject a new credential', async (t) => {
  const previous = Object.getOwnPropertyDescriptor(globalThis, 'localStorage')
  const stored = new Map()
  Object.defineProperty(globalThis, 'localStorage', { configurable: true, value: {
    getItem: (key) => stored.get(key) || null,
    setItem: (key, value) => stored.set(key, value),
    removeItem: (key) => stored.delete(key),
  } })
  t.after(() => {
    if (previous) Object.defineProperty(globalThis, 'localStorage', previous)
    else delete globalThis.localStorage
  })
  const api = await freshApi()
  const events = []
  const oldWindow = globalThis.window
  globalThis.window = { dispatchEvent: (event) => events.push(event.type) }
  t.after(() => { globalThis.window = oldWindow })
  const responses = []
  t.mock.method(globalThis, 'fetch', () => new Promise((resolve) => responses.push(resolve)))
  api.setApiKey('old')
  const stale = api.apiFetch('/api/nodes')
  api.setApiKey('new')
  responses.shift()({ status: 401 })
  await stale
  assert.deepEqual(events, [])
  // Even changing away and back invalidates an earlier in-flight request.
  const earlier = api.apiFetch('/api/nodes')
  api.clearApiKey()
  api.setApiKey('new')
  responses.shift()({ status: 401 })
  await earlier
  assert.deepEqual(events, [])
  const current = api.apiFetch('/api/nodes')
  responses.shift()({ status: 401 })
  await current
  assert.deepEqual(events, ['lg:auth-required'])
})

for (const remember of [false, true]) {
  test(`credential persistence is ${remember ? 'browser-wide' : 'tab-only'} and clearing removes both copies`, async (t) => {
    const stores = { localStorage: new Map(), sessionStorage: new Map() }
    for (const [name, map] of Object.entries(stores)) {
      const previous = Object.getOwnPropertyDescriptor(globalThis, name)
      Object.defineProperty(globalThis, name, { configurable: true, value: {
        getItem: (key) => map.get(key) || null,
        setItem: (key, value) => map.set(key, value),
        removeItem: (key) => map.delete(key),
      } })
      t.after(() => {
        if (previous) Object.defineProperty(globalThis, name, previous)
        else delete globalThis[name]
      })
      map.set('lg-client-key', 'stale')
    }
    const api = await freshApi()
    api.setApiKey('new-key', remember)
    assert.equal(stores.localStorage.get('lg-client-key'), remember ? 'new-key' : undefined)
    assert.equal(stores.sessionStorage.get('lg-client-key'), remember ? undefined : 'new-key')
    assert.equal((await freshApi()).getApiKey(), 'new-key')
    api.clearApiKey()
    assert.equal(api.getApiKey(), '')
    assert.equal((await freshApi()).getApiKey(), '')
  })
}
