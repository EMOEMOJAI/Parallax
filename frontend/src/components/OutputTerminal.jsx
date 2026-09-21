import IconSwap from './IconSwap'
import StatusToast from './StatusToast'
import SharePreview from './SharePreview'
import { resultDocument, shareDocument, downloadFile } from '../lib/resultExport'
import { useRef, useEffect, useState, memo, useCallback, useMemo } from 'react'
import { Terminal as TermIcon, Copy, Check, Download, Trash2, Share2, X, Loader } from 'lucide-react'
import SummaryBadges from './SummaryBadges'
import { apiFetch } from '../lib/api'

import { detectHopAddress } from '../lib/hops'
export { detectHopAddress } from '../lib/hops'

// `as` from ip-api is "AS13335 Cloudflare, Inc."; the suffix shows the ASN and
// the city, and renders nothing at all when neither is known — never an error
// line, and never a partial "· ".
export function hopSuffix(geo) {
  if (!geo) return ''
  const asn = typeof geo.as === 'string' ? (geo.as.match(/^AS\d{1,10}/) || [''])[0] : ''
  const city = typeof geo.city === 'string' ? geo.city.slice(0, 64) : ''
  return [asn, city].filter(Boolean).join(' · ')
}

// How many distinct hop addresses one terminal will ever look up. traceroute's
// own ceiling is 64 hops; the extra headroom covers a kit run's several traces.
const MAX_HOP_LOOKUPS = 256
const GEO_BATCH_SIZE = 50

// ---- RDAP field extraction --------------------------------------------------
//
// The response is upstream JSON (proxied, auth-gated and size-capped by
// backend/rdap.go). Only a handful of short fields are read, each clamped and
// stripped of control characters; all of them render as JSX text children.

function rdapText(v, max) {
  return typeof v === 'string' ? v.replace(/[\u0000-\u001f\u007f]/g, '').slice(0, max) : ''
}

function vcardField(vcardArray, key) {
  if (!Array.isArray(vcardArray) || !Array.isArray(vcardArray[1])) return ''
  for (const field of vcardArray[1]) {
    if (Array.isArray(field) && field[0] === key && typeof field[3] === 'string') return field[3]
  }
  return ''
}

function abuseEmail(entities, depth = 0) {
  if (!Array.isArray(entities) || depth > 3) return ''
  for (const e of entities) {
    const roles = Array.isArray(e?.roles) ? e.roles : []
    if (roles.includes('abuse')) {
      const email = vcardField(e?.vcardArray, 'email')
      if (email) return email
    }
    const nested = abuseEmail(e?.entities, depth + 1)
    if (nested) return nested
  }
  return ''
}

export function extractRdap(data) {
  if (!data || typeof data !== 'object') return null
  return {
    handle: rdapText(data.handle, 64),
    name: rdapText(data.name, 120),
    country: rdapText(data.country, 8),
    range: [rdapText(data.startAddress, 45), rdapText(data.endAddress, 45)].filter(Boolean).join(' – '),
    abuse: rdapText(abuseEmail(data.entities), 120),
  }
}

// `geo` is the whole ip -> geo map, not this line's suffix: detection lives in
// here (so each line is examined once), which means the parent cannot know which
// entry a given line needs. The map's identity only changes when a lookup batch
// lands, so the extra render is rare.
export const OutputLine = memo(function OutputLine({ type, text, geo = null, onIpClick = null }) {
  const className = `whitespace-pre-wrap break-all ${
    type === 'error'
      ? 'text-danger'
      : type === 'info'
      ? 'text-cyan'
      : type === 'success'
      ? 'text-success'
      : 'text-text-secondary'
  }`
  // Detection runs once per line instance: this component is memoized on its
  // props, so a line is only re-examined when its text actually changes.
  const hop = useMemo(() => (type === 'output' && onIpClick ? detectHopAddress(text) : null), [type, text, onIpClick])
  if (!hop) {
    return <div className={className}>{text}</div>
  }
  const suffix = hopSuffix(geo ? geo[hop.ip] : null)
  // The line's own text is never rewritten: the three pieces below concatenate
  // back to `text` exactly, all of them JSX text children (React escapes them),
  // and the geo suffix is a sibling element — so a hop carrying something that
  // looks like markup still renders literally.
  return (
    <div className={className}>
      {text.slice(0, hop.start)}
      <button
        type="button"
        onClick={() => onIpClick(hop.ip)}
        title={`RDAP lookup for ${hop.ip}`}
        className="underline decoration-dotted underline-offset-2 hover:text-cyan cursor-pointer"
      >
        {text.slice(hop.start, hop.end)}
      </button>
      {text.slice(hop.end)}
      {suffix ? <span className="text-text-muted">{`  ${suffix}`}</span> : null}
    </div>
  )
})

// canShare=false hides the Share button (public mode). The server refuses
// POST /api/runs for public sessions regardless — this is presentation only.
export default function OutputTerminal({ lines, nodeName, onClear, runMeta, canShare = true, summary = null }) {
  const containerRef = useRef(null)
  const [copied, setCopied] = useState(false)
  const copiedTimer = useRef(null)
  const shareTimer = useRef(null)
  useEffect(() => () => {
    clearTimeout(copiedTimer.current)
    clearTimeout(shareTimer.current)
  }, [])
  const [shareSnapshot, setShareSnapshot] = useState(null)
  const [search, setSearch] = useState('')
  const [following, setFollowing] = useState(true)
  const [paused, setPaused] = useState(false)
  const filteredLines = useMemo(() => {
    const query = search.toLowerCase()
    return query ? lines.filter((line) => line.text.toLowerCase().includes(query)) : lines
  }, [lines, search])
  const [shareToast, setShareToast] = useState('')
  const autoScroll = useRef(true)

  // ---- hop enrichment (S10) ----
  // ip -> geo record for every hop address resolved so far. A fresh object on
  // each batch so the memoized lines pick the new suffixes up.
  const [hopGeo, setHopGeo] = useState({})
  // Addresses already requested. One that resolved to nothing is never retried
  // and simply renders without a suffix; one whose request failed outright is
  // un-marked so a later run may try again.
  const askedRef = useRef(new Set())
  // Where the incremental scan got to, by line id (ids are monotonic, so this
  // survives the front-trimming appendLines does at MAX_OUTPUT_LINES).
  const scannedIdRef = useRef(0)
  // One controller for the component's whole lifetime: aborting per batch would
  // cancel the previous hop's lookup every time a new hop line arrives, so only
  // the last hop of a streaming traceroute would ever get a suffix.
  const geoAbortRef = useRef(null)
  // Addresses whose batch was cancelled by the debounce before it ran. The scan
  // cursor has already moved past their lines, so this is the only way they can
  // come back — without it the last hop of every run loses its suffix (the
  // completion line always lands inside the debounce window) and any burst of
  // hops closer together than the debounce is dropped permanently.
  const requeueRef = useRef(new Set())
  useEffect(() => {
    const controller = new AbortController()
    geoAbortRef.current = controller
    return () => controller.abort()
  }, [])

  useEffect(() => {
    if (lines.length === 0) {
      // Cleared terminal: keep the resolved addresses (they are a cache) but
      // rewind the scan cursor.
      scannedIdRef.current = 0
      return
    }
    const startAt = scannedIdRef.current
    const fresh = []
    // Anything a cancelled batch gave back goes first: its lines are already
    // behind the scan cursor.
    for (const ip of requeueRef.current) fresh.push(ip)
    requeueRef.current.clear()
    let highestId = startAt
    for (const line of lines) {
      const id = line._id || 0
      if (id > highestId) highestId = id
      if (id <= startAt) continue
      if (line.type !== 'output') continue
      const hop = detectHopAddress(line.text)
      if (!hop) continue
      if (askedRef.current.has(hop.ip) || fresh.includes(hop.ip)) continue
      fresh.push(hop.ip)
    }
    scannedIdRef.current = highestId
    if (fresh.length === 0) return
    if (askedRef.current.size >= MAX_HOP_LOOKUPS) return

    const wanted = fresh.slice(0, MAX_HOP_LOOKUPS - askedRef.current.size)
    for (const ip of wanted) askedRef.current.add(ip)

    // Hops stream in one line at a time; a short debounce coalesces a whole
    // trace into a couple of batched lookups instead of one request per hop.
    let fired = false
    const timer = setTimeout(async () => {
      fired = true
      const signal = geoAbortRef.current?.signal
      for (let i = 0; i < wanted.length; i += GEO_BATCH_SIZE) {
        const batch = wanted.slice(i, i + GEO_BATCH_SIZE)
        try {
          const res = await apiFetch('/api/geoip/', {
            method: 'POST',
            headers: { 'Content-Type': 'application/json' },
            body: JSON.stringify({ ips: batch }),
            signal,
          })
          if (!res.ok) {
            // A 429 (this shares GeoMap's per-IP bucket, backend geoip.go) or a
            // 401 means those hops render with **no suffix** — never an error
            // line. Un-mark them so a later run may try again.
            for (const ip of batch) askedRef.current.delete(ip)
            continue
          }
          const results = await res.json()
          if (signal?.aborted || !Array.isArray(results)) continue
          const found = {}
          for (const r of results) {
            if (r && r.status === 'success' && typeof r.query === 'string') found[r.query] = r
          }
          if (Object.keys(found).length > 0) setHopGeo((prev) => ({ ...prev, ...found }))
        } catch {
          // AbortError (unmount) or a network failure: leave those hops bare.
          if (!signal?.aborted) for (const ip of batch) askedRef.current.delete(ip)
          return
        }
      }
    }, 350)
    return () => {
      clearTimeout(timer)
      // Cancelled before it ran: hand the addresses back so the next scan
      // re-queues them. Marking them asked up front is what makes the debounce
      // coalesce, but it must not outlive a batch that never happened.
      if (!fired) {
        for (const ip of wanted) {
          askedRef.current.delete(ip)
          requeueRef.current.add(ip)
        }
      }
    }
  }, [lines])

  // ---- RDAP popover ----
  const [rdap, setRdap] = useState(null) // { ip, loading, info, error }
  const rdapSeqRef = useRef(0)

  const openRdap = useCallback(async (ip) => {
    const seq = ++rdapSeqRef.current
    setRdap({ ip, loading: true })
    try {
      // The path segment is built from the detected **IP literal** and
      // encodeURIComponent — never from raw line text.
      const res = await apiFetch(`/api/rdap/${encodeURIComponent(ip)}`)
      if (!res.ok) throw new Error(res.status === 429 ? 'rate limited, try again' : `HTTP ${res.status}`)
      const data = await res.json()
      if (seq !== rdapSeqRef.current) return
      setRdap({ ip, loading: false, info: extractRdap(data) })
    } catch (err) {
      if (seq !== rdapSeqRef.current) return
      setRdap({ ip, loading: false, error: err.message })
    }
  }, [])

  const closeRdap = useCallback(() => {
    rdapSeqRef.current++
    setRdap(null)
  }, [])

  useEffect(() => {
    if (autoScroll.current && !paused && !search && containerRef.current) {
      containerRef.current.scrollTop = containerRef.current.scrollHeight
    }
  }, [lines, paused, search])

  const handleScroll = () => {
    if (!containerRef.current) return
    const { scrollTop, scrollHeight, clientHeight } = containerRef.current
    autoScroll.current = scrollHeight - scrollTop - clientHeight < 40
    setFollowing(autoScroll.current)
  }

  const getOutputText = () => lines.map((l) => l.text).join('\n')

  const notify = (message) => {
    clearTimeout(shareTimer.current)
    setShareToast(message)
    shareTimer.current = setTimeout(() => setShareToast(''), 4000)
  }

  const copyOutput = async () => {
    try {
      await navigator.clipboard.writeText(getOutputText())
      setCopied(true)
      clearTimeout(copiedTimer.current)
      copiedTimer.current = setTimeout(() => setCopied(false), 2000)
    } catch {
      notify('Couldn’t copy output. Download it or select the text and copy manually.')
    }
  }

  const exportOutput = (format = 'txt') => {
    const text = format === 'json' ? JSON.stringify(resultDocument(runMeta, lines, summary), null, 2) : getOutputText()
    const timestamp = new Date().toISOString().replace(/[:.]/g, '-').slice(0, 19)
    const safeName = (nodeName || 'output').replace(/[^a-zA-Z0-9_-]/g, '_')
    downloadFile(text, format === 'json' ? 'application/json' : 'text/plain', `parallax-${safeName}-${timestamp}.${format}`)
  }

  const jumpToLatest = () => {
    setSearch('')
    setPaused(false)
    setFollowing(true)
    autoScroll.current = true
    requestAnimationFrame(() => {
      if (containerRef.current) containerRef.current.scrollTop = containerRef.current.scrollHeight
    })
  }

  return (
    <div className="flex flex-col rounded-2xl border border-border/40 bg-bg-secondary/20 backdrop-blur-sm overflow-hidden">
      {/* Title bar */}
      <div className="flex flex-wrap items-center justify-between gap-2 px-4 py-2.5 border-b border-border/30 bg-bg-secondary/40">
        <div className="flex items-center gap-3">
          <div className="flex items-center gap-1.5">
            <div className="w-2.5 h-2.5 rounded-full bg-[#ff5f57]" />
            <div className="w-2.5 h-2.5 rounded-full bg-[#febc2e]" />
            <div className="w-2.5 h-2.5 rounded-full bg-[#28c840]" />
          </div>
          <span className="text-xs text-text-muted font-mono">
            {nodeName || 'parallax'}
          </span>
        </div>
        <div className="flex items-center gap-1">
          {onClear && (
            <button
              onClick={onClear}
              disabled={lines.length === 0}
              className="flex min-h-11 min-w-11 items-center justify-center rounded-md hover:bg-hover-overlay transition-colors text-text-muted hover:text-text-primary cursor-pointer disabled:opacity-30 disabled:cursor-not-allowed"
              title="Clear output"
            >
              <Trash2 size={13} />
            </button>
          )}
          {canShare && runMeta && (
            <button
              onClick={() => setShareSnapshot(shareDocument(runMeta, lines))}
              disabled={lines.length === 0}
              className="flex min-h-11 min-w-11 items-center justify-center rounded-md hover:bg-hover-overlay transition-colors text-text-muted hover:text-text-primary cursor-pointer disabled:opacity-30 disabled:cursor-not-allowed"
              title="Share output (creates a 24-hour permalink)"
            >
              <Share2 size={13} />
            </button>
          )}
          <button
            onClick={() => exportOutput('txt')}
            disabled={lines.length === 0}
            className="flex min-h-11 min-w-11 items-center justify-center rounded-md hover:bg-hover-overlay transition-colors text-text-muted hover:text-text-primary cursor-pointer disabled:opacity-30 disabled:cursor-not-allowed"
            title="Download output"
          >
            <Download size={13} />
          </button>
          <button onClick={() => exportOutput('json')} disabled={lines.length === 0} title="Download JSON"
            className="min-h-11 min-w-11 rounded-md px-2 text-xs text-text-muted hover:text-text-primary hover:bg-hover-overlay disabled:opacity-30 cursor-pointer">JSON</button>
          <button
            onClick={copyOutput}
            disabled={lines.length === 0}
            className="flex min-h-11 min-w-11 items-center justify-center rounded-md hover:bg-hover-overlay transition-colors text-text-muted hover:text-text-primary cursor-pointer disabled:opacity-30 disabled:cursor-not-allowed"
            title="Copy output"
          >
            <IconSwap active={copied} from={<Copy size={13} />} to={<Check size={13} className="text-success" />} />
          </button>
        </div>
      </div>

      {/* Structured summary of the finished probe, when the agent could
          parse one. Renders nothing otherwise. */}
      {summary && (
        <SummaryBadges summary={summary} className="px-4 py-2 border-b border-border/20 bg-bg-secondary/20" />
      )}

      <StatusToast message={shareToast} />
      {shareSnapshot && <SharePreview snapshot={shareSnapshot} onClose={() => setShareSnapshot(null)} onShared={notify} />}
      {lines.length > 0 && (
        <div className="flex flex-wrap items-center gap-2 border-b border-border/20 px-4 py-2 text-xs text-text-muted">
          <input type="search" aria-label="Search output" placeholder="Search output…" value={search} onChange={(event) => setSearch(event.target.value)}
            className="min-h-11 min-w-0 flex-1 rounded-lg border border-border/40 bg-bg-primary/60 px-3 text-text-primary" />
          {search && <span role="status">{filteredLines.length} matching lines</span>}
          <button aria-pressed={paused} onClick={() => paused ? jumpToLatest() : setPaused(true)}
            className="min-h-11 rounded-lg border border-border/40 px-3 hover:text-text-primary cursor-pointer">
            {paused ? 'Resume auto-scroll' : 'Pause auto-scroll'}
          </button>
          {(!following || paused || search) && <button onClick={jumpToLatest}
            className="min-h-11 rounded-lg border border-accent/40 px-3 text-accent-text cursor-pointer">Jump to latest</button>}
        </div>
      )}

      {/* Body */}
      <div
        ref={containerRef}
        role="region"
        aria-label="Diagnostic output"
        onScroll={handleScroll}
        className="flex-1 overflow-y-auto p-4 font-mono text-[13px] leading-relaxed"
        style={{ background: 'rgba(14, 14, 20, 0.75)', minHeight: 420, maxHeight: 'calc(100vh - 220px)' }}
      >
        {rdap && (
          <div
            role="dialog"
            aria-label={`RDAP for ${rdap.ip}`}
            className="sticky top-0 z-10 float-right ml-3 mb-2 w-64 rounded-xl
              bg-bg-elevated/95 border border-border/50 backdrop-blur-md shadow-xl shadow-black/40 p-3"
          >
            <div className="flex items-center justify-between gap-2 mb-1.5">
              <span className="text-[10px] font-semibold uppercase tracking-widest text-cyan">RDAP</span>
              <button
                onClick={closeRdap}
                aria-label="Close RDAP details"
                className="p-0.5 rounded text-text-muted hover:text-text-primary cursor-pointer"
              >
                <X size={11} />
              </button>
            </div>
            <div className="font-mono text-[11px] text-text-primary break-all">{rdap.ip}</div>
            {rdap.loading && (
              <div className="flex items-center gap-1 mt-1.5 text-[11px] text-text-muted">
                <Loader size={11} className="animate-spin" /> Looking up…
              </div>
            )}
            {rdap.error && <div className="mt-1.5 text-[11px] text-text-muted">No RDAP record ({rdap.error})</div>}
            {rdap.info && (
              <div className="mt-1.5 space-y-0.5 text-[11px] text-text-secondary">
                {rdap.info.handle && <div><span className="text-text-muted">handle </span>{rdap.info.handle}</div>}
                {rdap.info.name && <div><span className="text-text-muted">name </span>{rdap.info.name}</div>}
                {rdap.info.range && <div className="break-all"><span className="text-text-muted">range </span>{rdap.info.range}</div>}
                {rdap.info.country && <div><span className="text-text-muted">country </span>{rdap.info.country}</div>}
                {rdap.info.abuse && <div className="break-all"><span className="text-text-muted">abuse </span>{rdap.info.abuse}</div>}
                {!rdap.info.handle && !rdap.info.name && !rdap.info.abuse && (
                  <div className="text-text-muted">No handle, name or abuse contact in the record.</div>
                )}
              </div>
            )}
          </div>
        )}

        {lines.length === 0 ? (
          <div className="flex items-center justify-center h-full text-text-muted min-h-[300px]">
            <div className="text-center">
              <TermIcon size={28} className="mx-auto mb-2 opacity-40" />
              <p className="text-sm">Run a command to see output here.</p>
            </div>
          </div>
        ) : (
          filteredLines.map((line, i) => (
            <OutputLine
              key={line._id || i}
              type={line.type}
              text={line.text}
              geo={hopGeo}
              onIpClick={openRdap}
            />
          ))
        )}
      </div>
    </div>
  )
}
