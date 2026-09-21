import { commandSucceeded } from './lib/commandResult'
import { randomId } from './lib/id'
import { useState, useCallback, useEffect, useRef, useMemo, lazy, Suspense } from 'react'
import { LogOut } from 'lucide-react'
import DashboardTools from './components/DashboardTools'
import AmbientBackground from './components/AmbientBackground'
import NodeSelector from './components/NodeSelector'
import NodeInfo from './components/NodeInfo'
import CommandBar from './components/CommandBar'
import OutputTerminal from './components/OutputTerminal'
import SpeedTestPanel from './components/SpeedTestPanel'
import NodeHealthOverview from './components/NodeHealthOverview'
import LatencyMatrix from './components/LatencyMatrix'
import MultiNodeCompare from './components/MultiNodeCompare'
import Schedules from './components/Schedules'
import { parseSummary } from './components/SummaryBadges'
import KeyPrompt from './components/KeyPrompt'
import ErrorBoundary from './components/ErrorBoundary'
import { useNodes } from './hooks/useNodes'
import { useWebSocket } from './hooks/useWebSocket'
import { useCommandHistory } from './hooks/useCommandHistory'
import { useKits } from './hooks/useKits'
import { useMountTransition } from './hooks/useMountTransition'
import { apiFetch, getApiKey, setApiKey, clearApiKey, AUTH_REQUIRED_EVENT } from './lib/api'

// Heavy panels — Leaflet (~150 KB) and xterm.js (~100 KB) — are only loaded
// the first time their modal opens, so they don't bloat the initial bundle.
const GeoMap = lazy(() => import('./components/GeoMap'))
const ShellTerminal = lazy(() => import('./components/ShellTerminal'))

const MAX_OUTPUT_LINES = 10000

export default function App() {
  const [session, setSession] = useState(0)
  const signOut = useCallback(() => {
    clearApiKey()
    // Remount to close sockets and panels, abort requests, and discard displayed
    // data from the old session before connecting without its credential.
    setSession((value) => value + 1)
  }, [])
  return <Dashboard key={session} onSignOut={signOut} />
}

function Dashboard({ onSignOut }) {
  // The client key drives both transports: it re-dials /ws/client (as the
  // lg.bearer subprotocol) and rides along on every apiFetch. Keeping it in
  // state — not just localStorage — is what makes saving a key take effect
  // without a page reload.
  const [authKey, setAuthKey] = useState(() => getApiKey())
  const [authRevision, setAuthRevision] = useState(0)
  const [keyPromptOpen, setKeyPromptOpen] = useState(false)
  const [keyRejected, setKeyRejected] = useState(false)

  const ws = useWebSocket('/ws/client', authKey, authRevision)
  const { connected, send, subscribe, reconnectAttempt } = ws
  const { nodes, loading: nodesLoading, error: nodesError, refetch: retryNodes } = useNodes(subscribe, connected, authKey, authRevision)
  const { history, push: pushHistory, navigate: navigateHistory, clear: clearHistory } = useCommandHistory()
  const [selectedNodeId, setSelectedNodeId] = useState(null)
  const [lines, setLines] = useState([])
  // Structured summary of the run currently in the terminal (S2). Replaced on
  // every dispatch — including each step of a diagnostic kit — so it always
  // describes what is on screen.
  const [summary, setSummary] = useState(null)
  const [running, setRunning] = useState(false)
  const [lastCommandType, setLastCommandType] = useState(null)
  // Metadata for the most recent run, used by the Share button to record
  // what was executed alongside the captured output.
  const [runMeta, setRunMeta] = useState(null)
  const currentCmdId = useRef(null)
  const currentNodeId = useRef(null)
  const commandErrorRef = useRef(false)
  const kitFailedRef = useRef(false)
  const lineIdCounter = useRef(0)
  const replayDismissedRef = useRef(false)
  const replayAbortRef = useRef(null)
  const dismissReplay = useCallback(() => {
    replayDismissedRef.current = true
    replayAbortRef.current?.abort()
  }, [])

  // Single modal state instead of five booleans
  const [activeModal, setActiveModal] = useState(null) // 'health' | 'matrix' | 'compare' | 'map' | 'shell' | 'schedules' | null
  // The map and shell modals are lazy-loaded, so App owns their mount —
  // keep them mounted through the close transition like the others.
  const mapModal = useMountTransition(activeModal === 'map')
  const shellModal = useMountTransition(activeModal === 'shell')

  // Public-mode config from /api/public-config + the session_kind hello frame
  // from /ws/client. Either signal flips us into the stripped UI; we keep
  // both so the UI can render correctly even before the WS is established.
  const [publicConfig, setPublicConfig] = useState({ public_mode: false, allowed_commands: [], allowed_targets: [] })
  const [publicSession, setPublicSession] = useState(false)
  const isPublic = publicConfig.public_mode && (publicSession || !connected)

  useEffect(() => {
    apiFetch('/api/public-config')
      .then((r) => (r.ok ? r.json() : null))
      .then((cfg) => {
        if (!cfg) return
        setPublicConfig({ ...cfg, allowed_commands: Array.isArray(cfg.allowed_commands) ? cfg.allowed_commands : [], allowed_targets: Array.isArray(cfg.allowed_targets) ? cfg.allowed_targets : [] })
        // The server says a key is needed and this browser has none: ask once,
        // before anything tries to load.
        if (cfg.auth_required && !getApiKey()) setKeyPromptOpen(true)
      })
      .catch(() => { /* fail-open: fall back to authenticated chrome */ })
  }, [])

  // Any 401 (apiFetch) or 4401 socket close (useWebSocket) lands here. The
  // socket does not retry a rejected key, so the prompt is the only way back.
  useEffect(() => {
    const onAuthRequired = () => {
      setKeyPromptOpen(true)
      setKeyRejected(Boolean(getApiKey()))
    }
    window.addEventListener(AUTH_REQUIRED_EVENT, onAuthRequired)
    return () => window.removeEventListener(AUTH_REQUIRED_EVENT, onAuthRequired)
  }, [])

  const handleSaveKey = useCallback((key, remember) => {
    setApiKey(key, remember)
    setAuthKey(key)
    // An explicit Connect retries even when the entered key is unchanged.
    setAuthRevision((revision) => revision + 1)
    setPublicSession(false)
    setKeyRejected(false)
    setKeyPromptOpen(false)
  }, [])

  useEffect(() => {
    if (!subscribe) return
    return subscribe('session-kind', (data) => {
      if (data?.type === 'session_kind') setPublicSession(data.kind === 'public')
    })
  }, [subscribe])

  // Append lines with a cap to prevent unbounded growth.
  // Declared before any useEffect that closes over it — referencing a const
  // in a dep array before its declaration is a TDZ error at render time.
  const appendLines = useCallback((newLines) => {
    setLines((prev) => {
      const stamped = newLines.map((l) => ({ ...l, _id: ++lineIdCounter.current }))
      const combined = [...prev, ...stamped]
      if (combined.length > MAX_OUTPUT_LINES) {
        return combined.slice(combined.length - MAX_OUTPUT_LINES)
      }
      return combined
    })
  }, [])

  // The user-defined diagnostic kit (S10). **Exactly one useKits() instance
  // exists, and it is this one** — the hook's state is per-instance and the
  // `storage` event does not fire in the document that wrote the value, so a
  // second instance in CommandBar would never reach this run path; CommandBar
  // gets the kit and these callbacks as props. Declared up here, with the refs
  // below, because the subscribe useEffect advances the kit on `done` (consts
  // referenced before their declaration throw TDZ at render — git history holds
  // two fixes for exactly that, abf1f6d among them).
  const { kit, addStep: addKitStep, removeStep: removeKitStep, reset: resetKit, max: maxKitSteps, min: minKitSteps } = useKits()
  // kitRef is what that effect reads: closing over the kit state directly would
  // run the *pre-edit* steps, and adding it to the effect's dependency array
  // would resubscribe the 'app' channel on every render and drop frames landing
  // in the gap — so the dep array stays exactly as it was.
  const kitRef = useRef(kit)
  useEffect(() => { kitRef.current = kit }, [kit])
  // The snapshot the in-flight run is using, captured at run start, so editing
  // the kit mid-run cannot change the sequence under it.
  const kitRunRef = useRef([])
  const kitTargetRef = useRef(null)
  const kitStepRef = useRef(-1)

  const dispatchCommand = useCallback((command, nodeId = selectedNodeId) => {
    const cmdId = randomId()
    commandErrorRef.current = false
    currentCmdId.current = cmdId
    currentNodeId.current = nodeId
    setLastCommandType(command.type)
    setSummary(null)
    setRunning(true)
    send({ node_id: nodeId, command: { id: cmdId, ...command } })
  }, [selectedNodeId, send])

  const resetKitRun = useCallback(() => {
    kitRunRef.current = []
    kitFailedRef.current = false
    kitTargetRef.current = null
    kitStepRef.current = -1
  }, [])

  // Reset running state on disconnect so UI doesn't get stuck
  const prevConnected = useRef(connected)
  useEffect(() => {
    if (prevConnected.current && !connected && running) {
      setRunning(false)
      appendLines([{ type: 'error', text: '\n✗ Connection lost.' }])
      currentCmdId.current = null
      currentNodeId.current = null
      resetKitRun()
    }
    prevConnected.current = connected
  }, [connected, running, appendLines, resetKitRun])

  // If the URL has ?run=<id>, fetch that saved run and replay it into the
  // terminal. Runs once on mount; users can dismiss by clearing the URL.
  useEffect(() => {
    const params = new URLSearchParams(window.location.search)
    const runId = params.get('run')
    if (!runId || replayDismissedRef.current) return
    const controller = new AbortController()
    replayAbortRef.current = controller
    apiFetch(`/api/runs/${encodeURIComponent(runId)}`, { signal: controller.signal })
      .then((r) => {
        if (!r.ok) throw new Error(`HTTP ${r.status}`)
        return r.json()
      })
      .then((rec) => {
        if (controller.signal.aborted || replayDismissedRef.current) return
        const headerLines = [
          { _id: ++lineIdCounter.current, type: 'info', text: `↩ Replaying shared run ${runId.slice(0, 8)}…` },
          { _id: ++lineIdCounter.current, type: 'info', text: `   ${rec.command} ${rec.target || ''}${rec.options ? ' (' + rec.options + ')' : ''}  on  ${rec.node_flag || ''} ${rec.node_name} (${rec.node_location})` },
          { _id: ++lineIdCounter.current, type: 'info', text: `   captured ${new Date(rec.created_at).toLocaleString()}\n` },
        ]
        const replayLines = (rec.lines || []).map((l) => ({ ...l, _id: ++lineIdCounter.current }))
        setLines([...headerLines, ...replayLines].slice(-MAX_OUTPUT_LINES))
        setLastCommandType(rec.command)
        setSummary(null)
        setRunMeta({
          command: rec.command,
          target: rec.target,
          options: rec.options,
          nodeName: rec.node_name,
          nodeFlag: rec.node_flag,
          nodeLocation: rec.node_location,
        })
      })
      .catch((err) => {
        if (controller.signal.aborted || replayDismissedRef.current) return
        setLines([
          { _id: ++lineIdCounter.current, type: 'error', text: `✗ Couldn't load shared run ${runId}: ${err.message}` },
        ])
      })
    return () => controller.abort()
  }, [authKey, authRevision])

  // Close modals on Escape (except shell — terminal needs Escape key)
  useEffect(() => {
    const handleEsc = (e) => {
      if (e.key === 'Escape' && activeModal !== 'shell') setActiveModal(null)
    }
    document.addEventListener('keydown', handleEsc)
    return () => document.removeEventListener('keydown', handleEsc)
  }, [activeModal])

  const selectedNode = nodes.find((n) => n.id === selectedNodeId)

  const traceHops = useMemo(() => !running && ['traceroute', 'nexttrace', 'mtr'].includes(lastCommandType)
    ? lines.filter((l) => l.type === 'output').map((l) => l.text)
    : [], [running, lastCommandType, lines])

  useEffect(() => {
    if ((!selectedNodeId || !nodes.some((n) => n.id === selectedNodeId)) && nodes.length > 0) {
      const online = nodes.find((n) => n.online)
      if (online) setSelectedNodeId(online.id)
    }
  }, [nodes, selectedNodeId])

  useEffect(() => {
    const unsub = subscribe('app', (data) => {
      if (!data.id || data.id !== currentCmdId.current) return
      if (data.type === 'done') {
        const succeeded = commandSucceeded(data.data, commandErrorRef.current)
        if (kitStepRef.current >= 0 && !succeeded) kitFailedRef.current = true
        // If we're mid-kit, advance to the next step instead of finishing. The
        // sequence is the snapshot taken at run start, never the live kit.
        const sequence = kitRunRef.current
        const nextStep = kitStepRef.current + 1
        if (kitStepRef.current >= 0 && nextStep < sequence.length && kitTargetRef.current) {
          const step = sequence[nextStep]
          kitStepRef.current = nextStep
          appendLines([
            { type: succeeded ? 'success' : 'error', text: succeeded ? '\n✓ Step done.' : '\n✗ Step failed.' },
            { type: 'info', text: `\n── [${nextStep + 1}/${sequence.length}] ${step.type} ${kitTargetRef.current} ──` },
          ])
          dispatchCommand({ type: step.type, target: kitTargetRef.current, options: step.options }, currentNodeId.current)
          return
        }
        // Either single command finished or kit is complete.
        if (kitStepRef.current >= 0) {
          appendLines([{ type: kitFailedRef.current ? 'error' : 'success', text: kitFailedRef.current ? '\n✗ Diagnostic kit finished with errors.' : '\n✓ Diagnostic kit complete.' }])
          kitStepRef.current = -1
          kitTargetRef.current = null
          kitRunRef.current = []
        } else {
          appendLines([{ type: succeeded ? 'success' : 'error', text: succeeded ? '\n✓ Command completed.' : '\n✗ Command failed.' }])
        }
        setRunning(false)
        currentCmdId.current = null
      } else if (data.type === 'error') {
        commandErrorRef.current = true
        appendLines([{ type: 'error', text: data.data }])
      } else if (data.type === 'output') {
        appendLines([{ type: 'output', text: data.data }])
      } else if (data.type === 'summary') {
        // Arrives as a JSON-encoded string; anything unparsable is ignored.
        const parsed = parseSummary(data.data)
        if (parsed) setSummary(parsed)
      }
    })
    return () => unsub()
  }, [subscribe, appendLines, dispatchCommand])

  // Capture metadata about the run currently in the terminal so the Share
  // button can record what produced this output. Updated when handleRun
  // dispatches and when a permalink replay loads.
  const captureRunMeta = useCallback((command) => {
    const node = nodes.find((n) => n.id === selectedNodeId)
    setRunMeta({
      command: command.type,
      target: command.target || '',
      options: command.options || '',
      nodeName: node?.name || '',
      nodeFlag: node?.flag || '',
      nodeLocation: node?.location || '',
    })
  }, [nodes, selectedNodeId])

  const handleRun = useCallback(
    (command) => {
      if (!selectedNodeId) return
      if (!connected) {
        setLines([{ _id: ++lineIdCounter.current, type: 'error', text: '✗ Not connected to server.' }])
        return
      }
      if (running) return
      resetKitRun()

      // 'shell' opens the interactive PTY modal instead of dispatching a
      // regular command — the rest of the command pipeline doesn't apply.
      if (command.type === 'shell') {
        setActiveModal('shell')
        return
      }

      dismissReplay()
      pushHistory({ type: command.type, target: command.target })

      // 'kit' chains the user's configured steps in sequence (the rest of the
      // sequence advances in the 'done' subscription handler above). The kit is
      // read from the ref and **snapshotted** for this run: useKits guarantees
      // at least one valid step, and the guard below keeps a hand-broken value
      // from throwing inside this event handler, which no ErrorBoundary covers.
      if (command.type === 'kit') {
        const sequence = Array.isArray(kitRef.current) ? kitRef.current : []
        if (sequence.length === 0) {
          setLines([{ _id: ++lineIdCounter.current, type: 'error', text: '✗ The diagnostic kit has no steps.' }])
          return
        }
        kitRunRef.current = sequence
        kitTargetRef.current = command.target
        kitStepRef.current = 0
        const step = sequence[0]
        setLines([
          { _id: ++lineIdCounter.current, type: 'info', text: `$ diagnostic kit ${command.target}` },
          { _id: ++lineIdCounter.current, type: 'info', text: `Running on ${selectedNode?.name} (${selectedNode?.location})\n` },
          { _id: ++lineIdCounter.current, type: 'info', text: `── [1/${sequence.length}] ${step.type} ${command.target} ──` },
        ])
        captureRunMeta({ type: 'kit', target: command.target, options: '' })
        dispatchCommand({ type: step.type, target: command.target, options: step.options })
        return
      }

      setLines([
        { _id: ++lineIdCounter.current, type: 'info', text: `$ ${command.type} ${command.target}` },
        { _id: ++lineIdCounter.current, type: 'info', text: `Running on ${selectedNode?.name} (${selectedNode?.location})...\n` },
      ])
      captureRunMeta(command)
      dispatchCommand(command)
    },
    [selectedNodeId, selectedNode, send, pushHistory, connected, running, dispatchCommand, captureRunMeta, resetKitRun, dismissReplay]
  )

  const handleStop = useCallback(() => {
    if (currentCmdId.current && currentNodeId.current) {
      send({ action: 'cancel', node_id: currentNodeId.current, command_id: currentCmdId.current })
    }
    setRunning(false)
    appendLines([{ type: 'error', text: '\n✗ Cancelled.' }])
    currentCmdId.current = null
    currentNodeId.current = null
    resetKitRun()
  }, [send, appendLines, resetKitRun])

  // Show map hint when traceroute finishes
  const hasTraceData = traceHops.length > 0 && !running

  return (
    <div className="min-h-screen relative flex flex-col">
      <AmbientBackground />

      {/* ── Top bar ── */}
      <header className="relative z-40 border-b border-border/20">
        <div className="max-w-[1400px] mx-auto px-5 py-3 flex flex-wrap items-center gap-4">
          <div className="flex items-center gap-2.5 shrink-0">
            <img src="/icon-192.png" alt="Parallax" width={32} height={32} className="h-8 w-8 shrink-0" />
            <span aria-hidden="true" className="text-base font-bold tracking-tight text-text-primary hidden sm:block">
              Parallax
            </span>
          </div>

          <div className="w-px h-6 bg-border/40 shrink-0 hidden sm:block" />

          <div className="shrink-0">
            <NodeSelector nodes={nodes} selectedNode={selectedNodeId} onSelect={setSelectedNodeId} compact loading={nodesLoading} error={nodesError} onRetry={retryNodes} />
          </div>

          <div className="flex-1" />

          <DashboardTools onSelect={setActiveModal} isPublic={isPublic} hasTraceData={hasTraceData} />
          {authKey && (
            <button onClick={onSignOut} title="Forget key and sign out"
              className="flex min-h-11 items-center gap-2 rounded-lg px-3 text-xs text-text-muted hover:text-text-primary hover:bg-hover-overlay cursor-pointer">
              <LogOut size={16} />
              <span>Sign out</span>
            </button>
          )}

          <div className="w-px h-6 bg-border/40 shrink-0" />

          <div className="flex items-center gap-1.5 px-2.5 py-1 rounded-full text-xs font-medium
            bg-bg-secondary/30 backdrop-blur-sm border border-border/30">
            <span className={`w-1.5 h-1.5 rounded-full ${connected ? 'bg-success' : 'bg-danger'}`}
              style={connected ? { animation: 'pulse-glow 2s ease-in-out infinite' } : {}} />
            <span className="text-text-muted">
              {connected ? 'Live' : reconnectAttempt > 0 ? `Reconnecting (${reconnectAttempt})` : 'Offline'}
            </span>
          </div>
        </div>
      </header>

      {isPublic && (
        <div className="relative z-30 px-5 py-1.5 bg-warning/10 border-b border-warning/20 text-[11px] text-warning text-center">
          Public read-only mode — limited to {publicConfig.allowed_commands.join(', ') || 'no commands'} against {publicConfig.allowed_targets.length || 0} preset target{publicConfig.allowed_targets.length === 1 ? '' : 's'}.
        </div>
      )}

      {nodesError && !keyPromptOpen && (
        <div role="alert" className="relative flex flex-wrap items-center justify-center gap-3 border-b border-warning/30 bg-warning/10 px-5 py-3 text-sm text-warning">
          <span>Couldn’t load agents. {nodes.length > 0 ? 'Showing the last known list.' : 'Check your connection and try again.'}</span>
          <button onClick={retryNodes} disabled={nodesLoading}
            className="min-h-11 rounded-lg border border-warning/40 px-4 font-medium disabled:opacity-50 cursor-pointer">
            {nodesLoading ? 'Retrying…' : 'Retry'}
          </button>
        </div>
      )}

      {/* ── Command bar ── */}
      <div className="relative z-30 border-b border-border/15 bg-bg-secondary/10 backdrop-blur-sm">
        <div className="max-w-[1400px] mx-auto px-5 py-3">
          <CommandBar
            onRun={handleRun}
            onStop={handleStop}
            running={running}
            disabled={!selectedNodeId || !selectedNode?.online}
            /* The selected node itself, for its reported tools/version. This is
               a different concept from allowedCommands below, which is the
               public-mode server allowlist. */
            node={selectedNode}
            history={history}
            onNavigateHistory={navigateHistory}
            onClearHistory={clearHistory}
            allowedCommands={isPublic ? publicConfig.allowed_commands : null}
            allowedTargets={isPublic ? publicConfig.allowed_targets : null}
            /* The user-defined kit, from App's single useKits() instance. Passed
               as props precisely so CommandBar does not call the hook itself. */
            kit={kit}
            onAddKitStep={addKitStep}
            onRemoveKitStep={removeKitStep}
            onResetKit={resetKit}
            maxKitSteps={maxKitSteps}
            minKitSteps={minKitSteps}
          />
        </div>
      </div>

      {/* ── Main ── */}
      <main className="relative z-10 flex-1 max-w-[1400px] mx-auto w-full px-5 py-5">
        <div className="flex flex-col lg:flex-row gap-5 h-full">
          <div className="flex-1 min-w-0">
            <OutputTerminal
              lines={lines}
              summary={summary}
              nodeName={runMeta ? (runMeta.nodeName || 'Unknown node') : selectedNode?.name}
              onClear={() => { dismissReplay(); setLines([]); setSummary(null) }}
              runMeta={runMeta}
              canShare={!isPublic}
            />
          </div>
          <div className="w-full lg:w-72 shrink-0 space-y-3">
            <NodeInfo node={selectedNode} />
            {/* Both are refused server-side for public sessions (403); hiding
                them is cosmetic so the stripped UI doesn't offer dead buttons. */}
            {!isPublic && <SpeedTestPanel authKey={authKey} />}
          </div>
        </div>
      </main>

      {/* ── Footer ── */}
      <footer className="relative z-10 border-t border-border/15 py-4 px-5">
        <div className="max-w-[1400px] mx-auto flex items-center justify-between">
          <div className="flex items-center gap-2">
            <img src="/icon-192.png" alt="" width={20} height={20} className="h-5 w-5 shrink-0" />
            <span className="text-xs font-medium text-text-muted">Parallax</span>
          </div>
          <span className="text-[11px] text-text-muted">Network diagnostics</span>
        </div>
      </footer>

      <KeyPrompt visible={keyPromptOpen} onSave={handleSaveKey} invalid={keyRejected} />

      {/* ── Modals ── */}
      <ErrorBoundary resetKey={activeModal} fallback={<div className="fixed inset-0 z-50 flex items-center justify-center bg-black/60 text-danger text-sm"><button onClick={() => setActiveModal(null)}>Component error. Close panel.</button></div>}>
        <NodeHealthOverview visible={activeModal === 'health'} onClose={() => setActiveModal(null)} />
        <LatencyMatrix canMeasure={!isPublic} visible={activeModal === 'matrix'} onClose={() => setActiveModal(null)} wsRef={ws} />
        <MultiNodeCompare allowedCommands={isPublic ? publicConfig.allowed_commands : null} allowedTargets={isPublic ? publicConfig.allowed_targets : null} visible={activeModal === 'compare'} onClose={() => setActiveModal(null)} nodes={nodes} wsRef={ws} />
        <Schedules visible={activeModal === 'schedules'} onClose={() => setActiveModal(null)} nodes={nodes} />
        {/* Lazy-loaded modals: render the chunk only on first open so the
            initial bundle stays small. Suspense fallback is invisible — these
            chunks are tiny relative to the user-perceived modal animation. */}
        <Suspense fallback={null}>
          {mapModal.mounted && (
            <GeoMap visible state={mapModal.state} onClose={() => setActiveModal(null)} nodes={nodes} traceHops={traceHops} />
          )}
          {shellModal.mounted && (
            <ShellTerminal
              visible
              state={shellModal.state}
              onClose={() => setActiveModal(null)}
              nodeId={selectedNodeId}
              nodeName={selectedNode?.name}
              ws={ws}
            />
          )}
        </Suspense>
      </ErrorBoundary>
    </div>
  )
}
