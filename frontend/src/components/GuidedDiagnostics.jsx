import { useCallback, useEffect, useRef, useState } from 'react'
import { RECIPES, validDiagnosticHost, explainRun, MAX_RUN_BYTES } from '../lib/investigations'
import { isToolAvailable, unavailableTitle } from '../lib/capabilities'
import { commandSucceeded } from '../lib/commandResult'
import { resultDocument } from '../lib/resultExport'
import { randomId } from '../lib/id'
import { parseSummary } from './SummaryBadges'

const control = 'min-w-0 max-w-full min-h-11 rounded-lg border border-border-hover bg-bg-secondary px-3 text-sm text-text-primary focus-visible:outline-2 focus-visible:outline-accent-text disabled:opacity-40'

export default function GuidedDiagnostics({ nodes, ws, onCollect }) {
  const [recipe, setRecipe] = useState('website')
  const [nodeId, setNodeId] = useState('')
  const [host, setHost] = useState('')
  const [running, setRunning] = useState(false)
  const [results, setResults] = useState([])
  const [progress, setProgress] = useState('')
  const active = useRef(null)
  const node = nodes.find((item) => item.id === nodeId)
  const steps = RECIPES[recipe].steps(host.trim())
  const missing = node ? steps.filter((step, index) => !isToolAvailable(node, step.type) && steps.findIndex((item) => item.type === step.type) === index) : []
  const { send, subscribe, connected } = ws

  const dispatch = useCallback((sequence) => {
    const step = sequence.steps[sequence.index]
    sequence.id = randomId()
    sequence.lines = []
    sequence.summary = null
    sequence.failed = false
    sequence.bytes = 0
    sequence.startedAt = new Date().toISOString()
    sequence.deadline = Date.now() + 615000
    setProgress(`Check ${sequence.index + 1}/${sequence.steps.length}: ${step.type} ${step.target}`)
    send({ node_id: sequence.node.id, command: { id: sequence.id, ...step } })
  }, [send])

  const finishStep = useCallback((sequence) => {
    const step = sequence.steps[sequence.index]
    const run = resultDocument({ command: step.type, target: step.target, options: step.options,
      startedAt: sequence.startedAt, nodeName: sequence.node.name, nodeLocation: sequence.node.location }, sequence.lines, sequence.summary)
    setResults((previous) => [...previous, run])
  }, [])

  const stop = useCallback((reason = 'Cancelled by user.') => {
    const sequence = active.current
    if (!sequence) return
    active.current = null
    send({ action: 'cancel', node_id: sequence.node.id, command_id: sequence.id })
    sequence.lines.push({ type: 'error', text: reason })
    finishStep(sequence)
    setRunning(false)
    setProgress(reason)
  }, [send, finishStep])

  useEffect(() => subscribe('guided', (data) => {
    const sequence = active.current
    if (!sequence || data.id !== sequence.id) return
    if (data.type === 'output' || data.type === 'error') {
      if (data.type === 'error') sequence.failed = true
      const text = typeof data.data === 'string' ? data.data : ''
      sequence.bytes += new TextEncoder().encode(JSON.stringify({ type: data.type, text })).length
      if (sequence.bytes > MAX_RUN_BYTES / 2 || sequence.lines.length >= 2000) { stop('Output limit reached; remaining checks were stopped.'); return }
      sequence.lines.push({ type: data.type, text })
    } else if (data.type === 'summary') sequence.summary = parseSummary(data.data)
    else if (data.type === 'done') {
      const succeeded = commandSucceeded(data.data, sequence.failed)
      sequence.lines.push({ type: succeeded ? 'success' : 'error', text: succeeded ? 'Command completed.' : 'Command failed.' })
      // Retire the old ID before advancing; delayed frames cannot enter the next check.
      sequence.id = null
      finishStep(sequence)
      sequence.index += 1
      if (sequence.index < sequence.steps.length) dispatch(sequence)
      else { active.current = null; setRunning(false); setProgress('Checks finished. Review the observations below.') }
    }
  }), [subscribe, dispatch, finishStep, stop])

  useEffect(() => { if (!connected) stop('Connection lost; remaining checks were stopped.') }, [connected, stop])
  useEffect(() => {
    const timer = setInterval(() => { if (active.current && Date.now() > active.current.deadline) stop('Check timed out; remaining checks were stopped.') }, 1000)
    return () => {
      clearInterval(timer)
      const sequence = active.current
      active.current = null
      if (sequence) send({ action: 'cancel', node_id: sequence.node.id, command_id: sequence.id })
    }
  }, [send, stop])

  const start = () => {
    if (active.current || !connected || !node?.online || !validDiagnosticHost(host.trim()) || missing.length) return
    const sequence = { node: { ...node }, steps, index: 0 }
    active.current = sequence
    setResults([])
    setRunning(true)
    dispatch(sequence)
  }

  return <section aria-labelledby="guided-title" className="space-y-3">
    <h3 id="guided-title" className="font-semibold">Guided diagnostics</h3>
    <p className="text-sm text-text-muted">Choose a symptom and run a short sequence from one node. These observations help narrow the problem; they do not establish a root cause.</p>
    <div className="flex flex-wrap gap-2">
      <select aria-label="Diagnostic symptom" className={control} value={recipe} disabled={running} onChange={(e) => setRecipe(e.target.value)}>
        {Object.entries(RECIPES).map(([id, item]) => <option key={id} value={id}>{item.label}</option>)}
      </select>
      <select aria-label="Diagnostic node" className={control} value={nodeId} disabled={running} onChange={(e) => setNodeId(e.target.value)}>
        <option value="">Choose a node</option>
        {nodes.map((item) => <option key={item.id} value={item.id} disabled={!item.online}>{item.name}{!item.online ? ' (offline)' : ''}</option>)}
      </select>
      <input aria-label="Diagnostic hostname" placeholder="example.com" maxLength={253} className={`${control} min-w-0 flex-1`} value={host} disabled={running} onChange={(e) => setHost(e.target.value)} />
    </div>
    <p className="text-sm text-text-muted">{RECIPES[recipe].description} Enter a hostname without a URL, port or path.</p>
    {missing.map((step) => <p key={step.type} className="text-sm text-warning">{unavailableTitle(node, step.type)}</p>)}
    <div className="flex flex-wrap gap-2">
      {running ? <button className={control} onClick={() => stop()}>Stop guided checks</button> : <button className={control} disabled={!connected || !node?.online || !validDiagnosticHost(host.trim()) || missing.length > 0} onClick={start}>Run guided checks</button>}
      <button className={control} disabled={running || !results.length} onClick={() => onCollect(results)}>Add guided results to incident</button>
    </div>
    <p role="status" className="text-sm text-text-muted">{progress}</p>
    {results.map((run, index) => <details key={index} className="rounded-lg border border-border p-3">
      <summary className="cursor-pointer text-sm break-words">{run.command} {run.target} — {explainRun(run)}</summary>
      <pre className="mt-3 whitespace-pre-wrap break-all text-xs text-text-secondary">{run.lines.map((line) => line.text).join('\n')}</pre>
    </details>)}
  </section>
}
