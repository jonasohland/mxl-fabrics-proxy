import { createPinia } from 'pinia'
import { createApp } from 'vue'

import App from './App.vue'
import { router } from './router'
import './styles/base.css'
// Global on purpose, and prefixed `ed-` for exactly that reason — the two editors share one idiom
// and a `<style scoped>` cannot be shared. See the file's own note.
import './styles/editor.css'
// Global for the same reason and prefixed `dt-`: six detail views share one shape.
import './styles/detail.css'
// And `ls-`: three index views share one table.
import './styles/list.css'

createApp(App).use(createPinia()).use(router).mount('#app')
