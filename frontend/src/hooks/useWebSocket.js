import { useEffect, useRef, useState, useCallback, useMemo } from 'react'
import { openAuthedSocket, notifyAuthRequired } from '../lib/api'

const INITIAL_DELAY = 1000
const MAX_DELAY = 30000
const BACKOFF_FACTOR = 1.5

// Private-use close code the server sends when the client key is rejected.
// Pause automatic retries on rejection until the user explicitly reconnects.
const CLOSE_UNAUTHORIZED = 4401

/**
 * @param {string} url  server-relative path, e.g. '/ws/client'
 * @param {string} [authKey]  client API key; a string, so the connect callback
 *   keeps a stable identity until the credential changes.
 * @param {number} [authRevision] explicit Connect attempts, including the same key.
 */
export function useWebSocket(url, authKey = '', authRevision = 0) {
  const wsRef = useRef(null)
  const [connected, setConnected] = useState(false)
  const [reconnectAttempt, setReconnectAttempt] = useState(0)
  const listenersRef = useRef(new Map())
  const closedRef = useRef(false)
  const reconnectTimer = useRef(null)
  const attemptRef = useRef(0)
  const keepaliveTimer = useRef(null)
  // Generation counter to prevent stale onclose handlers from reconnecting
  const generationRef = useRef(0)

  const connect = useCallback(() => {
    if (closedRef.current) return

    // Clear any existing keepalive from a previous connection attempt
    clearInterval(keepaliveTimer.current)

    const myGeneration = generationRef.current

    // The key travels as the lg.bearer subprotocol when it is token-safe,
    // otherwise as ?key= — see lib/api.js.
    const ws = openAuthedSocket(url, authKey)

    ws.onopen = () => {
      if (generationRef.current !== myGeneration) { ws.close(); return }
      attemptRef.current = 0
      setReconnectAttempt(0)
      setConnected(true)
    }

    ws.onclose = (event) => {
      if (generationRef.current !== myGeneration || wsRef.current !== ws) return
      setConnected(false)
      wsRef.current = null
      clearInterval(keepaliveTimer.current)
      // 4401: the server rejected this key. Stop the backoff loop and ask the
      // app for a key; the effect below re-dials on the next Connect attempt.
      // Generation-checked like the reconnect branch: a 4401 that arrives from
      // the previous socket after a new key was entered must not close down the
      // connection that key just opened.
      if (event?.code === CLOSE_UNAUTHORIZED) {
        if (generationRef.current === myGeneration) {
          closedRef.current = true
          notifyAuthRequired()
        }
        return
      }
      // Only reconnect if this connection belongs to the current generation
      if (!closedRef.current && generationRef.current === myGeneration) {
        attemptRef.current += 1
        setReconnectAttempt(attemptRef.current)
        const delay = Math.min(INITIAL_DELAY * Math.pow(BACKOFF_FACTOR, attemptRef.current - 1), MAX_DELAY)
        reconnectTimer.current = setTimeout(connect, delay)
      }
    }

    ws.onerror = () => {
      // onclose will fire after this, so reconnection is handled there
    }

    ws.onmessage = (event) => {
      if (generationRef.current !== myGeneration || closedRef.current) return
      try {
        const data = JSON.parse(event.data)
        listenersRef.current.forEach((cb) => {
          try { cb(data) } catch (err) { console.error('WebSocket listener error:', err) }
        })
      } catch {
        // ignore non-JSON messages
      }
    }
    wsRef.current = ws

    // Application-level keepalive to detect silent connection drops
    // (e.g., NAT timeout, load balancer idle timeout)
    keepaliveTimer.current = setInterval(() => {
      if (ws.readyState === WebSocket.OPEN) {
        ws.send(JSON.stringify({ action: 'ping' }))
      }
    }, 30000)
  }, [url, authKey])

  useEffect(() => {
    closedRef.current = false
    setConnected(false)
    attemptRef.current = 0
    generationRef.current += 1
    connect()
    return () => {
      closedRef.current = true
      clearTimeout(reconnectTimer.current)
      clearInterval(keepaliveTimer.current)
      generationRef.current += 1
      const socket = wsRef.current
      wsRef.current = null
      socket?.close()
    }
  }, [connect, authRevision])

  const send = useCallback((data) => {
    if (wsRef.current?.readyState === WebSocket.OPEN) {
      wsRef.current.send(JSON.stringify(data))
    }
  }, [])

  // Use unique IDs per subscription to avoid collisions
  const subscribeIdCounter = useRef(0)
  const subscribe = useCallback((prefix, callback) => {
    const id = `${prefix}-${++subscribeIdCounter.current}`
    listenersRef.current.set(id, callback)
    return () => listenersRef.current.delete(id)
  }, [])

  return useMemo(() => ({ connected, send, subscribe, reconnectAttempt }), [connected, send, subscribe, reconnectAttempt])
}
