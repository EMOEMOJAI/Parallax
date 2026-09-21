// Exports are explicit snapshots; never include browser credentials or React IDs.
export function resultDocument(meta, lines, summary, exportedAt = new Date().toISOString()) {
  return {
    schema_version: 1,
    exported_at: exportedAt,
    started_at: meta?.startedAt || null,
    shared_at: meta?.sharedAt || null,
    node: { name: meta?.nodeName || '', location: meta?.nodeLocation || '' },
    command: meta?.command || '', target: meta?.target || '', options: meta?.options || '',
    summary: summary || null,
    lines: lines.map(({ type, text }) => ({ type, text })),
  }
}

export function shareDocument(meta, lines) {
  return {
    node_name: meta.nodeName || '', node_flag: meta.nodeFlag || '', node_location: meta.nodeLocation || '',
    command: meta.command || '', target: meta.target || '', options: meta.options || '',
    lines: lines.map(({ type, text }) => ({ type, text })),
  }
}

// Literal, case-sensitive replacements across metadata and output. Never treat
// user input as regex syntax, and never rewrite schema keys or line types.
export function redactShare(snapshot, terms) {
  const words = [...new Set(terms.split('\n').filter(Boolean))].sort((a, b) => b.length - a.length)
  if (!words.length) return snapshot
  const pattern = new RegExp(words.map((word) => word.replace(/[.*+?^${}()|[\]\\]/g, '\\$&')).join('|'), 'g')
  const redact = (value) => value.replace(pattern, '[redacted]')
  return {
    ...Object.fromEntries(Object.entries(snapshot).filter(([key]) => key !== 'lines').map(([key, value]) => [key, redact(value)])),
    lines: snapshot.lines.map(({ type, text }) => ({ type, text: redact(text) })),
  }
}

// Quote every cell and neutralize spreadsheet formula prefixes, including
// prefixes hidden behind whitespace/control characters in agent-provided text.
export function csvCell(value) {
  let text = String(value ?? '')
  if (/^[\s\u0000-\u001f]*[=+@-]/.test(text)) text = "'" + text
  return '"' + text.replaceAll('"', '""') + '"'
}

export function comparisonCsv(meta, results, summaries) {
  const rows = [['node', 'location', 'command', 'target', 'options', 'started_at', 'summary', 'output']]
  for (const node of meta.nodes) rows.push([
    node.name, node.location, meta.command, meta.target, meta.options, meta.startedAt,
    JSON.stringify(summaries[node.id] || null), (results[node.id] || []).map((line) => line.text).join('\n'),
  ])
  return rows.map((row) => row.map(csvCell).join(',')).join('\r\n') + '\r\n'
}

export function downloadFile(text, mime, filename) {
  const url = URL.createObjectURL(new Blob([text], { type: mime }))
  const anchor = document.createElement('a')
  anchor.href = url
  anchor.download = filename
  anchor.click()
  setTimeout(() => URL.revokeObjectURL(url), 10000)
}
