import { useEffect, useState } from 'react'
import { useReducedMotion } from './useReducedMotion'

export function motionStateClass(state) {
  return state === 'open' ? 'is-open' : state === 'closing' ? 'is-closing' : ''
}

function closeDuration(token) {
  const value = getComputedStyle(document.documentElement).getPropertyValue(token).trim()
  const amount = parseFloat(value)
  return Number.isFinite(amount)
    ? Math.max(0, amount * (value.endsWith('ms') ? 1 : 1000))
    : 150
}

/** Keep the exit mounted for its CSS duration. Reopening cancels stale cleanup;
 * reduced motion skips both the entrance frames and the exit timer. */
export function useMountTransition(visible, closeToken = '--modal-close-dur') {
  const reducedMotion = useReducedMotion()
  const [mounted, setMounted] = useState(visible)
  const [phase, setPhase] = useState(visible ? 'enter' : 'closed')

  useEffect(() => {
    if (visible) {
      setMounted(true)
      if (reducedMotion) {
        setPhase('open')
        return
      }
      // Reverse an interrupted exit without flashing back to the entry scale.
      setPhase((previous) => previous === 'closed' || previous === 'enter' ? 'enter' : 'open')
      let inner
      const outer = requestAnimationFrame(() => {
        inner = requestAnimationFrame(() => setPhase('open'))
      })
      return () => {
        cancelAnimationFrame(outer)
        if (inner) cancelAnimationFrame(inner)
      }
    }

    const finish = () => {
      setMounted(false)
      setPhase('closed')
    }
    if (reducedMotion) {
      finish()
      return
    }
    setPhase((previous) => previous === 'closed' ? previous : 'closing')
    const timer = setTimeout(finish, closeDuration(closeToken))
    return () => clearTimeout(timer)
  }, [visible, closeToken, reducedMotion])

  // Keep refs available on the opening commit; disable exits immediately.
  const state = visible ? (reducedMotion ? 'open' : phase) : mounted ? 'closing' : 'closed'
  return { mounted: reducedMotion ? visible : visible || mounted, state }
}
