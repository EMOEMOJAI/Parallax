import { Activity, Grid3x3, Columns3, Map as MapIcon, Clock, Menu } from 'lucide-react'
import { useEffect } from 'react'
import { useDropdown } from '../hooks/useDropdown'
import { motionStateClass } from '../hooks/useMountTransition'

const tools = [
  { id: 'map', label: 'Map', title: 'Network map', icon: MapIcon },
  { id: 'compare', label: 'Compare', title: 'Multi-node comparison', icon: Columns3 },
  { id: 'health', label: 'Health', title: 'Node health overview', icon: Activity },
  { id: 'matrix', label: 'Matrix', title: 'Latency matrix', icon: Grid3x3 },
  { id: 'schedules', label: 'Schedules', title: 'Scheduled probes', icon: Clock },
]

export default function DashboardTools({ onSelect, isPublic, hasTraceData }) {
  const { ref, open, setOpen, mounted, state } = useDropdown()
  const available = tools.filter((tool) => tool.id !== 'schedules' || !isPublic)
  useEffect(() => {
    if (!open) return
    const media = window.matchMedia('(min-width: 768px)')
    const closeOnDesktop = () => { if (media.matches) setOpen(false) }
    media.addEventListener('change', closeOnDesktop)
    return () => media.removeEventListener('change', closeOnDesktop)
  }, [open, setOpen])

  const choose = (id) => {
    setOpen(false)
    onSelect(id)
  }
  const buttonClass = 'flex min-h-11 items-center gap-2 rounded-lg border px-3 text-xs font-medium cursor-pointer transition-colors duration-200 '
  const renderButton = (tool, mobile) => {
    const Icon = tool.icon
    return (
      <button key={tool.id} onClick={() => choose(tool.id)} title={tool.title}
        className={`${buttonClass} ${mobile ? 'w-full text-left ' : ''}${tool.id === 'map' && hasTraceData
          ? 'bg-cyan/15 border-cyan/40 text-cyan hover:bg-cyan/25'
          : 'bg-bg-secondary/30 border-border/30 text-text-muted hover:text-text-primary hover:border-border-hover'}`}>
        <Icon size={16} />
        <span>{mobile ? tool.title : tool.label}</span>
      </button>
    )
  }

  return (
    <>
      <div className="hidden md:flex items-center gap-1.5">
        {available.map((tool) => renderButton(tool, false))}
      </div>
      <div ref={ref} className="relative md:hidden">
        <button aria-expanded={open} aria-controls="dashboard-tools" onClick={() => setOpen(!open)}
          className={`${buttonClass} border-border/40 bg-bg-secondary/30 text-text-primary`}>
          <Menu size={18} /> Tools
        </button>
        {mounted && (
          <div id="dashboard-tools" data-state={state} inert={state === 'closing'} aria-hidden={state === 'closing'}
            className={`t-dropdown ${motionStateClass(state)} absolute z-50 right-0 top-full mt-2 w-60 max-w-[calc(100vw-2rem)] rounded-xl border border-border/60 bg-bg-elevated p-2 shadow-xl space-y-1`}>
            {available.map((tool) => renderButton(tool, true))}
          </div>
        )}
      </div>
    </>
  )
}
