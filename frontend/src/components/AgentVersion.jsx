import { useEffect, useRef, useState } from 'react'
import { Copy, Check } from 'lucide-react'

export default function AgentVersion({ version }) {
  const value = typeof version === 'string' ? version : ''
  const label = /^[a-f0-9]{12,64}$/i.test(value) ? `${value.slice(0, 8)}…` : value || 'unknown'
  const [copied, setCopied] = useState(false)
  const [failed, setFailed] = useState(false)
  const timer = useRef(null)
  const revision = useRef(0)
  useEffect(() => {
    revision.current++
    setCopied(false)
    setFailed(false)
    return () => { revision.current++; clearTimeout(timer.current) }
  }, [value])
  const copy = async () => {
    const current = revision.current
    try {
      await navigator.clipboard.writeText(value)
      if (revision.current !== current) return
      setCopied(true)
      setFailed(false)
      clearTimeout(timer.current)
      timer.current = setTimeout(() => setCopied(false), 2000)
    } catch {
      if (revision.current === current) setFailed(true)
    }
  }
  return (
    <div className="min-w-0 text-[11px]">
      <div className="flex items-center gap-1.5">
        <span title={value || 'unknown'} className="min-w-0 break-all font-mono">{label}</span>
        {value && value !== 'unknown' && (
          <button onClick={copy} aria-label="Copy full agent version" title="Copy full agent version"
            className="flex min-h-11 min-w-11 shrink-0 items-center justify-center rounded-lg hover:bg-hover-overlay cursor-pointer">
            {copied ? <Check size={13} className="text-success" /> : <Copy size={13} />}
          </button>
        )}
      </div>
      <span className="sr-only" role="status">{copied ? 'Agent version copied' : ''}</span>
      {failed && <div role="status" className="text-warning">
        Couldn’t copy. Select the full version below and copy it manually.
        <div className="break-all font-mono select-all text-text-primary">{value}</div>
      </div>}
    </div>
  )
}
