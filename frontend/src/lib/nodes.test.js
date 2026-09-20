import test from 'node:test'
import assert from 'node:assert/strict'
import { applyNodeStatus, rememberNodeStatus, reconcileNodeSnapshot } from './nodes.js'

const node = { id: 'a', name: 'Node A', online: true, version: 'old', tools: { ping: true } }

test('late snapshots retain newly online nodes and newer offline status', () => {
  const updates = new Map()
  rememberNodeStatus(updates, { node_id: 'b', online: true, name: 'Node B', tools: { tcp: true } })
  rememberNodeStatus(updates, { node_id: 'a', online: false })
  const result = reconcileNodeSnapshot([node], updates)
  assert.equal(result.find((n) => n.id === 'b').name, 'Node B')
  assert.equal(result.find((n) => n.id === 'a').online, false)
  assert.deepEqual(result.find((n) => n.id === 'a').tools, { ping: true })
})

test('coalesced online then offline frames retain updated capabilities', () => {
  const updates = new Map()
  rememberNodeStatus(updates, { node_id: 'a', online: true, name: 'Node A', version: 'new', tools: { tcp: true } })
  rememberNodeStatus(updates, { node_id: 'a', online: false })
  for (const snapshot of [[], [node]]) {
    const [result] = reconcileNodeSnapshot(snapshot, updates)
    assert.equal(result.online, false)
    assert.equal(result.version, 'new')
    assert.deepEqual(result.tools, { tcp: true })
  }
})

test('legacy reconnect clears stale capabilities, including after disconnect', () => {
  const updates = new Map()
  rememberNodeStatus(updates, { node_id: 'a', online: true, name: 'Node A' })
  rememberNodeStatus(updates, { node_id: 'a', online: false })
  const [result] = reconcileNodeSnapshot([node], updates)
  assert.equal(result.tools, undefined)
  assert.equal(result.version, undefined)
})

test('unchanged status preserves state identity and snapshots retain untouched nodes', () => {
  const existing = [node]
  assert.equal(applyNodeStatus(existing, { node_id: 'a', online: true, version: 'old', tools: { ping: true } }), existing)
  assert.deepEqual(reconcileNodeSnapshot(existing, new Map()), existing)
})
