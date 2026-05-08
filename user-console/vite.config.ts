import { defineConfig } from 'vite'
import react from '@vitejs/plugin-react'

export default defineConfig({
  plugins: [react()],
  server: {
    port: 3000,
    proxy: {
      '/api': 'http://localhost:8080',
      '/ws': {
        target: 'ws://localhost:8080',
        ws: true,
      },
      // Browser -> Vite -> Sepolia RPC (avoid CORS in frontend code)
      '/rpc': {
        target: 'https://rpc.sepolia.org',
        changeOrigin: true,
        secure: true,
        rewrite: (path) => path.replace(/^\/rpc/, ''),
      },
    },
  },
})
