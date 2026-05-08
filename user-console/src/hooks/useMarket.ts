import { useEffect, useMemo, useState } from 'react'

export interface SnapshotInstrument {
  label: string
  venue: string
  pool_price: number
  pool_time: number
  pool_fresh: boolean
  target_price: number
  target_source: string
  target_fresh: boolean
  deviation: number
  quality_level?: 'live' | 'fallback' | 'stale' | 'invalid'
}

export interface MarketSnapshot {
  ready: boolean
  generated_at: number
  binance: Record<string, { mid: number; time: number; stale: boolean }>
  instruments: Record<string, SnapshotInstrument>
}

export interface HealthState {
  status?: string
  engine?: string
  rpc?: string
  db?: string
  circuit_broken?: boolean
  last_snapshot?: number
}

export function useMarket() {
  const [snapshot, setSnapshot] = useState<MarketSnapshot | null>(null)
  const [risk, setRisk] = useState<Record<string, any>>({})
  const [health, setHealth] = useState<HealthState>({})
  const [connected, setConnected] = useState(false)

  useEffect(() => {
    let cancelled = false
    const load = async () => {
      try {
        const ts = Date.now()
        const [snapRes, riskRes, healthRes] = await Promise.all([
          fetch(`/api/snapshot?_t=${ts}`, { cache: 'no-store' }),
          fetch(`/api/risk?_t=${ts}`, { cache: 'no-store' }),
          fetch(`/api/health?_t=${ts}`, { cache: 'no-store' }),
        ])
        const [snap, riskData, healthData] = await Promise.all([
          snapRes.json(),
          riskRes.json(),
          healthRes.json(),
        ])
        if (!cancelled) {
          setSnapshot(snap)
          setRisk(riskData || {})
          setHealth(healthData || {})
        }
      } catch {
        // ignore transient fetch errors
      }
    }

    load()
    const timer = setInterval(load, 1000)
    return () => {
      cancelled = true
      clearInterval(timer)
    }
  }, [])

  useEffect(() => {
    const protocol = window.location.protocol === 'https:' ? 'wss:' : 'ws:'
    const wsUrl = `${protocol}//${window.location.host}/ws`
    let ws: WebSocket | null = null
    let reconnectTimer: number | null = null
    let disposed = false

    const connect = () => {
      if (disposed) return
      ws = new WebSocket(wsUrl)
      ws.onopen = () => setConnected(true)
      ws.onclose = () => {
        if (disposed) return
        setConnected(false)
        reconnectTimer = window.setTimeout(connect, 3000)
      }
      ws.onerror = () => ws?.close()
    }

    connect()
    return () => {
      disposed = true
      if (reconnectTimer) window.clearTimeout(reconnectTimer)
      // In React dev strict-mode, effects mount/unmount twice. Closing a socket
      // while CONNECTING causes noisy "closed before established" messages.
      if (ws?.readyState === WebSocket.OPEN) ws.close()
      else if (ws?.readyState === WebSocket.CONNECTING) ws.onopen = () => ws?.close()
    }
  }, [])

  const deviations = useMemo(() => {
    const items = snapshot?.instruments ?? {}
    return Object.entries(items).map(([id, inst]) => ({
      id,
      label: inst.label,
      value: inst.deviation,
      fresh: inst.pool_fresh && inst.target_fresh,
    }))
  }, [snapshot])

  return { snapshot, risk, health, connected, deviations }
}

