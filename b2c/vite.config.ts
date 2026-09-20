/// <reference types="vitest/config" />
import react from '@vitejs/plugin-react'
import { defineConfig } from 'vite'

// Stage 6 B2C brand frontend. Mirrors the Back Office's dev-server proxy
// pattern (backoffice/vite.config.ts) exactly: a browser hitting the Vite
// dev server (http://localhost:5174) can call the Platform API on a
// different origin/port with no CORS involved, forwarding any request
// under /v1 to the real API. Production hosting (same-origin reverse
// proxy vs. CORS headers on the API) is a deployment-time decision out of
// scope for this stage.
const apiProxyTarget = process.env.VITE_API_PROXY_TARGET ?? 'http://localhost:8080'

export default defineConfig({
  plugins: [react()],
  server: {
    port: 5174,
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
