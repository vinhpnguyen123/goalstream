import { defineConfig } from 'vite'
import react from '@vitejs/plugin-react'
import tailwindcss from '@tailwindcss/vite'

// The browser only ever talks to its own origin: /api/* is proxied here in dev and rewritten
// by Vercel in production, prefix untouched (docs/API.md § Base path).
export default defineConfig({
  plugins: [react(), tailwindcss()],
  server: {
    proxy: { '/api': 'http://127.0.0.1:8080' },
  },
})
