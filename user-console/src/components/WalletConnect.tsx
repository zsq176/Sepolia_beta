import { WalletState } from '../hooks/useWallet'

export function WalletConnect({ wallet }: { wallet: WalletState }) {
  const isCorrectChain = wallet.chainId === '0xaa36a7'

  if (!wallet.address) {
    return <button className="btn btn-primary" onClick={wallet.connect}>Connect MetaMask</button>
  }

  return (
    <div className="flex-row" style={{ alignItems: 'center', gap: 12 }}>
      {!isCorrectChain && (
        <span className="badge badge-red" style={{ fontSize: 11 }}>
          Wrong Network - Switch to Sepolia
        </span>
      )}
      <span style={{ fontSize: 12, color: 'var(--text-dim)' }}>
        {wallet.balance ? `${wallet.balance} ETH` : ''}
      </span>
      <span style={{ fontSize: 13, fontFamily: 'monospace' }}>
        {wallet.address.slice(0, 6)}...{wallet.address.slice(-4)}
      </span>
      <button className="btn btn-sm" onClick={wallet.disconnect}>Disconnect</button>
    </div>
  )
}
