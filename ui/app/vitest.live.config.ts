import { fileURLToPath, URL } from 'node:url'

import vue from '@vitejs/plugin-vue'
import { defineConfig } from 'vitest/config'

/**
 * The integration suite: real components, real client, real server.
 *
 * Separate from `vitest.config.ts` so that `npm test` stays hermetic and a control plane that is
 * not running never looks like a failing unit test.
 */
export default defineConfig({
  plugins: [vue()],
  resolve: {
    alias: { '@': fileURLToPath(new URL('./src', import.meta.url)) },
  },
  test: {
    environment: 'node',
    include: ['src/**/*.live.ts'],
    // One control plane, one store: parallel files would race each other's fixtures.
    fileParallelism: false,
    testTimeout: 20000,
    hookTimeout: 30000,
  },
})
