import { useEffect, useState } from 'react'

/**
 * Keeps a surface mounted for the length of its close transition, so a modal or
 * dropdown can animate out instead of vanishing. Returns `{ mounted, state }`
 * where `state` is 'enter' | 'open' | 'closing' | 'closed' — render while
 * `mounted`, and put `state` on `data-state` so the `.motion-*` rules in
 * index.css can drive the motion.
 *
 * `closeMs` must match the close duration in CSS (--duration-quick, 150ms) or
 * the element unmounts mid-fade.
 */
export function useMountTransition(visible, closeMs = 150) {
  const [mounted, setMounted] = useState(visible)
  const [state, setState] = useState(visible ? 'open' : 'closed')

  useEffect(() => {
    if (visible) {
      setMounted(true)
      // Two frames: the first commits the 'enter' styles, the second flips to
      // 'open' so the browser has something to transition from.
      setState('enter')
      let inner
      const outer = requestAnimationFrame(() => {
        inner = requestAnimationFrame(() => setState('open'))
      })
      return () => {
        cancelAnimationFrame(outer)
        if (inner) cancelAnimationFrame(inner)
      }
    }

    setState((s) => (s === 'closed' ? s : 'closing'))
    const timer = setTimeout(() => {
      setMounted(false)
      setState('closed')
    }, closeMs)
    return () => clearTimeout(timer)
  }, [visible, closeMs])

  // Render on the opening commit so dialog refs exist when focus effects run.
  return { mounted: visible || mounted, state }
}
