import { motionStateClass, useMountTransition } from '../hooks/useMountTransition'
import { randomId } from '../lib/id'
import { useEffect, useRef, useCallback, useState } from 'react'
import { Terminal } from '@xterm/xterm'
import { FitAddon } from '@xterm/addon-fit'
import { SquareTerminal, X, Maximize2, Minimize2 } from 'lucide-react'
import '@xterm/xterm/css/xterm.css'
import { useFocusTrap } from '../hooks/useFocusTrap'
import { useReducedMotion } from '../hooks/useReducedMotion'
import IconSwap from './IconSwap'

export default function ShellTerminal({ visible, onClose, nodeId, nodeName, ws, state: parentState = 'open' }) {
  // The parent retains the exit; start entry only once this lazy chunk mounts.
  const { state } = useMountTransition(visible && parentState !== 'closing')
  const dialogRef = useRef(null)
  useFocusTrap(dialogRef, visible && state !== 'closing')
  const termRef = useRef(null)
  const containerRef = useRef(null)
  const fitAddonRef = useRef(null)
  const sessionIdRef = useRef(null)
  // Track the nodeId that owns the current session to avoid orphaned sessions
  const sessionNodeIdRef = useRef(null)
  const [maximized, setMaximized] = useState(false)
  const reducedMotion = useReducedMotion()
  // Keep a ref to ws so the effect doesn't re-run when connected/reconnectAttempt changes
  const wsRef = useRef(ws)
  wsRef.current = ws

  // Initialize xterm
  useEffect(() => {
    if (!visible || !containerRef.current || !nodeId) return

    const term = new Terminal({
      cursorBlink: !window.matchMedia('(prefers-reduced-motion: reduce)').matches,
      fontSize: 13,
      fontFamily: "'JetBrains Mono', 'Fira Code', 'Cascadia Code', monospace",
      theme: {
        background: '#0e0e14',
        foreground: '#c8c8d0',
        cursor: '#7c5bf0',
        selectionBackground: '#7c5bf044',
        black: '#1a1a24',
        red: '#ff5f57',
        green: '#28c840',
        yellow: '#febc2e',
        blue: '#7c5bf0',
        magenta: '#c678dd',
        cyan: '#56b6c2',
        white: '#c8c8d0',
      },
      allowProposedApi: true,
    })

    const fitAddon = new FitAddon()
    term.loadAddon(fitAddon)
    term.open(containerRef.current)
    fitAddon.fit()

    termRef.current = term
    fitAddonRef.current = fitAddon

    // Start shell session — capture nodeId at creation time
    const id = randomId()
    const currentNodeId = nodeId
    sessionIdRef.current = id
    sessionNodeIdRef.current = currentNodeId
    if (!wsRef.current.connected) {
      term.write('\r\n\x1b[31m[Not connected to server — cannot start shell]\x1b[0m\r\n')
      sessionIdRef.current = null
      sessionNodeIdRef.current = null
      return () => {
        term.dispose()
        termRef.current = null
        fitAddonRef.current = null
      }
    }
    // Include the first fitted grid in startup; a resize frame can arrive
    // before the agent has created the PTY, and an unchanged fit emits none.
    wsRef.current.send({ action: 'shell_start', node_id: currentNodeId, id, cols: term.cols, rows: term.rows })

    // Send keystrokes to server
    term.onData((data) => {
      if (!sessionIdRef.current) return
      wsRef.current.send({
        action: 'shell_input',
        node_id: currentNodeId,
        id: sessionIdRef.current,
        input: { action: 'data', data },
      })
    })

    // Handle resize
    term.onResize(({ cols, rows }) => {
      if (!sessionIdRef.current) return
      wsRef.current.send({
        action: 'shell_input',
        node_id: currentNodeId,
        id: sessionIdRef.current,
        input: { action: 'resize', cols, rows },
      })
    })

    // Window resize handler
    const handleResize = () => fitAddon.fit()
    window.addEventListener('resize', handleResize)
    // Fit the actual box throughout maximize/restore, including its final size.
    const observer = new ResizeObserver(handleResize)
    observer.observe(containerRef.current)

    return () => {
      window.removeEventListener('resize', handleResize)
      observer.disconnect()
      // Kill the shell session using the captured nodeId, not the current one
      if (sessionIdRef.current && sessionNodeIdRef.current) {
        wsRef.current.send({
          action: 'cancel',
          node_id: sessionNodeIdRef.current,
          command_id: sessionIdRef.current,
        })
      }
      term.dispose()
      termRef.current = null
      fitAddonRef.current = null
      sessionIdRef.current = null
      sessionNodeIdRef.current = null
    }
  }, [visible, nodeId])

  // Notify user if WebSocket disconnects while shell is open
  const prevWsConnected = useRef(ws.connected)
  useEffect(() => {
    if (prevWsConnected.current && !ws.connected && termRef.current && visible) {
      sessionIdRef.current = null
      sessionNodeIdRef.current = null
      termRef.current.write('\r\n\x1b[31m[Connection lost — session may be dead. Close and reopen to start a new session.]\x1b[0m\r\n')
    }
    prevWsConnected.current = ws.connected
  }, [ws.connected, visible])

  // Subscribe to shell output — use a stable subscription key to avoid
  // missing messages during nodeId transitions
  useEffect(() => {
    if (!visible || !wsRef.current?.subscribe) return
    return wsRef.current.subscribe('shell-terminal', (data) => {
      if (!termRef.current) return
      if (data.id !== sessionIdRef.current) return
      if (data.type === 'shell_output') {
        termRef.current.write(data.data)
      } else if (data.type === 'done') {
        termRef.current.write('\r\n\x1b[90m[Session ended]\x1b[0m\r\n')
        sessionIdRef.current = null
        sessionNodeIdRef.current = null
      } else if (data.type === 'error') {
        // Sanitize error text to prevent terminal escape injection
        const safeText = (data.data || '').replace(/[\x00-\x1f\x7f]/g, '')
        termRef.current.write('\r\n\x1b[31m' + safeText + '\x1b[0m\r\n')
      }
    })
  }, [visible])

  // Update presentation without restarting an active PTY session.
  useEffect(() => {
    if (termRef.current) termRef.current.options.cursorBlink = !reducedMotion
  }, [reducedMotion])

  const handleClose = useCallback(() => {
    onClose()
  }, [onClose])

  if (!visible) return null

  return (
    <div data-state={state} className="motion-backdrop fixed inset-0 z-[100] flex items-center justify-center bg-black/60 backdrop-blur-sm">
      <div
        ref={dialogRef}
        role="dialog"
        aria-modal="true"
        aria-label={`Shell on ${nodeName || nodeId}`}
        tabIndex={-1}
        data-state={state}
        inert={state === 'closing'}
        aria-hidden={state === 'closing'}
        className={`t-modal shell-size ${motionStateClass(state)} flex flex-col rounded-2xl border border-border/40 bg-[#0e0e14] shadow-2xl shadow-black/60 overflow-hidden ${
          maximized ? 'w-full h-full m-0 rounded-none' : 'w-[min(90vw,64rem)] h-[75vh]'
        }`}
      >
        {/* Title bar */}
        <div className="flex items-center justify-between px-4 py-2.5 border-b border-border/30 bg-bg-secondary/40 shrink-0">
          <div className="flex items-center gap-3">
            <SquareTerminal size={14} className="text-accent-text" />
            <span className="text-xs text-text-muted font-mono">
              shell @ {nodeName || nodeId}
            </span>
          </div>
          <div className="flex items-center gap-1">
            <button
              onClick={() => setMaximized(!maximized)}
              aria-label={maximized ? 'Restore terminal size' : 'Maximize terminal'}
              className="p-1.5 rounded-md hover:bg-hover-overlay transition-colors text-text-muted hover:text-text-primary cursor-pointer"
            >
              <IconSwap active={maximized} from={<Maximize2 size={13} />} to={<Minimize2 size={13} />} />
            </button>
            <button
              onClick={handleClose}
              aria-label="Close shell"
              className="p-1.5 rounded-md hover:bg-hover-overlay transition-colors text-text-muted hover:text-danger cursor-pointer"
            >
              <X size={13} />
            </button>
          </div>
        </div>

        {/* Terminal */}
        <div ref={containerRef} className="flex-1 p-1" />
      </div>
    </div>
  )
}
