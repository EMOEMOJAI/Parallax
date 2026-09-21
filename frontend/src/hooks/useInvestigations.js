import { useCallback, useEffect, useRef, useState } from 'react'
import { randomId } from '../lib/id'
import { BASELINE_KEY, MAX_COLLECTION_BYTES, MAX_RUNS, RETENTION_DAYS, readBaselines, writeBaselines, validateRun } from '../lib/investigations'

export function useInvestigations() {
  const [drafts, setDrafts] = useState([])
  const draftsRef = useRef([])
  const [baselines, setBaselines] = useState([])
  const [notice, setNotice] = useState('')
  const reload = useCallback(() => {
    try {
      const entries = readBaselines(localStorage)
      writeBaselines(localStorage, entries)
      setBaselines(entries)
    } catch { setNotice('Couldn’t read or clean saved baselines. Browser storage may be blocked or invalid.') }
  }, [])
  useEffect(() => {
    reload()
    const sync = (event) => { if (event.key === BASELINE_KEY || event.key === null) reload() }
    window.addEventListener('storage', sync)
    // Expire open-tab entries too; do not persist old in-memory entries.
    const timer = setInterval(reload, 60000)
    return () => { window.removeEventListener('storage', sync); clearInterval(timer) }
  }, [reload])

  const addDrafts = useCallback((runs) => {
    if (!runs.length || runs.some((run) => !validateRun(run))) {
      setNotice('This result is too large for an incident draft (256 KiB per check). Download its output separately.')
      return
    }
    const copies = runs.map((run) => ({ id: randomId(), run: structuredClone(run) }))
    const next = [...draftsRef.current, ...copies]
    if (next.length > MAX_RUNS || new TextEncoder().encode(JSON.stringify(next)).length > MAX_COLLECTION_BYTES) {
      setNotice('Incident draft is full (20 checks / 2 MiB). Remove checks before adding more.')
      return
    }
    draftsRef.current = next
    setDrafts(next)
    setNotice(`${runs.length} ${runs.length === 1 ? 'check added' : 'checks added'} to the incident draft.`)
  }, [])

  const saveBaseline = (run, days) => {
    if (!validateRun(run) || !RETENTION_DAYS.includes(Number(days))) { setNotice('This result cannot be saved as a baseline.'); return }
    try {
      const next = [...readBaselines(localStorage), { id: randomId(), expiresAt: Date.now() + Number(days) * 86400000, run: structuredClone(run) }]
      writeBaselines(localStorage, next)
      setBaselines(next)
      setNotice('Baseline saved in this browser. It remains after sign out until deleted or expired.')
    } catch { setNotice('Couldn’t save baseline. Storage may be blocked or full; delete an older baseline and retry.') }
  }
  const deleteBaseline = (id) => {
    try {
      const next = id === null ? [] : readBaselines(localStorage).filter((entry) => entry.id !== id)
      writeBaselines(localStorage, next)
      setBaselines(next)
      setNotice('Saved baseline data deleted.')
    } catch { setNotice('Couldn’t delete saved baseline data. Check browser storage settings.') }
  }
  return { drafts, baselines, notice, addDrafts, saveBaseline, deleteBaseline,
    removeDraft: (id) => { draftsRef.current = draftsRef.current.filter((entry) => entry.id !== id); setDrafts(draftsRef.current) },
    clearDrafts: () => { draftsRef.current = []; setDrafts([]) } }
}
