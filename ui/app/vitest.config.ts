import { fileURLToPath, URL } from 'node:url'

import vue from '@vitejs/plugin-vue'
import { defineConfig } from 'vitest/config'

export default defineConfig({
  plugins: [vue()],
  resolve: {
    alias: { '@': fileURLToPath(new URL('./src', import.meta.url)) },
  },
  test: {
    environment: 'node',
    // `*.test.ts` is hermetic and runs anywhere. `*.live.ts` drives the real components against a
    // running control plane and is a separate script, so a failing fixture never looks like a
    // failing unit.
    include: ['src/**/*.test.ts'],
  },
})
