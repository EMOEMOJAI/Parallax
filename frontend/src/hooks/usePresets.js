import { randomId } from '../lib/id'
import { useCallback, useEffect, useState } from 'react'
import { KIT_ALLOWED_STEP_TYPES } from '../lib/capabilities'

const STORAGE_KEY = 'lookingGlass.presets'
const MAX_PRESETS = 10

function load() {
  try {
    const raw = localStorage.getItem(STORAGE_KEY)
    if (!raw) return []
    const parsed = JSON.parse(raw)
    if (!Array.isArray(parsed)) return []
    // Defensive: ignore entries missing required fields so a corrupted blob
    // doesn't crash the dropdown render.
    return parsed.filter((p) => p && [...KIT_ALLOWED_STEP_TYPES, 'kit', 'shell'].includes(p.type) &&
      typeof p.target === 'string' && (p.options == null || typeof p.options === 'string')
    ).slice(0, MAX_PRESETS).map((p) => ({
      id: typeof p.id === 'string' ? p.id : randomId(),
      type: p.type, target: p.target.slice(0, 2048), options: (p.options || '').slice(0, 256),
      label: typeof p.label === 'string' ? p.label.slice(0, 256) : p.type,
    }))
  } catch {
    return []
  }
}

function save(list) {
  try {
    localStorage.setItem(STORAGE_KEY, JSON.stringify(list))
  } catch {
    // Storage may be full or disabled (private mode). Silent fail is fine —
    // the in-memory state still works for the current session.
  }
}

/**
 * Persistent saved-command presets backed by localStorage.
 *
 * Each preset: { id, type, target, options, label }
 * Capped at 10 entries; oldest evicted on add.
 */
export function usePresets() {
  const [presets, setPresets] = useState(() => load())

  // Sync across tabs.
  useEffect(() => {
    const onStorage = (e) => {
      if (e.key === STORAGE_KEY || e.key === null) setPresets(load())
    }
    window.addEventListener('storage', onStorage)
    return () => window.removeEventListener('storage', onStorage)
  }, [])

  const add = useCallback((entry) => {
    setPresets((prev) => {
      // Reject exact duplicates so spamming the save button doesn't fill the slot.
      const dup = prev.find((p) =>
        p.type === entry.type && p.target === entry.target && (p.options || '') === (entry.options || '')
      )
      if (dup) return prev
      const next = [
        { id: randomId(), ...entry },
        ...prev,
      ].slice(0, MAX_PRESETS)
      save(next)
      return next
    })
  }, [])

  const remove = useCallback((id) => {
    setPresets((prev) => {
      const next = prev.filter((p) => p.id !== id)
      save(next)
      return next
    })
  }, [])

  const clear = useCallback(() => {
    save([])
    setPresets([])
  }, [])

  return { presets, add, remove, clear, max: MAX_PRESETS }
}
