import { commandSucceeded } from '../lib/commandResult'
import { randomId } from '../lib/id'
import { useState, useEffect, useRef, useCallback } from 'react'
import { Columns3, Play, Square, Globe, X } from 'lucide-react'
import SummaryBadges, { parseSummary } from './SummaryBadges'
import { useFocusTrap } from '../hooks/useFocusTrap'
import { motionStateClass, useMountTransition } from '../hooks/useMountTransition'
import { isToolAvailable, unavailableTitle, unavailableLabel, NATIVE_COMMAND_TYPES } from '../lib/capabilities'
// The per-tool option descriptors come from CommandBar rather than a copy, so
// the two surfaces cannot drift — the same class of hazard as the
// command-whitelist-sync invariant between the server and the agent.
import { COMMAND_OPTIONS, IP_VERSIONS, IP_VERSION_COMMANDS, encodeOptions, targetPlaceholder } from './CommandBar'

const COMMANDS = [
  { id: 'ping', label: 'ping' },
  { id: 'traceroute', label: 'traceroute' },
  { id: 'mtr', label: 'mtr' },
  { id: 'dns', label: 'dns lookup' },
  // The four Go-native probes (S7), same set as CommandBar. They take no
  // options, so the option row below simply renders nothing for them.
  { id: 'tcp', label: 'tcp connect' },
  { id: 'tls', label: 'tls certificate' },
  { id: 'dnsbench', label: 'dns benchmark' },
  { id: 'download', label: 'download speed' },
]

const MAX_LINES_PER_NODE = 2000
// Match the agent's 10-minute command budget, plus cleanup grace. Valid
// 100-packet pings and long traces must not be cancelled after just a minute.
const NODE_TIMEOUT_MS = 10 * 60_000 + 15_000

export default function MultiNodeCompare({ visible, onClose, nodes, wsRef, allowedCommands = null, allowedTargets = null }) {
  const [selectedNodes, setSelectedNodes] = useState([])
  const [command, setCommand] = useState('ping')
  const visibleCommands = allowedCommands === null ? COMMANDS : COMMANDS.filter((c) => allowedCommands.includes(c.id))
  const commandDisallowed = allowedCommands !== null && !allowedCommands.includes(command)
  const [target, setTarget] = useState('')
  const targetDisallowed = allowedTargets !== null && !allowedTargets.includes(target.trim())

  useEffect(() => {
    if (commandDisallowed && visibleCommands.length > 0) setCommand(visibleCommands[0].id)
  }, [allowedCommands, commandDisallowed])
  const [ipVersion, setIpVersion] = useState('Auto')
  // Per-tool option values, keyed by command then option key — same shape as
  // CommandBar, so `count` for ping and `count` for mtr keep their own bounds.
  const [optValues, setOptValues] = useState({})
  const [running, setRunning] = useState(false)
  // Offline chips disappear from the picker. Drop their selections while idle
  // so an invisible node cannot be dispatched repeatedly or keep Run enabled.
  useEffect(() => {
    if (running) return
    setSelectedNodes((previous) => {
      const next = previous.filter((id) => nodes.some((node) => node.id === id && node.online))
      return next.length === previous.length ? previous : next
    })
  }, [nodes, running])
  const [results, setResults] = useState({}) // nodeId -> lines[]
  const [summaries, setSummaries] = useState({}) // nodeId -> parsed summary
  const cmdIds = useRef({})
  // Reverse lookup: cmdId -> nodeId for O(1) message routing
  const cmdToNode = useRef({})
  const lineIdCounter = useRef(0)
  // Per-node command-start timestamps so we can mark a node as timed out if
  // the agent never sends a `done` (e.g., agent crashed mid-command).
  const cmdStartTimes = useRef({})
  const commandErrors = useRef(new Set())
  const dialogRef = useRef(null)
  useFocusTrap(dialogRef, visible)
  const toggleNode = (id) => {
    setSelectedNodes((prev) =>
      prev.includes(id) ? prev.filter((n) => n !== id) : [...prev, id]
    )
  }

  // Cancel running commands on close or unmount
  const handleClose = useCallback(() => {
    if (running) {
      Object.entries(cmdIds.current).forEach(([nodeId, cmdId]) => {
        wsRef.send({ action: 'cancel', node_id: nodeId, command_id: cmdId })
      })
      cmdIds.current = {}
      cmdToNode.current = {}
      cmdStartTimes.current = {}
      pendingLines.current = {}
      flushScheduled.current = false
      setRunning(false)
    }
    onClose()
  }, [running, wsRef, onClose])

  // Cancel running commands when modal is hidden (e.g., Escape key)
  useEffect(() => {
    if (!visible && running) {
      Object.entries(cmdIds.current).forEach(([nodeId, cmdId]) => {
        wsRef.send({ action: 'cancel', node_id: nodeId, command_id: cmdId })
      })
      cmdIds.current = {}
      cmdToNode.current = {}
      cmdStartTimes.current = {}
      pendingLines.current = {}
      flushScheduled.current = false
      setRunning(false)
    }
  }, [visible, running, wsRef])

  // Reset running state on disconnect so UI doesn't get stuck
  const prevConnected = useRef(wsRef?.connected)
  useEffect(() => {
    if (prevConnected.current && !wsRef?.connected && running) {
      cmdIds.current = {}
      cmdToNode.current = {}
      cmdStartTimes.current = {}
      pendingLines.current = {}
      flushScheduled.current = false
      setRunning(false)
    }
    prevConnected.current = wsRef?.connected
  }, [wsRef?.connected, running])

  // Ensure running commands are cancelled if component unmounts
  // Capture wsRef at mount time to avoid stale reference in cleanup
  const mountedWsRef = useRef(wsRef)
  useEffect(() => {
    mountedWsRef.current = wsRef
  }, [wsRef])

  useEffect(() => {
    const capturedWs = mountedWsRef.current
    return () => {
      Object.entries(cmdIds.current).forEach(([nodeId, cmdId]) => {
        capturedWs.send({ action: 'cancel', node_id: nodeId, command_id: cmdId })
      })
      cmdIds.current = {}
      cmdToNode.current = {}
      cmdStartTimes.current = {}
      pendingLines.current = {}
      flushScheduled.current = false
    }
  }, [])

  // Listen for results — batch output lines to reduce re-renders
  const pendingLines = useRef({})
  const flushScheduled = useRef(false)

  useEffect(() => {
    if (!wsRef?.subscribe) return
    return wsRef.subscribe('multinode', (data) => {
      if (!data.id) return
      // O(1) lookup via reverse map
      const nodeId = cmdToNode.current[data.id]
      if (!nodeId) return

      if (data.type === 'error') commandErrors.current.add(data.id)
      const succeeded = data.type === 'done' && commandSucceeded(data.data, commandErrors.current.has(data.id))
      if (['output', 'error', 'done'].includes(data.type)) {
        // Buffer output lines and flush on next animation frame
        if (!pendingLines.current[nodeId]) pendingLines.current[nodeId] = []
        pendingLines.current[nodeId].push({
          type: data.type === 'done' ? (succeeded ? 'success' : 'error') : data.type,
          text: data.type === 'done' ? (succeeded ? '✓ Done' : '✗ Failed') : data.data, _id: ++lineIdCounter.current,
        })
        if (pendingLines.current[nodeId].length > MAX_LINES_PER_NODE) pendingLines.current[nodeId].splice(0, pendingLines.current[nodeId].length - MAX_LINES_PER_NODE)
        if (!flushScheduled.current) {
          flushScheduled.current = true
          requestAnimationFrame(() => {
            flushScheduled.current = false
            const batch = { ...pendingLines.current }
            pendingLines.current = {}
            setResults((prev) => {
              const next = { ...prev }
              for (const [nid, lines] of Object.entries(batch)) {
                const existing = next[nid] || []
                const updated = [...existing, ...lines]
                next[nid] = updated.length > MAX_LINES_PER_NODE ? updated.slice(-MAX_LINES_PER_NODE) : updated
              }
              return next
            })
          })
        }
      } else if (data.type === 'summary') {
        const parsed = parseSummary(data.data)
        if (parsed) setSummaries((prev) => ({ ...prev, [nodeId]: parsed }))
      }
      if (data.type === 'done') {
        commandErrors.current.delete(data.id)
        const cmdId = cmdIds.current[nodeId]
        delete cmdIds.current[nodeId]
        delete cmdStartTimes.current[nodeId]
        if (cmdId) delete cmdToNode.current[cmdId]
        if (Object.keys(cmdIds.current).length === 0) setRunning(false)
      }
    })
  }, [wsRef])

  const handleRun = useCallback(() => {
    if (!target.trim() || selectedNodes.length === 0 || commandDisallowed || targetDisallowed) return
    if (!wsRef?.connected) return
    if (running) return
    setRunning(true)
    commandErrors.current.clear()
    cmdIds.current = {}
    cmdToNode.current = {}
    cmdStartTimes.current = {}
    pendingLines.current = {}
    flushScheduled.current = false

    // Initialize all results at once instead of N separate state updates
    const initial = Object.fromEntries(selectedNodes.map(id => [id, []]))
    setResults(initial)
    setSummaries({})

    // One string for every node, so a comparison really compares like with like.
    const options = encodeOptions(command, optValues[command], ipVersion)

    const now = Date.now()
    selectedNodes.forEach((nodeId) => {
      const cmdId = randomId()
      cmdIds.current[nodeId] = cmdId
      cmdToNode.current[cmdId] = nodeId
      cmdStartTimes.current[nodeId] = now
      wsRef.send({
        node_id: nodeId,
        command: { id: cmdId, type: command, target: target.trim(), options }
      })
    })
  }, [selectedNodes, command, target, wsRef, running, optValues, ipVersion, commandDisallowed, targetDisallowed])

  // Watchdog: time out per-node commands that never produce a `done` message.
  useEffect(() => {
    if (!running) return
    const interval = setInterval(() => {
      const now = Date.now()
      const expired = []
      for (const [nodeId, startedAt] of Object.entries(cmdStartTimes.current)) {
        if (now - startedAt > NODE_TIMEOUT_MS) expired.push(nodeId)
      }
      if (expired.length === 0) return

      expired.forEach((nodeId) => {
        const cmdId = cmdIds.current[nodeId]
        if (cmdId && wsRef?.send) {
          wsRef.send({ action: 'cancel', node_id: nodeId, command_id: cmdId })
          delete cmdToNode.current[cmdId]
        }
        delete cmdIds.current[nodeId]
        delete cmdStartTimes.current[nodeId]
        setResults((prev) => ({
          ...prev,
          [nodeId]: [
            ...(prev[nodeId] || []),
            { type: 'error', text: '✗ Timed out — command exceeded the 10-minute limit', _id: ++lineIdCounter.current },
          ],
        }))
      })

      if (Object.keys(cmdIds.current).length === 0) setRunning(false)
    }, 5_000)
    return () => clearInterval(interval)
  }, [running, wsRef])

  const handleStop = () => {
    Object.entries(cmdIds.current).forEach(([nodeId, cmdId]) => {
      wsRef.send({ action: 'cancel', node_id: nodeId, command_id: cmdId })
    })
    setRunning(false)
    cmdIds.current = {}
    cmdToNode.current = {}
    pendingLines.current = {}
    flushScheduled.current = false
  }

  // Stay mounted through the close transition so the modal can animate out.
  const { mounted, state } = useMountTransition(visible)
  if (!mounted) return null

  const onlineNodes = nodes.filter((n) => n.online)

  // Here the node selection is a *set* while the command select is global, so
  // "the selected node" is undefined. A command is therefore disabled only when
  // EVERY currently selected node lacks it; with nothing selected it is never
  // disabled. A selected chip whose node lacks the chosen command carries the
  // same explanatory tooltip, so it is visible which node is the problem.
  const pickedNodes = onlineNodes.filter((n) => selectedNodes.includes(n.id))
  const commandMissingEverywhere = (type) => (
    pickedNodes.length > 0 && pickedNodes.every((n) => !isToolAvailable(n, type))
  )
  const chipTitle = (n) => unavailableTitle(n, command)

  // Option controls for the selected tool, from the shared table. Unlike the
  // command bar there is no separate DNS dropdown here, so `type` is rendered
  // inline with the rest.
  const currentOptions = COMMAND_OPTIONS[command] || []
  const currentValues = optValues[command] || {}
  const setOptValue = (key, value) => setOptValues((prev) => ({
    ...prev,
    [command]: { ...(prev[command] || {}), [key]: value },
  }))
  const encodedOptions = encodeOptions(command, currentValues, ipVersion)
  const showIpVersion = IP_VERSION_COMMANDS.includes(command)

  return (
    <div data-state={state} className="motion-layer fixed inset-0 z-50 flex items-center justify-center p-4" onClick={handleClose}>
      <div data-state={state} className="motion-backdrop absolute inset-0 bg-black/60 backdrop-blur-sm" />
      <div
        ref={dialogRef}
        role="dialog"
        aria-modal="true"
        aria-label="Multi-Node Comparison"
        tabIndex={-1}
        data-state={state}
        inert={state === 'closing'}
        aria-hidden={state === 'closing'}
        className={`t-modal ${motionStateClass(state)} relative w-full max-w-6xl max-h-[85vh] rounded-2xl border border-border/50
          bg-bg-elevated shadow-2xl shadow-black/50 overflow-hidden flex flex-col`}
        onClick={(e) => e.stopPropagation()}
      >
        {/* Header */}
        <div className="flex items-center justify-between px-6 py-4 border-b border-border/30">
          <div className="flex items-center gap-2">
            <Columns3 size={16} className="text-accent-text" />
            <span className="text-sm font-semibold text-text-primary">Multi-Node Comparison</span>
          </div>
          <button onClick={handleClose}
            className="text-xs text-text-muted hover:text-text-primary px-2 py-1 rounded-lg hover:bg-hover-overlay transition-colors cursor-pointer">
            Close
          </button>
        </div>

        {/* Controls */}
        <div className="px-6 py-3 border-b border-border/20 flex items-center gap-3 flex-wrap">
          <div className="flex items-center gap-1.5 flex-wrap flex-1">
            {onlineNodes.map((node) => {
              const picked = selectedNodes.includes(node.id)
              const lacksCommand = picked && !isToolAvailable(node, command)
              return (
                <button
                  key={node.id}
                  onClick={() => toggleNode(node.id)}
                  aria-pressed={picked}
                  title={lacksCommand ? chipTitle(node) : undefined}
                  className={`flex items-center gap-1.5 px-2.5 py-1 rounded-full text-xs font-medium
                    transition-colors duration-200 cursor-pointer border
                    ${picked
                      ? 'bg-accent/20 text-accent-text border-accent/40'
                      : 'bg-bg-secondary/40 text-text-muted border-border/30 hover:border-border-hover'}
                    ${lacksCommand ? 'opacity-50 line-through decoration-1' : ''}`}
                >
                  <span>{node.flag}</span>
                  {node.name}
                  {picked && <X size={10} />}
                </button>
              )
            })}
          </div>

          <select
            value={command}
            onChange={(e) => setCommand(e.target.value)}
            aria-label="Command type"
            className="px-3 py-1.5 rounded-lg text-xs bg-bg-secondary/60 border border-border/40
              text-text-primary outline-none cursor-pointer"
          >
            {visibleCommands.map((c) => {
              const missing = commandMissingEverywhere(c.id)
              return (
                <option key={c.id} value={c.id} disabled={missing}>
                  {c.label}{missing ? ` (${unavailableLabel(pickedNodes[0], c.id) || 'unavailable'})` : ''}
                </option>
              )
            })}
          </select>

          <div className="flex items-center gap-1.5 px-3 py-1.5 rounded-lg bg-bg-secondary/40 border border-border/40
            focus-within:border-accent/50 transition-colors duration-200">
            <Globe size={12} className="text-text-muted" />
            {allowedTargets !== null ? (
              <select value={allowedTargets.includes(target) ? target : ''} onChange={(e) => setTarget(e.target.value)}
                aria-label="Comparison target" className="bg-bg-secondary text-xs text-text-primary w-36">
                <option value="" disabled>Select an allowed target…</option>
                {allowedTargets.map((value) => <option key={value} value={value}>{value}</option>)}
              </select>
            ) : <input
              type="text"
              value={target}
              onChange={(e) => setTarget(e.target.value)}
              onKeyDown={(e) => e.key === 'Enter' && !running && handleRun()}
              aria-label="Comparison target"
              placeholder={targetPlaceholder(command)}
              className="bg-transparent text-xs text-text-primary placeholder:text-text-muted outline-none w-36"
            />}
          </div>

          {running ? (
            <button onClick={handleStop}
              className="flex items-center gap-1 px-3 py-1.5 rounded-lg text-xs font-semibold
                bg-danger text-white cursor-pointer">
              <Square size={11} /> Stop
            </button>
          ) : (
            <button onClick={handleRun}
              disabled={selectedNodes.length === 0 || !target.trim() || commandDisallowed || targetDisallowed || commandMissingEverywhere(command)}
              title={commandMissingEverywhere(command)
                ? (NATIVE_COMMAND_TYPES.includes(command)
                  ? 'None of the selected nodes runs an agent new enough for this probe'
                  : 'None of the selected nodes has this tool installed')
                : undefined}
              className="flex items-center gap-1 px-3 py-1.5 rounded-lg text-xs font-semibold
                bg-accent text-white hover:bg-accent-hover disabled:opacity-40 cursor-pointer">
              <Play size={11} /> Run All
            </button>
          )}
        </div>

        {/* Per-tool options — the same descriptors the command bar uses, so the
            two surfaces cannot drift. Sent identically to every selected node. */}
        {(currentOptions.length > 0 || showIpVersion) && (
          <div className="px-6 py-2.5 border-b border-border/20 flex items-center gap-3 flex-wrap">
            <span className="text-[10px] font-semibold uppercase tracking-widest text-text-muted shrink-0">
              Options
            </span>

            {showIpVersion && (
              <label className="flex items-center gap-1.5 text-[11px] text-text-secondary">
                IP
                <select
                  value={ipVersion}
                  onChange={(e) => setIpVersion(e.target.value)}
                  aria-label="IP version"
                  className="px-2 py-1 rounded-md text-[11px] bg-bg-secondary/60 border border-border/40
                    text-text-primary outline-none cursor-pointer"
                >
                  {IP_VERSIONS.map((v) => <option key={v} value={v}>{v}</option>)}
                </select>
              </label>
            )}

            {currentOptions.map((d) => (
              d.kind === 'flag' ? (
                <label key={d.key} className="flex items-center gap-1.5 text-[11px] text-text-secondary cursor-pointer">
                  <input
                    type="checkbox"
                    checked={currentValues[d.key] === true}
                    onChange={(e) => setOptValue(d.key, e.target.checked)}
                    className="accent-accent cursor-pointer"
                  />
                  {d.label}
                </label>
              ) : (
                <label key={d.key} className="flex items-center gap-1.5 text-[11px] text-text-secondary">
                  {d.label}
                  {d.kind === 'int' ? (
                    <input
                      type="number"
                      inputMode="numeric"
                      min={d.min}
                      max={d.max}
                      placeholder={d.placeholder}
                      value={currentValues[d.key] ?? ''}
                      onChange={(e) => setOptValue(d.key, e.target.value)}
                      title={`${d.min}–${d.max}, default ${d.placeholder}`}
                      className="w-16 px-2 py-1 rounded-md bg-bg-secondary/60 border border-border/40
                        text-[11px] text-text-primary outline-none focus:border-accent/50
                        placeholder:text-text-muted"
                    />
                  ) : (
                    <select
                      value={currentValues[d.key] ?? ''}
                      onChange={(e) => setOptValue(d.key, e.target.value)}
                      title={d.hint || undefined}
                      className="px-2 py-1 rounded-md text-[11px] bg-bg-secondary/60 border border-border/40
                        text-text-primary outline-none cursor-pointer"
                    >
                      {d.values.map((o) => (
                        <option key={o.value || 'default'} value={o.value}>{o.label}</option>
                      ))}
                    </select>
                  )}
                </label>
              )
            ))}

            <span className="ml-auto text-[10px] text-text-muted font-mono break-all">
              {encodedOptions ? `sends: ${encodedOptions}` : 'sends: tool defaults'}
            </span>
          </div>
        )}
        {/* TCP traceroute is an agent start flag, not a reported capability, so
            the UI cannot know in advance whether a node will honour it. */}
        {command === 'traceroute' && currentValues.mode === 'tcp' && (
          <div className="px-6 pb-2 -mt-1 text-[10px] text-warning/80">
            TCP mode may be refused by a node — it needs the agent to have been started with
            -allow-tcp-traceroute. Nodes that refuse it say so in their output and fall back to UDP.
          </div>
        )}

        {/* Results grid */}
        <div className="flex-1 overflow-auto p-4">
          {selectedNodes.length === 0 ? (
            <div className="text-center py-12 text-text-muted text-sm">
              Select nodes above to compare results side by side.
            </div>
          ) : (
            <div className={`grid gap-3 ${
              selectedNodes.length === 1 ? 'grid-cols-1' :
              selectedNodes.length === 2 ? 'grid-cols-2' :
              selectedNodes.length === 3 ? 'grid-cols-2 lg:grid-cols-3' :
              selectedNodes.length === 4 ? 'grid-cols-2 lg:grid-cols-4' :
              'grid-cols-2 lg:grid-cols-3'
            }`}>
              {selectedNodes.map((nodeId) => {
                const node = nodes.find((n) => n.id === nodeId)
                const lines = results[nodeId] || []
                return (
                  <div key={nodeId} className="rounded-xl border border-border/40 bg-bg-secondary/30 overflow-hidden flex flex-col">
                    <div className="px-3 py-2 border-b border-border/30 bg-bg-secondary/40 flex items-center gap-2">
                      <span className="text-sm">{node?.flag}</span>
                      <span className="text-xs font-medium text-text-primary">{node?.name}</span>
                      <span className="text-[10px] text-text-muted ml-auto">{node?.location}</span>
                    </div>
                    {summaries[nodeId] && (
                      <SummaryBadges summary={summaries[nodeId]} className="px-3 py-1.5 border-b border-border/20 bg-bg-secondary/20" />
                    )}
                    <div className="flex-1 overflow-y-auto p-3 font-mono text-[11px] leading-relaxed max-h-[400px]"
                      style={{ background: 'rgba(14, 14, 20, 0.75)' }}>
                      {lines.length === 0 ? (
                        <span className="text-text-muted">Waiting...</span>
                      ) : (
                        lines.map((line, i) => (
                          <div key={line._id || i} className={
                            line.type === 'error' ? 'text-danger' :
                            line.type === 'success' ? 'text-success' :
                            'text-text-secondary'
                          }>
                            {line.text}
                          </div>
                        ))
                      )}
                    </div>
                  </div>
                )
              })}
            </div>
          )}
        </div>
      </div>
    </div>
  )
}
