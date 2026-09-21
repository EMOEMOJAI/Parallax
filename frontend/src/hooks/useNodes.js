import { useState, useEffect, useCallback, useRef } from 'react'
import { apiFetch } from '../lib/api'
import { applyNodeStatus, rememberNodeStatus, reconcileNodeSnapshot } from '../lib/nodes'

export function useNodes(wsSubscribe, wsConnected, authKey = '', authRevision = 0) {
  const [nodes, setNodes] = useState([])
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState(null)
  const pendingRef = useRef(null)

  const fetchNodes = useCallback(async () => {
    pendingRef.current?.controller.abort()
    setLoading(true)
    const request = { controller: new AbortController(), updates: new Map() }
    pendingRef.current = request
    try {
      const res = await apiFetch('/api/nodes', { signal: request.controller.signal })
      if (!res.ok) throw new Error(`HTTP ${res.status}`)
      const snapshot = await res.json()
      if (pendingRef.current !== request || request.controller.signal.aborted) return
      if (!Array.isArray(snapshot)) throw new Error('Invalid node list')
      setError(null)
      // The HTTP snapshot may predate frames already delivered by the socket.
      // Replay those frames so a slow response cannot undo newer node state.
      setNodes(reconcileNodeSnapshot(snapshot, request.updates))
    } catch (err) {
      if (pendingRef.current === request && !request.controller.signal.aborted) {
        setError('Couldn’t load agents')
      }
    } finally {
      if (pendingRef.current === request) {
        pendingRef.current = null
        setLoading(false)
      }
    }
  }, [authKey, authRevision])

  useEffect(() => {
    fetchNodes()
    const interval = !wsConnected ? setInterval(() => { if (!pendingRef.current) fetchNodes() }, 15000) : null
    return () => {
      clearInterval(interval)
      pendingRef.current?.controller.abort()
      pendingRef.current = null
    }
  }, [fetchNodes, wsConnected])

  useEffect(() => {
    if (!wsSubscribe) return
    return wsSubscribe('node-status', (data) => {
      if (data.type !== 'node_status') return
      if (pendingRef.current) rememberNodeStatus(pendingRef.current.updates, data)
      setNodes((prev) => applyNodeStatus(prev, data))
    })
  }, [wsSubscribe])

  return { nodes, loading, error, refetch: fetchNodes }
}
