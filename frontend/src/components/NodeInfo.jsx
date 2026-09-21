import React from 'react'
import { Copy, Check, Server } from 'lucide-react'
import IconSwap from './IconSwap'

export default function NodeInfo({ node }) {
  const [copyError, setCopyError] = React.useState('')
  const [copied, setCopied] = React.useState(null)
  const copiedTimer = React.useRef(null)
  React.useEffect(() => () => clearTimeout(copiedTimer.current), [])

  const copyToClipboard = async (text, field) => {
    setCopyError('')
    try {
      await navigator.clipboard.writeText(text)
      setCopied(field)
      clearTimeout(copiedTimer.current)
      copiedTimer.current = setTimeout(() => setCopied(null), 2000)
    } catch {
      setCopyError('Couldn’t copy. Select the address and copy it manually.')
    }
  }

  if (!node) return null

  return (
    <div className="rounded-xl border border-border/40 bg-bg-secondary/20 backdrop-blur-sm p-4 overflow-hidden relative">
      <div className="absolute -top-16 -right-16 w-32 h-32 rounded-full bg-accent/[0.04] blur-3xl pointer-events-none" />
      <div className="flex items-center gap-2 mb-3">
        <Server size={13} className="text-accent-text" />
        <span className="text-xs font-semibold uppercase tracking-widest text-accent-text">Node Details</span>
      </div>
      {copyError && <p role="status" className="mb-3 text-xs text-warning">{copyError}</p>}
      <div className="grid grid-cols-2 gap-x-4 gap-y-2 text-xs">
        <div>
          <span className="text-text-muted">Provider</span>
          <div className="font-medium text-text-primary">{node.provider || 'N/A'}</div>
        </div>
        <div>
          <span className="text-text-muted">Location</span>
          <div className="font-medium text-text-primary">{node.location}</div>
        </div>
        <div className="col-span-2">
          {/* Reported by the agent, stored as an opaque string. An agent from
              before capability reporting sends nothing, hence "unknown". */}
          <span className="text-text-muted">Agent version</span>
          <div className="font-mono font-medium text-text-primary text-[11px]">{node.version || 'unknown'}</div>
        </div>
        <div className="col-span-2">
          <span className="text-text-muted">IPv4</span>
          <div className="flex items-center gap-1.5">
            <span className="font-mono font-medium text-text-primary">{node.ipv4 || 'N/A'}</span>
            {node.ipv4 && (
              <button onClick={() => copyToClipboard(node.ipv4, 'ipv4')} aria-label="Copy IPv4 address"
                className="p-0.5 rounded hover:bg-hover-overlay transition-colors cursor-pointer">
                <IconSwap active={copied === 'ipv4'} from={<Copy size={10} className="text-text-muted" />} to={<Check size={10} className="text-success" />} />
              </button>
            )}
          </div>
        </div>
        {node.ipv6 && (
          <div className="col-span-2">
            <span className="text-text-muted">IPv6</span>
            <div className="flex items-center gap-1.5">
              <span className="font-mono font-medium text-text-primary text-[11px]">{node.ipv6}</span>
              <button onClick={() => copyToClipboard(node.ipv6, 'ipv6')} aria-label="Copy IPv6 address"
                className="p-0.5 rounded hover:bg-hover-overlay transition-colors cursor-pointer">
                <IconSwap active={copied === 'ipv6'} from={<Copy size={10} className="text-text-muted" />} to={<Check size={10} className="text-success" />} />
              </button>
            </div>
          </div>
        )}
      </div>
    </div>
  )
}
