import { useEffect, useState } from 'react'
import { WalletState } from '../hooks/useWallet'

interface OpLog {
  id: number
  timestamp: number
  operation: string
  target: string
  detail: string
  status: string
  tx_hash?: string
}

export function OperationLogs({ wallet }: { wallet: WalletState }) {
  const [logs, setLogs] = useState<OpLog[]>([])

  useEffect(() => {
    if (!wallet.authToken) {
      setLogs([])
      return
    }
    let cancelled = false
    const load = async () => {
      try {
        const r = await fetch(`/api/account/op-logs?limit=100&_t=${Date.now()}`, {
          cache: 'no-store',
          headers: { Authorization: `Bearer ${wallet.authToken}` },
        })
        const data = await r.json()
        if (!cancelled && Array.isArray(data)) setLogs(data)
      } catch {
        // ignore
      }
    }
    const onEvent = () => load()
    load()
    const timer = setInterval(load, 7000)
    window.addEventListener('account-data-updated', onEvent)
    return () => {
      cancelled = true
      clearInterval(timer)
      window.removeEventListener('account-data-updated', onEvent)
    }
  }, [wallet.authToken])

  if (!wallet.authToken) return null

  return (
    <div className="card">
      <h3>Operation Audit</h3>
      {logs.length === 0 ? (
        <p style={{ fontSize: 12, color: 'var(--text-dim)' }}>No operation logs yet</p>
      ) : (
        <div style={{ maxHeight: 220, overflow: 'auto', fontSize: 12 }}>
          {logs.slice(0, 40).map((l) => (
            <div key={l.id} style={{ borderBottom: '1px solid var(--border)', padding: '6px 0' }}>
              <div style={{ color: 'var(--text-dim)', fontSize: 11 }}>
                {new Date(l.timestamp * 1000).toLocaleTimeString()} · {l.target}
              </div>
              <div>{l.operation} · {l.status}</div>
              <div style={{ color: 'var(--text-dim)' }}>{l.detail}</div>
              {l.tx_hash ? <div style={{ color: 'var(--blue)', fontSize: 11 }}>{l.tx_hash.slice(0, 18)}...</div> : null}
            </div>
          ))}
        </div>
      )}
    </div>
  )
}
