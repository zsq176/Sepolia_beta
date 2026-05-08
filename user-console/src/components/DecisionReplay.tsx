import { useEffect, useMemo, useState } from 'react'
import { WalletState } from '../hooks/useWallet'

interface DecisionLog {
  id: number
  timestamp: number
  instrument_id: string
  quality_level: string
  pool_price: number
  target_price: number
  deviation: number
  notional_usd: number
  state: string
  allowed: boolean
  reason: string
}

export function DecisionReplay({ wallet }: { wallet: WalletState }) {
  const [logs, setLogs] = useState<DecisionLog[]>([])
  const [instrument, setInstrument] = useState<string>('v4:BTCUSDT')

  useEffect(() => {
    if (!wallet.authToken) {
      setLogs([])
      return
    }
    const load = async () => {
      const to = Math.floor(Date.now() / 1000)
      const from = to - 3600
      try {
        const r = await fetch(`/api/replay/decisions?instrument_id=${encodeURIComponent(instrument)}&from=${from}&to=${to}&limit=300`, {
          headers: { Authorization: `Bearer ${wallet.authToken}` },
        })
        const data = await r.json()
        if (Array.isArray(data)) setLogs(data)
      } catch {
        // ignore
      }
    }
    load()
    const timer = setInterval(load, 8000)
    return () => clearInterval(timer)
  }, [wallet.authToken, instrument])

  const stats = useMemo(() => {
    const total = logs.length
    const allowed = logs.filter(l => l.allowed).length
    const avgDev = total ? logs.reduce((a, b) => a + Math.abs(b.deviation), 0) / total : 0
    return { total, allowed, avgDev }
  }, [logs])

  if (!wallet.authToken) return null

  return (
    <div className="card">
      <h3>策略复盘（最近1小时）</h3>
      <div className="flex-row" style={{ marginBottom: 8 }}>
        <select value={instrument} onChange={(e) => setInstrument(e.target.value)}>
          <option value="v3:BTCUSDT">v3:BTCUSDT</option>
          <option value="v4:BTCUSDT">v4:BTCUSDT</option>
          <option value="v4:ETHUSDT">v4:ETHUSDT</option>
          <option value="v4:ETHBTC">v4:ETHBTC</option>
        </select>
      </div>
      <div className="price-row"><span className="label">样本数</span><span className="value">{stats.total}</span></div>
      <div className="price-row"><span className="label">允许执行</span><span className="value">{stats.allowed}</span></div>
      <div className="price-row"><span className="label">平均偏离</span><span className="value">{(stats.avgDev*100).toFixed(3)}%</span></div>
      <div style={{ maxHeight: 220, overflow: 'auto', marginTop: 8 }}>
        <table>
          <thead>
            <tr>
              <th>时间</th>
              <th>偏离</th>
              <th>质量</th>
              <th>状态</th>
              <th>结果</th>
            </tr>
          </thead>
          <tbody>
            {logs.slice(-50).reverse().map((l) => (
              <tr key={l.id}>
                <td>{new Date(l.timestamp * 1000).toLocaleTimeString()}</td>
                <td>{(l.deviation * 100).toFixed(3)}%</td>
                <td>{l.quality_level}</td>
                <td>{l.state}</td>
                <td>{l.allowed ? 'allow' : 'skip'}</td>
              </tr>
            ))}
          </tbody>
        </table>
      </div>
    </div>
  )
}
