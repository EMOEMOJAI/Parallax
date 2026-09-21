// Exports are explicit snapshots; never include browser credentials or React IDs.
export function resultDocument(meta, lines, summary, exportedAt = new Date().toISOString()) {
  return {
    schema_version: 1,
    exported_at: exportedAt,
    started_at: meta?.startedAt || null,
    shared_at: meta?.sharedAt || null,
    node: { name: meta?.nodeName || '', location: meta?.nodeLocation || '' },
    command: meta?.command || '', target: meta?.target || '', options: meta?.options || '',
    // A kit's final summary belongs to its last step, not the whole sequence.
    summary: meta?.command === 'kit' ? null : summary || null,
    lines: lines.map(({ type, text }) => ({ type, text })),
  }
}

// Match the server's control-character normalization before redaction and
// preview. Reject lengths it would truncate so reviewed content stays intact.
const SHARE_FIELD_LIMITS = { node_name: 64, node_flag: 32, node_location: 128, command: 32, target: 1024, options: 512 }
const shareField = (value) => String(value || '').replace(/[\u0000-\u001f\u007f]/g, '')

export function shareDocument(meta, lines) {
  return {
    node_name: shareField(meta.nodeName), node_flag: shareField(meta.nodeFlag), node_location: shareField(meta.nodeLocation),
    command: shareField(meta.command), target: shareField(meta.target), options: shareField(meta.options),
    lines: lines.map(({ type, text }) => ({ type, text: text.replace(/[\u0000-\u0008\u000b-\u001f\u007f]/g, '') })),
  }
}

export function shareValidationError(snapshot) {
  for (const [field, limit] of Object.entries(SHARE_FIELD_LIMITS)) {
    if ([...snapshot[field]].length > limit) return `The shared ${field.replaceAll('_', ' ')} exceeds its ${limit}-character limit. Download the output instead.`
  }
  const encoder = new TextEncoder()
  if (snapshot.lines.some((line) => encoder.encode(line.text).length > 4096)) return 'A line exceeds the 4 KiB share limit. Download the output instead.'
  return ''
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
