import { memo } from 'react'

// Structured probe summaries (S2). The server validates and sanitizes every
// summary before it gets here, but this component is the last hop before the
// DOM, so it applies its own guards (F23):
//
//   - strings are clamped to MAX_SUMMARY_STRING characters;
//   - numbers go through Number() + isFinite and are dropped otherwise;
//   - only known keys are rendered — an unexpected key is ignored, never
//     printed blind.
//
// It accepts either the parsed object (the schedules API returns real JSON)
// or the raw JSON string the WebSocket carries in CommandResponse.data.

const MAX_SUMMARY_STRING = 64

export function clampSummaryString(value) {
  if (typeof value !== 'string') return null
  const clean = value.slice(0, MAX_SUMMARY_STRING)
  return clean.length > 0 ? clean : null
}

/**
 * summaryBool(value) — the boolean badge path. summaryNumber deliberately
 * returns null for booleans, so `chain_ok: false` would render nothing at all if
 * the numeric helper were reused for it — the one value a tls probe exists to
 * report.
 */
export function summaryBool(value) {
  return typeof value === 'boolean' ? value : null
}

export function summaryNumber(value) {
  if (value === null || value === undefined || value === '' || typeof value === 'boolean') return null
  const n = Number(value)
  return isFinite(n) ? n : null
}

// parseSummary turns whatever arrived into a plain object, or null. Unparsable
// payloads are ignored — never thrown.
export function parseSummary(input) {
  if (!input) return null
  if (typeof input === 'object') {
    return Array.isArray(input) ? null : input
  }
  if (typeof input !== 'string') return null
  try {
    const parsed = JSON.parse(input)
    if (!parsed || typeof parsed !== 'object' || Array.isArray(parsed)) return null
    return parsed
  } catch {
    return null
  }
}

function fmtNum(n) {
  if (Number.isInteger(n)) return String(n)
  return String(Math.round(n * 100) / 100)
}

// fmtBytes keeps a download's wire-byte count readable without inventing
// precision: whole MB above a megabyte, whole KB above a kilobyte.
function fmtBytes(n) {
  if (n >= 1024 * 1024) return `${fmtNum(Math.round((n / (1024 * 1024)) * 10) / 10)} MB`
  if (n >= 1024) return `${Math.round(n / 1024)} KB`
  return `${fmtNum(n)} B`
}

function lossTone(loss) {
  if (loss >= 100) return 'bad'
  if (loss >= 20) return 'warn'
  return 'good'
}

// summaryBadges maps a summary onto the ordered badge list the UI renders.
export function summaryBadges(input) {
  const s = parseSummary(input)
  if (!s) return []
  const out = []

  const sent = summaryNumber(s.sent)
  const received = summaryNumber(s.received)
  if (sent !== null && received !== null) {
    out.push({ key: 'packets', text: `${fmtNum(received)}/${fmtNum(sent)}`, tone: 'plain' })
  }

  const loss = summaryNumber(s.loss_pct)
  if (loss !== null) {
    out.push({ key: 'loss', text: `${fmtNum(loss)}% loss`, tone: lossTone(loss) })
  }

  const avg = summaryNumber(s.avg_ms)
  if (avg !== null) {
    out.push({ key: 'avg', text: `avg ${fmtNum(avg)} ms`, tone: 'plain' })
  }

  const min = summaryNumber(s.min_ms)
  const max = summaryNumber(s.max_ms)
  if (min !== null && max !== null) {
    out.push({ key: 'minmax', text: `${fmtNum(min)}–${fmtNum(max)} ms`, tone: 'plain' })
  }

  const code = summaryNumber(s.http_code)
  if (code !== null) {
    const tone = code >= 200 && code < 400 ? 'good' : code === 0 ? 'bad' : 'warn'
    out.push({ key: 'http', text: `HTTP ${fmtNum(code)}`, tone })
  }

  const ttfb = summaryNumber(s.ttfb_ms)
  if (ttfb !== null) out.push({ key: 'ttfb', text: `ttfb ${fmtNum(ttfb)} ms`, tone: 'plain' })

  const total = summaryNumber(s.total_ms)
  if (total !== null) out.push({ key: 'total', text: `total ${fmtNum(total)} ms`, tone: 'plain' })

  const size = summaryNumber(s.size_bytes)
  if (size !== null) out.push({ key: 'size', text: `${fmtNum(size)} B`, tone: 'plain' })

  const status = clampSummaryString(s.status)
  if (status !== null) {
    out.push({ key: 'status', text: status, tone: status === 'NOERROR' ? 'good' : 'warn' })
  }

  const answers = summaryNumber(s.answer_count)
  if (answers !== null) {
    out.push({ key: 'answers', text: `${fmtNum(answers)} answer${answers === 1 ? '' : 's'}`, tone: 'plain' })
  }

  const queryTime = summaryNumber(s.query_time_ms)
  if (queryTime !== null) out.push({ key: 'query', text: `${fmtNum(queryTime)} ms`, tone: 'plain' })

  const hops = summaryNumber(s.hop_count)
  if (hops !== null) out.push({ key: 'hops', text: `${fmtNum(hops)} hops`, tone: 'plain' })

  // ── The four native probes (S7) ──
  // tcp
  const connect = summaryNumber(s.connect_ms)
  if (connect !== null) out.push({ key: 'connect', text: `connect ${fmtNum(connect)} ms`, tone: 'plain' })

  // tls. days_remaining is signed on purpose: an expired certificate is the
  // finding, and it arrives with exit_ok true.
  const days = summaryNumber(s.days_remaining)
  if (days !== null) {
    const tone = days <= 0 ? 'bad' : days <= 14 ? 'warn' : 'good'
    out.push({ key: 'expiry', text: days < 0 ? `expired ${fmtNum(-days)}d ago` : `${fmtNum(days)}d left`, tone })
  }
  const chainOk = summaryBool(s.chain_ok)
  if (chainOk !== null) {
    out.push({ key: 'chain', text: chainOk ? 'chain ok' : 'chain invalid', tone: chainOk ? 'good' : 'bad' })
  }
  const issuer = clampSummaryString(s.issuer)
  if (issuer !== null) out.push({ key: 'issuer', text: issuer, tone: 'plain' })

  // dnsbench
  const fastest = summaryNumber(s.fastest_ms)
  if (fastest !== null) out.push({ key: 'fastest', text: `fastest ${fmtNum(fastest)} ms`, tone: 'plain' })
  const resolvers = summaryNumber(s.resolver_count)
  if (resolvers !== null) {
    out.push({ key: 'resolvers', text: `${fmtNum(resolvers)} resolver${resolvers === 1 ? '' : 's'}`, tone: 'plain' })
  }

  // download
  const mbits = summaryNumber(s.mbits)
  if (mbits !== null) out.push({ key: 'mbits', text: `${fmtNum(mbits)} Mbit/s`, tone: 'plain' })
  const bytes = summaryNumber(s.bytes)
  if (bytes !== null) out.push({ key: 'bytes', text: fmtBytes(bytes), tone: 'plain' })

  return out
}

const TONE_CLASS = {
  good: 'text-success border-success/30 bg-success/10',
  warn: 'text-warning border-warning/30 bg-warning/10',
  bad: 'text-danger border-danger/30 bg-danger/10',
  plain: 'text-text-secondary border-border/40 bg-bg-secondary/40',
}

// SummaryBadges renders one compact row of pills. It renders nothing when
// there is no summary or nothing recognizable in it.
const SummaryBadges = memo(function SummaryBadges({ summary, className = '' }) {
  const badges = summaryBadges(summary)
  if (badges.length === 0) return null
  return (
    <div className={`flex flex-wrap items-center gap-1.5 ${className}`} data-testid="summary-badges">
      {badges.map((b) => (
        <span
          key={b.key}
          className={`px-2 py-0.5 rounded-full border text-[10px] font-medium font-mono tracking-tight ${TONE_CLASS[b.tone] || TONE_CLASS.plain}`}
        >
          {b.text}
        </span>
      ))}
    </div>
  )
})

export default SummaryBadges
