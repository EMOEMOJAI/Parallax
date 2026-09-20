import { useCallback, useEffect, useState } from 'react'
import { KIT_ALLOWED_STEP_TYPES } from '../lib/capabilities'

// The user-defined diagnostic kit (S10).
//
// Storage key frozen by CLAUDE.md's Naming section — renaming it would silently
// discard every saved kit.
const STORAGE_KEY = 'lg-kit'

// 1 to 8 steps. The minimum is load-bearing: App's run path reads
// KIT_SEQUENCE[0].type inside an event handler that no ErrorBoundary covers
// (the boundary wraps only the modals), so an empty kit would throw on
// `undefined`. The maximum bounds one click's worth of dispatches on a node.
export const MIN_KIT_STEPS = 1
export const MAX_KIT_STEPS = 8

// An options string is a short token list (`count=5`, `type=A`). The agent
// re-validates every token, so this cap is only about keeping a hand-edited
// localStorage value from bloating the dispatch.
const MAX_OPTIONS_LEN = 256

// Today's three hardcoded steps, so an existing user sees no change. `type=A`
// is the S6 form — the bare `A` was migrated and must not come back.
export const DEFAULT_KIT = [
  { type: 'ping', options: 'count=5' },
  { type: 'traceroute', options: '' },
  { type: 'dns', options: 'type=A' },
]

function defaultKit() {
  return DEFAULT_KIT.map((step) => ({ ...step }))
}

/**
 * validateKit(value) — the persisted value is untrusted input (hand-edited
 * localStorage, or a blob written by an older build). Every entry is coerced to
 * `{type, options}` with the type filtered through KIT_ALLOWED_STEP_TYPES —
 * which excludes 'kit' (self-referential) and 'shell' (a modal, not a
 * dispatched command) — and the list clamped to MAX_KIT_STEPS.
 *
 * **Zero surviving steps falls back to the default kit**, never an empty list:
 * an empty kit throws in the run path and renders as permanently unavailable.
 */
export function validateKit(value) {
  if (!Array.isArray(value)) return defaultKit()
  const steps = []
  for (const entry of value) {
    if (steps.length >= MAX_KIT_STEPS) break
    // A bare string ('ping') is accepted as a type-only step so a hand-written
    // value is usable, but it is normalized on the way in.
    const type = typeof entry === 'string' ? entry : entry?.type
    if (typeof type !== 'string' || !KIT_ALLOWED_STEP_TYPES.includes(type)) continue
    const rawOptions = typeof entry === 'string' ? '' : entry?.options
    const options = typeof rawOptions === 'string' ? rawOptions.slice(0, MAX_OPTIONS_LEN) : ''
    // A step carries no target of its own — the target comes from the run.
    steps.push({ type, options })
  }
  return steps.length === 0 ? defaultKit() : steps
}

function load() {
  try {
    const raw = localStorage.getItem(STORAGE_KEY)
    if (!raw) return defaultKit()
    return validateKit(JSON.parse(raw))
  } catch {
    // Unparsable blob, or storage disabled (private-mode Safari): the default
    // kit still works for this session.
    return defaultKit()
  }
}

function save(steps) {
  try {
    localStorage.setItem(STORAGE_KEY, JSON.stringify(steps))
  } catch {
    // Storage full or unavailable — the in-memory kit still runs.
  }
}

/**
 * The user-defined diagnostic kit, persisted in localStorage.
 *
 * `kit` is a **referentially stable** value: it only changes identity when the
 * kit actually changes, so a consumer may mirror it into a ref without
 * resubscribing anything on every render.
 *
 * **Exactly one instance of this hook exists, in App.jsx.** Two instances do
 * not share state — the `storage` event does not fire in the document that
 * wrote the value — so an edit made through a second instance would never reach
 * App's run path. Consumers get the kit and these callbacks as props.
 */
export function useKits() {
  const [kit, setKit] = useState(() => load())

  // Sync across tabs (never fires for our own writes; see above).
  useEffect(() => {
    const onStorage = (e) => {
      if (e.key === STORAGE_KEY) setKit(load())
    }
    window.addEventListener('storage', onStorage)
    return () => window.removeEventListener('storage', onStorage)
  }, [])

  const addStep = useCallback((step) => {
    setKit((prev) => {
      if (prev.length >= MAX_KIT_STEPS) return prev
      const next = validateKit([...prev, step])
      // validateKit dropped it (type not allowed): keep the identity stable so
      // nothing downstream re-renders for a no-op.
      if (next.length === prev.length) return prev
      save(next)
      return next
    })
  }, [])

  const removeStep = useCallback((index) => {
    setKit((prev) => {
      // Never below the minimum: the caller also disables the control at one
      // step, but the rule is enforced here so no path can empty the kit.
      if (prev.length <= MIN_KIT_STEPS) return prev
      if (!Number.isInteger(index) || index < 0 || index >= prev.length) return prev
      const next = prev.filter((_, i) => i !== index)
      save(next)
      return next
    })
  }, [])

  const reset = useCallback(() => {
    const next = defaultKit()
    save(next)
    setKit(next)
  }, [])

  return { kit, addStep, removeStep, reset, max: MAX_KIT_STEPS, min: MIN_KIT_STEPS }
}
