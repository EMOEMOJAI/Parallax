import { motionStateClass, useMountTransition } from '../hooks/useMountTransition'
import { useState, useEffect, useCallback, useRef, useMemo } from 'react'
import { MapContainer, TileLayer, Marker, Popup, Polyline, CircleMarker, useMap } from 'react-leaflet'
import L from 'leaflet'
import { Map as MapIcon, X, Loader, Radio } from 'lucide-react'
import 'leaflet/dist/leaflet.css'
import { apiFetch } from '../lib/api'
import { parseRouteHops } from '../lib/hops'
import { useFocusTrap } from '../hooks/useFocusTrap'

// Escape HTML to prevent XSS in Leaflet divIcon
function escapeHtml(str) {
  const div = document.createElement('div')
  div.appendChild(document.createTextNode(str))
  return div.innerHTML
}

// Custom node marker icon — memoized to avoid creating new L.divIcon on every render
const nodeIconCache = new Map()
function createNodeIcon(flag, online) {
  const key = `${flag || ''}-${online}`
  if (nodeIconCache.has(key)) return nodeIconCache.get(key)
  // Safety cap — shouldn't be reached in practice since key space is small
  if (nodeIconCache.size > 200) nodeIconCache.clear()
  const icon = L.divIcon({
    className: '',
    html: `<div style="
      display:flex;align-items:center;justify-content:center;
      width:32px;height:32px;border-radius:10px;
      background:${online ? 'rgba(34,197,94,0.2)' : 'rgba(239,68,68,0.2)'};
      border:2px solid ${online ? '#22c55e' : '#ef4444'};
      font-size:16px;backdrop-filter:blur(4px);
    ">${escapeHtml(flag || '')}</div>`,
    iconSize: [32, 32],
    iconAnchor: [16, 16],
    popupAnchor: [0, -20],
  })
  nodeIconCache.set(key, icon)
  return icon
}

// Fit map to show all points — only on initial mount or when hops change
function FitBounds({ points }) {
  const map = useMap()
  const fittedRef = useRef(null)

  useEffect(() => {
    if (points.length === 0) return
    // Hop coordinates arrive asynchronously after the trace text. Refit when
    // actual points change, including that second render.
    const pointsKey = JSON.stringify(points)
    if (fittedRef.current === pointsKey) return
    fittedRef.current = pointsKey

    if (points.length === 1) {
      map.setView(points[0], 4)
    } else {
      map.fitBounds(points, { padding: [50, 50], maxZoom: 6 })
    }
  }, [points, map])
  return null
}

export default function GeoMap({ visible, onClose, nodes, traceHops, state: parentState = 'open' }) {
  // The parent retains the exit; start entry only once this lazy chunk mounts.
  const { state } = useMountTransition(visible && parentState !== 'closing')
  const [hopsGeo, setHopsGeo] = useState([])
  const [loadingHops, setLoadingHops] = useState(false)
  const dialogRef = useRef(null)
  useFocusTrap(dialogRef, visible && state !== 'closing')

  // Resolve traceroute hop IPs to geo coordinates
  const abortControllerRef = useRef(null)
  const resolveHops = useCallback(async (hops) => {
    // Abort any in-flight resolution
    if (abortControllerRef.current) abortControllerRef.current.abort()

    if (!hops || hops.length === 0) {
      setHopsGeo([])
      setLoadingHops(false)
      return
    }

    const routeHops = parseRouteHops(hops)
    if (routeHops.length === 0) {
      setHopsGeo([])
      setLoadingHops(false)
      return
    }

    const uniqueIps = [...new Set(routeHops.map(({ ip }) => ip))]
    const controller = new AbortController()
    abortControllerRef.current = controller

    setLoadingHops(true)
    try {
      // Batch IPs in chunks of 50 to respect server-side maxGeoIPBatchSize
      const BATCH_SIZE = 50
      let allResults = []
      for (let i = 0; i < uniqueIps.length; i += BATCH_SIZE) {
        if (controller.signal.aborted) return
        const batch = uniqueIps.slice(i, i + BATCH_SIZE)
        const res = await apiFetch('/api/geoip/', {
          method: 'POST',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({ ips: batch }),
          signal: controller.signal,
        })
        if (!res.ok) continue
        const results = await res.json()
        allResults = allResults.concat(results)
      }

      if (controller.signal.aborted) return

      const geoMap = {}
      for (const r of allResults) {
        if (r.status === 'success' && r.lat != null && r.lon != null) {
          geoMap[r.query] = r
        }
      }

      const ordered = []
      for (const { hop, ip } of routeHops) {
        const geo = geoMap[ip]
        if (geo) {
          ordered.push({
            hop, ip,
            lat: geo.lat,
            lon: geo.lon,
            city: geo.city,
            country: geo.country,
            isp: geo.isp,
            org: geo.org,
            as: geo.as,
          })
        }
      }
      setHopsGeo(ordered)
    } catch (err) {
      if (err.name === 'AbortError') return
      console.error('GeoIP batch lookup failed:', err)
      setHopsGeo([])
    } finally {
      if (!controller.signal.aborted) {
        setLoadingHops(false)
      }
    }
  }, [])

  // Abort in-flight GeoIP requests when component unmounts
  useEffect(() => {
    return () => {
      if (abortControllerRef.current) abortControllerRef.current.abort()
    }
  }, [])

  // Resolve hops when they change
  const hopsKey = useMemo(() => {
    if (!traceHops || traceHops.length === 0) return ''
    return JSON.stringify(traceHops)
  }, [traceHops])

  useEffect(() => {
    // The effect must restart after StrictMode's setup/cleanup replay too.
    // A persistent "already seen" ref would suppress the replacement for an
    // aborted first lookup. Equal route contents keep hopsKey stable.
    resolveHops(traceHops)
  }, [hopsKey, resolveHops])

  const allPoints = useMemo(() => {
    const pts = []
    for (const n of nodes) {
      if (n.lat != null && n.lon != null) pts.push([n.lat, n.lon])
    }
    for (const h of hopsGeo) {
      pts.push([h.lat, h.lon])
    }
    return pts
  }, [nodes, hopsGeo])

  const tracePath = useMemo(() => {
    return hopsGeo.map((h) => [h.lat, h.lon])
  }, [hopsGeo])

  if (!visible) return null

  return (
    <div data-state={state} className="motion-layer fixed inset-0 z-50 flex items-center justify-center p-4" onClick={onClose}>
      <div data-state={state} className="motion-backdrop absolute inset-0 bg-black/60 backdrop-blur-sm" />
      <div
        ref={dialogRef}
        role="dialog"
        aria-modal="true"
        aria-label="Network Map"
        tabIndex={-1}
        data-state={state}
        inert={state === 'closing'}
        aria-hidden={state === 'closing'}
        className={`t-modal ${motionStateClass(state)} relative w-full max-w-5xl h-[75vh] rounded-2xl border border-border/50
          bg-bg-elevated shadow-2xl shadow-black/50 overflow-hidden flex flex-col`}
        onClick={(e) => e.stopPropagation()}
      >
        {/* Header */}
        <div className="flex items-center justify-between px-5 py-3 border-b border-border/30 shrink-0">
          <div className="flex items-center gap-2">
            <MapIcon size={16} className="text-cyan" />
            <span className="text-sm font-semibold text-text-primary">Network Map</span>
            {loadingHops && (
              <span className="flex items-center gap-1 text-[11px] text-text-muted">
                <Loader size={11} className="animate-spin" /> Resolving hops...
              </span>
            )}
            {hopsGeo.length > 0 && (
              <span className="px-2 py-0.5 rounded-full text-[10px] font-medium bg-cyan-muted text-cyan">
                {hopsGeo.length} hops mapped
              </span>
            )}
          </div>
          <button onClick={onClose} aria-label="Close network map"
            className="p-1.5 rounded-lg hover:bg-hover-overlay transition-colors text-text-muted hover:text-text-primary cursor-pointer">
            <X size={16} />
          </button>
        </div>

        {/* Map */}
        <div className="flex-1 relative">
          <MapContainer
            center={[20, 0]}
            zoom={2}
            className="w-full h-full"
            style={{ background: '#121218' }}
            zoomControl={false}
            attributionControl={true}
          >
            <TileLayer
              url="https://{s}.basemaps.cartocdn.com/dark_all/{z}/{x}/{y}{r}.png"
              attribution='&copy; <a href="https://www.openstreetmap.org/copyright">OpenStreetMap</a> &copy; <a href="https://carto.com/">CARTO</a>'
            />

            <FitBounds points={allPoints} />

            {nodes.filter(n => n.lat != null && n.lon != null).map((node) => (
              <Marker
                key={node.id}
                position={[node.lat, node.lon]}
                icon={createNodeIcon(node.flag, node.online)}
              >
                <Popup className="dark-popup">
                  <div style={{ color: '#f0f0f5', fontSize: 12, lineHeight: 1.5 }}>
                    <div style={{ fontWeight: 600 }}>{node.flag} {node.name}</div>
                    <div style={{ color: '#8888a0' }}>{node.location}</div>
                    <div style={{ color: '#8888a0' }}>{node.provider}</div>
                    {node.ipv4 && <div style={{ fontFamily: 'monospace', color: '#b0b0be' }}>{node.ipv4}</div>}
                  </div>
                </Popup>
              </Marker>
            ))}

            {tracePath.length > 1 && (
              <Polyline
                positions={tracePath}
                pathOptions={{
                  color: '#06b6d4',
                  weight: 2.5,
                  opacity: 0.7,
                  dashArray: '8 6',
                }}
              />
            )}

            {hopsGeo.map((hop, i) => (
              <CircleMarker
                key={`${hop.ip}-${i}`}
                center={[hop.lat, hop.lon]}
                radius={5}
                pathOptions={{
                  color: '#06b6d4',
                  fillColor: i === hopsGeo.length - 1 ? '#22c55e' : '#06b6d4',
                  fillOpacity: 0.8,
                  weight: 2,
                }}
              >
                <Popup className="dark-popup">
                  <div style={{ color: '#f0f0f5', fontSize: 11, lineHeight: 1.5 }}>
                    <div style={{ fontWeight: 600 }}>Hop {hop.hop}</div>
                    <div style={{ fontFamily: 'monospace', color: '#b0b0be' }}>{hop.ip}</div>
                    <div style={{ color: '#8888a0' }}>{hop.city}, {hop.country}</div>
                    {hop.isp && <div style={{ color: '#8888a0', fontSize: 10 }}>{hop.isp}</div>}
                    {hop.as && <div style={{ color: '#71717a', fontSize: 10 }}>{hop.as}</div>}
                  </div>
                </Popup>
              </CircleMarker>
            ))}
          </MapContainer>

          {hopsGeo.length > 0 && (
            <div className="absolute bottom-4 left-4 z-[1000] max-w-xs max-h-48 overflow-y-auto
              rounded-xl bg-bg-elevated/90 backdrop-blur-md border border-border/40 p-3 shadow-xl">
              <div className="text-[10px] font-semibold uppercase tracking-widest text-cyan mb-2">
                Route Hops
              </div>
              {hopsGeo.map((hop, i) => (
                <div key={i} className="flex items-center gap-2 py-1 text-[11px]">
                  <span className="w-4 text-right text-text-muted tabular-nums">{hop.hop}</span>
                  <Radio size={8} className={i === hopsGeo.length - 1 ? 'text-success' : 'text-cyan'} />
                  <span className="font-mono text-text-secondary">{hop.ip}</span>
                  <span className="text-text-muted truncate">{hop.city}</span>
                </div>
              ))}
            </div>
          )}
        </div>
      </div>
    </div>
  )
}
