export const nodes = ['a', 'b'].map((id, i) => ({
  id,
  name: `Node ${id.toUpperCase()}`,
  location: `Site ${i + 1}`,
  flag: '🏠',
  provider: 'Example',
  online: true,
  ipv4: `192.0.2.${i + 1}`,
  version: 'integration-test',
  tools: {},
}))
export async function openDashboard(context, options = {}) {
  const p = await context.newPage()
  p.setDefaultTimeout(4000)
  p.apiHeaders = []
  p.errors = []
  p.on('pageerror', (e) => p.errors.push(e.message))
  await p.route('https://**', (r) => r.abort())
  await p.route('**/api/**', async (route) => {
    const path = new URL(route.request().url()).pathname
    if (options.delayedReplay && path.startsWith('/api/runs/')) {
      p.replayRoute = route
      return
    }
    p.apiHeaders.push(route.request().headers().authorization)
    const body =
      path === '/api/public-config'
        ? {
            public_mode: !!options.publicMode,
            allowed_commands: options.commands || [],
            allowed_targets: options.targets || [],
            auth_required: false,
          }
        : path === '/api/nodes' || path === '/api/nodes/health'
          ? nodes
          : path === '/api/latency-matrix'
            ? { nodes, latency: {} }
            : []
    await route.fulfill({
      status: 200,
      contentType: 'application/json',
      body: JSON.stringify(body),
    })
  })
  await p.addInitScript((opts) => {
    window.sent = []
    window.sockets = []
    if (opts.savedKey) localStorage.setItem('lg-client-key', opts.savedKey)
    if (opts.badStorage) {
      localStorage.setItem(
        'lg-cmd-history',
        '[null,{"type":"ping","target":{}},{"type":"ping","target":"example.com"}]',
      )
      localStorage.setItem(
        'lookingGlass.presets',
        '[{"type":"ping","target":{},"options":{}},null]',
      )
    }
    if (opts.noUuid)
      Object.defineProperty(crypto, 'randomUUID', {
        value: undefined,
        configurable: true,
      })
    if (opts.blockStorage) {
      Storage.prototype.setItem = () => {
        throw new Error('blocked')
      }
      Storage.prototype.getItem = () => {
        throw new Error('blocked')
      }
    }
    const Native = window.WebSocket
    window.NativeWebSocket = Native
    class Mock {
      static CONNECTING = 0
      static OPEN = 1
      static CLOSING = 2
      static CLOSED = 3
      constructor(url, protocols) {
        if (
          !String(url).includes('/ws/client') &&
          !String(url).includes('/ws/speedtest')
        )
          return new Native(url, protocols)
        this.readyState = 0
        this.url = url
        this.protocols = protocols
        window.sockets.push(this)
        if (opts.hangingSpeedtest && String(url).includes('/ws/speedtest'))
          return
        setTimeout(() => {
          if (this.readyState !== 0) return
          this.readyState = 1
          this.onopen?.({})
          if (opts.publicMode)
            this.emit({ type: 'session_kind', kind: 'public' })
        }, 20)
      }
      close() {
        this.readyState = 2
        const done = () => {
          this.readyState = 3
          this.onclose?.({ code: 1000 })
        }
        if (opts.staleClose) setTimeout(done, 100)
        else done()
      }
      send(raw) {
        window.sent.push(JSON.parse(raw))
      }
      emit(data) {
        this.onmessage?.({ data: JSON.stringify(data) })
      }
    }
    window.WebSocket = Mock
  }, options)
  await p.goto(options.delayedReplay ? '/?run=saved-run' : '/')
  await p.getByText('Live', { exact: true }).waitFor({ timeout: 10000 })
  await p.waitForTimeout(200)
  return p
}

export async function chooseCommand(page, name) {
  await page.getByRole('button', { name: /^Command type:/ }).click()
  await page.getByRole('button', { name, exact: true }).click()
}
