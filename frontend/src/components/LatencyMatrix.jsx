import { useState, useEffect, useCallback, useRef } from 'react'
import { Grid3x3, RefreshCw, Loader } from 'lucide-react'
import { useFocusTrap } from '../hooks/useFocusTrap'
import { useMountTransition } from '../hooks/useMountTransition'
import { apiFetch } from '../lib/api'

// Measurement is a server-side job now: the browser asks for one and then
// polls the matrix. It no longer sends pings of its own, scrapes "avg" out of
// ping output, or POSTs per-pair results — the server measures every ordered
// pair on a timer, reads the value from the structured probe summary, and
// stamps each cell with when it was measured and by whom.

const POLL_INTERVAL_MS = 2000
const POLL_TIMEOUT_MS = 120000
// A cell older than this is shown dimmed: it describes the network as it was,
// not as it is.
const STALE_AFTER_MS = 60 * 60 * 1000

export default function LatencyMatrix({ visible, onClose, wsRef, canMeasure = true }) {
  // wsRef is still accepted (App passes it) but the component no longer sends
  // anything over the socket.
  void wsRef

  const [data, setData] = useState(null)
  const [measuring, setMeasuring] = useState(false)
  const [notice, setNotice] = useState('')
  const requestRef = useRef(null)
  const measurementRef = useRef(null)
  const dialogRef = useRef(null)
  useFocusTrap(dialogRef, visible)

  const fetchMatrix = useCallback(async () => {
    requestRef.current?.abort()
    const controller = new AbortController()
    requestRef.current = controller
    try {
      const res = await apiFetch('/api/latency-matrix', { signal: controller.signal })
      if (!res.ok) throw new Error(`HTTP ${res.status}`)
      const d = await res.json()
      if (requestRef.current !== controller || controller.signal.aborted) return null
      setData(d)
      return d
    } catch {
      // ignore fetch errors — the table just keeps showing what it has
      return null
    } finally {
      if (requestRef.current === controller) requestRef.current = null
    }
  }, [])

  useEffect(() => {
    if (visible) fetchMatrix()
    return () => {
      requestRef.current?.abort()
      requestRef.current = null
      measurementRef.current?.abort()
      measurementRef.current = null
      setMeasuring(false)
    }
  }, [visible, fetchMatrix])

  const runMeasurement = useCallback(async () => {
    if (measurementRef.current || !canMeasure) return
    const controller = new AbortController()
    measurementRef.current = controller
    setNotice('')
    setMeasuring(true)
    const startedAt = Date.now()

    try {
      const res = await apiFetch('/api/latency-matrix/measure', { method: 'POST', signal: controller.signal })
      let body = {}
      try {
        body = await res.json()
      } catch {
        body = {}
      }

      if (controller.signal.aborted) return
      if (res.status === 200 && body.started === false) {
        setNotice(body.reason || 'Nothing to measure.')
        return
      }
      if (!res.ok && res.status !== 409) {
        setNotice(body.error || `Could not start a measurement (HTTP ${res.status}).`)
        return
      }
      if (res.status === 409) {
        // Someone else (or the interval ticker) is already measuring; poll
        // along with it instead of reporting an error.
        setNotice('A measurement was already running — showing its results.')
      }

      const expected = typeof body.pairs === 'number' ? body.pairs : 0
      const deadline = startedAt + POLL_TIMEOUT_MS
      // Poll until every expected cell is fresher than the moment we asked,
      // or until the run has had long enough.
      for (;;) {
        await sleep(POLL_INTERVAL_MS)
        if (controller.signal.aborted) return
        const d = await fetchMatrix()
        if (controller.signal.aborted) return
        if (expected > 0 && countFreshCells(d, startedAt) >= expected) break
        if (Date.now() > deadline) break
      }
    } catch (err) {
      if (controller.signal.aborted) return
      console.error('Latency measurement failed:', err)
      setNotice('Could not start a measurement.')
    } finally {
      if (measurementRef.current === controller) {
        measurementRef.current = null
        setMeasuring(false)
      }
    }
  }, [fetchMatrix, canMeasure])

  // Stay mounted through the close transition so the modal can animate out.
  const { mounted, state } = useMountTransition(visible)
  if (!mounted) return null

  const nodes = data?.nodes || []
  const latency = data?.latency || {}

  return (
    <div className="fixed inset-0 z-50 flex items-center justify-center p-4" onClick={onClose}>
      <div data-state={state} className="motion-backdrop absolute inset-0 bg-black/60 backdrop-blur-sm" />
      <div
        ref={dialogRef}
        role="dialog"
        aria-modal="true"
        aria-label="Latency Matrix"
        tabIndex={-1}
        data-state={state}
        className="motion-modal relative w-full max-w-4xl max-h-[85vh] rounded-2xl border border-border/50
          bg-bg-elevated shadow-2xl shadow-black/50 overflow-hidden flex flex-col"
        onClick={(e) => e.stopPropagation()}
      >
        <div className="flex items-center justify-between px-6 py-4 border-b border-border/30">
          <div className="flex items-center gap-2">
            <Grid3x3 size={16} className="text-cyan" />
            <span className="text-sm font-semibold text-text-primary">Latency Matrix</span>
            <span className="text-[11px] text-text-muted">(measured server-side between nodes)</span>
          </div>
          <div className="flex items-center gap-2">
            {canMeasure && <button
              onClick={runMeasurement}
              disabled={measuring || nodes.length < 2}
              className="flex items-center gap-1.5 px-3 py-1.5 rounded-lg text-xs font-medium
                bg-cyan/15 text-cyan hover:bg-cyan/25
                disabled:opacity-40 transition-[background-color,opacity] duration-200 cursor-pointer"
            >
              {measuring ? <Loader size={12} className="animate-spin" /> : <RefreshCw size={12} />}
              {measuring ? 'Measuring...' : 'Measure now'}
            </button>}
            <button onClick={onClose}
              className="text-xs text-text-muted hover:text-text-primary px-2 py-1 rounded-lg hover:bg-hover-overlay transition-colors cursor-pointer">
              Close
            </button>
          </div>
        </div>

        <div className="flex-1 overflow-auto p-5">
          {notice ? (
            <div className="mb-3 text-[11px] text-text-muted">{notice}</div>
          ) : null}
          {nodes.length < 2 ? (
            <div className="text-center py-12 text-text-muted text-sm">
              Need at least 2 nodes to build a latency matrix.
            </div>
          ) : (
            <div className="overflow-x-auto">
              <table className="w-full border-collapse text-xs">
                <thead>
                  <tr>
                    <th className="p-2 text-left text-text-muted font-medium">From ↓ / To →</th>
                    {nodes.map((n) => (
                      <th key={n.id} className="p-2 text-center text-text-muted font-medium min-w-[80px]">
                        <span className="mr-1">{n.flag}</span>
                        {n.name}
                      </th>
                    ))}
                  </tr>
                </thead>
                <tbody>
                  {nodes.map((from) => (
                    <tr key={from.id} className="border-t border-border/20">
                      <td className="p-2 text-text-primary font-medium">
                        <span className="mr-1">{from.flag}</span>
                        {from.name}
                      </td>
                      {nodes.map((to) => {
                        if (from.id === to.id) {
                          return (
                            <td key={to.id} className="p-2 text-center">
                              <span className="text-text-muted">—</span>
                            </td>
                          )
                        }
                        return (
                          <td key={to.id} className="p-2 text-center">
                            <MatrixCell cell={latency[from.id]?.[to.id]} />
                          </td>
                        )
                      })}
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
          )}
        </div>
      </div>
    </div>
  )
}

function MatrixCell({ cell }) {
  const ms = Number(cell?.latency_ms)
  if (cell == null || !Number.isFinite(ms)) {
    return <span className="text-text-muted">?</span>
  }
  const measuredAt = parseTime(cell.measured_at)
  const age = measuredAt == null ? null : Date.now() - measuredAt
  const stale = age != null && age > STALE_AFTER_MS
  const title = measuredAt == null
    ? undefined
    : `measured ${new Date(measuredAt).toLocaleString()}${cell.source ? ` (${cell.source})` : ''}`

  return (
    <span className={`flex flex-col items-center ${stale ? 'opacity-40' : ''}`} title={title}>
      <span className={`font-mono font-medium tabular-nums ${getLatencyColor(ms)}`}>
        {ms.toFixed(1)}ms
      </span>
      {age != null ? (
        <span className="text-[10px] text-text-muted">{formatAge(age)}</span>
      ) : null}
    </span>
  )
}

function sleep(ms) {
  return new Promise((resolve) => setTimeout(resolve, ms))
}

function parseTime(value) {
  if (typeof value !== 'string' || value === '') return null
  const t = Date.parse(value)
  if (!Number.isFinite(t) || t <= 0) return null
  return t
}

// countFreshCells counts the cells measured at or after `since` — how the
// poller knows the run it asked for has landed.
function countFreshCells(data, since) {
  const latency = data?.latency
  if (!latency) return 0
  let n = 0
  for (const row of Object.values(latency)) {
    for (const cell of Object.values(row || {})) {
      const t = parseTime(cell?.measured_at)
      // One second of slack: the server stamps in UTC from its own clock.
      if (t != null && t >= since - 1000) n++
    }
  }
  return n
}

function formatAge(ms) {
  if (ms < 0) return 'measured just now'
  const secs = Math.floor(ms / 1000)
  if (secs < 60) return 'measured just now'
  const mins = Math.floor(secs / 60)
  if (mins < 60) return `measured ${mins}m ago`
  const hours = Math.floor(mins / 60)
  if (hours < 24) return `measured ${hours}h ago`
  return `measured ${Math.floor(hours / 24)}d ago`
}

function getLatencyColor(ms) {
  if (ms < 10) return 'text-success'
  if (ms < 50) return 'text-cyan'
  if (ms < 100) return 'text-warning'
  return 'text-danger'
}
