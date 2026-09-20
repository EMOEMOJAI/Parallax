// Single place the "can this node run this command?" rule lives. The server
// stores a node's self-reported `tools` map (rebuilt against its own command
// whitelist, so the keys are always whitelist literals) and ships it with
// GET /api/nodes, GET /api/nodes/health and every online `node_status` frame.
//
// This is a UI affordance only. Dispatch authorization is entirely server-side
// (allowedCommandTypes in backend/client_ws.go); a node claiming a tool it does
// not have still gets the command, and a node claiming a tool that is not a
// command type changes nothing.

// The command types an agent actually probes for a binary. Anything outside this
// list and NATIVE_COMMAND_TYPES below can never be a key in `tools`, so its
// absence says nothing.
export const PROBED_COMMAND_TYPES = [
  'ping', 'traceroute', 'mtr', 'nexttrace', 'iperf3', 'speedtest', 'dns', 'http',
]

// The Go-native probes (S7). They are the first types where *silence is
// informative*: every S7 agent reports them `true` because they need no binary,
// so a node that reports a non-empty `tools` map **without** them is an older
// agent that would refuse the command. For these types only, absence from a
// non-empty map means unavailable.
//
// A node with no `tools` map at all is still treated as fully capable — the S5
// compatibility rule is untouched — and such a node rejects the command cleanly
// with "Unknown command type".
export const NATIVE_COMMAND_TYPES = ['tcp', 'tls', 'dnsbench', 'download']

// Steps of the *default* diagnostic kit. 'kit' is not a server command type, so
// it is never a key in `tools`; it is unavailable only when a step it actually
// runs is missing. Since S10 the kit is user-defined (hooks/useKits.js), so this
// describes the default sequence only — never "the kit". Anything judging a
// live kit must use isKitAvailable(node, kitSteps) below and pass the steps in.
export const KIT_STEP_TYPES = ['ping', 'traceroute', 'dns']

// The command types a user-defined kit step may carry — the one home for this
// allowlist (S10). Derived from the two lists above rather than restated, so it
// cannot drift from them, and structurally excluding 'kit' (which would be
// self-referential: "add to kit" while the bar sits on *diagnostic kit* would
// write a step with no way back) and 'shell' (a modal, not a dispatched
// command, and excluded from the server whitelist on purpose).
//
// It lives here, not in CommandBar, because CommandBar's COMMANDS is
// module-private and importing it into a hook would create a
// component -> hook -> component cycle.
export const KIT_ALLOWED_STEP_TYPES = [...PROBED_COMMAND_TYPES, ...NATIVE_COMMAND_TYPES]

/**
 * isToolAvailable(node, type) — false only when the node positively reported
 * that this probe's binary is missing.
 *
 * Deliberately permissive in every other case:
 *  - a node with no `tools` map, or an empty one, is an agent from before this
 *    feature: everything is available, never nothing;
 *  - a type that is not a probed command type — 'shell' (which the server's
 *    whitelist excludes on purpose) and any future client-side pseudo-command —
 *    is always available;
 *  - a probed type the node simply did not mention is unknown, not missing, so a
 *    newer server talking to an agent that reports a subset never greys out the
 *    rest.
 */
export function isToolAvailable(node, type) {
  if (type === 'kit') {
    // The *default* kit. A caller that has a user-defined kit in scope must
    // call isKitAvailable(node, kitSteps) instead — this signature deliberately
    // gains no third parameter.
    return isKitAvailable(node, KIT_STEP_TYPES.map((step) => ({ type: step })))
  }
  const isNative = NATIVE_COMMAND_TYPES.includes(type)
  if (!isNative && !PROBED_COMMAND_TYPES.includes(type)) return true
  const tools = node?.tools
  if (!tools || typeof tools !== 'object') return true
  const keys = Object.keys(tools)
  if (keys.length === 0) return true
  // A native type missing from a non-empty map is a pre-S7 agent; a *probed*
  // type missing from one is merely unknown.
  if (!keys.includes(type)) return !isNative
  return tools[type] === true
}

/**
 * missingToolsFor(node, type) — the probe binaries that make `type`
 * unavailable. For 'kit' that is the missing steps; for a probed type it is the
 * type itself. Empty when the command is available.
 */
export function missingToolsFor(node, type) {
  if (isToolAvailable(node, type)) return []
  if (type === 'kit') return missingKitTools(node, KIT_STEP_TYPES.map((step) => ({ type: step })))
  return [type]
}

// ---- kit-aware variants (S10) ----------------------------------------------
//
// The kit is user-defined now, so "can this node run the kit?" depends on the
// steps the user actually configured. The steps arrive as an explicit argument:
// no module-level mutable binding holds them, because a module variable written
// from an effect is read during the *previous* render pass with nothing
// scheduling a re-render — the stale enabled/disabled state this avoids.
//
// kitSteps is the hook's shape: [{ type, options }]. Step types the hook could
// not validate never reach here, but a bad entry is ignored rather than trusted.

/** The distinct step types of a kit, in order, with duplicates collapsed. */
function kitStepTypes(kitSteps) {
  if (!Array.isArray(kitSteps)) return []
  const seen = []
  for (const step of kitSteps) {
    const type = typeof step === 'string' ? step : step?.type
    if (typeof type === 'string' && type !== '' && !seen.includes(type)) seen.push(type)
  }
  return seen
}

/**
 * isKitAvailable(node, kitSteps) — whether every step of this kit can run on
 * this node. An **empty** kit is never available: `Array.every` over an empty
 * list is `true`, which would report a kit with nothing in it as runnable.
 */
export function isKitAvailable(node, kitSteps) {
  const types = kitStepTypes(kitSteps)
  if (types.length === 0) return false
  return types.every((type) => isToolAvailable(node, type))
}

/** The step binaries that make this kit unavailable. */
export function missingKitTools(node, kitSteps) {
  const types = kitStepTypes(kitSteps)
  if (types.length === 0) return []
  return types.filter((type) => !isToolAvailable(node, type))
}

/**
 * unavailableTitle(node, type) — the tooltip explaining a greyed-out entry, or
 * undefined when the command is available (so callers can spread it straight
 * into a title attribute).
 */
export function unavailableTitle(node, type) {
  return titleForMissing(missingToolsFor(node, type))
}

/**
 * kitUnavailableTitle(node, kitSteps) — unavailableTitle for a user-defined
 * kit. Separate from unavailableTitle because that one judges 'kit' on the
 * default step list and would go stale the moment the user edits the kit.
 * Returns a dedicated string for an empty kit, never `undefined` while the
 * entry is greyed out.
 */
export function kitUnavailableTitle(node, kitSteps) {
  if (kitStepTypes(kitSteps).length === 0) return 'The diagnostic kit has no steps'
  if (isKitAvailable(node, kitSteps)) return undefined
  return titleForMissing(missingKitTools(node, kitSteps))
}

function titleForMissing(missing) {
  if (missing.length === 0) return undefined
  const subject = missing.join(', ')
  if (missing.every((m) => NATIVE_COMMAND_TYPES.includes(m))) {
    return `${subject} ${missing.length === 1 ? 'needs' : 'need'} a newer agent on this node`
  }
  return `${subject} ${missing.length === 1 ? 'is' : 'are'} not installed on this node`
}

/**
 * unavailableLabel(node, type) — the short inline note next to a greyed-out
 * entry, or undefined when the command is available. Native probes need no
 * binary, so "not installed" would be the wrong reason for them.
 */
export function unavailableLabel(node, type) {
  return labelForMissing(missingToolsFor(node, type))
}

/**
 * kitUnavailableLabel(node, kitSteps) — the short inline note for a greyed-out
 * kit entry. Like the title, it never returns `undefined` for a kit that is
 * shown as unavailable.
 */
export function kitUnavailableLabel(node, kitSteps) {
  if (kitStepTypes(kitSteps).length === 0) return 'no steps'
  if (isKitAvailable(node, kitSteps)) return undefined
  return labelForMissing(missingKitTools(node, kitSteps))
}

function labelForMissing(missing) {
  if (missing.length === 0) return undefined
  if (missing.every((m) => NATIVE_COMMAND_TYPES.includes(m))) return 'needs a newer agent'
  return 'not installed'
}
