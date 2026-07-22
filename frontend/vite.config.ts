import { defineConfig } from 'vite'
import react from '@vitejs/plugin-react'
import tailwindcss from '@tailwindcss/vite'

// https://vite.dev/config/
export default defineConfig({
  plugins: [react(), tailwindcss()],
  server: {
    port: 5173,
    // Proxy the API through the dev server so the browser sees a single
    // origin. That keeps the session cookie same-origin (SameSite=Strict is
    // honoured without special-casing) and sidesteps CORS entirely in dev.
    proxy: {
      '/api': { target: 'http://localhost:8080', changeOrigin: false },
      '/healthz': { target: 'http://localhost:8080', changeOrigin: false },
    },
  },
})
