// ---- hop address detection (S10) -------------------------------------------
//
// Detection is **per line and command-agnostic**: it matches the shape of a hop
// line (a leading hop index followed by an address) rather than keying off the
// run's command type. runMeta.command is 'kit' for a kit run and the replayed
// command for a permalink, so a per-line rule is the only unambiguous one.
//
// The detected value is always a strict IP literal, re-checked below — it is
// what gets interpolated into the /api/rdap/<ip> path, never raw line text.

// A hop line is a leading hop index, separators, and then an address or a
// hostname — `  1.|-- host` (mtr), ` 1  host` (traceroute), `1 | host`
// (nexttrace). Requiring that first token to be an address or a hostname is
// what keeps ping's `64 bytes from 1.1.1.1: …` out: "bytes" is neither.
const HOP_HEAD_RE = /^[ \t]{0,8}\d{1,3}(?:\.\|--|[.):])?[ \t|]{1,6}(\S+)/
const HOSTNAME_RE = /^[A-Za-z0-9_][A-Za-z0-9._-]*\.[A-Za-z]{2,63}\.?$/
// Candidate addresses inside the rest of the line. Both are deliberately loose;
// isIpLiteral below is the gate.
const ADDR_RE = /\d{1,3}(?:\.\d{1,3}){3}|[0-9A-Fa-f]{0,4}(?::[0-9A-Fa-f]{0,4}){2,7}/g

function isIpv4(v) {
  const parts = v.split('.')
  if (parts.length !== 4) return false
  return parts.every((p) => /^\d{1,3}$/.test(p) && Number(p) <= 255 && (p === '0' || p[0] !== '0'))
}

function isIpv6(v) {
  if (!v.includes(':') || !/^[0-9A-Fa-f:]{2,45}$/.test(v)) return false
  if (v.includes(':::') || v.split('::').length - 1 > 1) return false
  if (v.startsWith(':') && !v.startsWith('::')) return false
  if (v.endsWith(':') && !v.endsWith('::')) return false
  const parts = v.split(':')
  if (parts.some((p) => p.length > 4)) return false
  if (v.includes('::')) return parts.filter((p) => p !== '').length <= 7
  return parts.length === 8 && parts.every((p) => p.length > 0)
}

function isIpLiteral(v) {
  return isIpv4(v) || isIpv6(v)
}

// Addresses that have no public geo or RDAP record. Skipped entirely: a private
// first hop would otherwise spend a token from the per-IP bucket this shares
// with GeoMap (backend geoip.go) and still render without a suffix.
function isNonRoutable(v) {
  if (isIpv4(v)) {
    const [a, b] = v.split('.').map(Number)
    if (a === 0 || a === 10 || a === 127 || a === 255) return true
    if (a === 172 && b >= 16 && b <= 31) return true
    if (a === 192 && b === 168) return true
    if (a === 169 && b === 254) return true
    if (a === 100 && b >= 64 && b <= 127) return true
    if (a >= 224) return true
    return false
  }
  const lower = v.toLowerCase()
  if (lower === '::' || lower === '::1') return true
  return /^(f[cd][0-9a-f]{2}|fe[89ab][0-9a-f]|ff[0-9a-f]{2})/.test(lower)
}

/**
 * detectHopAddress(text) — `{ip, start, end}` for the first usable address on a
 * hop line, or null. `start`/`end` are indexes into the *original* text, so the
 * renderer can make the address clickable without rewriting what the line says.
 */
export function detectHopAddress(text) {
  if (typeof text !== 'string' || text.length === 0 || text.length > 512) return null
  const head = HOP_HEAD_RE.exec(text)
  if (!head) return null
  const first = head[1]
  const trimmed = first.replace(/^[[(]+/, '').replace(/[\]),:]+$/, '')
  const looksLikeHop = isIpLiteral(first) || isIpLiteral(trimmed) ||
    HOSTNAME_RE.test(first) || HOSTNAME_RE.test(trimmed)
  if (!looksLikeHop) return null
  ADDR_RE.lastIndex = 0
  let m
  while ((m = ADDR_RE.exec(text)) !== null) {
    const candidate = m[0]
    // Do not turn a valid-looking prefix of a malformed literal into a link.
    const before = text[m.index - 1] || ''
    const after = text[m.index + candidate.length] || ''
    if (/[0-9a-fA-F:.]/.test(before) || /[0-9a-fA-F:.]/.test(after)) continue
    if (!isIpLiteral(candidate) || isNonRoutable(candidate)) continue
    return { ip: candidate, start: m.index, end: m.index + candidate.length }
  }
  return null
}

/** Numbered public route hops, excluding headers, timeouts and summaries. */
export function parseRouteHops(lines) {
  return lines.flatMap((line) => {
    const address = detectHopAddress(line)
    if (!address) return []
    const hop = Number(line.match(/^\s*(\d+)/)[1])
    return [{ hop, ip: address.ip }]
  })
}
