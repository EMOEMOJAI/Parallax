import { useState, useRef, useCallback, useEffect } from 'react'
import { Gauge, ArrowDown, ArrowUp, Timer, Zap, Loader } from 'lucide-react'
import { openAuthedSocket } from '../lib/api'

const PHASE_LABELS = {
  latency: 'Measuring latency...',
  download: 'Testing download...',
  upload: 'Testing upload...',
}

/** Wait for a specific JSON action on the WebSocket, with close/error handling and timeout */
function waitForMessage(ws, action, timeoutMs = 15000) {
  return new Promise((resolve, reject) => {
    // Bail immediately if the socket is already closed
    if (ws.readyState === WebSocket.CLOSING || ws.readyState === WebSocket.CLOSED) {
      reject(new Error('WebSocket already closed'))
      return
    }

    const prev = ws.onmessage
    const prevClose = ws.onclose
    const prevError = ws.onerror

    const timer = setTimeout(() => {
      cleanup()
      reject(new Error(`Timeout waiting for "${action}" response`))
    }, timeoutMs)

    const cleanup = () => {
      clearTimeout(timer)
      ws.onmessage = prev
      ws.onclose = prevClose
      ws.onerror = prevError
    }

    ws.onmessage = (e) => {
      if (typeof e.data === 'string') {
        try {
          const msg = JSON.parse(e.data)
          if (msg.action === action) {
            cleanup()
            resolve(msg)
            return
          }
        } catch {
          // ignore
        }
      }
      if (prev) prev(e)
    }
    ws.onclose = () => {
      cleanup()
      reject(new Error('Connection closed'))
    }
    ws.onerror = () => {
      cleanup()
      reject(new Error('WebSocket error during speed test'))
    }
  })
}

async function measureLatency(ws) {
  const latencies = []
  for (let i = 0; i < 10; i++) {
    const t0 = performance.now()
    ws.send(JSON.stringify({ action: 'ping' }))
    await waitForMessage(ws, 'pong')
    latencies.push(performance.now() - t0)
  }
  const avg = latencies.reduce((a, b) => a + b, 0) / latencies.length
  const jitter = Math.max(...latencies) - Math.min(...latencies)
  return { avg, jitter }
}

async function measureDownload(ws) {
  ws.send(JSON.stringify({ action: 'download', duration: 5 }))
  await waitForMessage(ws, 'download_start')

  let dlBytes = 0
  const dlStart = performance.now()
  const prevOnMessage = ws.onmessage
  const prevOnClose = ws.onclose
  const prevOnError = ws.onerror

  const cleanup = () => {
    ws.onmessage = prevOnMessage
    ws.onclose = prevOnClose
    ws.onerror = prevOnError
  }

  await new Promise((resolve, reject) => {
    const dlTimeout = setTimeout(() => {
      cleanup()
      reject(new Error('Download phase timed out'))
    }, 30000)

    ws.onmessage = (e) => {
      if (e.data instanceof ArrayBuffer) {
        dlBytes += e.data.byteLength
      } else {
        try {
          const msg = JSON.parse(e.data)
          if (msg.action === 'download_done') {
            clearTimeout(dlTimeout)
            cleanup()
            resolve(msg)
          }
        } catch {
          // ignore
        }
      }
    }
    ws.onclose = () => { clearTimeout(dlTimeout); cleanup(); reject(new Error('Connection closed during download')) }
    ws.onerror = () => { clearTimeout(dlTimeout); cleanup(); reject(new Error('WebSocket error during download')) }
  })
  let dlElapsed = (performance.now() - dlStart) / 1000
  if (dlElapsed <= 0) dlElapsed = 0.001
  return (dlBytes * 8) / dlElapsed / 1_000_000
}

async function measureUpload(ws) {
  ws.send(JSON.stringify({ action: 'upload_start' }))

  const chunk = new ArrayBuffer(1024 * 1024)
  const ulDeadline = performance.now() + 5000
  while (performance.now() < ulDeadline) {
    // Stop if socket is no longer open (server may have closed after cap)
    if (ws.readyState !== WebSocket.OPEN) break
    // Backpressure: wait if the send buffer is too large
    if (ws.bufferedAmount > 4 * 1024 * 1024) {
      await new Promise((r) => setTimeout(r, 10))
      continue
    }
    ws.send(chunk)
    // Yield to the event loop to prevent UI freezing
    // Use setTimeout instead of requestAnimationFrame so upload works in background tabs
    await new Promise((r) => setTimeout(r, 0))
  }
  if (ws.readyState === WebSocket.OPEN) {
    ws.send(JSON.stringify({ action: 'upload_done' }))
  }
  const ulResult = await waitForMessage(ws, 'upload_done')
  return ulResult.speedMbps
}

export default function SpeedTestPanel({ authKey = '' }) {
  const [testing, setTesting] = useState(false)
  const [phase, setPhase] = useState(null)
  const [results, setResults] = useState(null)
  const wsRef = useRef(null)

  // Clean up WebSocket if component unmounts during a test
  useEffect(() => {
    return () => {
      if (wsRef.current) {
        wsRef.current.close()
        wsRef.current = null
      }
    }
  }, [])

  const targetLabel = 'Browser ↔ Server'

  const runTest = useCallback(async () => {
    if (testing) return
    setTesting(true)
    setResults(null)

    try {
      // Same credential plumbing as the main socket: lg.bearer subprotocol when
      // the key is token-safe, ?key= otherwise, nothing at all without a key.
      const ws = await new Promise((resolve, reject) => {
        const s = openAuthedSocket('/ws/speedtest', authKey)
        wsRef.current = s
        s.binaryType = 'arraybuffer'
        const timer = setTimeout(() => {
          s.close()
          reject(new Error('WebSocket connection timed out'))
        }, 15000)
        s.onopen = () => { clearTimeout(timer); resolve(s) }
        s.onclose = () => { clearTimeout(timer); reject(new Error('Connection closed')) }
        s.onerror = () => { clearTimeout(timer); reject(new Error('WebSocket connection failed')) }
      })
      wsRef.current = ws

      setPhase('latency')
      const { avg: avgLatency, jitter } = await measureLatency(ws)

      setPhase('download')
      const dlSpeed = await measureDownload(ws)

      setPhase('upload')
      const ulSpeed = await measureUpload(ws)

      ws.close()
      wsRef.current = null

      setResults({
        download: dlSpeed.toFixed(2),
        upload: ulSpeed.toFixed(2),
        latency: avgLatency.toFixed(1),
        jitter: jitter.toFixed(1),
      })
    } catch (err) {
      setResults({ error: err.message })
    } finally {
      if (wsRef.current) {
        wsRef.current.close()
        wsRef.current = null
      }
      setTesting(false)
      setPhase(null)
    }
  }, [testing, authKey])

  return (
    <div className="rounded-xl border border-border/40 bg-bg-secondary/20 backdrop-blur-sm p-4 overflow-hidden relative">
      <div className="absolute -top-16 -right-16 w-32 h-32 rounded-full bg-warning/[0.04] blur-3xl pointer-events-none" />
      <div className="flex items-center justify-between mb-3">
        <div className="flex items-center gap-2">
          <Zap size={13} className="text-warning" />
          <span className="text-xs font-semibold uppercase tracking-widest text-warning">Speed Test</span>
        </div>
        <button
          onClick={runTest}
          disabled={testing}
          className="px-2.5 py-1 rounded-full text-[11px] font-medium
            bg-warning/15 text-warning hover:bg-warning/25
            disabled:opacity-40 transition-[background-color,opacity] duration-200 cursor-pointer"
        >
          {testing ? 'Testing...' : 'Run'}
        </button>
      </div>

      <div className="mb-3">
        <div className="flex items-center justify-between w-full px-2.5 py-1.5 rounded-lg text-[11px]
          bg-bg-tertiary/40 border border-border/30 text-text-secondary">
          <span className="truncate">Target: {targetLabel}</span>
        </div>
      </div>

      {phase && (
        <div className="flex items-center gap-2 text-xs text-text-muted mb-2">
          <Loader size={12} className="animate-spin" />
          {PHASE_LABELS[phase]}
        </div>
      )}

      {results ? (
        results.error ? (
          <div className="text-xs text-danger">{results.error}</div>
        ) : (
          <div className="grid grid-cols-2 gap-2">
            <StatCell icon={<ArrowDown size={13} className="text-success" />} label="Download" value={`${results.download} Mbps`} />
            <StatCell icon={<ArrowUp size={13} className="text-cyan" />} label="Upload" value={`${results.upload} Mbps`} />
            <StatCell icon={<Timer size={13} className="text-accent-text" />} label="Latency" value={`${results.latency} ms`} />
            <StatCell icon={<Gauge size={13} className="text-warning" />} label="Jitter" value={`${results.jitter} ms`} />
          </div>
        )
      ) : !testing ? (
        <div className="text-xs text-text-muted text-center py-3">
          Measures real throughput between your browser and the target.
        </div>
      ) : null}
    </div>
  )
}

function StatCell({ icon, label, value }) {
  return (
    <div className="flex items-center gap-1.5 p-2.5 rounded-lg bg-bg-tertiary/40">
      <div className="shrink-0">{icon}</div>
      <div className="min-w-0">
        <div className="text-[10px] text-text-muted">{label}</div>
        <div className="text-xs font-semibold text-text-primary tabular-nums">{value}</div>
      </div>
    </div>
  )
}
