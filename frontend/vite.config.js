import { defineConfig } from 'vite'
import react from '@vitejs/plugin-react'
import tailwindcss from '@tailwindcss/vite'
import { readFileSync } from 'node:fs'

// Keep one documentation index in the repository and serve that same text.
const documentationIndex = () => readFileSync(new URL('../llms.txt', import.meta.url), 'utf8')
const documentationPlugin = {
  name: 'parallax-documentation-index',
  configureServer(server) {
    server.middlewares.use((req, res, next) => {
      if (req.url?.split('?')[0] !== '/llms.txt') return next()
      res.setHeader('Content-Type', 'text/plain; charset=utf-8')
      res.end(documentationIndex())
    })
  },
  generateBundle() {
    this.emitFile({ type: 'asset', fileName: 'llms.txt', source: documentationIndex() })
  },
}

export default defineConfig({
  plugins: [react(), tailwindcss(), documentationPlugin],
  server: {
    proxy: {
      '/api': 'http://localhost:8080',
      '/ws': { target: 'ws://localhost:8080', ws: true },
    },
  },
})
