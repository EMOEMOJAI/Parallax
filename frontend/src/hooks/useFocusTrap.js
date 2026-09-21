import { useEffect } from 'react'

const FOCUSABLE = [
  'a[href]',
  'button:not([disabled])',
  'input:not([disabled])',
  'select:not([disabled])',
  'textarea:not([disabled])',
  '[tabindex]:not([tabindex="-1"])',
].join(',')

/**
 * Trap keyboard focus inside `containerRef` while `active` is true.
 * On activation, focus the first focusable element. On deactivation, restore
 * focus to whatever was focused before the trap engaged. Tab and Shift+Tab
 * cycle within the trapped subtree.
 */
export function useFocusTrap(containerRef, active) {
  useEffect(() => {
    if (!active) return
    const container = containerRef.current
    if (!container) return

    const previouslyFocused = document.activeElement

    const focusables = () =>
      Array.from(container.querySelectorAll(FOCUSABLE))
        .filter((el) => !el.matches(':disabled') && !el.closest('[aria-hidden="true"], [inert]') && el.getClientRects().length > 0)

    const initial = focusables()[0] || container
    if (initial && typeof initial.focus === 'function') {
      initial.focus({ preventScroll: true })
    }

    const onKeyDown = (e) => {
      if (e.key !== 'Tab') return
      const list = focusables()
      if (list.length === 0) {
        e.preventDefault()
        return
      }
      const first = list[0]
      const last = list[list.length - 1]
      const current = document.activeElement
      if (e.shiftKey) {
        if (current === first || !container.contains(current) || current === container) {
          e.preventDefault()
          last.focus()
        }
      } else {
        if (current === last || !container.contains(current) || current === container) {
          e.preventDefault()
          first.focus()
        }
      }
    }

    container.addEventListener('keydown', onKeyDown)
    return () => {
      container.removeEventListener('keydown', onKeyDown)
      if (previouslyFocused && typeof previouslyFocused.focus === 'function') {
        previouslyFocused.focus({ preventScroll: true })
      }
    }
  }, [active, containerRef])
}
