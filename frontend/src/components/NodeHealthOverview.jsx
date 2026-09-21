import FleetReadiness from './FleetReadiness'
import AgentVersion from './AgentVersion'
import { useState, useEffect, useCallback, useRef } from 'react'
import { Activity, Cpu, HardDrive, Clock, Wifi, WifiOff, RefreshCw } from 'lucide-react'
import { useFocusTrap } from '../hooks/useFocusTrap'
import { motionStateClass, useMountTransition } from '../hooks/useMountTransition'
import { apiFetch } from '../lib/api'

export default function NodeHealthOverview({ visible, onClose }) {
  const [nodes, setNodes] = useState([])
  const [error, setError] = useState('')
  // Spin the refresh control for both manual refresh and periodic polling.
  const [refreshing, setRefreshing] = useState(false)
  const dialogRef = useRef(null)
  const requestRef = useRef(null)

  const fetchHealth = useCallback(async () => {
    requestRef.current?.abort()
    const controller = new AbortController()
    requestRef.current = controller
    setRefreshing(true)
    try {
      const res = await apiFetch('/api/nodes/health', { signal: controller.signal })
      if (!res.ok) throw new Error(`HTTP ${res.status}`)
      const data = await res.json()
      if (requestRef.current === controller && !controller.signal.aborted) { setNodes(data || []); setError('') }
    } catch (err) {
      if (!controller.signal.aborted) setError('Couldn’t refresh node health. Displayed data may be stale; try Refresh node health.')
    } finally {
      if (requestRef.current === controller) {
        requestRef.current = null
        setRefreshing(false)
      }
    }
  }, [])

  useFocusTrap(dialogRef, visible)

  useEffect(() => {
    if (!visible) return
    fetchHealth()
    const iv = setInterval(() => { if (!requestRef.current) fetchHealth() }, 15000)
    return () => {
      clearInterval(iv)
      requestRef.current?.abort()
      requestRef.current = null
    }
  }, [visible, fetchHealth])

  // Stay mounted through the close transition so the modal can animate out.
  const { mounted, state } = useMountTransition(visible)
  if (!mounted) return null

  return (
    <div data-state={state} className="motion-layer fixed inset-0 z-50 flex items-center justify-center p-4" onClick={onClose}>
      <div data-state={state} className="motion-backdrop absolute inset-0 bg-black/60 backdrop-blur-sm" />
      <div
        ref={dialogRef}
        role="dialog"
        aria-modal="true"
        aria-label="Node Health Overview"
        tabIndex={-1}
        data-state={state}
        inert={state === 'closing'}
        aria-hidden={state === 'closing'}
        className={`t-modal ${motionStateClass(state)} relative w-full max-w-3xl max-h-[80vh] rounded-2xl border border-border/50
          bg-bg-elevated shadow-2xl shadow-black/50 overflow-hidden flex flex-col`}
        onClick={(e) => e.stopPropagation()}
      >
        {/* Header */}
        <div className="flex items-center justify-between px-6 py-4 border-b border-border/30">
          <div className="flex items-center gap-2">
            <Activity size={16} className="text-success" />
            <span className="text-sm font-semibold text-text-primary">Node Health Overview</span>
          </div>
          <div className="flex items-center gap-2">
            <button onClick={fetchHealth}
              aria-label="Refresh node health"
              className="p-1.5 rounded-lg hover:bg-hover-overlay transition-colors text-text-muted hover:text-text-primary cursor-pointer">
              <RefreshCw size={14} className={refreshing ? 'animate-spin' : ''} />
            </button>
            <button onClick={onClose}
              className="text-xs text-text-muted hover:text-text-primary px-2 py-1 rounded-lg hover:bg-hover-overlay transition-colors cursor-pointer">
              Close
            </button>
          </div>
        </div>

        {/* Node list */}
        <div className="flex-1 overflow-y-auto p-4">
          {error && <p role="alert" className="text-sm text-warning mb-3">{error}</p>}
          {nodes.length > 0 && <FleetReadiness nodes={nodes} />}
          {nodes.length === 0 ? (
            <div className="text-center py-12 text-text-muted text-sm">{refreshing ? 'Loading node health…' : error ? 'Node health unavailable' : 'No nodes registered'}</div>
          ) : (
            <div className="grid gap-3">
              {nodes.map((node) => (
                <NodeCard key={node.id} node={node} />
              ))}
            </div>
          )}
        </div>
      </div>
    </div>
  )
}

function NodeCard({ node }) {
  const lastSeen = node.last_seen ? timeSince(new Date(node.last_seen)) : 'never'
  const h = node.health

  return (
    <div data-testid="node-health-card" className={`rounded-xl border p-4 transition-colors duration-200
      ${node.online
        ? 'border-border/40 bg-bg-secondary/40'
        : 'border-danger/20 bg-danger/5'}`}>
      <div className="flex items-center gap-3 mb-3">
        <span className="text-lg">{node.flag}</span>
        <div className="flex-1 min-w-0">
          <div className="text-sm font-semibold text-text-primary">{node.name}</div>
          <div className="text-xs text-text-muted">{node.location} · {node.provider}</div>
          {/* Agent build identity, so a node still running an old binary is
              visible here. Always rendered — "unknown" is itself the signal. */}
          <div className="text-[11px] text-text-muted font-mono" title="Agent version">
            <span className="sr-only">Agent version </span>
            <AgentVersion version={node.version} />
          </div>
        </div>
        <div className="flex items-center gap-1.5">
          {node.online ? (
            <span className="flex items-center gap-1 px-2 py-0.5 rounded-full text-[11px] font-medium bg-success-muted text-success">
              <Wifi size={10} /> Online
            </span>
          ) : (
            <span className="flex items-center gap-1 px-2 py-0.5 rounded-full text-[11px] font-medium bg-danger-muted text-danger">
              <WifiOff size={10} /> Offline
            </span>
          )}
        </div>
      </div>

      {h ? (
        <div className="grid grid-cols-4 gap-3">
          <Stat icon={<Cpu size={12} />} label="CPUs" value={h.cpus} color="text-accent-text" />
          <Stat icon={<HardDrive size={12} />} label="Memory" value={`${(h.mem_used_pc ?? 0).toFixed(0)}%`} color="text-cyan" />
          <Stat icon={<Activity size={12} />} label="Load" value={h.load_avg || 'N/A'} color="text-warning" />
          <Stat icon={<Clock size={12} />} label="Last seen" value={lastSeen} color="text-text-muted" />
        </div>
      ) : (
        <div className="text-xs text-text-muted">
          {node.online ? 'Waiting for health data...' : `Last seen: ${lastSeen}`}
        </div>
      )}
    </div>
  )
}

function Stat({ icon, label, value, color }) {
  return (
    <div className="flex items-center gap-1.5">
      <span className={color}>{icon}</span>
      <div>
        <div className="text-[10px] text-text-muted">{label}</div>
        <div className="text-xs font-medium text-text-primary tabular-nums">{value}</div>
      </div>
    </div>
  )
}

function timeSince(date) {
  const s = Math.floor((Date.now() - date.getTime()) / 1000)
  if (s < 60) return `${s}s ago`
  if (s < 3600) return `${Math.floor(s / 60)}m ago`
  if (s < 86400) return `${Math.floor(s / 3600)}h ago`
  return `${Math.floor(s / 86400)}d ago`
}
