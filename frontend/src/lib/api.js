// Client API key plumbing.
//
// The server accepts the key three ways: an `Authorization: Bearer` header on
// plain HTTP, the `lg.bearer` WebSocket subprotocol, and `?key=` as a fallback.
// A browser can't set headers on a WebSocket handshake, which is why the
// subprotocol exists — it keeps the key out of URLs, and therefore out of
// reverse-proxy access logs. `?key=` is used only when the key contains
// characters that aren't legal in a subprotocol token or equals `lg.bearer`.

const STORAGE_KEY = 'lg-client-key'
let sessionKey = ''
let sessionOverride = false
let credentialGeneration = 0

// Window event fired whenever the server answers "unauthorized". App listens
// and re-opens the key prompt; nothing else reacts to it.
export const AUTH_REQUIRED_EVENT = 'lg:auth-required'

// RFC 7230 token: the grammar a Sec-WebSocket-Protocol entry must satisfy.
// A key with a space, comma or non-ASCII byte would corrupt the handshake
// header, so those fall back to the query parameter.
const TOKEN_RE = /^[!#$%&'*+\-.^_`|~0-9A-Za-z]+$/

// localStorage throws in private-mode Safari and inside sandboxed iframes;
// every access is guarded so a storage failure degrades to "no key" instead of
// breaking the app.
export function getApiKey() {
  if (sessionOverride) return sessionKey
  try {
    const tabKey = sessionStorage.getItem(STORAGE_KEY)
    if (tabKey) return tabKey
  } catch { /* tab storage may be unavailable */ }
  try {
    return localStorage.getItem(STORAGE_KEY) || ''
  } catch {
    return sessionKey
  }
}

export function setApiKey(key, remember = true) {
  credentialGeneration += 1
  sessionKey = key
  // This tab always uses its explicitly selected credential, even if an old
  // saved key cannot be removed from read-only storage.
  sessionOverride = true
  try { localStorage.removeItem(STORAGE_KEY) } catch { /* storage unavailable */ }
  try { sessionStorage.removeItem(STORAGE_KEY) } catch { /* storage unavailable */ }
  try {
    const storage = remember ? localStorage : sessionStorage
    storage.setItem(STORAGE_KEY, key)
  } catch { /* storage unavailable — retain the key in memory for this visit */ }
}

export function clearApiKey() {
  credentialGeneration += 1
  sessionKey = ''
  sessionOverride = true
  try { localStorage.removeItem(STORAGE_KEY) } catch { /* storage unavailable */ }
  try { sessionStorage.removeItem(STORAGE_KEY) } catch { /* storage unavailable */ }
}

export function notifyAuthRequired() {
  window.dispatchEvent(new Event(AUTH_REQUIRED_EVENT))
}

/**
 * fetch() with the client key attached. Every call site in the app uses this
 * instead of fetch() so a key is never forgotten on a newly added endpoint.
 * A 401 raises AUTH_REQUIRED_EVENT and is then returned unchanged, so callers
 * keep their existing error handling.
 */
export async function apiFetch(path, init = {}) {
  const key = getApiKey()
  const generation = credentialGeneration
  const headers = new Headers(init.headers || {})
  if (key && !headers.has('Authorization')) {
    headers.set('Authorization', `Bearer ${key}`)
  }
  const res = await fetch(path, { ...init, headers })
  if (res.status === 401 && generation === credentialGeneration && key === getApiKey() &&
    (!headers.has('Authorization') || headers.get('Authorization') === `Bearer ${key}`)) notifyAuthRequired()
  return res
}

/**
 * How to carry `key` on a WebSocket handshake.
 * @returns {{protocols: string[], query: string}} `protocols` for the
 * WebSocket constructor (empty when none apply) and `query` to append to the
 * URL (empty string when the subprotocol carries the key).
 */
export function wsAuth(key) {
  if (!key) return { protocols: [], query: '' }
  // Duplicate subprotocol entries are rejected by the browser constructor.
  // This reserved literal uses the already-supported legacy query transport.
  if (TOKEN_RE.test(key) && key !== 'lg.bearer') return { protocols: ['lg.bearer', key], query: '' }
  return { protocols: [], query: `?key=${encodeURIComponent(key)}` }
}

/** Open a WebSocket with the key applied, on either transport. */
export function openAuthedSocket(path, key) {
  const scheme = window.location.protocol === 'https:' ? 'wss:' : 'ws:'
  const base = path.startsWith('ws') ? path : `${scheme}//${window.location.host}${path}`
  const { protocols, query } = wsAuth(key)
  return protocols.length > 0
    ? new WebSocket(base + query, protocols)
    : new WebSocket(base + query)
}
