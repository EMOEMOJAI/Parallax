import { useState, useRef, useEffect } from 'react'
import { useMountTransition } from './useMountTransition'

/**
 * Reusable dropdown hook with outside-click handling.
 * Returns { ref, open, setOpen, mounted, state } — attach ref to the dropdown
 * container, render the panel while `mounted`, and pass `state` to the panel's
 * data-state so it can animate out before it unmounts.
 */
export function useDropdown() {
  const [open, setOpen] = useState(false)
  const ref = useRef(null)
  const { mounted, state } = useMountTransition(open)

  useEffect(() => {
    if (!open) return
    const handleClick = (e) => {
      if (ref.current && !ref.current.contains(e.target)) setOpen(false)
    }
    const handleKey = (e) => {
      if (e.key === 'Escape') setOpen(false)
    }
    document.addEventListener('mousedown', handleClick)
    document.addEventListener('keydown', handleKey)
    return () => {
      document.removeEventListener('mousedown', handleClick)
      document.removeEventListener('keydown', handleKey)
    }
  }, [open])

  return { ref, open, setOpen, mounted, state }
}
