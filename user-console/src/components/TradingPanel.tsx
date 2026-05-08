import { useState, useEffect } from 'react'
import { ethers } from 'ethers'
import { WalletState } from '../hooks/useWallet'

const V2_ROUTER = '0xeE567Fe1712Faf6149d80dA1E6934E354124CfE3'

const ERC20_ABI = [
  'function approve(address spender, uint256 amount) external returns (bool)',
  'function allowance(address owner, address spender) external view returns (uint256)',
  'function decimals() external view returns (uint8)',
  'function balanceOf(address owner) external view returns (uint256)',
]

const V2_ROUTER_ABI = [
  'function swapExactTokensForTokens(uint amountIn, uint amountOutMin, address[] calldata path, address to, uint deadline) external returns (uint[] memory amounts)',
  'function getAmountsOut(uint amountIn, address[] calldata path) external view returns (uint[] memory amounts)',
]

interface Props {
  wallet: WalletState
  prices: Record<string, number>
  config: { btc_beta_addr?: string; usdt_beta_addr?: string; v2_pair?: string }
}

export function TradingPanel({ wallet, prices, config }: Props) {
  const [amount, setAmount] = useState('')
  const [direction, setDirection] = useState<'buy' | 'sell'>('buy')
  const [loading, setLoading] = useState(false)
  const [txHash, setTxHash] = useState('')
  const [error, setError] = useState('')
  const [estimatedOut, setEstimatedOut] = useState('')
  const [slippageBps, setSlippageBps] = useState(100) // default 1%
  const [maxGasGwei, setMaxGasGwei] = useState(50)
  const [estimatedGasUsd, setEstimatedGasUsd] = useState<number | null>(null)

  const btcAddr = config.btc_beta_addr || import.meta.env.VITE_BTC_BETA_ADDR
  const usdtAddr = config.usdt_beta_addr || import.meta.env.VITE_USDT_BETA_ADDR
  const btcPrice = prices['v2:BTCUSDT'] || prices['v3:BTCUSDT'] || prices['v4:BTCUSDT'] || 100000
  const ethPrice = prices['v4:ETHUSDT'] || 2300

  const logOp = async (payload: { operation: string; target: string; detail: string; status: string; tx_hash?: string }) => {
    if (!wallet.authToken) return
    try {
      await fetch('/api/account/op-logs', {
        method: 'POST',
        headers: {
          'Content-Type': 'application/json',
          Authorization: `Bearer ${wallet.authToken}`,
        },
        body: JSON.stringify(payload),
      })
    } catch {
      // best-effort audit log
    }
  }

  useEffect(() => {
    if (!wallet.provider || !amount || !btcAddr || !usdtAddr) return
    const fetchEstimate = async () => {
      try {
        const router = new ethers.Contract(V2_ROUTER, V2_ROUTER_ABI, wallet.provider!)
        const btc = new ethers.Contract(btcAddr, ERC20_ABI, wallet.provider!)
        const usdt = new ethers.Contract(usdtAddr, ERC20_ABI, wallet.provider!)
        const btcDec = Number(await btc.decimals())
        const usdtDec = Number(await usdt.decimals())

        const amountIn = direction === 'buy'
          ? ethers.parseUnits(amount, usdtDec)
          : ethers.parseUnits(amount, btcDec)

        if (amountIn <= 0n) return
        const path = direction === 'buy' ? [usdtAddr, btcAddr] : [btcAddr, usdtAddr]
        const amounts = await router.getAmountsOut(amountIn, path)
        const outDec = direction === 'buy' ? btcDec : usdtDec
        const out = ethers.formatUnits(amounts[amounts.length - 1], outDec)
        setEstimatedOut(Number(out).toFixed(6))

        const feeData = await wallet.provider!.getFeeData()
        const gas = 230000n
        const gasPrice = feeData.maxFeePerGas ?? feeData.gasPrice ?? 0n
        const usd = Number(ethers.formatEther(gasPrice * gas)) * ethPrice
        setEstimatedGasUsd(Number.isFinite(usd) ? usd : null)
      } catch { setEstimatedOut('') }
    }
    const timer = setTimeout(fetchEstimate, 300)
    return () => clearTimeout(timer)
  }, [amount, direction, wallet.provider, btcAddr, usdtAddr, ethPrice])

  const handleTrade = async () => {
    if (!wallet.provider || !wallet.address) {
      setError('Wallet not connected')
      logOp({ operation: 'manual_trade_click', target: 'V2:BTC-Beta/USDT-Beta', detail: 'wallet not connected', status: 'failed' })
      return
    }
    if (!btcAddr || !usdtAddr) {
      setError('Token addresses not configured')
      logOp({ operation: 'manual_trade_click', target: 'V2:BTC-Beta/USDT-Beta', detail: 'token addresses missing', status: 'failed' })
      return
    }
    setError('')
    setLoading(true)
    setTxHash('')

    try {
      const signer = await wallet.provider.getSigner()
      const btc = new ethers.Contract(btcAddr, ERC20_ABI, signer)
      const usdt = new ethers.Contract(usdtAddr, ERC20_ABI, signer)
      const router = new ethers.Contract(V2_ROUTER, V2_ROUTER_ABI, signer)

      const btcDec = Number(await btc.decimals())
      const usdtDec = Number(await usdt.decimals())

      const tokenIn = direction === 'buy' ? usdt : btc
      const amountIn = direction === 'buy'
        ? ethers.parseUnits(amount, usdtDec)
        : ethers.parseUnits(amount, btcDec)

      const path = direction === 'buy' ? [usdtAddr, btcAddr] : [btcAddr, usdtAddr]
      const amounts = await router.getAmountsOut(amountIn, path)
      const amountOutMin = amounts[amounts.length - 1] * BigInt(10000 - slippageBps) / 10000n
      const deadline = Math.floor(Date.now() / 1000) + 1200

      const feeData = await wallet.provider.getFeeData()
      const maxFeePerGas = feeData.maxFeePerGas ?? feeData.gasPrice ?? ethers.parseUnits('1', 'gwei')
      const cap = ethers.parseUnits(String(maxGasGwei), 'gwei')
      if (maxFeePerGas > cap) {
        throw new Error(`Gas too high: ${ethers.formatUnits(maxFeePerGas, 'gwei')} gwei > cap ${maxGasGwei}`)
      }

      // Approve tokenIn
      const allowance = await tokenIn.allowance(wallet.address, V2_ROUTER)
      if (allowance < amountIn) {
        const approveTx = await tokenIn.approve(V2_ROUTER, amountIn)
        await approveTx.wait()
      }

      const tx = await router.swapExactTokensForTokens(
        amountIn, amountOutMin, path, wallet.address, deadline,
        { gasLimit: 300000 }
      )
      const receipt = await tx.wait()
      setTxHash(receipt.hash)

      // Record trade to backend
      const outDec = direction === 'buy' ? btcDec : usdtDec
      await fetch(`/api/trades`, {
        method: 'POST',
        headers: {
          'Content-Type': 'application/json',
          ...(wallet.authToken ? { Authorization: `Bearer ${wallet.authToken}` } : {}),
        },
        body: JSON.stringify({
          pair: 'V2:BTC-Beta/USDT-Beta',
          action: direction.toUpperCase(),
          amount: amount,
          price: btcPrice,
          tx_hash: receipt.hash,
        }),
      })
      await logOp({
        operation: 'manual_trade_submit',
        target: 'V2:BTC-Beta/USDT-Beta',
        detail: `${direction.toUpperCase()} amount=${amount} slippageBps=${slippageBps}`,
        status: 'confirmed',
        tx_hash: receipt.hash,
      })
      window.dispatchEvent(new Event('account-data-updated'))
    } catch (e: any) {
      const msg = e.reason || e.message?.slice(0, 100) || 'Trade failed'
      setError(msg)
      await logOp({
        operation: 'manual_trade_submit',
        target: 'V2:BTC-Beta/USDT-Beta',
        detail: `${direction.toUpperCase()} amount=${amount} error=${msg}`,
        status: 'failed',
      })
    } finally {
      setLoading(false)
    }
  }

  return (
    <div className="card">
      <h3>V2 BTC-Beta/USDT-Beta Trading</h3>
      <div className="flex-col" style={{ gap: 10 }}>
        <div className="flex-row">
          <button
            className={`btn ${direction === 'buy' ? 'btn-green' : ''}`}
            onClick={() => setDirection('buy')}
          >Buy</button>
          <button
            className={`btn ${direction === 'sell' ? 'btn-red' : ''}`}
            onClick={() => setDirection('sell')}
          >Sell</button>
        </div>

        <div className="input-group">
          <input
            type="number"
            placeholder={direction === 'buy' ? 'USDT-Beta amount' : 'BTC-Beta amount'}
            value={amount}
            onChange={(e) => setAmount(e.target.value)}
            step="0.0001"
          />
        </div>

        {estimatedOut && (
          <div className="price-row">
            <span className="label">Est. Output:</span>
            <span className="value">{estimatedOut} {direction === 'buy' ? 'BTC-Beta' : 'USDT-Beta'}</span>
          </div>
        )}

        <div className="price-row">
          <span className="label">Slippage:</span>
          <span className="value">{(slippageBps / 100).toFixed(2)}%</span>
        </div>
        <input
          type="range"
          min={20}
          max={500}
          step={5}
          value={slippageBps}
          onChange={(e) => setSlippageBps(Number(e.target.value))}
        />

        <div className="price-row">
          <span className="label">Max Gas Cap:</span>
          <span className="value">{maxGasGwei} gwei</span>
        </div>
        <input
          type="range"
          min={5}
          max={150}
          step={1}
          value={maxGasGwei}
          onChange={(e) => setMaxGasGwei(Number(e.target.value))}
        />

        {estimatedGasUsd !== null && (
          <div className="price-row">
            <span className="label">Est. Gas Cost:</span>
            <span className="value">${estimatedGasUsd.toFixed(3)}</span>
          </div>
        )}

        <div className="price-row">
          <span className="label">Price:</span>
          <span className="value">${btcPrice.toLocaleString()} / BTC-Beta</span>
        </div>

        <button
          className="btn btn-primary"
          onClick={handleTrade}
          disabled={!wallet.address || loading || !amount}
        >
          {loading ? 'Processing...' : `${direction.toUpperCase()} BTC-Beta`}
        </button>

        {txHash && (
          <div style={{ fontSize: 11, wordBreak: 'break-all', color: 'var(--green)' }}>
            Tx: <a href={`https://sepolia.etherscan.io/tx/${txHash}`} target="_blank" style={{ color: 'var(--blue)' }}>{txHash.slice(0, 24)}...</a>
          </div>
        )}
        {error && <div style={{ fontSize: 12, color: 'var(--red)' }}>{error}</div>}
      </div>
    </div>
  )
}
