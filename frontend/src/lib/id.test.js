import test from 'node:test'
import assert from 'node:assert/strict'
import { randomId } from './id.js'

test('HTTP fallback generates unique RFC 4122 version-4 command IDs', (t) => {
  const descriptor = Object.getOwnPropertyDescriptor(globalThis.crypto, 'randomUUID')
  Object.defineProperty(globalThis.crypto, 'randomUUID', { value: undefined, configurable: true })
  t.after(() => {
    if (descriptor) Object.defineProperty(globalThis.crypto, 'randomUUID', descriptor)
    else delete globalThis.crypto.randomUUID
  })
  const ids = Array.from({ length: 100 }, randomId)
  assert.equal(new Set(ids).size, ids.length)
  for (const id of ids) assert.match(id, /^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/)
})
