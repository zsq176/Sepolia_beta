/// <reference types="vite/client" />

interface ImportMetaEnv {
  readonly VITE_BTC_BETA_ADDR: string
  readonly VITE_USDT_BETA_ADDR: string
  readonly VITE_ETH_BETA_ADDR: string
  readonly VITE_V2_PAIR: string
  readonly VITE_V3_POOL: string
  readonly VITE_V4_POOL_MANAGER: string
  readonly VITE_RPC_URL: string
}

interface ImportMeta {
  readonly env: ImportMetaEnv
}

interface Window {
  ethereum?: {
    on: (event: string, handler: (...args: any[]) => void) => void
    removeListener: (event: string, handler: (...args: any[]) => void) => void
    request: (args: any) => Promise<any>
    isMetaMask?: boolean
  }
}
