import { defineConfig } from 'vite'
import react from '@vitejs/plugin-react'

export default defineConfig({
  plugins: [react()],
  build: { outDir: '../internal/node/web', emptyOutDir: true },
  server: {
    host: '127.0.0.1',
    proxy: {
      '/v1': {
        target: 'http://127.0.0.1:7091',
        changeOrigin: true,
        configure(proxy) {
          proxy.on('proxyReq', (proxyReq, req) => {
            if (req.headers.origin) proxyReq.setHeader('Origin', 'http://127.0.0.1:7091')
          })
        },
      },
    },
  },
})
