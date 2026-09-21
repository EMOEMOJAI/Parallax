import { useState } from 'react'
import GuidedDiagnostics from './GuidedDiagnostics'
import { compareRuns, compatibleRuns, incidentText, redactText, RETENTION_DAYS } from '../lib/investigations'
import { downloadFile } from '../lib/resultExport'

const control = 'min-h-11 rounded-lg border border-border-hover bg-bg-secondary px-3 text-sm text-text-primary focus-visible:outline-2 focus-visible:outline-accent-text disabled:opacity-40'
const label = (run) => `${run.node.name} · ${run.command} ${run.target}${run.options ? ` (${run.options})` : ''}`

export default function Investigations({ current, library, nodes, ws }) {
  const [days, setDays] = useState('7')
  const [baselineId, setBaselineId] = useState('')
  const [candidateId, setCandidateId] = useState('current')
  const [excluded, setExcluded] = useState([])
  const [title, setTitle] = useState('')
  const [notes, setNotes] = useState('')
  const [preview, setPreview] = useState(null)
  const [terms, setTerms] = useState('')
  const candidate = candidateId === 'current' ? current : library.drafts.find((entry) => entry.id === candidateId)?.run
  const baseline = library.baselines.find((entry) => entry.id === baselineId && entry.expiresAt > Date.now())
  const comparison = baseline && candidate ? compareRuns(baseline.run, candidate) : null
  const selected = library.drafts.filter((entry) => !excluded.includes(entry.id))
  const redacted = preview === null ? '' : redactText(preview, terms)

  return <details className="mt-4 rounded-2xl border border-border/50 bg-bg-secondary/20" data-testid="investigations">
    <summary className="min-h-11 p-4 cursor-pointer font-semibold text-text-primary">Investigations <span className="font-normal text-sm text-text-muted">· Baselines, guided checks & reports</span></summary>
    <div className="p-4 pt-0 space-y-6">
      <GuidedDiagnostics nodes={nodes} ws={ws} onCollect={library.addDrafts} />
      <section aria-labelledby="baseline-title" className="space-y-3 border-t border-border pt-4">
        <h3 id="baseline-title" className="font-semibold">Compare with a baseline</h3>
        <p className="text-sm text-text-muted">Save only results you choose. Baselines include node details and raw output, stay in this browser after sign out, and are removed on the next visit or within a minute while this dashboard is open after expiry. Up to 10 baselines / 2 MiB.</p>
        <label className="block text-sm">Result to compare or save
          <select aria-label="Result to compare or save" className={`${control} block w-full mt-1`} value={candidateId} onChange={(e) => setCandidateId(e.target.value)}>
            <option value="current">Current terminal result{!current ? ' (run a check first)' : ''}</option>
            {library.drafts.map((entry, index) => <option key={entry.id} value={entry.id}>{index + 1}. {label(entry.run)}</option>)}
          </select>
        </label>
        <div className="flex flex-wrap gap-2 items-center">
          <label className="text-sm">Keep for <select aria-label="Baseline retention" className={control} value={days} onChange={(e) => setDays(e.target.value)}>{RETENTION_DAYS.map((day) => <option key={day} value={day}>{day} {day === 1 ? 'day' : 'days'}</option>)}</select></label>
          <button className={control} disabled={!candidate} onClick={() => library.saveBaseline(candidate, days)}>Save baseline</button>
          <button className={control} disabled={!current} onClick={() => library.addDrafts([current])}>Add current result to incident</button>
        </div>
        <label className="block text-sm">Saved baseline
          <select aria-label="Saved baseline" className={`${control} block w-full mt-1`} value={baselineId} onChange={(e) => setBaselineId(e.target.value)}>
            <option value="">Choose a baseline</option>
            {library.baselines.map((entry) => <option key={entry.id} value={entry.id}>{label(entry.run)} · {entry.run.started_at || entry.run.shared_at || 'time unknown'}</option>)}
          </select>
        </label>
        <div className="flex flex-wrap gap-2">
          <button className={control} disabled={!baseline} onClick={() => { library.deleteBaseline(baselineId); setBaselineId('') }}>Delete selected baseline</button>
          <button className={control} onClick={() => { library.deleteBaseline(null); setBaselineId('') }}>Clear saved baselines</button>
        </div>
        {baseline && <p className="text-xs text-text-muted">Expires {new Date(baseline.expiresAt).toLocaleString()}</p>}
        {baseline && candidate && !compatibleRuns(baseline.run, candidate) && <p role="status" className="text-sm text-warning">Choose results with the same node name, location, command, target and options. Kit comparisons also require a recorded step sequence.</p>}
        {comparison && <div className="space-y-2 text-sm" aria-label="Baseline comparison">
          <p className="text-text-muted">Baseline → selected result. Differences are observations, not proof of degradation.</p>
          {comparison.metrics.length > 0 && <div className="overflow-x-auto"><table className="w-full text-left"><caption className="sr-only">Metric changes</caption><thead><tr><th>Metric</th><th>Before</th><th>After</th><th>Change</th></tr></thead><tbody>
            {comparison.metrics.map((metric) => <tr key={metric.label}><th className="py-2 font-normal">{metric.label}</th><td>{metric.before}</td><td>{metric.after}</td><td>{metric.delta > 0 ? '+' : ''}{metric.delta} {metric.unit}</td></tr>)}
          </tbody></table></div>}
          {comparison.parsed && <div className="break-words"><p>{comparison.kind} (recognized records only):</p>
            {comparison.removed.map((value) => <p key={value} className="text-warning">Removed: {value}</p>)}
            {comparison.added.map((value) => <p key={value} className="text-success">Added: {value}</p>)}
            {!comparison.removed.length && !comparison.added.length && <p>No changes in recognized records.</p>}
          </div>}
          {!comparison.parsed && !comparison.metrics.length && <p>No comparable structured data. Compare the raw output below.</p>}
          <details><summary className="cursor-pointer min-h-11 py-3">Compare raw output</summary><div className="grid sm:grid-cols-2 gap-3">{[['Baseline', baseline.run], ['Selected result', candidate]].map(([name, run]) => <div key={name} className="min-w-0"><h4>{name}</h4><pre className="text-xs whitespace-pre-wrap break-all">{run.lines.map((line) => line.text).join('\n')}</pre></div>)}</div></details>
        </div>}
      </section>
      <section aria-labelledby="incident-title" className="space-y-3 border-t border-border pt-4">
        <h3 id="incident-title" className="font-semibold">Incident report</h3>
        <p className="text-sm text-text-muted">Collect checks from the terminal, guided diagnostics or multi-node comparison. Drafts stay in memory until reload or sign out. Nothing is uploaded.</p>
        <p className="text-sm">{library.drafts.length}/20 checks collected</p>
        {library.drafts.map((entry, index) => <div key={entry.id} className="flex items-center gap-2">
          <label className="text-sm min-w-0 flex-1 break-words"><input type="checkbox" checked={!excluded.includes(entry.id)} onChange={(e) => setExcluded((previous) => e.target.checked ? previous.filter((id) => id !== entry.id) : [...previous, entry.id])} /> {index + 1}. {label(entry.run)}</label>
          <button className={control} aria-label={`Remove check ${index + 1}`} onClick={() => library.removeDraft(entry.id)}>Remove</button>
        </div>)}
        <button className={control} disabled={!library.drafts.length && preview === null} onClick={() => { library.clearDrafts(); setPreview(null); setTerms(''); setTitle(''); setNotes(''); setExcluded([]) }}>Clear incident draft</button>
        <label className="block text-sm">Report title<input className={`${control} block mt-1 w-full`} maxLength={200} value={title} onChange={(e) => setTitle(e.target.value)} /></label>
        <label className="block text-sm">Notes<textarea className={`${control} block mt-1 w-full py-2`} maxLength={4000} value={notes} onChange={(e) => setNotes(e.target.value)} /></label>
        <button className={control} disabled={!selected.length} onClick={() => { setPreview(incidentText(selected.map((entry) => entry.run), title, notes)); setTerms('') }}>Preview incident report</button>
        {preview !== null && <div className="space-y-3">
          <p className="text-sm text-text-muted">This preview is a frozen snapshot. Preview again to include draft changes. Redaction replaces exact, case-sensitive text everywhere; it does not detect secrets automatically.</p>
          <label className="block text-sm">Redact exact text (one value per line)<textarea aria-label="Redact incident text" className={`${control} block w-full py-2 mt-1`} maxLength={4000} value={terms} onChange={(e) => setTerms(e.target.value)} /></label>
          <label className="block text-sm">Report preview<textarea aria-label="Incident report preview" readOnly className={`${control} block w-full h-72 py-2 mt-1 font-mono text-xs`} value={redacted} /></label>
          <button className={control} onClick={() => downloadFile(redacted, 'text/plain;charset=utf-8', 'parallax-incident.txt')}>Download incident report</button>
          <button className={`${control} ml-2`} onClick={() => { setPreview(null); setTerms('') }}>Discard report preview</button>
        </div>}
      </section>
      <p role="status" className="text-sm text-text-muted">{library.notice}</p>
    </div>
  </details>
}
