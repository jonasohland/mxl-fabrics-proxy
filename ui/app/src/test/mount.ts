import { mount } from '@vue/test-utils'
import { createPinia } from 'pinia'
import { createRouter, createWebHistory } from 'vue-router'

import App from '@/App.vue'
import { routes } from '@/router'
import { until } from './live'

/**
 * Mount the whole application at a route and wait for the first good poll.
 *
 * A fresh router per mount rather than the singleton from `@/router`: several test files navigate
 * independently, and a shared history would leak one file's location into the next. The **routes**
 * are the application's own, so a view that is unreachable in the product is unreachable here too —
 * a copied table would let a test drive a screen no operator can get to.
 */
export async function mountAt(path: string) {
  const router = createRouter({ history: createWebHistory(), routes })

  await router.push(path)
  await router.isReady()

  const wrapper = mount(App, { global: { plugins: [createPinia(), router] } })

  await until(() => !wrapper.text().includes('Loading…'), { timeoutMs: 15000 })
  await wrapper.vm.$nextTick()

  return wrapper
}
