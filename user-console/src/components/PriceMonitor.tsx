import { useEffect, useMemo, useRef, useState } from 'react'

interface Props {
  prices: Record<string, number>
  connected: boolean
}

export function PriceMonitor({ prices, connected }: Props) {
  const isCircuitBroken = prices['circuit_broken'] === 1
  const prevRef = useRef<Record<string, number>>({})
  const [flash, setFlash] = useState<Record<string, string>>({})

  const displayPairs = [
    { keys: ['v2:BTCUSDT', 'V2 BTC-Beta/USDT-Beta'], label: 'V2 BTC/USDT' },
    { keys: ['v3:BTCUSDT', 'V3 BTC-Beta/USDT-Beta'], label: 'V3 BTC/USDT' },
    { keys: ['v4:BTCUSDT', 'V4 BTC-Beta/USDT-Beta'], label: 'V4 BTC/USDT' },
    { keys: ['v4:ETHUSDT', 'V4 ETH-Beta/USDT-Beta'], label: 'V4 ETH/USDT' },
    { keys: ['v4:ETHBTC', 'V4 ETH-Beta/BTC-Beta'], label: 'V4 ETH/BTC' },
  ]

  const displayData = useMemo(() => {
    return displayPairs.map((p) => {
      const key = p.keys.find((k) => typeof prices[k] === 'number' && prices[k] > 0)
      const value = key ? prices[key] : undefined
      return { ...p, key: key || p.label, value }
    })
  }, [displayPairs, prices])

  useEffect(() => {
    const nextFlash: Record<string, string> = {}
    for (const item of displayData) {
      if (typeof item.value !== 'number') continue
      const prev = prevRef.current[item.key]
      if (typeof prev === 'number' && item.value !== prev) {
        const dir = item.value > prev ? 'up' : 'down'
        // Include timestamp so CSS animation re-triggers on every update.
        nextFlash[item.key] = `${dir}-${Date.now()}`
      }
      prevRef.current[item.key] = item.value
    }
    if (Object.keys(nextFlash).length > 0) {
      setFlash((m) => ({ ...m, ...nextFlash }))
      const timer = setTimeout(() => {
        setFlash((m) => {
          const out = { ...m }
          for (const k of Object.keys(nextFlash)) out[k] = ''
          return out
        })
      }, 450)
      return () => clearTimeout(timer)
    }
  }, [displayData])

  return (
    <div className="status-bar">
      <div className="flex-row" style={{ alignItems: 'center', gap: 6, flex: 1, flexWrap: 'wrap' }}>
        <div className={`status-indicator ${connected ? 'status-connected' : 'status-disconnected'}`} />
        <span style={{ fontSize: 11 }}>{connected ? 'Live' : 'Disconnected'}</span>

        {isCircuitBroken && (
          <span className="badge badge-red">Circuit Breaker Active</span>
        )}

        {displayData.map((p) => {
          const val = p.value
          const fx = flash[p.key] || ''
          const dirClass = fx.startsWith('up') ? 'flash-up' : fx.startsWith('down') ? 'flash-down' : ''
          const decimals = p.label.includes('ETH/BTC') ? 6 : 2
          return (
          <span key={p.label} style={{ fontSize: 11, marginLeft: 8 }}>
            <span style={{ color: 'var(--text-dim)' }}>{p.label}:</span>{' '}
            <span className={`ticker-value ${dirClass}`} style={{ fontFamily: 'monospace' }}>
              {val ? val.toFixed(decimals) : '...'}
            </span>
          </span>
        )})}
      </div>
    </div>
  )
}
