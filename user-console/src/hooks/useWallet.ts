import { useState, useEffect, useCallback } from 'react'
import { ethers } from 'ethers'

const SEPOLIA_CHAIN_ID = '0xaa36a7'
const API_BASE = '/api'

export interface WalletState {
  address: string | null
  provider: ethers.BrowserProvider | null
  chainId: string | null
  balance: string | null
  authToken: string | null
  role: string | null
  connect: () => Promise<void>
  disconnect: () => void
}

export function useWallet(): WalletState {
  const [address, setAddress] = useState<string | null>(null)
  const [provider, setProvider] = useState<ethers.BrowserProvider | null>(null)
  const [chainId, setChainId] = useState<string | null>(null)
  const [balance, setBalance] = useState<string | null>(null)
  const [authToken, setAuthToken] = useState<string | null>(localStorage.getItem('authToken'))
  const [role, setRole] = useState<string | null>(localStorage.getItem('authRole'))

  const updateBalance = useCallback(async (prov: ethers.BrowserProvider, addr: string) => {
    try {
      const bal = await prov.getBalance(addr)
      setBalance(ethers.formatEther(bal).slice(0, 8))
    } catch {
      setBalance(null)
    }
  }, [])

  const connect = useCallback(async () => {
    if (!window.ethereum) {
      alert('Please install MetaMask!')
      return
    }
    try {
      const prov = new ethers.BrowserProvider(window.ethereum)
      const accounts = await prov.send('eth_requestAccounts', [])
      const addr = accounts[0]
      const network = await prov.getNetwork()
      const chain = '0x' + network.chainId.toString(16)
      setAddress(addr)
      setProvider(prov)
      setChainId(chain)
      try {
        const challengeResp = await fetch(`${API_BASE}/auth/challenge`, {
          method: 'POST',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({ address: addr }),
        })
        const challenge = await challengeResp.json()
        if (challenge?.message && challenge?.nonce) {
          const signer = await prov.getSigner()
          const signature = await signer.signMessage(challenge.message)
          const verifyResp = await fetch(`${API_BASE}/auth/verify`, {
            method: 'POST',
            headers: { 'Content-Type': 'application/json' },
            body: JSON.stringify({ address: addr, nonce: challenge.nonce, signature }),
          })
          const verify = await verifyResp.json()
          if (verify?.token) {
            setAuthToken(verify.token)
            setRole(verify.role || 'viewer')
            localStorage.setItem('authToken', verify.token)
            localStorage.setItem('authRole', verify.role || 'viewer')
          }
        }
      } catch (err) {
        console.warn('Auth handshake failed:', err)
      }

      if (chain !== SEPOLIA_CHAIN_ID) {
        try {
          await window.ethereum.request({
            method: 'wallet_switchEthereumChain',
            params: [{ chainId: SEPOLIA_CHAIN_ID }],
          })
        } catch {
          await window.ethereum.request({
            method: 'wallet_addEthereumChain',
            params: [{
              chainId: SEPOLIA_CHAIN_ID,
              chainName: 'Sepolia Testnet',
              rpcUrls: ['https://sepolia.infura.io/v3/'],
              nativeCurrency: { name: 'Sepolia ETH', symbol: 'ETH', decimals: 18 },
            }],
          })
        }
      }
      await updateBalance(prov, addr)
    } catch (e: any) {
      console.error('Connect failed:', e)
    }
  }, [updateBalance])

  const disconnect = useCallback(() => {
    setAddress(null)
    setProvider(null)
    setChainId(null)
    setBalance(null)
    setAuthToken(null)
    setRole(null)
    localStorage.removeItem('authToken')
    localStorage.removeItem('authRole')
  }, [])

  useEffect(() => {
    const eth = window.ethereum
    if (eth) {
      const handleAccountsChanged = (accounts: string[]) => {
        if (accounts.length === 0) disconnect()
        else if (provider) {
          setAddress(accounts[0])
          updateBalance(provider, accounts[0])
        }
      }
      const handleChainChanged = () => window.location.reload()
      eth.on('accountsChanged', handleAccountsChanged)
      eth.on('chainChanged', handleChainChanged)
      return () => {
        eth.removeListener('accountsChanged', handleAccountsChanged)
        eth.removeListener('chainChanged', handleChainChanged)
      }
    }
  }, [provider, disconnect, updateBalance])

  return { address, provider, chainId, balance, authToken, role, connect, disconnect }
}


