import { useState, useEffect } from 'react'
import { WalletState } from '../hooks/useWallet'

interface Trade {
  id: number
  timestamp: number
  pair: string
  action: string
  amount: string
  price: number
  tx_hash: string
  status: string
  source?: string
}

interface Props {
  wallet: WalletState
}

function statusColor(status: string): string {
  switch (status) {
    case 'confirmed': return 'var(--green)'
    case 'submitted': return '#d29922'
    case 'failed': return 'var(--red)'
    default: return 'var(--text-dim)'
  }
}

export function TradeHistory({ wallet }: Props) {
  const [trades, setTrades] = useState<Trade[]>([])

  const fetchTrades = () => {
    if (!wallet.authToken) {
      setTrades([])
      return
    }
    fetch(`/api/account/trades?limit=100&_t=${Date.now()}`, {
      cache: 'no-store',
      headers: { Authorization: `Bearer ${wallet.authToken}` },
    })
      .then(async (r) => {
        if (!r.ok) {
          setTrades([])
          return null
        }
        return r.json()
      })
      .then(data => {
        if (!Array.isArray(data)) return
        const filtered = data.filter((t: Trade) => {
          const pair = String(t.pair || '').toUpperCase()
          const src = String(t.source || '').toLowerCase()
          const isV2PanelPair =
            pair === 'V2:BTC-BETA/USDT-BETA' ||
            (pair.includes('V2:') && pair.includes('BTC-BETA/USDT-BETA'))
          const isUserSource = src === '' || src === 'user' || src === 'manual'
          return isV2PanelPair && isUserSource
        })
        setTrades(filtered)
      })
      .catch(() => setTrades([]))
  }

  useEffect(() => {
    const onEvent = () => fetchTrades()
    fetchTrades()
    const interval = setInterval(fetchTrades, 5000)
    window.addEventListener('account-data-updated', onEvent)
    return () => {
      clearInterval(interval)
      window.removeEventListener('account-data-updated', onEvent)
    }
  }, [wallet.authToken, wallet.address])

  return (
    <div className="card">
      <h3>Trade History</h3>
      {!wallet.authToken && (
        <p style={{ fontSize: 12, color: 'var(--text-dim)', marginBottom: 8 }}>
          登录钱包后显示你的 V2 手工下单记录。
        </p>
      )}
      {trades.length === 0 ? (
        <p style={{ fontSize: 13, color: 'var(--text-dim)' }}>
          {wallet.authToken ? '暂无 V2 交易记录' : '未登录'}
        </p>
      ) : (
        <div style={{ maxHeight: 400, overflow: 'auto' }}>
          <table>
            <thead>
              <tr>
                <th>Time</th>
                <th>Pair</th>
                <th>Action</th>
                <th>Amount</th>
                <th>Price</th>
                <th>Status</th>
                <th>Tx</th>
              </tr>
            </thead>
            <tbody>
              {trades.map((t) => (
                <tr key={t.id}>
                  <td>{new Date(t.timestamp * 1000).toLocaleTimeString()}</td>
                  <td style={{ fontSize: 11 }}>{t.pair}</td>
                  <td>
                    <span className={`badge ${String(t.action).toUpperCase().includes('BUY') ? 'badge-green' : 'badge-red'}`}>
                      {t.action}
                    </span>
                  </td>
                  <td>{t.amount}</td>
                  <td>{Number(t.price).toFixed(2)}</td>
                  <td>
                    <span style={{ fontSize: 10, color: statusColor(t.status) }}>
                      {t.status}
                    </span>
                  </td>
                  <td>
                    {t.tx_hash ? (
                      <a
                        href={`https://sepolia.etherscan.io/tx/${t.tx_hash}`}
                        target="_blank"
                        rel="noreferrer"
                        style={{ fontSize: 10, color: 'var(--blue)' }}
                      >
                        {t.tx_hash.slice(0, 10)}...
                      </a>
                    ) : '-'}
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
    </div>
  )
}
