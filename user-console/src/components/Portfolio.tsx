import { useState, useEffect } from 'react'
import { ethers } from 'ethers'
import { WalletState } from '../hooks/useWallet'

const ERC20_ABI = [
  'function balanceOf(address owner) external view returns (uint256)',
  'function decimals() external view returns (uint8)',
]

const erc20Cache: Record<string, { decimals: number }> = {}

async function getDecimals(provider: ethers.Provider, addr: string): Promise<number> {
  if (erc20Cache[addr]) return erc20Cache[addr].decimals
  try {
    const c = new ethers.Contract(addr, ERC20_ABI, provider)
    const d = Number(await c.decimals())
    erc20Cache[addr] = { decimals: d }
    return d
  } catch { return 18 }
}

async function getBalance(provider: ethers.Provider, tokenAddr: string, owner: string): Promise<string> {
  try {
    const c = new ethers.Contract(tokenAddr, ERC20_ABI, provider)
    const bal = await c.balanceOf(owner)
    const dec = await getDecimals(provider, tokenAddr)
    return ethers.formatUnits(bal, dec)
  } catch { return '0' }
}

interface Props {
  wallet: WalletState
  prices: Record<string, number>
  config: { btc_beta_addr?: string; usdt_beta_addr?: string; eth_beta_addr?: string }
}

export function Portfolio({ wallet, prices, config }: Props) {
  const [btcBal, setBtcBal] = useState('0')
  const [usdtBal, setUsdtBal] = useState('0')
  const [ethBal, setEthBal] = useState('0')
  const [ethNative, setEthNative] = useState('0')
  const [accountPositions, setAccountPositions] = useState<{ token: string; amount: string }[]>([])

  const btcAddr = config.btc_beta_addr
  const usdtAddr = config.usdt_beta_addr
  const ethBetaAddr = config.eth_beta_addr
  const btcPrice = prices['v2:BTCUSDT'] || prices['v3:BTCUSDT'] || prices['v4:BTCUSDT'] || 100000
  const ethPrice = prices['v4:ETHUSDT'] || 3000

  useEffect(() => {
    if (!wallet.provider || !wallet.address) return
    const load = async () => {
      if (btcAddr) { const b = await getBalance(wallet.provider!, btcAddr, wallet.address!); setBtcBal(b) }
      if (usdtAddr) { const b = await getBalance(wallet.provider!, usdtAddr, wallet.address!); setUsdtBal(b) }
      if (ethBetaAddr) { const b = await getBalance(wallet.provider!, ethBetaAddr, wallet.address!); setEthBal(b) }
      try {
        const bal = await wallet.provider!.getBalance(wallet.address!)
        setEthNative(ethers.formatEther(bal).slice(0, 8))
      } catch {}
    }
    load()
    const interval = setInterval(load, 10000)
    return () => clearInterval(interval)
  }, [wallet.provider, wallet.address, btcAddr, usdtAddr, ethBetaAddr])

  useEffect(() => {
    if (!wallet.authToken) {
      setAccountPositions([])
      return
    }
    const loadAccount = async () => {
      try {
        const r = await fetch(`/api/account/positions?_t=${Date.now()}`, {
          headers: { Authorization: `Bearer ${wallet.authToken}` },
        })
        const data = await r.json()
        if (Array.isArray(data)) setAccountPositions(data)
      } catch {
        // ignore
      }
    }
    const onEvent = () => loadAccount()
    loadAccount()
    const timer = setInterval(loadAccount, 7000)
    window.addEventListener('account-data-updated', onEvent)
    return () => {
      clearInterval(timer)
      window.removeEventListener('account-data-updated', onEvent)
    }
  }, [wallet.authToken])

  if (!wallet.address) return (
    <div className="card">
      <h3>Portfolio</h3>
      <p style={{ fontSize: 13, color: 'var(--text-dim)' }}>Connect wallet to view portfolio</p>
    </div>
  )

  const btcVal = Number(btcBal) * btcPrice
  const ethVal = Number(ethBal) * ethPrice
  const usdtVal = Number(usdtBal)
  const totalVal = btcVal + ethVal + usdtVal

  return (
    <div className="card">
      <h3>Portfolio</h3>
      <div className="flex-col" style={{ gap: 4 }}>
        <div className="price-row">
          <span className="label">Address:</span>
          <span className="value" style={{ fontSize: 11 }}>
            {wallet.address.slice(0, 8)}...{wallet.address.slice(-6)}
          </span>
        </div>
        <div className="price-row">
          <span className="label">ETH:</span>
          <span className="value">{ethNative} ETH</span>
        </div>
        <div className="price-row">
          <span className="label">BTC-Beta:</span>
          <span className="value">{Number(btcBal).toFixed(6)} (${btcVal.toLocaleString()})</span>
        </div>
        <div className="price-row">
          <span className="label">USDT-Beta:</span>
          <span className="value">{Number(usdtBal).toFixed(2)} (${usdtVal.toLocaleString()})</span>
        </div>
        <div className="price-row">
          <span className="label">ETH-Beta:</span>
          <span className="value">{Number(ethBal).toFixed(4)} (${ethVal.toLocaleString()})</span>
        </div>
        <hr style={{ width: '100%', borderColor: 'var(--border)' }} />
        <div className="price-row" style={{ fontWeight: 700 }}>
          <span className="label">Total:</span>
          <span className="value">${totalVal.toLocaleString()}</span>
        </div>
        {accountPositions.length > 0 && (
          <>
            <hr style={{ width: '100%', borderColor: 'var(--border)' }} />
            <div style={{ fontSize: 12, color: 'var(--text-dim)' }}>Strategy Position Snapshots</div>
            {accountPositions.slice(0, 5).map((p) => (
              <div className="price-row" key={p.token}>
                <span className="label">{p.token}:</span>
                <span className="value">{Number(p.amount).toFixed(6)}</span>
              </div>
            ))}
          </>
        )}
      </div>
    </div>
  )
}
