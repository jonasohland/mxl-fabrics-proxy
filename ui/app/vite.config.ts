import { fileURLToPath, URL } from 'node:url'

import vue from '@vitejs/plugin-vue'
import { defineConfig } from 'vite'

// The API this dev server proxies to. `ui.md` §9 uses 12999 deliberately: 2283 is what a real
// mxl-replicator listens on, and pointing a dev harness at a fleet somebody is running registers
// fake nodes into it — durable, with no deregister API.
const API = process.env.API ?? '127.0.0.1:12999'

// Everything the server exposes that the UI is allowed to reach. `/agent/v1` is deliberately
// absent and must stay absent: anything that can call it can claim to be a node, inject fabricated
// inventory and read other nodes' RDMA rkeys (`ui.md` §2, §6). The browser is given no route to it.
const PROXIED = ['/v1', '/healthz', '/readyz', '/metrics']

// The UI is **always same-origin with the API** (`ui.md` §6). In production it is served by the
// server binary or behind a proxy fronting both; in development that origin is faked here, so that
// both deployments speak the same relative URLs and no API-base setting ever exists to be
// configured. The server sends no `Access-Control-*` headers and returns 405 to a preflight, by
// design — this proxy is the supported answer, and adding CORS middleware for development's sake
// is how the deployment shape stops being true.
export default defineConfig({
  plugins: [vue()],
  resolve: {
    alias: { '@': fileURLToPath(new URL('./src', import.meta.url)) },
  },
  server: {
    proxy: Object.fromEntries(
      PROXIED.map((prefix) => [prefix, { target: `http://${API}`, changeOrigin: false }]),
    ),
  },
  build: {
    // Consumed by `go:embed` in the server binary. Kept inside the app directory so the embed
    // directive has a stable relative path and a stale build is visible in `git status`.
    outDir: 'dist',
    emptyOutDir: true,
  },
})
