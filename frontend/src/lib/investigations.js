import { summaryNumber } from './summaryNumber.js'

export const BASELINE_KEY = 'lg-baselines-v1'
export const MAX_RUN_BYTES = 256 * 1024
export const MAX_COLLECTION_BYTES = 2 * 1024 * 1024
export const MAX_RUNS = 20
export const RETENTION_DAYS = [1, 7, 30]
const bytes = (value) => new TextEncoder().encode(JSON.stringify(value)).length

export function validateRun(run) {
  return !!run && run.schema_version === 1 && typeof run.command === 'string' &&
    typeof run.target === 'string' && typeof run.options === 'string' &&
    ['started_at', 'shared_at', 'exported_at'].every((key) => run[key] === null || (typeof run[key] === 'string' && Number.isFinite(Date.parse(run[key])))) &&
    typeof run.node?.name === 'string' && typeof run.node?.location === 'string' &&
    Array.isArray(run.lines) && run.lines.length <= 10000 &&
    run.lines.every((line) => typeof line?.type === 'string' && typeof line.text === 'string') &&
    (run.summary === null || (typeof run.summary === 'object' && !Array.isArray(run.summary))) &&
    bytes(run) <= MAX_RUN_BYTES
}

export function readBaselines(storage, now = Date.now()) {
  const raw = storage.getItem(BASELINE_KEY)
  if (!raw) return []
  if (raw.length > MAX_COLLECTION_BYTES) throw new Error('Saved baselines exceed the storage limit. Clear them to start again.')
  const entries = JSON.parse(raw)
  if (!Array.isArray(entries)) throw new Error('Saved baselines are invalid. Clear them to start again.')
  return entries.filter((entry) => typeof entry?.id === 'string' && Number.isFinite(entry.expiresAt) &&
    entry.expiresAt > now && entry.expiresAt <= now + 30 * 86400000 && validateRun(entry.run)).slice(0, 10)
}

export function writeBaselines(storage, entries) {
  if (entries.length > 10 || bytes(entries) > MAX_COLLECTION_BYTES) throw new Error('Baseline storage is full. Delete a baseline first.')
  if (!entries.length) storage.removeItem(BASELINE_KEY)
  else storage.setItem(BASELINE_KEY, JSON.stringify(entries))
}

export function compatibleRuns(a, b) {
  return !!a && !!b && (a.command !== 'kit' || Boolean(a.options && b.options)) && ['command', 'target', 'options'].every((key) => a[key] === b[key]) &&
    a.node.name === b.node.name && a.node.location === b.node.location
}

function outputLines(run) {
  return run.lines.filter((line) => line.type === 'output').flatMap((line) => line.text.split('\n'))
}

export function dnsAnswers(run) {
  // dig answer records: ignore TTL and ordering, retain owner/class/type/value.
  // Restrict to ANSWER SECTION so authority/additional records are not answers.
  let answer = false
  const records = []
  for (const line of outputLines(run)) {
    if (/^;; ANSWER SECTION:/.test(line)) { answer = true; continue }
    if (/^;;/.test(line)) { answer = false; continue }
    const match = answer && line.match(/^(\S+)\s+\d+\s+(IN|CH|HS)\s+(\S+)\s+(.+)$/)
    if (match) records.push(`${match[1]} ${match[2]} ${match[3]} ${match[4].trim()}`)
  }
  return [...new Set(records)].sort()
}

export function routeHops(run) {
  // Compare observed hop identities, including private hops/timeouts. Timing
  // variation is omitted; unfamiliar trace formats remain in the raw output.
  return outputLines(run).flatMap((line) => {
    const match = line.match(/^\s*(\d{1,3})(?:\.\|--|[.):])?\s+(?:\|\s*)?(\S+)(?:\s+\(([^)]+)\))?/)
    if (!match || !/^(?:\*|\?\?\?|[\da-fA-F:.]+|[\w.-]+\.[\w.-]+)$/.test(match[2])) return []
    return [`${Number(match[1])}: ${match[3] || match[2]}`]
  })
}

export function compareRuns(before, after) {
  if (!compatibleRuns(before, after)) return null
  const metrics = [['avg_ms', 'Average latency', 'ms'], ['loss_pct', 'Packet loss', 'percentage points'],
    ['query_time_ms', 'DNS query time', 'ms'], ['connect_ms', 'Connect time', 'ms'], ['hop_count', 'Hop count', 'hops']]
    .flatMap(([key, label, unit]) => {
      const a = summaryNumber(before.summary?.[key]), b = summaryNumber(after.summary?.[key])
      return a === null || b === null ? [] : [{ label, before: a, after: b, delta: Math.round((b - a) * 1000) / 1000, unit }]
    })
  const extract = after.command === 'dns' ? dnsAnswers : ['traceroute', 'nexttrace', 'mtr'].includes(after.command) ? routeHops : null
  const a = extract ? extract(before) : [], b = extract ? extract(after) : []
  return { metrics, kind: after.command === 'dns' ? 'DNS answers' : 'Observed hops',
    added: b.filter((value) => !a.includes(value)), removed: a.filter((value) => !b.includes(value)),
    parsed: a.length > 0 || b.length > 0 }
}

export const RECIPES = {
  website: { label: 'Website unreachable', description: 'Check DNS, TCP port 443, the TLS certificate, and an HTTPS response.',
    steps: (host) => [{ type: 'dns', target: host, options: 'type=A' }, { type: 'tcp', target: `${host}:443`, options: '' },
      { type: 'tls', target: `${host}:443`, options: '' }, { type: 'http', target: `https://${host}`, options: '' }] },
  dns: { label: 'DNS looks wrong', description: 'Look up IPv4 and IPv6 answers, then compare resolver response times.',
    steps: (host) => [{ type: 'dns', target: host, options: 'type=A' }, { type: 'dns', target: host, options: 'type=AAAA' },
      { type: 'dnsbench', target: host, options: '' }] },
}

export function validDiagnosticHost(host) {
  return host.length <= 253 && host.split('.').every((label) => /^[a-zA-Z0-9](?:[a-zA-Z0-9-]{0,61}[a-zA-Z0-9])?$/.test(label))
}

export function explainRun(run) {
  const s = run.summary || {}
  if (run.lines.some((line) => line.type === 'error')) return 'This check failed or was interrupted. Review its output; this alone does not identify the cause.'
  if (s.chain_ok === false) return 'The TLS certificate chain was not trusted. Check the certificate and issuing chain.'
  const days = summaryNumber(s.days_remaining)
  if (days !== null && days <= 0) return 'The certificate is expired or expires today. Check its validity dates.'
  if (typeof s.status === 'string' && s.status !== 'NOERROR') return `DNS returned ${s.status}. Review the resolver response and domain configuration.`
  if (summaryNumber(s.answer_count) === 0) return 'No answers for this record type. A missing IPv6 answer alone does not mean the site is unavailable.'
  const code = summaryNumber(s.http_code)
  if (code !== null) return code >= 200 && code < 400 ? `The server returned HTTP ${code} from this node.` : `The server returned HTTP ${code}. Review the response and application configuration.`
  return 'The command completed. Review the output for details; completion alone does not prove the service is healthy.'
}

export function incidentText(runs, title, notes, createdAt = new Date().toISOString()) {
  return [`Parallax incident report`, title.trim() || 'Network investigation', `Exported: ${createdAt}`, notes.trim(),
    ...runs.map((run, index) => [`\n--- Check ${index + 1} ---`, `Node: ${run.node.name} (${run.node.location})`,
      `Started: ${run.started_at || 'unknown'}`, `Shared: ${run.shared_at || 'not a shared replay'}`,
      `Command: ${run.command} ${run.target}`, `Options: ${run.options || '(default)'}`,
      `Observation: ${explainRun(run)}`, `Summary: ${JSON.stringify(run.summary)}`, '\nOutput:', ...run.lines.map((line) => line.text)].join('\n'))].join('\n') + '\n'
}

export function redactText(text, terms) {
  const words = [...new Set(terms.split('\n').filter(Boolean))].sort((a, b) => b.length - a.length)
  if (!words.length) return text
  return text.replace(new RegExp(words.map((word) => word.replace(/[.*+?^${}()|[\]\\]/g, '\\$&')).join('|'), 'g'), '[redacted]')
}
