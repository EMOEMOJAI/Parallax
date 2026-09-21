import { useMemo, useRef, useState, useEffect } from 'react'
import { createPortal } from 'react-dom'
import { useFocusTrap } from '../hooks/useFocusTrap'
import { apiFetch, AUTH_REQUIRED_EVENT } from '../lib/api'
import { redactShare } from '../lib/resultExport'

export default function SharePreview({ snapshot, onClose, onShared }) {
  const ref = useRef(null)
  const linkRef = useRef(null)
  const pending = useRef(false)
  const controller = useRef(null)
  const [terms, setTerms] = useState('')
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState('')
  const [url, setUrl] = useState('')
  useEffect(() => { if (url) linkRef.current?.focus() }, [url])
  useEffect(() => {
    const root = document.getElementById('root')
    if (!root) return
    const wasInert = root.inert
    root.inert = true
    return () => { root.inert = wasInert }
  }, [])
  useFocusTrap(ref, true)
  useEffect(() => () => controller.current?.abort(), [])
  useEffect(() => {
    const escape = (event) => { if (event.key === 'Escape') { event.stopPropagation(); onClose() } }
    window.addEventListener(AUTH_REQUIRED_EVENT, onClose)
    window.addEventListener('keydown', escape, true)
    return () => {
      window.removeEventListener(AUTH_REQUIRED_EVENT, onClose)
      window.removeEventListener('keydown', escape, true)
    }
  }, [onClose])
  const payload = useMemo(() => redactShare(snapshot, terms), [snapshot, terms])
  const body = useMemo(() => JSON.stringify(payload), [payload])
  const preview = useMemo(() => [
    `Node: ${payload.node_flag} ${payload.node_name}`.trim(),
    `Location: ${payload.node_location}`,
    `Command: ${payload.command}`,
    `Target: ${payload.target}`,
    `Options: ${payload.options || '(default)'}`,
    '', 'Output', ...payload.lines.map((line) => line.text),
  ].join('\n'), [payload])
  const tooLarge = useMemo(() => payload.lines.length > 5000 || new TextEncoder().encode(body).length > 1048576, [payload, body])
  const create = async () => {
    if (pending.current || url || tooLarge) return
    pending.current = true
    setBusy(true)
    setError('')
    const request = new AbortController()
    controller.current = request
    try {
      const response = await apiFetch('/api/runs', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body, signal: request.signal })
      if (!response.ok) throw new Error(`HTTP ${response.status}`)
      const { id } = await response.json()
      if (typeof id !== 'string' || !/^[a-zA-Z0-9-]{1,64}$/.test(id)) throw new Error('Invalid link response')
      const link = `${window.location.origin}/?run=${id}`
      if (request.signal.aborted) return
      setUrl(link)
      try {
        await navigator.clipboard.writeText(link)
        if (!request.signal.aborted) onShared('Link copied to clipboard')
      } catch {
        if (!request.signal.aborted) setError('Couldn’t copy the link. Select it below and copy it manually.')
      }
    } catch (err) {
      if (!request.signal.aborted) setError(`Couldn’t create a share link (${err.message}). Try again.`)
    } finally {
      pending.current = false
      if (!request.signal.aborted) setBusy(false)
    }
  }
  return createPortal(
    <div className="fixed inset-0 z-[60] flex items-center justify-center bg-black/70 p-4">
      <section ref={ref} role="dialog" aria-modal="true" aria-label="Share preview"
        className="flex max-h-[90vh] w-full max-w-2xl flex-col gap-3 overflow-auto rounded-xl border border-border/50 bg-bg-elevated p-5 text-sm text-text-primary shadow-2xl">
        <div className="flex items-center justify-between gap-3">
          <h2 className="font-semibold">Review before sharing</h2>
          <button onClick={onClose} className="min-h-11 rounded-lg px-3 hover:bg-hover-overlay cursor-pointer">Close preview</button>
        </div>
        <p className="text-xs text-text-muted">Anyone with the link can read this snapshot without signing in. It expires 24 hours after creation and may disappear sooner if the server restarts or removes older runs.</p>
        {!url && <label className="text-xs space-y-2">Redact exact text (one value per line)
          <textarea aria-label="Redact exact text" value={terms} disabled={busy} maxLength={4096} onChange={(event) => setTerms(event.target.value)}
            placeholder="Paste an address, hostname, or name to hide" rows={2}
            className="mt-2 block w-full rounded-lg border border-border/40 bg-bg-primary p-3 text-text-primary" />
          <span className="block text-text-muted">Replaces matching text in metadata and output. Review the preview for anything else you want to remove.</span>
        </label>}
        <label className="text-xs">Shared content
          <textarea aria-label="Shared content" readOnly value={preview} rows={12}
            className="mt-2 block w-full rounded-lg border border-border/40 bg-bg-primary p-3 font-mono text-xs text-text-secondary" />
        </label>
        {tooLarge && <p role="alert">This snapshot is too large to share. Download it instead (limit: 5,000 lines and 1 MiB).</p>}
        {error && <p role="alert" className="text-warning">{error}</p>}
        {url ? <label className="text-xs">Share link
          <input ref={linkRef} aria-label="Share link" readOnly value={url} onFocus={(event) => event.target.select()}
            className="mt-2 min-h-11 w-full rounded-lg border border-border/40 bg-bg-primary px-3 text-text-primary" />
        </label> : <button onClick={create} disabled={busy || tooLarge}
          className="min-h-11 rounded-lg border border-accent/40 bg-accent/20 px-4 text-accent-text disabled:opacity-40 cursor-pointer">
          {busy ? 'Creating link…' : 'Create share link'}
        </button>}
      </section>
    </div>, document.body)
}
