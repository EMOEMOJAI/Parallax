import { useState, useCallback, useEffect, useRef } from 'react'
import { KIT_ALLOWED_STEP_TYPES } from '../lib/capabilities'

const STORAGE_KEY = 'lg-cmd-history'
const MAX_HISTORY = 50

function loadHistory() {
  try {
    const data = JSON.parse(localStorage.getItem(STORAGE_KEY))
    return Array.isArray(data) ? data.filter((entry) => (
      entry && [...KIT_ALLOWED_STEP_TYPES, 'kit', 'shell'].includes(entry.type) && typeof entry.target === 'string'
    )).slice(0, MAX_HISTORY).map((entry) => ({ type: entry.type, target: entry.target.slice(0, 2048) })) : []
  } catch {
    return []
  }
}

export function useCommandHistory() {
  const [history, setHistory] = useState(loadHistory)
  const indexRef = useRef(-1)
  // Keep a synchronous ref copy of history so navigate() can read it
  // without depending on React 18's batched state updates
  const historyRef = useRef(history)

  useEffect(() => {
    const sync = (event) => {
      if (event.key !== STORAGE_KEY && event.key !== null) return
      const next = loadHistory()
      historyRef.current = next
      indexRef.current = -1
      setHistory(next)
    }
    window.addEventListener('storage', sync)
    return () => window.removeEventListener('storage', sync)
  }, [])

  const push = useCallback((entry) => {
    setHistory((prev) => {
      // Deduplicate consecutive identical commands
      if (prev.length > 0 && prev[0].type === entry.type && prev[0].target === entry.target) {
        return prev
      }
      const next = [entry, ...prev].slice(0, MAX_HISTORY)
      try {
        localStorage.setItem(STORAGE_KEY, JSON.stringify(next))
      } catch {
        // QuotaExceededError in private browsing or full storage — ignore
      }
      indexRef.current = -1
      historyRef.current = next
      return next
    })
  }, [])

  const navigate = useCallback((direction) => {
    // direction: 1 = older (up), -1 = newer (down)
    // Read current history synchronously from ref to avoid React 18 batching issues
    const current = historyRef.current
    let newIndex = indexRef.current + direction
    if (newIndex < -1) newIndex = -1
    if (newIndex >= current.length) newIndex = current.length - 1
    indexRef.current = newIndex
    return newIndex === -1 ? null : current[newIndex] || null
  }, [])

  const clear = useCallback(() => {
    setHistory([])
    historyRef.current = []
    try { localStorage.removeItem(STORAGE_KEY) } catch { /* in-memory history is already cleared */ }
    indexRef.current = -1
  }, [])

  return { history, push, navigate, clear }
}
