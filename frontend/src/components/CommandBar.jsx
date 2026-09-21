import { motionStateClass } from '../hooks/useMountTransition'
import { useState, useEffect } from 'react'
import { ChevronDown, Play, Square, Terminal as TermIcon, Globe, History, Star, X, SlidersHorizontal } from 'lucide-react'
import { useDropdown } from '../hooks/useDropdown'
import { usePresets } from '../hooks/usePresets'
import {
  isToolAvailable, unavailableTitle, unavailableLabel,
  isKitAvailable, kitUnavailableTitle, kitUnavailableLabel,
  KIT_ALLOWED_STEP_TYPES,
} from '../lib/capabilities'

const COMMANDS = [
  { id: 'ping', label: 'ping', icon: '📡' },
  { id: 'traceroute', label: 'traceroute', icon: '🔀' },
  { id: 'mtr', label: 'mtr', icon: '📊' },
  { id: 'nexttrace', label: 'nexttrace', icon: '🌐' },
  { id: 'dns', label: 'dns lookup', icon: '🔍' },
  { id: 'http', label: 'http probe', icon: '🌍' },
  // The four Go-native probes (S7). They need no binary on the node and take
  // no options at all, which is why none of them appears in COMMAND_OPTIONS or
  // IP_VERSION_COMMANDS below.
  { id: 'tcp', label: 'tcp connect', icon: '🔌' },
  { id: 'tls', label: 'tls certificate', icon: '🔒' },
  { id: 'dnsbench', label: 'dns benchmark', icon: '⏱️' },
  { id: 'download', label: 'download speed', icon: '⬇️' },
  { id: 'iperf3', label: 'iperf3', icon: '⚡' },
  { id: 'speedtest', label: 'speedtest', icon: '🚀' },
  { id: 'kit', label: 'diagnostic kit', icon: '🧰' },
  { id: 'shell', label: 'shell', icon: '💻' },
]

// What each command's target box expects. The four native probes are the reason
// this is a table rather than one ternary: `host:port`, a URL and a domain name
// are not interchangeable, and getting it wrong costs a round trip to the node.
export const TARGET_PLACEHOLDERS = {
  dns: 'Domain name (e.g. example.com)',
  dnsbench: 'Domain name (e.g. name.example.com)',
  tcp: 'host:port (e.g. example.com:443)',
  tls: 'host or host:port (e.g. example.com)',
  download: 'https://url of a file to download',
  http: 'URL or hostname',
}

export function targetPlaceholder(command) {
  return TARGET_PLACEHOLDERS[command] || 'Hostname or IP address'
}

export const IP_VERSIONS = ['Auto', 'IPv4', 'IPv6']

// The command types that accept an IP-version flag. nexttrace, iperf3 and
// speedtest are absent on purpose: nexttrace takes no options at all, and the
// other two are exempt from this grammar entirely (agent/main.go optionSpecs).
export const IP_VERSION_COMMANDS = ['ping', 'traceroute', 'mtr', 'dns', 'http']

/**
 * Per-tool options, exported so CommandBar and MultiNodeCompare cannot drift —
 * the same class of hazard as the command-whitelist-sync invariant.
 *
 * This table only drives the UI. The agent re-validates every value against its
 * own optionSpecs and answers with a `[options] …` note line for anything it
 * drops, so a bound that disagrees with the agent's costs the user their option,
 * not their safety. Keep the bounds here equal to agent/main.go's optionSpecs.
 *
 * kind:
 *   'int'  — bounded integer, sent as `key=N`
 *   'enum' — one of `values`, sent as `key=value`; the '' value is the tool's
 *            own default and sends no token at all
 *   'flag' — a checkbox, sent as a bare `key`
 */
export const COMMAND_OPTIONS = {
  ping: [
    { key: 'count', kind: 'int', label: 'Packets', min: 1, max: 100, placeholder: '10' },
    { key: 'size', kind: 'int', label: 'Payload bytes', min: 16, max: 1472, placeholder: '56' },
  ],
  traceroute: [
    { key: 'maxhops', kind: 'int', label: 'Max hops', min: 1, max: 64, placeholder: '30' },
    {
      key: 'mode', kind: 'enum', label: 'Probe mode',
      // The node decides whether TCP mode is allowed: it is an agent start flag
      // (-allow-tcp-traceroute), not part of the reported tools map, so the UI
      // cannot know in advance. Ask anyway and read the note if it is refused.
      hint: 'ICMP and TCP mode both need extra privileges on the node, and TCP mode also needs the agent to have been started with -allow-tcp-traceroute — either can be refused.',
      values: [
        { value: '', label: 'UDP (default)' },
        { value: 'icmp', label: 'ICMP' },
        { value: 'tcp', label: 'TCP — may be refused' },
      ],
    },
  ],
  mtr: [
    { key: 'count', kind: 'int', label: 'Cycles', min: 1, max: 20, placeholder: '5' },
  ],
  dns: [
    {
      key: 'type', kind: 'enum', label: 'Record type',
      // Empty sends dig's default A query.
      values: [
        { value: '', label: 'A (default)' },
        { value: 'A', label: 'A' },
        { value: 'AAAA', label: 'AAAA' },
        { value: 'MX', label: 'MX' },
        { value: 'CNAME', label: 'CNAME' },
        { value: 'NS', label: 'NS' },
        { value: 'TXT', label: 'TXT' },
        { value: 'SOA', label: 'SOA' },
        { value: 'SRV', label: 'SRV' },
        { value: 'PTR', label: 'PTR' },
        { value: 'CAA', label: 'CAA' },
        { value: 'DNSKEY', label: 'DNSKEY' },
      ],
    },
    { key: 'short', kind: 'flag', label: 'Answers only (+short)' },
    { key: 'trace', kind: 'flag', label: 'Trace delegation (+trace)' },
    { key: 'dnssec', kind: 'flag', label: 'DNSSEC records (+dnssec)' },
  ],
  http: [
    {
      key: 'method', kind: 'enum', label: 'Method',
      values: [
        { value: '', label: 'GET (default)' },
        { value: 'head', label: 'HEAD' },
      ],
    },
  ],
}

export function optionsFor(command) {
  return COMMAND_OPTIONS[command] || []
}

/**
 * Encode UI state into the agent's option token grammar: `-4`/`-6` for the IP
 * version, `key=value` for ints and enums, a bare `key` for flags. A value the
 * descriptor rejects is left out rather than sent and dropped agent-side.
 */
export function encodeOptions(command, values, ipVersion) {
  const tokens = []
  if (IP_VERSION_COMMANDS.includes(command)) {
    if (ipVersion === 'IPv4') tokens.push('-4')
    else if (ipVersion === 'IPv6') tokens.push('-6')
  }
  for (const d of optionsFor(command)) {
    const v = values?.[d.key]
    if (d.kind === 'flag') {
      if (v === true) tokens.push(d.key)
      continue
    }
    if (v === undefined || v === null || v === '') continue
    if (d.kind === 'int') {
      const n = Number(v)
      if (!Number.isInteger(n) || n < d.min || n > d.max) continue
      tokens.push(`${d.key}=${n}`)
      continue
    }
    if (d.values.some((o) => o.value === v && o.value !== '')) tokens.push(`${d.key}=${v}`)
  }
  return tokens.join(' ')
}

/**
 * Decode an options string — a saved preset, a replayed run, a kit step — back
 * into UI state. Understands `type=A`, and also the bare uppercase record type
 * that pre-S6 presets and history entries carry, mirroring the agent's own
 * rescue so an old preset still restores its dropdown.
 */
export function decodeOptions(command, options) {
  const values = {}
  let ipVersion = 'Auto'
  const descriptors = optionsFor(command)
  const byKey = new Map(descriptors.map((d) => [d.key, d]))
  const leftovers = []
  const matchEnum = (d, raw) => d.values.find((o) => o.value !== '' && o.value.toUpperCase() === raw.toUpperCase())

  for (const tok of String(options || '').trim().split(/\s+/).filter(Boolean)) {
    if (tok === '-4') { ipVersion = 'IPv4'; continue }
    if (tok === '-6') { ipVersion = 'IPv6'; continue }
    const eq = tok.indexOf('=')
    const key = eq < 0 ? tok : tok.slice(0, eq)
    const raw = eq < 0 ? '' : tok.slice(eq + 1)
    const d = byKey.get(key)
    if (!d) { leftovers.push(tok); continue }
    if (d.kind === 'flag') { values[key] = true; continue }
    if (d.kind === 'int') {
      const n = raw === '' ? NaN : Number(raw)
      if (Number.isInteger(n) && n >= d.min && n <= d.max) values[key] = String(n)
      continue
    }
    const match = matchEnum(d, raw)
    if (match) values[key] = match.value
  }

  const typeDesc = byKey.get('type')
  if (typeDesc && values.type === undefined) {
    for (const tok of leftovers) {
      const match = matchEnum(typeDesc, tok)
      if (match) { values.type = match.value; break }
    }
  }
  return { values, ipVersion }
}

export default function CommandBar({
  onRun, onStop, running, disabled,
  history, onNavigateHistory, onClearHistory,
  // The currently selected node, for its self-reported tools. Distinct from
  // allowedCommands: this is "what this node can run", that is "what this
  // session may ask for".
  node = null,
  // In public mode the parent passes the server-side allowlists. When non-null
  // they trim the command dropdown and constrain the target input.
  allowedCommands = null, allowedTargets = null,
  // The user-defined diagnostic kit and its editing callbacks (S10). They come
  // from App's single useKits() instance as props on purpose: this component
  // must NOT call useKits itself — two instances do not share state (the
  // `storage` event does not fire in the document that wrote the value), so an
  // edit made here would never reach App's run path without a reload.
  kit = [], onAddKitStep = null, onRemoveKitStep = null, onResetKit = null,
  maxKitSteps = 8, minKitSteps = 1,
}) {
  const [command, setCommand] = useState('ping')
  const [target, setTarget] = useState('')
  const [ipVersion, setIpVersion] = useState('Auto')
  // Per-tool option values, keyed by command type then option key, so switching
  // tools doesn't carry ping's packet count into mtr's cycle count (the two share
  // the key name `count` but not its bounds).
  const [optValues, setOptValues] = useState({})

  // Filter the shown command list. Always include the currently selected
  // command even if disallowed, so the dropdown isn't empty mid-edit.
  const visibleCommands = allowedCommands !== null
    ? COMMANDS.filter((c) => allowedCommands.includes(c.id))
    : COMMANDS

  // If the current command isn't in the allowlist, snap to the first allowed
  // command on the next render.
  useEffect(() => {
    if (allowedCommands && allowedCommands.length > 0 && !allowedCommands.includes(command)) {
      setCommand(allowedCommands[0])
    }
  }, [allowedCommands, command])

  const cmdDrop = useDropdown()
  const ipDrop = useDropdown()
  const dnsDrop = useDropdown()
  const optDrop = useDropdown()
  const histDrop = useDropdown()
  const presetDrop = useDropdown()
  const { presets, add: addPreset, remove: removePreset, clear: clearPresets, max: maxPresets } = usePresets()

  const selectedCmd = COMMANDS.find((c) => c.id === command)

  // Capability helpers that know about the *live* kit. The generic helpers judge
  // 'kit' on capabilities.js's default step list, so a kit built from mtr and
  // http would be greyed out (or not) for the wrong reasons; these dispatch on
  // the command id and pass the active kit in. Plain functions of (node, kit),
  // so a kit edit re-renders this component and the answer changes in the same
  // interaction — no module-level mutable state is involved.
  const cmdAvailable = (type) => (type === 'kit' ? isKitAvailable(node, kit) : isToolAvailable(node, type))
  const cmdTitle = (type) => (type === 'kit' ? kitUnavailableTitle(node, kit) : unavailableTitle(node, type))
  const cmdLabel = (type) => (type === 'kit' ? kitUnavailableLabel(node, kit) : unavailableLabel(node, type))
  // A command the node reported it cannot run: the entry is greyed out and Run
  // is blocked, instead of dispatching something that fails 400 ms later. The
  // helper is permissive for 'shell' and for an agent that reports no tools at
  // all, so nothing is disabled on an older node.
  const commandUnavailable = !cmdAvailable(command) || (allowedCommands !== null && !allowedCommands.includes(command))
  const targetDisallowed = allowedTargets !== null && !allowedTargets.includes(target.trim())
  const commandUnavailableTitle = cmdTitle(command)
  // shell and speedtest don't take a target; shell opens a modal instead of
  // dispatching a regular command.
  const needsTarget = !['speedtest', 'shell'].includes(command)

  // Options for the selected tool. The DNS record type keeps its own dedicated
  // dropdown (it is the control people reach for most), so the popover shows
  // everything except that one — never both, or the user could set it twice.
  const currentValues = optValues[command] || {}
  const dnsTypeDesc = command === 'dns' ? optionsFor('dns').find((d) => d.key === 'type') : null
  const popoverOptions = optionsFor(command).filter((d) => d.key !== 'type')
  const setOptValue = (key, value) => setOptValues((prev) => ({
    ...prev,
    [command]: { ...(prev[command] || {}), [key]: value },
  }))
  const resetOptions = () => setOptValues((prev) => ({ ...prev, [command]: {} }))
  // The exact string that will be sent. Built in one place so Run, "Save
  // current" and the popover's preview can never disagree.
  const encodedOptions = encodeOptions(command, currentValues, ipVersion)
  const setOptionsCount = popoverOptions.filter((d) => {
    const v = currentValues[d.key]
    return d.kind === 'flag' ? v === true : v !== undefined && v !== null && v !== ''
  }).length

  const handleRun = () => {
    if (disabled) return
    if (commandUnavailable || targetDisallowed) return
    if (needsTarget && !target.trim()) return
    onRun({ type: command, target: target.trim(), options: encodedOptions })
  }

  const handleKeyDown = (e) => {
    if (e.key === 'Enter' && !running) {
      handleRun()
    } else if (e.key === 'ArrowUp') {
      e.preventDefault()
      const entry = onNavigateHistory?.(1)
      if (entry) { setCommand(entry.type); setTarget(entry.target) }
    } else if (e.key === 'ArrowDown') {
      e.preventDefault()
      const entry = onNavigateHistory?.(-1)
      if (entry) { setCommand(entry.type); setTarget(entry.target) }
      else setTarget('')
    }
  }

  const applyHistoryEntry = (entry) => {
    setCommand(entry.type)
    setTarget(entry.target)
    histDrop.setOpen(false)
  }

  const applyPreset = (preset) => {
    setCommand(preset.type)
    setTarget(preset.target || '')
    // Presets saved before per-tool options existed store the DNS record type as
    // a bare token ("MX"); decodeOptions understands both that and `type=MX`, so
    // an old preset still restores every control it set.
    const { values, ipVersion: presetIpVersion } = decodeOptions(preset.type, preset.options)
    setOptValues((prev) => ({ ...prev, [preset.type]: values }))
    setIpVersion(presetIpVersion)
    presetDrop.setOpen(false)
  }

  // --- diagnostic kit editing (S10) ---
  // "Add to kit" alone cannot produce a two-step kit, so the dropdown also
  // carries remove-step and reset-to-default. Every mutation goes through the
  // parent's useKits callbacks, which validate and persist.
  const kitStepAllowed = KIT_ALLOWED_STEP_TYPES.includes(command)
  const kitFull = kit.length >= maxKitSteps
  const canAddToKit = Boolean(onAddKitStep) && kitStepAllowed && !kitFull
  const addCurrentToKit = () => {
    if (!canAddToKit) return
    onAddKitStep({ type: command, options: encodedOptions })
  }
  const addToKitTitle = !kitStepAllowed
    ? `${command} cannot be a kit step`
    : kitFull
    ? `The kit is full (${maxKitSteps} steps)`
    : `Add ${command}${encodedOptions ? ' (' + encodedOptions + ')' : ''} as a kit step`

  const saveCurrentAsPreset = () => {
    if (needsTarget && !target.trim()) return
    const options = encodedOptions
    addPreset({
      type: command,
      target: target.trim(),
      options,
      label: `${command}${target.trim() ? ' ' + target.trim() : ''}${options ? ' (' + options + ')' : ''}`,
    })
  }

  return (
    <div className="flex flex-wrap items-center gap-2">
      {/* Command selector */}
      <div ref={cmdDrop.ref} className="relative shrink-0">
        <button
          onClick={() => cmdDrop.setOpen(!cmdDrop.open)}
          aria-label={`Command type: ${selectedCmd?.label || command}`}
          aria-expanded={cmdDrop.open}
          className="flex items-center gap-2 px-3.5 py-2 rounded-lg
            bg-bg-secondary/40 backdrop-blur-sm border border-border/40
            hover:border-border-hover hover:bg-bg-secondary/60
            transition-colors duration-200 text-sm font-medium text-text-primary cursor-pointer"
        >
          <TermIcon size={14} className="text-accent-text" />
          {selectedCmd?.label}
          <ChevronDown size={13} className={`text-text-muted transition-transform duration-[var(--duration-fast)] ease-[var(--ease-in-out)] ${cmdDrop.open ? 'rotate-180' : ''}`} />
        </button>

        {cmdDrop.mounted && (
          <div
            data-state={cmdDrop.state}
            inert={cmdDrop.state === 'closing'}
            aria-hidden={cmdDrop.state === 'closing'}
            data-origin="top-left"
            className={`t-dropdown ${motionStateClass(cmdDrop.state)} absolute z-50 top-full left-0 mt-2 w-48 rounded-xl
            bg-bg-elevated border border-border/60 backdrop-blur-md
            shadow-xl shadow-black/40 overflow-hidden py-1`}>
            {visibleCommands.map((cmd) => {
              const missing = !cmdAvailable(cmd.id)
              return (
                <button
                  key={cmd.id}
                  onClick={() => { setCommand(cmd.id); cmdDrop.setOpen(false) }}
                  disabled={missing}
                  title={cmdTitle(cmd.id)}
                  className={`flex items-center gap-3 w-full px-4 py-2.5 text-left text-sm
                    hover:bg-hover-overlay transition-colors duration-150 cursor-pointer
                    disabled:opacity-40 disabled:cursor-not-allowed disabled:hover:bg-transparent
                    ${cmd.id === command ? 'text-accent-text bg-accent/10' : 'text-text-primary'}`}
                >
                  <span>{cmd.icon}</span>
                  {cmd.label}
                  {missing && <span className="ml-auto text-[10px] text-text-muted">{cmdLabel(cmd.id)}</span>}
                </button>
              )
            })}
          </div>
        )}
      </div>

      {/* Target input — free-text in normal mode, dropdown when public-mode
          targets are allowlisted by the server. */}
      <div className="flex-1 min-w-[160px]">
        {allowedTargets !== null ? (
          <div className="flex items-center gap-2 px-3.5 py-2 rounded-lg
            bg-bg-secondary/40 backdrop-blur-sm border border-border/40">
            <Globe size={14} className="text-text-muted shrink-0" />
            <select
              value={allowedTargets.includes(target) ? target : ''}
              onChange={(e) => setTarget(e.target.value)}
              aria-label="Allowed targets"
              className="flex-1 bg-transparent text-sm text-text-primary outline-none cursor-pointer"
            >
              <option value="" disabled>Select an allowed target…</option>
              {allowedTargets.map((t) => <option key={t} value={t}>{t}</option>)}
            </select>
          </div>
        ) : (
          <div className="flex items-center gap-2 px-3.5 py-2 rounded-lg
            bg-bg-secondary/40 backdrop-blur-sm border border-border/40
            focus-within:border-accent/50 transition-colors duration-200">
            <Globe size={14} className="text-text-muted shrink-0" />
            <input
              type="text"
              value={target}
              onChange={(e) => setTarget(e.target.value)}
              onKeyDown={handleKeyDown}
              aria-label="Command target"
              placeholder={targetPlaceholder(command)}
              className="flex-1 bg-transparent text-sm text-text-primary placeholder:text-text-muted
                outline-none min-w-0"
            />
          </div>
        )}
      </div>

      {/* Presets button */}
      <div ref={presetDrop.ref} className="relative shrink-0">
        <button
          onClick={() => presetDrop.setOpen(!presetDrop.open)}
          className="p-2 rounded-lg bg-bg-secondary/40 backdrop-blur-sm border border-border/40
            hover:border-border-hover hover:bg-bg-secondary/60
            transition-colors duration-200 text-text-muted hover:text-text-primary cursor-pointer"
          title="Saved presets"
          aria-label="Saved presets"
          aria-expanded={presetDrop.open}
        >
          <Star size={14} className={presets.length > 0 ? 'text-warning' : ''} />
        </button>

        {presetDrop.mounted && (
          <div
            data-state={presetDrop.state}
            inert={presetDrop.state === 'closing'}
            aria-hidden={presetDrop.state === 'closing'}
            data-origin="top-right"
            className={`t-dropdown ${motionStateClass(presetDrop.state)} absolute z-50 top-full right-0 mt-2 w-72 rounded-xl
            bg-bg-elevated border border-border/60 backdrop-blur-md
            shadow-xl shadow-black/40 overflow-hidden`}>
            <div className="px-3 py-2 border-b border-border/30 flex items-center justify-between">
              <span className="text-[11px] font-semibold uppercase tracking-widest text-text-muted">
                Presets ({presets.length}/{maxPresets})
              </span>
              <div className="flex items-center gap-2">
                <button
                  onClick={() => { saveCurrentAsPreset() }}
                  disabled={needsTarget && !target.trim()}
                  className="text-[10px] text-accent-text hover:text-accent-text-hover transition-colors cursor-pointer
                    disabled:opacity-40 disabled:cursor-not-allowed"
                  title="Save current command"
                >
                  + Save current
                </button>
                {presets.length > 0 && (
                  <button
                    onClick={() => clearPresets()}
                    className="text-[10px] text-danger/70 hover:text-danger transition-colors cursor-pointer"
                  >
                    Clear
                  </button>
                )}
              </div>
            </div>
            <div className="max-h-56 overflow-y-auto py-1">
              {presets.length === 0 ? (
                <div className="px-3 py-4 text-center text-xs text-text-muted">
                  No saved presets. Set up a command and click "+ Save current".
                </div>
              ) : (
                presets.map((preset) => (
                  <div
                    key={preset.id}
                    className="group flex items-center gap-2 px-3 py-2
                      hover:bg-hover-overlay transition-colors duration-150"
                  >
                    <button
                      onClick={() => applyPreset(preset)}
                      className="flex items-center gap-2 flex-1 text-left cursor-pointer min-w-0"
                    >
                      <span className="text-xs">{COMMANDS.find(c => c.id === preset.type)?.icon || '?'}</span>
                      <span className="text-xs font-medium text-accent-text shrink-0">{preset.type}</span>
                      <span className="text-xs text-text-secondary truncate">
                        {preset.target}
                        {preset.options ? <span className="text-text-muted"> · {preset.options}</span> : null}
                      </span>
                    </button>
                    <button
                      onClick={() => removePreset(preset.id)}
                      className="opacity-0 group-hover:opacity-100 focus-visible:opacity-100 p-1 rounded
                        text-text-muted hover:text-danger hover:bg-danger/10
                        transition-[opacity,color,background-color] duration-150 cursor-pointer"
                      aria-label={`Remove preset ${preset.label}`}
                    >
                      <X size={11} />
                    </button>
                  </div>
                ))
              )}
            </div>

            {/* Diagnostic kit editor. The kit arrives as a prop from App's single
                useKits() instance — this component never calls the hook. */}
            <div className="border-t border-border/30">
              <div className="px-3 py-2 flex items-center justify-between">
                <span className="text-[11px] font-semibold uppercase tracking-widest text-text-muted">
                  Diagnostic kit ({kit.length}/{maxKitSteps})
                </span>
                <div className="flex items-center gap-2">
                  <button
                    onClick={addCurrentToKit}
                    disabled={!canAddToKit}
                    title={addToKitTitle}
                    className="text-[10px] text-accent-text hover:text-accent-text-hover transition-colors cursor-pointer
                      disabled:opacity-40 disabled:cursor-not-allowed"
                  >
                    + Add current
                  </button>
                  {onResetKit && (
                    <button
                      onClick={() => onResetKit()}
                      title="Restore the default kit (ping, traceroute, dns type=A)"
                      className="text-[10px] text-text-muted hover:text-text-primary transition-colors cursor-pointer"
                    >
                      Reset
                    </button>
                  )}
                </div>
              </div>
              <div className="max-h-40 overflow-y-auto pb-1">
                {kit.map((step, i) => (
                  <div
                    key={`${step.type}-${i}`}
                    className="group flex items-center gap-2 px-3 py-1.5
                      hover:bg-hover-overlay transition-colors duration-150"
                  >
                    <span className="w-3 text-[10px] text-text-muted tabular-nums">{i + 1}</span>
                    <span className="text-xs">{COMMANDS.find((c) => c.id === step.type)?.icon || '?'}</span>
                    <span className="text-xs font-medium text-accent-text shrink-0">{step.type}</span>
                    <span className="text-xs text-text-muted truncate">{step.options}</span>
                    <button
                      onClick={() => onRemoveKitStep?.(i)}
                      /* Disabled at the minimum: an empty kit throws in the run
                         path and renders as permanently unavailable. */
                      disabled={!onRemoveKitStep || kit.length <= minKitSteps}
                      title={kit.length <= minKitSteps ? 'A kit needs at least one step' : `Remove step ${i + 1}`}
                      aria-label={`Remove kit step ${i + 1} (${step.type})`}
                      className="ml-auto opacity-0 group-hover:opacity-100 focus-visible:opacity-100 p-1 rounded
                        text-text-muted hover:text-danger hover:bg-danger/10
                        transition-[opacity,color,background-color] duration-150 cursor-pointer
                        disabled:opacity-20 disabled:cursor-not-allowed disabled:hover:bg-transparent
                        disabled:hover:text-text-muted"
                    >
                      <X size={11} />
                    </button>
                  </div>
                ))}
              </div>
            </div>
          </div>
        )}
      </div>

      {/* History button */}
      <div ref={histDrop.ref} className="relative shrink-0">
        <button
          onClick={() => histDrop.setOpen(!histDrop.open)}
          aria-expanded={histDrop.open}
          className="p-2 rounded-lg bg-bg-secondary/40 backdrop-blur-sm border border-border/40
            hover:border-border-hover hover:bg-bg-secondary/60
            transition-colors duration-200 text-text-muted hover:text-text-primary cursor-pointer"
          title="Command history (↑/↓ in input)"
        >
          <History size={14} />
        </button>

        {histDrop.mounted && history && history.length > 0 && (
          <div
            data-state={histDrop.state}
            inert={histDrop.state === 'closing'}
            aria-hidden={histDrop.state === 'closing'}
            data-origin="top-right"
            className={`t-dropdown ${motionStateClass(histDrop.state)} absolute z-50 top-full right-0 mt-2 w-72 rounded-xl
            bg-bg-elevated border border-border/60 backdrop-blur-md
            shadow-xl shadow-black/40 overflow-hidden`}>
            <div className="px-3 py-2 border-b border-border/30 flex items-center justify-between">
              <span className="text-[11px] font-semibold uppercase tracking-widest text-text-muted">
                Recent Commands
              </span>
              {onClearHistory && (
                <button
                  onClick={() => { onClearHistory(); histDrop.setOpen(false) }}
                  className="text-[10px] text-danger/70 hover:text-danger transition-colors cursor-pointer"
                >
                  Clear
                </button>
              )}
            </div>
            <div className="max-h-56 overflow-y-auto py-1">
              {history.slice(0, 20).map((entry, i) => (
                <button
                  key={i}
                  onClick={() => applyHistoryEntry(entry)}
                  className="flex items-center gap-2 w-full px-3 py-2 text-left
                    hover:bg-hover-overlay transition-colors duration-150 cursor-pointer"
                >
                  <span className="text-xs">{COMMANDS.find(c => c.id === entry.type)?.icon || '?'}</span>
                  <span className="text-xs font-medium text-accent-text">{entry.type}</span>
                  <span className="text-xs text-text-secondary truncate">{entry.target}</span>
                </button>
              ))}
            </div>
          </div>
        )}
      </div>

      {/* DNS record type (only for dns command). Driven by the shared option
          table, so its list and the encoded `type=` token cannot drift. */}
      {dnsTypeDesc && (
        <div ref={dnsDrop.ref} className="relative shrink-0">
          <button
            onClick={() => dnsDrop.setOpen(!dnsDrop.open)}
            aria-label="DNS record type"
            aria-expanded={dnsDrop.open}
            className="flex items-center gap-1.5 px-3 py-2 rounded-lg
              bg-bg-secondary/40 backdrop-blur-sm border border-border/40
              hover:border-border-hover hover:bg-bg-secondary/60
              transition-colors duration-200 text-sm text-text-secondary cursor-pointer"
          >
            {dnsTypeDesc.values.find((o) => o.value === (currentValues.type || ''))?.label}
            <ChevronDown size={12} className={`text-text-muted transition-transform duration-[var(--duration-fast)] ease-[var(--ease-in-out)] ${dnsDrop.open ? 'rotate-180' : ''}`} />
          </button>

          {dnsDrop.mounted && (
            <div
              data-state={dnsDrop.state}
              inert={dnsDrop.state === 'closing'}
              aria-hidden={dnsDrop.state === 'closing'}
              data-origin="top-right"
              className={`t-dropdown ${motionStateClass(dnsDrop.state)} absolute z-50 top-full right-0 mt-2 w-24 max-h-56 overflow-y-auto rounded-xl
              bg-bg-elevated border border-border/60 backdrop-blur-md
              shadow-xl shadow-black/40 overflow-hidden py-1`}>
              {dnsTypeDesc.values.map((o) => (
                <button
                  key={o.value || 'default'}
                  onClick={() => { setOptValue('type', o.value); dnsDrop.setOpen(false) }}
                  className={`w-full px-3 py-2 text-left text-sm
                    hover:bg-hover-overlay transition-colors duration-150 cursor-pointer
                    ${o.value === (currentValues.type || '') ? 'text-accent-text bg-accent/10' : 'text-text-primary'}`}
                >
                  {o.label}
                </button>
              ))}
            </div>
          )}
        </div>
      )}

      {/* Per-tool options popover. Shown only for tools that have any: nexttrace
          takes no options at all, and iperf3/speedtest are outside this grammar. */}
      {popoverOptions.length > 0 && (
        <div ref={optDrop.ref} className="relative shrink-0">
          <button
            onClick={() => optDrop.setOpen(!optDrop.open)}
            aria-label={`${command} options`}
            aria-expanded={optDrop.open}
            aria-haspopup="dialog"
            title={encodedOptions ? `Options: ${encodedOptions}` : `${command} options`}
            className="relative p-2 rounded-lg bg-bg-secondary/40 backdrop-blur-sm border border-border/40
              hover:border-border-hover hover:bg-bg-secondary/60
              transition-colors duration-200 text-text-muted hover:text-text-primary cursor-pointer"
          >
            <SlidersHorizontal size={14} className={setOptionsCount > 0 ? 'text-accent-text' : ''} />
            {setOptionsCount > 0 && (
              <span className="absolute -top-1 -right-1 min-w-[14px] h-[14px] px-1 rounded-full
                bg-accent text-white text-[9px] font-semibold leading-[14px] text-center">
                {setOptionsCount}
              </span>
            )}
          </button>

          {optDrop.mounted && (
            <div
              data-state={optDrop.state}
              inert={optDrop.state === 'closing'}
              aria-hidden={optDrop.state === 'closing'}
              data-origin="top-right"
              role="dialog" aria-label={`${command} options`}
              className={`t-dropdown ${motionStateClass(optDrop.state)} absolute z-50 top-full right-0 mt-2 w-72 rounded-xl
                bg-bg-elevated border border-border/60 backdrop-blur-md
                shadow-xl shadow-black/40 overflow-hidden`}>
              <div className="px-3 py-2 border-b border-border/30 flex items-center justify-between">
                <span className="text-[11px] font-semibold uppercase tracking-widest text-text-muted">
                  {command} options
                </span>
                <button
                  onClick={resetOptions}
                  disabled={setOptionsCount === 0}
                  className="text-[10px] text-text-muted hover:text-text-primary transition-colors cursor-pointer
                    disabled:opacity-40 disabled:cursor-not-allowed"
                >
                  Reset
                </button>
              </div>
              <div className="px-3 py-2.5 flex flex-col gap-2.5">
                {popoverOptions.map((d) => (
                  <div key={d.key} className="flex flex-col gap-1">
                    {d.kind === 'flag' ? (
                      <label className="flex items-center gap-2 text-xs text-text-secondary cursor-pointer">
                        <input
                          type="checkbox"
                          checked={currentValues[d.key] === true}
                          onChange={(e) => setOptValue(d.key, e.target.checked)}
                          className="accent-accent cursor-pointer"
                        />
                        {d.label}
                      </label>
                    ) : (
                      <label className="flex items-center justify-between gap-2 text-xs text-text-secondary">
                        <span>{d.label}</span>
                        {d.kind === 'int' ? (
                          <input
                            type="number"
                            inputMode="numeric"
                            min={d.min}
                            max={d.max}
                            placeholder={d.placeholder}
                            value={currentValues[d.key] ?? ''}
                            onChange={(e) => setOptValue(d.key, e.target.value)}
                            className="w-20 px-2 py-1 rounded-md bg-bg-secondary/60 border border-border/40
                              text-xs text-text-primary outline-none focus:border-accent/50
                              placeholder:text-text-muted"
                          />
                        ) : (
                          <select
                            value={currentValues[d.key] ?? ''}
                            onChange={(e) => setOptValue(d.key, e.target.value)}
                            className="max-w-[9.5rem] px-2 py-1 rounded-md bg-bg-secondary/60 border border-border/40
                              text-xs text-text-primary outline-none cursor-pointer"
                          >
                            {d.values.map((o) => (
                              <option key={o.value || 'default'} value={o.value}>{o.label}</option>
                            ))}
                          </select>
                        )}
                      </label>
                    )}
                    {d.kind === 'int' && (
                      <span className="text-[10px] text-text-muted">{d.min}–{d.max}, default {d.placeholder}</span>
                    )}
                    {d.hint && <span className="text-[10px] text-text-muted">{d.hint}</span>}
                  </div>
                ))}
              </div>
              <div className="px-3 py-2 border-t border-border/30 text-[10px] text-text-muted font-mono break-all">
                {encodedOptions
                  ? `sends: ${encodedOptions}`
                  : 'sends: no options — each tool uses its own defaults'}
              </div>
            </div>
          )}
        </div>
      )}

      {/* IP version. Hidden for the tools that cannot take one (nexttrace,
          iperf3, speedtest, kit, shell) rather than sending a token the agent
          would drop with a note. */}
      {IP_VERSION_COMMANDS.includes(command) && (
        <div ref={ipDrop.ref} className="relative shrink-0">
          <button
            onClick={() => ipDrop.setOpen(!ipDrop.open)}
            aria-label="IP version"
            aria-expanded={ipDrop.open}
            className="flex items-center gap-1.5 px-3 py-2 rounded-lg
              bg-bg-secondary/40 backdrop-blur-sm border border-border/40
              hover:border-border-hover hover:bg-bg-secondary/60
              transition-colors duration-200 text-sm text-text-secondary cursor-pointer"
          >
            {ipVersion}
            <ChevronDown size={12} className={`text-text-muted transition-transform duration-[var(--duration-fast)] ease-[var(--ease-in-out)] ${ipDrop.open ? 'rotate-180' : ''}`} />
          </button>

          {ipDrop.mounted && (
            <div
              data-state={ipDrop.state}
              inert={ipDrop.state === 'closing'}
              aria-hidden={ipDrop.state === 'closing'}
              data-origin="top-right"
              className={`t-dropdown ${motionStateClass(ipDrop.state)} absolute z-50 top-full right-0 mt-2 w-24 rounded-xl
              bg-bg-elevated border border-border/60 backdrop-blur-md
              shadow-xl shadow-black/40 overflow-hidden py-1`}>
              {IP_VERSIONS.map((v) => (
                <button
                  key={v}
                  onClick={() => { setIpVersion(v); ipDrop.setOpen(false) }}
                  className={`w-full px-3 py-2 text-left text-sm
                    hover:bg-hover-overlay transition-colors duration-150 cursor-pointer
                    ${v === ipVersion ? 'text-accent-text bg-accent/10' : 'text-text-primary'}`}
                >
                  {v}
                </button>
              ))}
            </div>
          )}
        </div>
      )}

      {/* Run / Stop */}
      {running ? (
        <button
          onClick={onStop}
          className="flex items-center gap-1.5 px-4 py-2 rounded-lg shrink-0
            bg-danger text-white text-sm font-semibold
            hover:bg-red-600 active:scale-[0.98]
            shadow-lg shadow-danger/30 hover:shadow-danger/50
            transition-[transform,background-color,box-shadow] duration-200 cursor-pointer"
        >
          <Square size={13} />
          Stop
        </button>
      ) : (
        <button
          onClick={handleRun}
          disabled={disabled || commandUnavailable || targetDisallowed || (needsTarget && !target.trim())}
          title={commandUnavailableTitle}
          className="flex items-center gap-1.5 px-4 py-2 rounded-lg shrink-0
            bg-accent text-white text-sm font-semibold
            hover:bg-accent-hover active:scale-[0.98]
            shadow-lg shadow-accent/30 hover:shadow-accent/50
            transition-[transform,background-color,box-shadow] duration-200
            disabled:opacity-40 disabled:cursor-not-allowed disabled:shadow-none
            cursor-pointer"
        >
          <Play size={13} />
          Run
        </button>
      )}
    </div>
  )
}
