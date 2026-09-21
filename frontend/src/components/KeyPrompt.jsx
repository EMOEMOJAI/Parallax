import { useState, useRef, useEffect } from 'react'
import { KeyRound } from 'lucide-react'
import { useFocusTrap } from '../hooks/useFocusTrap'
import { motionStateClass, useMountTransition } from '../hooks/useMountTransition'

/**
 * Asks for the client API key. Shown when /api/public-config reports
 * auth_required and nothing is stored yet, or whenever the server answers 401 /
 * closes the socket with 4401 (the `lg:auth-required` event, handled in App).
 *
 * The key is never rendered back after saving and is not logged; it lives in
 * browser storage under `lg-client-key` and in App state for this tab.
 */
export default function KeyPrompt({ visible, onSave, invalid }) {
  const [value, setValue] = useState('')
  const [remember, setRemember] = useState(false)
  const dialogRef = useRef(null)
  useFocusTrap(dialogRef, visible)

  // Clear the field whenever the prompt re-opens so a rejected key isn't
  // silently resubmitted.
  useEffect(() => {
    if (visible) {
      setValue('')
      setRemember(false)
    }
  }, [visible])

  // Stay mounted through the close transition so the modal can animate out.
  const { mounted, state } = useMountTransition(visible)
  if (!mounted) return null

  const submit = (e) => {
    e.preventDefault()
    const key = value.trim()
    if (!key) return
    onSave(key, remember)
  }

  return (
    <div data-state={state} className="motion-layer fixed inset-0 z-[60] flex items-center justify-center p-4">
      <div data-state={state} className="motion-backdrop absolute inset-0 bg-black/70 backdrop-blur-sm" />
      <form
        ref={dialogRef}
        role="dialog"
        aria-modal="true"
        aria-label="API key required"
        onSubmit={submit}
        data-state={state}
        inert={state === 'closing'}
        aria-hidden={state === 'closing'}
        className={`t-modal ${motionStateClass(state)} relative w-full max-w-sm rounded-xl border border-border/40 bg-bg-secondary/95
          p-5 shadow-2xl space-y-4`}
      >
        <div className="flex items-center gap-2.5">
          <div className="h-8 w-8 rounded-lg bg-accent/15 flex items-center justify-center ring-1 ring-accent/20">
            <KeyRound size={15} className="text-accent-text" />
          </div>
          <div>
            <h2 className="text-sm font-semibold text-text-primary">Connect to Parallax</h2>
            <p className="text-[11px] text-text-muted">Enter your access key to open this private dashboard.</p>
          </div>
        </div>

        {invalid && (
          <p className="text-[11px] text-danger" role="alert">
            That key was rejected. Check it and try again.
          </p>
        )}

        <input
          type="password"
          autoComplete="off"
          spellCheck="false"
          value={value}
          onChange={(e) => setValue(e.target.value)}
          placeholder="Client API key"
          aria-label="Client API key"
          className="w-full rounded-lg bg-bg-primary/60 border border-border/40 px-3 py-2
            text-sm text-text-primary placeholder:text-text-muted
            focus:outline-none focus:border-accent/60"
        />

        <label className="flex min-h-11 items-center gap-2.5 text-xs text-text-secondary cursor-pointer">
          <input type="checkbox" checked={remember} onChange={(e) => setRemember(e.target.checked)}
            className="h-4 w-4 accent-accent" />
          Remember this browser
        </label>

        <button
          type="submit"
          disabled={!value.trim()}
          className="w-full rounded-lg bg-accent/20 border border-accent/40 px-3 py-2
            text-sm font-medium text-accent-text hover:bg-accent/30 disabled:opacity-40
            disabled:cursor-not-allowed transition-colors duration-200 cursor-pointer"
        >
          Connect
        </button>

        <p className="text-[11px] text-text-muted">
          {remember
            ? 'Stay connected on this browser. Use only on a device you trust.'
            : 'Your key stays in this tab until you close it. Ask your administrator if you need a key.'}
        </p>
      </form>
    </div>
  )
}
