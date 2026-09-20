import { useState, useEffect, useCallback, useRef } from 'react'
import { Clock, Play, Trash2, Power, RefreshCw, Plus } from 'lucide-react'
import SummaryBadges from './SummaryBadges'
import { useFocusTrap } from '../hooks/useFocusTrap'
import { useMountTransition } from '../hooks/useMountTransition'
import { apiFetch } from '../lib/api'
import { isToolAvailable, unavailableTitle, unavailableLabel } from '../lib/capabilities'

// Must stay in step with scheduleAllowedCommands (backend/scheduler.go).
// `download` is deliberately absent: the server refuses to schedule it (F10).
const ALLOWED_COMMANDS = [
  { id: 'ping', label: 'ping' },
  { id: 'traceroute', label: 'traceroute' },
  { id: 'mtr', label: 'mtr' },
  { id: 'dns', label: 'dns A' },
  { id: 'http', label: 'http probe' },
  { id: 'tcp', label: 'tcp connect' },
  { id: 'tls', label: 'tls certificate' },
  { id: 'dnsbench', label: 'dns benchmark' },
]

// What each schedulable command's target box expects.
const SCHEDULE_TARGET_PLACEHOLDERS = {
  tcp: 'host:port (e.g. example.com:443)',
  tls: 'host or host:port (e.g. example.com)',
  dnsbench: 'name.example.com',
  dns: 'domain name (e.g. example.com)',
  http: 'URL or hostname',
}

// Common interval presets covering the realistic monitoring range.
// 60s minimum matches the server's scheduleMinInterval guard.
const INTERVAL_PRESETS = [
  { label: '1 min', sec: 60 },
  { label: '5 min', sec: 300 },
  { label: '15 min', sec: 900 },
  { label: '1 hour', sec: 3600 },
]

// How many history points the sparkline shows. The server keeps 288 (24 h at
// one run per 5 min) and serves them from /api/schedules/<id>/history — the
// list response deliberately omits them.
const SPARK_POINTS = 48

// One colour per status. degraded is deliberately a different hue from error:
// "some packets lost" and "the probe failed" are different operational states.
// Unknown statuses fall back to muted — the map is also what keeps a
// server-supplied string out of the DOM as anything but a lookup key.
const STATUS_FILL = {
  ok: 'var(--color-success)',
  degraded: 'var(--color-warning)',
  error: 'var(--color-danger)',
  agent_offline: 'var(--color-danger)',
  running: 'var(--color-cyan)',
}

const STATUS_TEXT = {
  ok: 'text-success',
  degraded: 'text-warning',
  error: 'text-danger',
  agent_offline: 'text-danger',
  running: 'text-cyan',
}

// Same numeric guard the summary badges use: anything that is not a finite
// number is treated as "no value" rather than rendered.
function numberOrNull(value) {
  if (value === null || value === undefined || value === '' || typeof value === 'boolean') return null
  const n = Number(value)
  return isFinite(n) ? n : null
}

function fmtMs(n) {
  return Number.isInteger(n) ? String(n) : String(Math.round(n * 100) / 100)
}

// Sparkline is an inline SVG (no chart dependency): one bar per run, height
// scaled by RTT when the probe reports one, colour by status. Probes without
// an RTT (http, dns, mtr) still get a full-height status strip, so gaps and
// failures stay visible.
function Sparkline({ points }) {
  const shown = points.slice(-SPARK_POINTS)
  if (shown.length === 0) return null
  const w = 168
  const h = 26
  const gap = shown.length > 24 ? 0.5 : 1
  const bw = Math.max(1, w / shown.length - gap)
  const rtts = shown.map((p) => numberOrNull(p.rtt_ms)).filter((v) => v !== null && v >= 0)
  const max = rtts.length ? Math.max(...rtts) : 0

  return (
    <svg
      viewBox={`0 0 ${w} ${h}`}
      width={w}
      height={h}
      role="img"
      aria-label={`Last ${shown.length} runs`}
      className="mt-2 block"
      preserveAspectRatio="none"
    >
      {shown.map((p, i) => {
        const v = numberOrNull(p.rtt_ms)
        const frac = max > 0 && v !== null && v >= 0 ? Math.max(0.1, Math.min(1, v / max)) : 1
        const bh = Math.max(2, frac * h)
        return (
          <rect
            key={i}
            x={i * (bw + gap)}
            y={h - bh}
            width={bw}
            height={bh}
            rx={0.5}
            fill={STATUS_FILL[p.status] || 'var(--color-text-muted)'}
            opacity={v === null && max > 0 ? 0.45 : 0.9}
          />
        )
      })}
    </svg>
  )
}

export default function Schedules({ visible, onClose, nodes }) {
  const [items, setItems] = useState([])
  const [loading, setLoading] = useState(true)
  const [creating, setCreating] = useState(false)
  const [form, setForm] = useState({ node_id: '', command: 'ping', target: '', interval_sec: 300 })
  const [err, setErr] = useState('')
  const [pendingActions, setPendingActions] = useState(new Set())
  const pendingRef = useRef(new Set())
  const fetchRef = useRef(null)
  const beginAction = (key) => {
    if (pendingRef.current.has(key)) return false
    pendingRef.current.add(key)
    setPendingActions(new Set(pendingRef.current))
    return true
  }
  const endAction = (key) => {
    pendingRef.current.delete(key)
    setPendingActions(new Set(pendingRef.current))
  }
  const dialogRef = useRef(null)
  useFocusTrap(dialogRef, visible)

  const fetchSchedules = useCallback(async () => {
    fetchRef.current?.abort()
    const controller = new AbortController()
    fetchRef.current = controller
    setLoading(true)
    try {
      const r = await apiFetch('/api/schedules', { signal: controller.signal })
      if (!r.ok) throw new Error(`HTTP ${r.status}`)
      const items = await r.json()
      if (fetchRef.current !== controller || controller.signal.aborted) return
      setItems(items || [])
      setErr('')
    } catch (e) {
      if (fetchRef.current === controller && !controller.signal.aborted) setErr(e.message)
    } finally {
      if (fetchRef.current === controller) {
        fetchRef.current = null
        setLoading(false)
      }
    }
  }, [])

  useEffect(() => {
    if (!visible) return
    setLoading(true)
    fetchSchedules()
    // Poll while modal is open so users see "running" → "ok" transitions
    // and refreshed last_run_at timestamps without manual refresh.
    const iv = setInterval(() => { if (!fetchRef.current) fetchSchedules() }, 5000)
    return () => {
      clearInterval(iv)
      fetchRef.current?.abort()
      fetchRef.current = null
    }
  }, [visible, fetchSchedules])

  const onlineNodes = nodes.filter((n) => n.online)

  // A schedule targets exactly one node, so the single-node rule applies: grey
  // out the commands the node picked in the form reported it cannot run.
  const formNode = onlineNodes.find((n) => n.id === form.node_id) || null

  // Default the create form to the first online node so it's not empty.
  useEffect(() => {
    if (visible && !form.node_id && onlineNodes.length > 0) {
      setForm((f) => ({ ...f, node_id: onlineNodes[0].id }))
    }
  }, [visible, onlineNodes, form.node_id])

  const create = async () => {
    setErr('')
    if (!form.node_id || !form.command || (!form.target && form.command !== 'speedtest')) {
      setErr('Pick a node, command, and target.')
      return
    }
    if (!beginAction('create')) return
    try {
      const r = await apiFetch('/api/schedules', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify(form),
      })
      if (!r.ok) {
        const body = await r.json().catch(() => ({}))
        throw new Error(body.error || `HTTP ${r.status}`)
      }
      setForm((f) => ({ ...f, target: '' }))
      setCreating(false)
      await fetchSchedules()
    } catch (e) {
      setErr(e.message)
    } finally {
      endAction('create')
    }
  }

  const action = async (id, op) => {
    if (!beginAction(id)) return
    setErr('')
    try {
      const url = op === 'delete' ? `/api/schedules/${id}` : `/api/schedules/${id}/${op}`
      const r = await apiFetch(url, { method: op === 'delete' ? 'DELETE' : 'POST' })
      if (!r.ok) {
        const body = await r.json().catch(() => ({}))
        throw new Error(body.error || `HTTP ${r.status}`)
      }
      await fetchSchedules()
    } catch (e) {
      setErr(e.message)
    } finally {
      endAction(id)
    }
  }

  // Stay mounted through the close transition so the modal can animate out.
  const { mounted, state } = useMountTransition(visible)
  if (!mounted) return null

  return (
    <div className="fixed inset-0 z-50 flex items-center justify-center p-4" onClick={onClose}>
      <div data-state={state} className="motion-backdrop absolute inset-0 bg-black/60 backdrop-blur-sm" />
      <div
        ref={dialogRef}
        role="dialog"
        aria-modal="true"
        aria-label="Scheduled probes"
        tabIndex={-1}
        data-state={state}
        className="motion-modal relative w-full max-w-3xl max-h-[85vh] rounded-2xl border border-border/50
          bg-bg-elevated shadow-2xl shadow-black/50 overflow-hidden flex flex-col"
        onClick={(e) => e.stopPropagation()}
      >
        <div className="flex items-center justify-between px-6 py-4 border-b border-border/30">
          <div className="flex items-center gap-2">
            <Clock size={16} className="text-cyan" />
            <span className="text-sm font-semibold text-text-primary">Scheduled Probes</span>
            <span className="text-[11px] text-text-muted">(run periodically without a browser open)</span>
          </div>
          <div className="flex items-center gap-2">
            <button onClick={fetchSchedules}
              aria-label="Refresh"
              className="p-1.5 rounded-lg hover:bg-hover-overlay text-text-muted hover:text-text-primary cursor-pointer">
              <RefreshCw size={14} className={loading ? 'animate-spin' : ''} />
            </button>
            <button onClick={onClose}
              className="text-xs text-text-muted hover:text-text-primary px-2 py-1 rounded-lg hover:bg-hover-overlay cursor-pointer">
              Close
            </button>
          </div>
        </div>

        <div className="flex-1 overflow-y-auto p-4 space-y-3">
          {err && (
            <div className="px-3 py-2 rounded-lg border border-danger/30 bg-danger/5 text-xs text-danger">
              {err}
            </div>
          )}

          {creating ? (
            <div className="rounded-xl border border-accent/30 bg-accent/5 p-3 space-y-2">
              <div className="text-xs font-semibold text-accent-text uppercase tracking-widest">New schedule</div>
              <fieldset disabled={pendingActions.has('create')} className="flex flex-wrap gap-2 items-center">
                <select value={form.node_id} onChange={(e) => setForm({ ...form, node_id: e.target.value })}
                  aria-label="Node"
                  className="px-2 py-1 rounded text-xs bg-bg-secondary/60 border border-border/40 text-text-primary outline-none">
                  {onlineNodes.map((n) => <option key={n.id} value={n.id}>{n.flag} {n.name}</option>)}
                </select>
                <select value={form.command} onChange={(e) => setForm({ ...form, command: e.target.value })}
                  aria-label="Command"
                  className="px-2 py-1 rounded text-xs bg-bg-secondary/60 border border-border/40 text-text-primary outline-none">
                  {ALLOWED_COMMANDS.map((c) => {
                    const missing = !isToolAvailable(formNode, c.id)
                    return (
                      <option key={c.id} value={c.id} disabled={missing}
                        title={unavailableTitle(formNode, c.id)}>
                        {c.label}{missing ? ` (${unavailableLabel(formNode, c.id) || 'unavailable'})` : ''}
                      </option>
                    )
                  })}
                </select>
                <input type="text" value={form.target}
                  onChange={(e) => setForm({ ...form, target: e.target.value })}
                  placeholder={SCHEDULE_TARGET_PLACEHOLDERS[form.command] || 'target (e.g. 1.1.1.1)'}
                  aria-label="Target"
                  className="px-2 py-1 rounded text-xs bg-bg-secondary/60 border border-border/40 text-text-primary outline-none flex-1 min-w-[180px]" />
                <select value={form.interval_sec}
                  onChange={(e) => setForm({ ...form, interval_sec: parseInt(e.target.value, 10) })}
                  aria-label="Interval"
                  className="px-2 py-1 rounded text-xs bg-bg-secondary/60 border border-border/40 text-text-primary outline-none">
                  {INTERVAL_PRESETS.map((p) => <option key={p.sec} value={p.sec}>{p.label}</option>)}
                </select>
                <button onClick={create}
                  className="px-3 py-1 rounded text-xs font-semibold bg-accent text-white hover:bg-accent-hover cursor-pointer">
                  Create
                </button>
                <button onClick={() => { setCreating(false); setErr('') }}
                  className="px-2 py-1 rounded text-xs text-text-muted hover:text-text-primary cursor-pointer">
                  Cancel
                </button>
              </fieldset>
            </div>
          ) : (
            <button onClick={() => setCreating(true)}
              className="flex items-center gap-1.5 px-3 py-1.5 rounded-lg text-xs font-medium
                bg-accent/15 border border-accent/30 text-accent-text hover:bg-accent/25 cursor-pointer">
              <Plus size={12} /> New schedule
            </button>
          )}

          {items.length === 0 && !loading && !creating ? (
            <div className="text-center py-8 text-text-muted text-sm">
              No schedules yet. Create one above to start probing on a timer.
            </div>
          ) : (
            items.map((sc) => <ScheduleCard key={sc.id} sc={sc} action={action} pending={pendingActions.has(sc.id)} />)
          )}
        </div>
      </div>
    </div>
  )
}

function ScheduleCard({ sc, action, pending }) {
  const next = sc.next_run_at ? new Date(sc.next_run_at).getTime() : 0
  const last = sc.last_run_at ? new Date(sc.last_run_at).getTime() : 0
  const inSec = next ? Math.max(0, Math.round((next - Date.now()) / 1000)) : null
  const ago = last ? Math.max(0, Math.round((Date.now() - last) / 1000)) : null
  const [history, setHistory] = useState([])

  // The history lives behind its own endpoint (the 5 s list poll must stay
  // small), so refetch it only when a new run has finalized.
  useEffect(() => {
    let cancelled = false
    apiFetch(`/api/schedules/${encodeURIComponent(sc.id)}/history`)
      .then((r) => (r.ok ? r.json() : null))
      .then((body) => {
        if (cancelled || !body || !Array.isArray(body.history)) return
        setHistory(body.history)
      })
      .catch(() => {
        // A history that cannot be fetched just means no sparkline.
      })
    return () => { cancelled = true }
  }, [sc.id, sc.last_run_at, sc.last_status])

  const statusClass = STATUS_TEXT[sc.last_status] || 'text-text-muted'
  const lastPoint = history.length ? history[history.length - 1] : null
  const lastLoss = lastPoint ? numberOrNull(lastPoint.loss_pct) : null
  const lastRtt = lastPoint ? numberOrNull(lastPoint.rtt_ms) : null
  const running = sc.last_status === 'running'
  const runDisabled = pending || !sc.enabled || running

  return (
    <div className={`rounded-xl border p-3 ${sc.enabled ? 'border-border/40 bg-bg-secondary/40' : 'border-border/20 bg-bg-secondary/10 opacity-60'}`}>
      <div className="flex items-start justify-between gap-3">
        <div className="min-w-0 flex-1">
          <div className="flex items-center gap-2 flex-wrap">
            <span className="text-xs font-semibold text-text-primary">{sc.node_name || sc.node_id.slice(0, 8)}</span>
            <span className="text-[10px] uppercase tracking-widest text-accent-text">{sc.command}</span>
            <span className="text-xs text-text-secondary truncate">{sc.target}</span>
            {sc.options ? <span className="text-[10px] text-text-muted">({sc.options})</span> : null}
            <span className="text-[10px] text-text-muted">every {sc.interval_sec}s</span>
          </div>
          <div className="mt-1 flex items-center gap-3 text-[11px]">
            <span className={statusClass}>● {sc.last_status || 'pending'}</span>
            {ago !== null && <span className="text-text-muted">ran {ago}s ago</span>}
            {inSec !== null && sc.enabled && <span className="text-text-muted">next in {inSec}s</span>}
          </div>
          {/* last_summary is already a JSON object here — the schedules API
              serves it as one. Absent for runs that failed or produced no
              parsable output. */}
          {sc.last_summary && <SummaryBadges summary={sc.last_summary} className="mt-2" />}
          {history.length > 0 && (
            <div>
              <Sparkline points={history} />
              <div className="mt-1 flex items-center gap-3 text-[10px] text-text-muted">
                <span>last {Math.min(history.length, SPARK_POINTS)} runs</span>
                {lastLoss !== null && (
                  <span className={lastLoss >= 100 ? 'text-danger' : lastLoss >= 20 ? 'text-warning' : 'text-success'}>
                    {fmtMs(lastLoss)}% loss
                  </span>
                )}
                {lastRtt !== null && <span>avg {fmtMs(lastRtt)} ms</span>}
              </div>
            </div>
          )}
          {sc.last_result && (
            <pre className="mt-2 px-2 py-1.5 rounded bg-bg-tertiary/40 text-[11px] text-text-secondary
              font-mono whitespace-pre-wrap break-all max-h-32 overflow-y-auto">{sc.last_result}</pre>
          )}
        </div>
        <div className="flex flex-col gap-1 shrink-0">
          {/* The server answers 409 for both cases (a disabled schedule and
              one whose run is still in flight), so the button is disabled
              rather than offering an action that can only fail. */}
          <button onClick={() => action(sc.id, 'run')}
            disabled={runDisabled}
            title={!sc.enabled ? 'Enable the schedule to run it' : running ? 'A run is already in flight' : 'Run now'}
            aria-label="Run now"
            className={`p-1.5 rounded ${runDisabled
              ? 'text-text-muted cursor-not-allowed'
              : 'text-text-muted hover:text-success hover:bg-success/10 cursor-pointer'}`}>
            <Play size={12} />
          </button>
          <button onClick={() => action(sc.id, 'toggle')} disabled={pending}
            title={sc.enabled ? 'Disable' : 'Enable'}
            aria-label={sc.enabled ? 'Disable' : 'Enable'}
            className={`p-1.5 rounded hover:bg-hover-overlay cursor-pointer ${sc.enabled ? 'text-success' : 'text-text-muted'}`}>
            <Power size={12} />
          </button>
          <button onClick={() => action(sc.id, 'delete')} disabled={pending}
            title="Delete"
            aria-label="Delete"
            className="p-1.5 rounded text-text-muted hover:text-danger hover:bg-danger/10 cursor-pointer">
            <Trash2 size={12} />
          </button>
        </div>
      </div>
    </div>
  )
}
