import test from 'node:test'
import assert from 'node:assert/strict'
import { resultDocument, shareDocument, redactShare, comparisonCsv, csvCell } from './resultExport.js'

test('JSON export preserves the captured run and strips internal line IDs', () => {
  const meta = { nodeName: 'Original', nodeLocation: 'Site 1', target: 'example.com', command: 'ping', startedAt: '2026-01-01T00:00:00Z' }
  const result = resultDocument(meta, [{ _id: 7, type: 'output', text: '<test>' }], { loss: 0 }, '2026-01-01T00:00:05Z')
  assert.equal(result.started_at, meta.startedAt)
  assert.equal(result.node.name, 'Original')
  assert.deepEqual(result.lines, [{ type: 'output', text: '<test>' }])
  assert.deepEqual(result.summary, { loss: 0 })
})

test('redaction covers metadata and output with literal, simultaneous replacements', () => {
  const snapshot = shareDocument({ nodeName: 'a.b', command: 'ping', target: 'a.b', options: 'a.b a' }, [{ type: 'output', text: 'a.b a <script>' }])
  const result = redactShare(snapshot, 'a\na.b\n<script>')
  assert.equal(result.node_name, '[redacted]')
  assert.equal(result.target, '[redacted]')
  assert.equal(result.options, '[redacted] [redacted]')
  assert.equal(result.lines[0].text, '[redacted] [redacted] [redacted]')
  assert.equal(result.lines[0].type, 'output')
  assert.equal(snapshot.lines[0].text, 'a.b a <script>')
})

test('CSV quotes multiline cells and neutralizes spreadsheet formulas', () => {
  for (const value of ['=CMD()', '+1', '-1', '@SUM(A1)', ' \t=CMD()']) assert.ok(csvCell(value).startsWith('"\''))
  assert.equal(csvCell('a,"b"\nc'), '"a,""b""\nc"')
  const csv = comparisonCsv({ nodes: [{ id: 'a', name: '=formula', location: 'A,B' }], command: 'ping', target: 'example.com', startedAt: 'time' }, { a: [{ text: 'first\nsecond' }] }, { a: { loss: 0 } })
  assert.ok(csv.includes('"\'=formula","A,B"'))
  assert.ok(csv.includes('"first\nsecond"'))
  assert.ok(csv.endsWith('\r\n'))
})

test('share review preserves server normalization and blocks every truncation boundary', async () => {
  const { shareValidationError } = await import('./resultExport.js')
  const meta = { nodeName: 'Node\u0000 A', command: 'ping', target: 'example.com' }
  const snapshot = shareDocument(meta, [{ type: 'output', text: 'a\u0000b\n\tc\r' }])
  assert.equal(snapshot.node_name, 'Node A')
  assert.deepEqual(snapshot.lines, [{ type: 'output', text: 'ab\n\tc' }])
  assert.equal(shareValidationError(snapshot), '')
  assert.equal(shareValidationError(shareDocument(meta, [{ type: 'output', text: 'é'.repeat(2048) }])), '')
  assert.match(shareValidationError(shareDocument(meta, [{ type: 'output', text: 'é'.repeat(2049) }])), /4 KiB/)
  assert.match(shareValidationError(shareDocument({ ...meta, nodeName: '界'.repeat(65) }, [])), /64-character/)
  assert.equal(shareValidationError(shareDocument({ ...meta, nodeName: '界'.repeat(64) }, [])), '')
})

test('kit exports preserve recorded steps without presenting a final-step summary as aggregate data', () => {
  const options = JSON.stringify([{ type: 'dns', options: 'type=AAAA' }])
  const exported = resultDocument({ command: 'kit', options }, [], { answer_count: 1 })
  assert.equal(exported.options, options)
  assert.equal(exported.summary, null)
})
