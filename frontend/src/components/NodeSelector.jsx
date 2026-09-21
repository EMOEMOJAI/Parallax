import { motionStateClass } from '../hooks/useMountTransition'
import { useEffect, useMemo, useRef, useState } from 'react'
import { ChevronDown, Wifi, WifiOff, MapPin, Loader, Search } from 'lucide-react'
import { useDropdown } from '../hooks/useDropdown'

// Threshold above which a search box is rendered. Below this, the list is short
// enough to scan visually and a search box is just chrome.
const SEARCH_THRESHOLD = 6

export default function NodeSelector({ nodes, selectedNode, onSelect, compact, loading }) {
  const { ref, open, setOpen, mounted, state } = useDropdown()
  const [query, setQuery] = useState('')
  const searchRef = useRef(null)

  const selected = nodes.find((n) => n.id === selectedNode)

  // Reset search when the dropdown closes; focus the input when it opens.
  useEffect(() => {
    if (!open) {
      setQuery('')
      return
    }
    if (nodes.length > SEARCH_THRESHOLD) {
      // Defer focus until the input has actually mounted.
      const frame = requestAnimationFrame(() => searchRef.current?.focus())
      return () => cancelAnimationFrame(frame)
    }
  }, [open, nodes.length])

  const filtered = useMemo(() => {
    const q = query.trim().toLowerCase()
    if (!q) return nodes
    return nodes.filter((n) =>
      (n.name || '').toLowerCase().includes(q) ||
      (n.location || '').toLowerCase().includes(q) ||
      (n.provider || '').toLowerCase().includes(q)
    )
  }, [nodes, query])

  const handleKeyDown = (e) => {
    if (e.key === 'Enter' && filtered.length > 0) {
      // Pick the first match on Enter so the user doesn't have to click.
      onSelect(filtered[0].id)
      setOpen(false)
    } else if (e.key === 'Escape') {
      setOpen(false)
    }
  }

  return (
    <div ref={ref} className="relative">
      <button
        onClick={() => setOpen(!open)}
        aria-expanded={open}
        aria-label={selected ? `Selected node: ${selected.name} at ${selected.location}` : 'Select a node'}
        className={`flex items-center gap-2.5 rounded-xl
          bg-bg-secondary/30 backdrop-blur-sm border border-border/40
          hover:border-border-hover hover:bg-bg-secondary/50
          transition-colors duration-200 text-left cursor-pointer
          ${compact ? 'px-3 py-2' : 'px-4 py-3 w-full'}`}
      >
        {selected ? (
          <>
            <span className="text-base leading-none">{selected.flag}</span>
            <div className="min-w-0">
              <div className="text-sm font-semibold text-text-primary truncate">
                {selected.location}
              </div>
              {!compact && (
                <div className="text-xs text-text-muted truncate">{selected.name}</div>
              )}
            </div>
            {selected.online ? (
              <span className="w-2 h-2 rounded-full bg-success shrink-0"
                style={{ animation: 'pulse-glow 2s ease-in-out infinite' }} />
            ) : (
              <span className="w-2 h-2 rounded-full bg-danger shrink-0" />
            )}
          </>
        ) : (
          <>
            {loading ? (
              <Loader size={14} className="text-text-muted animate-spin" />
            ) : (
              <MapPin size={14} className="text-text-muted" />
            )}
            <span className="text-sm text-text-muted">{loading ? 'Loading nodes...' : 'Select node...'}</span>
          </>
        )}
        <ChevronDown size={14} className={`text-text-muted transition-transform duration-[var(--duration-fast)] ease-[var(--ease-in-out)] ml-auto ${open ? 'rotate-180' : ''}`} />
      </button>

      {mounted && (
        <div
          data-state={state}
          inert={state === 'closing'}
          aria-hidden={state === 'closing'}
          data-origin="top-left"
          className={`t-dropdown ${motionStateClass(state)} absolute z-50 top-full left-0 mt-2 min-w-[280px] rounded-xl
          bg-bg-elevated border border-border/60 backdrop-blur-md
          shadow-xl shadow-black/40 overflow-hidden`}>
          {nodes.length === 0 ? (
            <div className="px-4 py-6 text-center text-sm text-text-muted">
              No nodes available
            </div>
          ) : (
            <>
              {nodes.length > SEARCH_THRESHOLD && (
                <div className="flex items-center gap-2 px-3 py-2 border-b border-border/30">
                  <Search size={13} className="text-text-muted shrink-0" />
                  <input
                    ref={searchRef}
                    type="text"
                    value={query}
                    onChange={(e) => setQuery(e.target.value)}
                    onKeyDown={handleKeyDown}
                    placeholder="Search nodes..."
                    aria-label="Search nodes"
                    className="flex-1 bg-transparent text-xs text-text-primary
                      placeholder:text-text-muted outline-none"
                  />
                  <span className="text-[10px] text-text-muted tabular-nums">
                    {filtered.length}/{nodes.length}
                  </span>
                </div>
              )}
              <div className="max-h-72 overflow-y-auto py-1">
                {filtered.length === 0 ? (
                  <div className="px-4 py-6 text-center text-sm text-text-muted">
                    No matches
                  </div>
                ) : (
                  filtered.map((node) => (
                    <button
                      key={node.id}
                      onClick={() => { onSelect(node.id); setOpen(false) }}
                      className={`flex items-center gap-3 w-full px-4 py-3 text-left
                        hover:bg-hover-overlay transition-colors duration-150 cursor-pointer
                        ${node.id === selectedNode ? 'bg-accent/10' : ''}`}
                    >
                      <span className="text-lg">{node.flag}</span>
                      <div className="flex-1 min-w-0">
                        <div className="text-sm font-medium text-text-primary truncate">
                          {node.location}
                        </div>
                        <div className="text-xs text-text-muted truncate">
                          {node.name} · {node.provider}
                        </div>
                      </div>
                      {node.online ? (
                        <Wifi size={14} className="text-success shrink-0" />
                      ) : (
                        <WifiOff size={14} className="text-danger shrink-0" />
                      )}
                    </button>
                  ))
                )}
              </div>
            </>
          )}
        </div>
      )}
    </div>
  )
}
