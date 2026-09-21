import { useState } from 'react'
import { KIT_ALLOWED_STEP_TYPES, isToolAvailable, unavailableTitle } from '../lib/capabilities'

export function capabilityStatus(node, command) {
  if (!isToolAvailable(node, command)) return { label: 'Unavailable', detail: unavailableTitle(node, command) }
  if (node.tools?.[command] !== true) return { label: 'Unknown', detail: 'This agent has not reported support for this check.' }
  return { label: 'Available', detail: 'Reported by the agent.' }
}

export default function FleetReadiness({ nodes }) {
  const [expected, setExpected] = useState('')
  const [command, setCommand] = useState('ping')
  const known = nodes.filter((node) => node.version && node.version !== 'unknown')
  const builds = new Set(known.map((node) => node.version))
  return <section aria-labelledby="fleet-readiness-title" className="mb-4 rounded-xl border border-border p-4 space-y-3">
    <h3 id="fleet-readiness-title" className="text-sm font-semibold">Agent readiness</h3>
    <p className="text-sm text-text-muted">{builds.size} reported {builds.size === 1 ? 'build' : 'builds'} · {nodes.length - known.length} unknown versions. Different build IDs do not establish which is newer.</p>
    <div className="grid sm:grid-cols-2 gap-3">
      <label className="text-sm">Expected version (optional)<input className="block mt-1 w-full min-h-11 bg-bg-secondary rounded-lg border border-border-hover px-3" maxLength={128} placeholder="Paste a full release tag or commit ID" value={expected} onChange={(e) => setExpected(e.target.value)} /></label>
      <label className="text-sm">Required diagnostic<select aria-label="Required diagnostic" className="block mt-1 w-full min-h-11 bg-bg-secondary rounded-lg border border-border-hover px-3" value={command} onChange={(e) => setCommand(e.target.value)}>{KIT_ALLOWED_STEP_TYPES.map((type) => <option key={type}>{type}</option>)}</select></label>
    </div>
    <div className="space-y-2">
      {nodes.map((node) => {
        const capability = capabilityStatus(node, command)
        const versionKnown = node.version && node.version !== 'unknown'
        const versionLabel = !versionKnown ? 'Version unknown' : expected.trim() ? node.version === expected.trim() ? 'Matches expected version' : 'Different from expected version' : 'Version reported'
        return <div key={node.id} className="text-sm border-t border-border pt-2" data-testid="agent-readiness">
          <p className="font-medium break-words">{node.name}{!node.online ? ' · Offline' : ''}</p>
          <p className="text-text-muted">{versionLabel} · {command}: {capability.label}</p>
          <p className="text-xs text-text-muted">{capability.detail}{!node.online ? ' Reconnect this agent before running checks.' : ''}</p>
        </div>
      })}
    </div>
    <p className="text-xs text-text-muted">For missing native probes, deploy a newer agent build. For missing external tools, install the tool on that node. Unknown capability reports keep the existing compatibility behavior; support is not assumed here.</p>
  </section>
}
