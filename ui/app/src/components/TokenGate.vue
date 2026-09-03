<script setup lang="ts">
/**
 * What the operator sees when the server has answered 401.
 *
 * It replaces the view rather than sitting above it (`App.vue`), because there is nothing to sit
 * above: every user-API read is refused by the same middleware, so the alternative is a banner over
 * an empty fleet — which reads as "the fleet is empty" with an explanation nobody joins up.
 *
 * **It is shown by the refusal, never by the absence of a token.** A deployment whose proxy injects
 * the header never reaches this component, which is the property that keeps the recommended shape in
 * `ui.md` §6 untouched by the fallback existing.
 *
 * It asks, and that is all it does. The security reasoning behind it is real (`api/auth.ts`) and
 * belongs in these comments and in `ui.md` — not on the screen, where an operator holding a token in
 * a paste buffer has one thing to do and is not choosing whether to do it.
 */
import type { Directive } from 'vue'
import { ref } from 'vue'

import { setToken, token } from '@/api/auth'
import { useFleetStore } from '@/stores/fleet'

const fleet = useFleetStore()

/**
 * A local draft, seeded from whatever is already held: reaching this screen with a token in hand
 * means that token was refused, and the likely repair is an edit to it — a truncated paste, the
 * wrong fleet's secret — rather than a fresh one typed from nothing.
 */
const draft = ref(token.value)

/** In flight. The submit is one request and the operator is watching for its answer. */
const checking = ref(false)

/**
 * A token was submitted from here and the screen is still up, which can only mean the read that
 * followed was refused too — the component survives a failed attempt precisely so this can be said
 * (`api/auth.ts`, [setToken]). Distinguishing the second attempt from the first is the whole of the
 * feedback available: the server will not say whether it got nothing or got the wrong thing.
 */
const refused = ref(false)

async function submit() {
  if (draft.value.trim() === '' || checking.value) return
  setToken(draft.value)
  checking.value = true
  try {
    // Immediately, rather than waiting out the poll interval. On success the refusal clears from
    // under this component and `App.vue` swaps it for the view mid-call, which is why nothing here
    // reads as "signed in" — there is no such state, only reads that work.
    await fleet.refresh()
  } finally {
    checking.value = false
    refused.value = true
  }
}

/**
 * Local, and one line, rather than an `onMounted` reaching for a template ref: the element is the
 * only thing being focused and the directive keeps that where the reader is already looking.
 */
const vFocus: Directive<HTMLInputElement> = { mounted: (el) => el.focus() }
</script>

<template>
  <main class="gate">
    <form @submit.prevent="submit">
      <h1>Authentication required</h1>

      <!-- Autofocused because it is the only control on the screen and the operator arrived here
           holding a token in a paste buffer. -->
      <input
        v-model="draft"
        v-focus
        type="password"
        class="field mono"
        autocomplete="off"
        spellcheck="false"
        placeholder="bearer token"
        aria-label="Bearer token"
      />

      <div class="row">
        <button type="submit" class="act go" :disabled="draft.trim() === '' || checking">continue</button>
        <span v-if="checking" class="note">checking…</span>
        <span v-else-if="refused" class="note bad">refused</span>
      </div>
    </form>
  </main>
</template>

<style scoped>
.gate {
  flex: 1;
  display: flex;
  align-items: flex-start;
  justify-content: center;
  padding: 12vh 16px 16px;
}

form {
  width: min(460px, 100%);
  display: flex;
  flex-direction: column;
  gap: 10px;
  padding: 16px;
  background: var(--bg-raised);
  border: 1px solid var(--line);
  border-radius: 4px;
}

h1 {
  margin: 0;
  font-size: 13px;
  font-weight: 600;
}

.field {
  background: var(--bg-sunken);
  color: var(--fg);
  border: 1px solid var(--line);
  border-radius: 3px;
  padding: 5px 7px;
  font: inherit;
  font-family: ui-monospace, SFMono-Regular, 'SF Mono', Menlo, monospace;
}

.field:focus { outline: none; border-color: var(--accent); }

.row { display: flex; align-items: baseline; gap: 10px; }

/* The same button idiom as `PendingBar.vue`'s — one control shape, wherever it appears. */
.act {
  background: none;
  border: 1px solid var(--line);
  border-radius: 3px;
  color: var(--fg-dim);
  cursor: pointer;
  font: inherit;
  font-size: 12px;
  padding: 2px 12px;
}

.act:hover:not(:disabled) { color: var(--fg); border-color: var(--fg-dim); }
.act:disabled { color: var(--fg-faint); border-color: var(--line-soft); cursor: not-allowed; }

.go { border-color: var(--accent); color: var(--accent); }
.go:hover:not(:disabled) { background: var(--accent); color: var(--bg); border-color: var(--accent); }

.note { font-size: 12px; color: var(--fg-dim); }
.note.bad { color: var(--s-failed); }
</style>
