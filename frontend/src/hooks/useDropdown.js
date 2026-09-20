import { useState, useRef, useEffect, useCallback } from 'react'
import { useMountTransition } from './useMountTransition'

/**
 * Reusable dropdown hook with outside-click handling.
 * Returns { ref, open, setOpen, mounted, state } — attach ref to the dropdown
 * container, render the panel while `mounted`, and pass `state` to the panel's
 * data-state so it can animate out before it unmounts.
 */
export function useDropdown() {
  const [open, updateOpen] = useState(false)
  const ref = useRef(null)
  const { mounted, state } = useMountTransition(open, '--dropdown-close-dur')
  const setOpen = useCallback((next) => {
    const value = typeof next === 'function' ? next(open) : next
    // Restore focus before inert can blur it. Outside clicks still get their
    // normal browser focus afterward; they must not be pulled back on cleanup.
    if (!value && ref.current?.contains(document.activeElement)) {
      ref.current.querySelector('[aria-expanded]')?.focus({ preventScroll: true })
    }
    updateOpen(value)
  }, [open])

  useEffect(() => {
    if (!open) return
    const handleClick = (e) => {
      if (ref.current && !ref.current.contains(e.target)) setOpen(false)
    }
    const handleKey = (e) => {
      if (e.key === 'Escape') {
        setOpen(false)
      }
    }
    document.addEventListener('mousedown', handleClick)
    document.addEventListener('keydown', handleKey)
    return () => {
      document.removeEventListener('mousedown', handleClick)
      document.removeEventListener('keydown', handleKey)
    }
  }, [open, setOpen])

  return { ref, open, setOpen, mounted, state }
}
