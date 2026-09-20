/// <reference types="vitest/config" />
import react from '@vitejs/plugin-react'
import { defineConfig } from 'vite'

// Stage 5 Back Office. The dev-server proxy below exists purely so a
// browser running the Vite dev server (origin http://localhost:5173) can
// call the Platform API (a different origin/port) without needing the
// backend to grow CORS headers just for local development - it forwards
// any request under /v1 to the real API. Production hosting of this app
// is a deployment-time decision (same-origin reverse proxy, or CORS
// headers added to the API) that is out of scope for this stage; see the
// Stage 5 completion report.
const apiProxyTarget = process.env.VITE_API_PROXY_TARGET ?? 'http://localhost:8080'

export default defineConfig({
  plugins: [react()],
  server: {
    proxy: {
      '/v1': {
        target: apiProxyTarget,
        changeOrigin: true,
      },
    },
  },
  test: {
    environment: 'jsdom',
    globals: true,
    setupFiles: ['./src/test/setup.ts'],
    css: true,
  },
})
