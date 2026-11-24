import { defineConfig } from 'vite'
import react from '@vitejs/plugin-react'

// The built assets are embedded into the Go binary from internal/web/dist.
export default defineConfig({
  plugins: [react()],
  build: {
    outDir: '../internal/web/dist',
    emptyOutDir: true,
  },
  server: {
    port: 5173,
    proxy: {
      '/api': 'http://127.0.0.1:8080',
    },
  },
})
