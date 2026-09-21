import test from 'node:test'
import assert from 'node:assert/strict'
import { resultDocument } from './resultExport.js'
import { BASELINE_KEY, readBaselines, writeBaselines, validateRun, compatibleRuns, compareRuns, dnsAnswers, routeHops, incidentText, redactText, validDiagnosticHost, explainRun } from './investigations.js'

const run = (command = 'ping', lines = [], summary = null) => resultDocument({ command, nodeName: 'Node A', nodeLocation: 'Site 1', target: 'example.com', options: '', startedAt: '2026-01-01T00:00:00Z' }, lines.map((text) => ({ type: 'output', text })), summary)

test('baselines validate stored data and expire without accepting malformed snapshots', () => {
  const storage = new Map()
  storage.getItem = (key) => storage.get(key)
  storage.setItem = (key, value) => storage.set(key, value)
  storage.removeItem = (key) => storage.delete(key)
  const now = Date.now()
  const good = { id: 'one', expiresAt: now + 60000, run: run() }
  storage.set(BASELINE_KEY, JSON.stringify([good, { ...good, id: 'expired', expiresAt: now }, { ...good, run: { ...run(), started_at: {} } }, { ...good, run: { ...run(), lines: [null] } }]))
  assert.deepEqual(readBaselines(storage, now), [good])
  assert.equal(validateRun(run('ping', ['x'.repeat(256 * 1024)])), false)
  assert.throws(() => writeBaselines(storage, Array(11).fill(good)), /full/)
  writeBaselines(storage, [])
  assert.equal(storage.has(BASELINE_KEY), false)
  storage.set(BASELINE_KEY, '{broken')
  assert.throws(() => readBaselines(storage))
})

test('baseline comparison uses matching context and rejects missing numeric data', () => {
  const before = run('ping', [], { avg_ms: 10, loss_pct: 0 })
  const after = run('ping', [], { avg_ms: 15, loss_pct: 20 })
  assert.equal(compareRuns(before, after).metrics[0].delta, 5)
  assert.equal(compareRuns(before, after).metrics[1].delta, 20)
  for (const changed of [{ target: 'other.example' }, { options: 'count=10' }, { node: { name: 'Node B', location: 'Site 1' } }]) {
    assert.equal(compatibleRuns(before, { ...after, ...changed }), false)
    assert.equal(compareRuns(before, { ...after, ...changed }), null)
  }
  assert.deepEqual(compareRuns(before, run('ping', [], { avg_ms: null, loss_pct: false })).metrics, [])
})

test('DNS comparison ignores TTL, order and additional records, but reveals answer changes', () => {
  const before = run('dns', [';; ANSWER SECTION:', 'example.com. 60 IN A 192.0.2.1', 'example.com. 60 IN A 192.0.2.2', ';; ADDITIONAL SECTION:', 'ns.example.com. 60 IN A 192.0.2.9'])
  const after = run('dns', [';; ANSWER SECTION:', 'example.com. 30 IN A 192.0.2.2', 'example.com. 30 IN A 192.0.2.3'])
  assert.equal(dnsAnswers(before).length, 2)
  assert.deepEqual(compareRuns(before, after).removed, ['example.com. IN A 192.0.2.1'])
  assert.deepEqual(compareRuns(before, after).added, ['example.com. IN A 192.0.2.3'])
})

test('route comparison preserves private hops, timeouts and loop positions without RTT noise', () => {
  const before = run('traceroute', ['traceroute to example.com (192.0.2.1)', ' 1  10.0.0.1  1 ms', ' 2  * * *', ' 3  router.example (192.0.2.1) 2 ms', '64 bytes from 192.0.2.1: time=1 ms'])
  assert.deepEqual(routeHops(before), ['1: 10.0.0.1', '2: *', '3: 192.0.2.1'])
  const after = run('traceroute', ['1 10.0.0.1 9 ms', '2 192.0.2.2 10 ms', '3 router.example (192.0.2.1) 4 ms'])
  assert.deepEqual(compareRuns(before, after).added, ['2: 192.0.2.2'])
})

test('incident preview redacts all fields literally without introducing HTML or partial overlaps', () => {
  const source = incidentText([run('dns', ['example.com and a+b and <script>'])], 'example.com incident', 'a+b', '2026-01-01T00:00:00Z')
  const preview = redactText(source, 'example.com\nexample\na+b\nNode A')
  // Exact report content is the contract, including literal markup as text.
  assert.equal(preview, [
    'Parallax incident report', '[redacted] incident', 'Exported: 2026-01-01T00:00:00Z', '[redacted]',
    '', '--- Check 1 ---', 'Node: [redacted] (Site 1)', 'Started: 2026-01-01T00:00:00Z',
    'Shared: not a shared replay', 'Command: dns [redacted]', 'Options: (default)',
    'Observation: The command completed. Review the output for details; completion alone does not prove the service is healthy.',
    'Summary: null', '', 'Output:', '[redacted] and [redacted] and <script>', '',
  ].join('\n'))
  assert.match(explainRun(run('tls', [], { chain_ok: false })), /not trusted/)
  assert.match(explainRun(run('dns', [], { answer_count: 0 })), /No answers/)
})

test('guided target validation accepts hostnames and rejects URLs, ports, shell syntax and oversized labels', () => {
  for (const host of ['example.com', 'localhost', '192.0.2.1', 'xn--bcher-kva.example']) assert.equal(validDiagnosticHost(host), true)
  for (const host of ['', '-bad.example', 'https://example.com', 'example.com:443', 'example.com/a', 'a;ls', 'a'.repeat(64) + '.com', 'a..com']) assert.equal(validDiagnosticHost(host), false)
})

test('kit baseline matching requires identical recorded sequences', () => {
  const a = { ...run('kit'), options: JSON.stringify([{ type: 'ping', options: 'count=5' }]) }
  const b = { ...run('kit'), options: JSON.stringify([{ type: 'ping', options: 'count=10' }]) }
  assert.equal(compatibleRuns(a, b), false)
  assert.equal(compatibleRuns(a, { ...a }), true)
  assert.equal(compatibleRuns(run('kit'), run('kit')), false)
})

test('successful guided observations show measurements without masking failures or inventing missing values', () => {
  assert.equal(explainRun(run('dns', [], { status: 'NOERROR', answer_count: 2, query_time_ms: 0 })), 'DNS returned 2 answers in 0 ms (NOERROR).')
  assert.equal(explainRun(run('dns', [], { status: 'NOERROR', answer_count: 1 })), 'DNS returned 1 answer (NOERROR).')
  assert.equal(explainRun(run('tcp', [], { connect_ms: 3.174 })), 'TCP connected in 3.174 ms.')
  assert.equal(explainRun(run('tls', [], { chain_ok: true, days_remaining: 36 })), 'The TLS certificate chain is trusted; 36 days until expiry.')
  assert.equal(explainRun(run('tls', [], { chain_ok: true })), 'The TLS certificate chain is trusted.')
  for (const value of [null, false, [], {}, '', -1]) assert.doesNotMatch(explainRun(run('tcp', [], { connect_ms: value })), /TCP connected/)
  assert.match(explainRun(run('dns', [], { status: 'NXDOMAIN', answer_count: 0 })), /NXDOMAIN/)
  assert.match(explainRun(run('tls', [], { chain_ok: true, days_remaining: 0 })), /expired/)
  assert.match(explainRun(run('tls', [], { chain_ok: false, days_remaining: 36 })), /not trusted/)
  for (const [command, summary] of [['dns', { status: 'NOERROR', answer_count: 2 }], ['tcp', { connect_ms: 3 }], ['tls', { chain_ok: true }]]) {
    assert.match(explainRun({ ...run(command, [], summary), lines: [{ type: 'error', text: 'Interrupted' }] }), /failed or was interrupted/)
  }
})

test('incident comparisons include signed deltas and both evidence snapshots, with global redaction', () => {
  const before = run('tcp', ['baseline-private-output'], { connect_ms: 3.174 })
  const after = { ...run('tcp', ['current-private-output'], { connect_ms: 2.874 }), started_at: '2026-01-02T00:00:00Z' }
  const text = incidentText([], 'Comparison only', '', '2026-01-03T00:00:00Z', { before, after })
  assert.match(text, /Connect time: 3.174 → 2.874 ms \(change: -0.3 ms\)/)
  for (const item of ['Baseline run', 'Compared run', before.started_at, after.started_at, 'baseline-private-output', 'current-private-output']) assert.ok(text.includes(item))
  const redacted = redactText(text, 'Node A\nSite 1\nexample.com\nbaseline-private-output\ncurrent-private-output')
  for (const item of ['Node A', 'Site 1', 'example.com', 'baseline-private-output', 'current-private-output']) assert.ok(!redacted.includes(item))
  assert.doesNotMatch(incidentText([], '', '', undefined, { before, after: { ...after, target: 'other.example' } }), /Baseline comparison/)
  const unknown = incidentText([], '', '', undefined, { before: run(), after: run() })
  assert.match(unknown, /No comparable structured data/)
  const dns = incidentText([], '', '', undefined, {
    before: run('dns', [';; ANSWER SECTION:', 'example.com. 60 IN A 192.0.2.1']),
    after: run('dns', [';; ANSWER SECTION:', 'example.com. 30 IN A 192.0.2.2']),
  })
  assert.match(dns, /Removed: example.com. IN A 192.0.2.1/)
  assert.match(dns, /Added: example.com. IN A 192.0.2.2/)
})
