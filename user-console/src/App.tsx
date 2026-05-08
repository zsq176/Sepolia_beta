import { useState, useEffect, useMemo } from 'react'
import { useWallet } from './hooks/useWallet'
import { useMarket } from './hooks/useMarket'
import { WalletConnect } from './components/WalletConnect'
import { KLineChart } from './components/KLineChart'
import { TradingPanel } from './components/TradingPanel'
import { Portfolio } from './components/Portfolio'
import { TradeHistory } from './components/TradeHistory'
import { PriceMonitor } from './components/PriceMonitor'
import { OperationLogs } from './components/OperationLogs'

interface AppConfig {
  btc_beta_addr?: string
  usdt_beta_addr?: string
  eth_beta_addr?: string
  v2_pair?: string
  v3_pool?: string
  v4_pool_manager?: string
  chain_id?: number
}

function DeviationPanel({ rows }: { rows: { label: string; dev: number; fresh: boolean; quality?: string }[] }) {

  return (
    <div className="card" style={{ padding: '8px 16px' }}>
      <div style={{ display: 'flex', gap: 16, flexWrap: 'wrap', fontSize: 12 }}>
        {rows.map(p => {
          const devPct = Math.abs(p.dev) * 100
          const warn = p.fresh && devPct > 0.5
          return (
            <div key={p.label}>
              <span style={{ color: 'var(--text-dim)' }}>{p.label}: </span>
              <span style={{ color: warn ? 'var(--red)' : 'var(--green)', marginLeft: 4 }}>
                {p.fresh ? `${devPct.toFixed(3)}%` : 'stale'}
              </span>
              {p.quality && (
                <span style={{ marginLeft: 6, fontSize: 10, color: 'var(--text-dim)' }}>
                  [{p.quality}]
                </span>
              )}
            </div>
          )
        })}
      </div>
    </div>
  )
}

export default function App() {
  const wallet = useWallet()
  const market = useMarket()
  const [config, setConfig] = useState<AppConfig>({})

  useEffect(() => {
    fetch('/api/config')
      .then(r => r.json())
      .then(data => setConfig(data))
      .catch(() => {})
  }, [])

  const isCircuitBroken = !!market.health.circuit_broken

  const legacyPrices = useMemo(() => {
    const p: Record<string, number> = {}
    const s = market.snapshot
    if (!s) return p
    Object.entries(s.instruments || {}).forEach(([id, inst]) => {
      p[id] = inst.pool_price
      p[inst.label] = inst.pool_price
    })
    p.circuit_broken = isCircuitBroken ? 1 : 0
    return p
  }, [market.snapshot, isCircuitBroken])

  const deviationRows = useMemo(() => {
    return Object.entries(market.snapshot?.instruments || {}).map(([id, i]) => ({
      label: `${id} (${i.venue})`,
      dev: i.deviation,
      fresh: i.pool_fresh && i.target_fresh,
      quality: i.quality_level,
    }))
  }, [market.snapshot])

  return (
    <div>
      <div className="header">
        <h1>Beta Trading Platform</h1>
        <div style={{ display: 'flex', alignItems: 'center', gap: 12 }}>
          {market.health.last_snapshot && (
            <span className="badge" style={{ fontSize: 11 }}>
              snapshot: {new Date(market.health.last_snapshot * 1000).toLocaleTimeString()}
            </span>
          )}
          {isCircuitBroken && (
            <span className="badge badge-red" style={{ fontSize: 12, padding: '4px 12px' }}>
              CIRCUIT BREAKER
            </span>
          )}
          <span className={`badge ${market.connected ? 'badge-green' : 'badge-red'}`} style={{ fontSize: 11 }}>
            {market.connected ? '● Live' : '○ Disconnected'}
          </span>
          <WalletConnect wallet={wallet} />
        </div>
      </div>

      <PriceMonitor prices={legacyPrices} connected={market.connected} />

      <DeviationPanel rows={deviationRows} />

      <div className="grid" style={{ padding: '16px' }}>
        {/* Charts */}
        <div className="flex-col" style={{ gridColumn: '1' }}>
          <div className="card">
            <h3>V2 BTC-Beta/USDT-Beta</h3>
            <KLineChart pair="v2:BTCUSDT" height={240} />
          </div>
          <div className="card">
            <h3>V3 BTC-Beta/USDT-Beta</h3>
            <KLineChart pair="v3:BTCUSDT" height={250} />
          </div>
          <div className="card">
            <h3>V4 BTC-Beta/USDT-Beta</h3>
            <KLineChart pair="v4:BTCUSDT" height={250} />
          </div>
          <div className="card">
            <h3>V4 ETH-Beta/USDT-Beta</h3>
            <KLineChart pair="v4:ETHUSDT" height={250} />
          </div>
          <div className="card">
            <h3>V4 ETH-Beta/BTC-Beta</h3>
            <KLineChart pair="v4:ETHBTC" height={250} />
          </div>
        </div>

        {/* Sidebar */}
        <div className="flex-col">
          <Portfolio wallet={wallet} prices={legacyPrices} config={config} />
          <TradeHistory wallet={wallet} />
          <TradingPanel wallet={wallet} prices={legacyPrices} config={config} />
          <OperationLogs wallet={wallet} />
        </div>
      </div>
    </div>
  )
}
