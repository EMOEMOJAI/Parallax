// Shallow equality for a node's `tools` map, so a node_status frame that
// re-sends the same capabilities doesn't force a re-render.
function sameTools(a, b) {
  if (a === b) return true
  if (!a || !b) return false
  const ak = Object.keys(a)
  const bk = Object.keys(b)
  if (ak.length !== bk.length) return false
  return ak.every((k) => a[k] === b[k])
}

/** Apply one status frame without discarding metadata absent from offline frames. */
export function applyNodeStatus(nodes, data) {
  const existing = nodes.find((n) => n.id === data.node_id)
  if (existing) {
    // Update online status and any metadata the server sent
    const updated = { ...existing, online: data.online }
    if (data.name) updated.name = data.name
    if (data.location) updated.location = data.location
    if (data.flag) updated.flag = data.flag
    if (data.ipv4 !== undefined) updated.ipv4 = data.ipv4
    if (data.ipv6 !== undefined) updated.ipv6 = data.ipv6
    if (data.provider) updated.provider = data.provider
    if (data.lat !== undefined) updated.lat = data.lat
    if (data.lon !== undefined) updated.lon = data.lon
    // Agent capabilities. An *online* frame always projects the node's
    // current stored capabilities, so an absent key there means "this
    // node has none" — e.g. an older agent reconnected onto the same
    // node — and must clear the stale value instead of preserving it.
    // An offline frame carries no metadata at all, so it must not
    // clear anything.
    if (data.online || Object.hasOwn(data, 'version') || Object.hasOwn(data, 'tools')) {
      updated.version = data.version
      updated.tools = data.tools
    }
    // Skip re-render if nothing actually changed. `tools` is an object,
    // so a fresh-but-equal map would always compare unequal by
    // reference — compare it key by key instead.
    const changed = Object.keys(updated).some((k) => (
      k === 'tools' ? !sameTools(updated.tools, existing.tools) : updated[k] !== existing[k]
    ))
    if (!changed) return nodes
    return nodes.map((n) =>
      n.id === data.node_id ? updated : n
    )
  }
  // A coalesced online/offline sequence still carries the new node metadata.
  if (data.name) {
    return [...nodes, {
      id: data.node_id,
      name: data.name,
      location: data.location,
      flag: data.flag,
      ipv4: data.ipv4,
      ipv6: data.ipv6,
      provider: data.provider,
      lat: data.lat,
      lon: data.lon,
      online: data.online,
      // Undefined when the agent reported neither, which the
      // capability helper reads as "everything available".
      version: data.version,
      tools: data.tools,
    }]
  }
  return nodes
}

/** Coalesce in-flight status updates by node while preserving online metadata. */
export function rememberNodeStatus(updates, data) {
  const update = { ...updates.get(data.node_id), ...data }
  if (data.online) { update.version = data.version; update.tools = data.tools }
  updates.set(data.node_id, update)
}

/** Replay status updates newer than the request over its captured snapshot. */
export function reconcileNodeSnapshot(snapshot, updates) {
  return Array.from(updates.values()).reduce(applyNodeStatus, snapshot || [])
}
