/**
 * The bearer token the browser holds, when it has to hold one.
 *
 * `ui.md` §6 left this as an open question and recommended against exactly this answer, for a reason
 * that has not gone away and is worth restating where the code lives: **auth is one shared token and
 * it also opens `/agent/v1`** (`internal/server/auth.go`, §13). Anything holding it can claim to be
 * any node, inject fabricated inventory, and read every node's RDMA rkeys. So a token in
 * `localStorage` is a fleet-wide credential sitting in a browser profile, readable by any script that
 * ever runs on this origin, and that is a real cost rather than a theoretical one.
 *
 * The recommended deployment is still a proxy (or the server) injecting `Authorization` on the way
 * through, and this module is written so that deployment never notices it exists: **nothing here
 * asks for a token until the server has refused a request with 401.** Where the header is injected
 * upstream the browser sees 200s, [authRequired] stays false, no prompt is ever shown and no token is
 * ever stored. This is the fallback for the deployment that has a token configured and no proxy to
 * inject it — previously that combination simply could not use the UI at all.
 *
 * Two consequences worth keeping in mind when changing this:
 *
 * - **The token is never sent anywhere but this origin.** `client.ts` is relative-URL only and has no
 *   API base by construction (see its header), which is what makes attaching a credential to every
 *   request safe to do unconditionally.
 * - **`localStorage`, not `sessionStorage`.** An operator watching a matrix all day reloads and opens
 *   tabs, and a credential that has to be re-pasted per tab gets pasted into a sticky note instead.
 *   It is deliberately forgettable — see the control in `App.vue` — because a shared secret on a
 *   shared workstation should have a way out that is not "clear site data".
 */

import { ref } from 'vue'

/**
 * Guarded rather than assumed: the unit suite runs in node, where there is no `localStorage`, and a
 * module that threw on import would take every test that transitively imports the client with it.
 * Same pattern, and the same reason, as `stores/current.ts`.
 */
const KEY = 'mxl.token'

function load(): string {
  try {
    return globalThis.localStorage?.getItem(KEY) ?? ''
  } catch {
    return ''
  }
}

function save(value: string): void {
  try {
    if (value) globalThis.localStorage?.setItem(KEY, value)
    else globalThis.localStorage?.removeItem(KEY)
  } catch {
    // A browser with storage denied still works; it just asks again after a reload.
  }
}

/**
 * The token, or the empty string for none. Read it; write it with [setToken], which is what persists.
 */
export const token = ref(load())

/**
 * The server has refused a request with 401 — a token is configured and what the browser is sending
 * (nothing, or the wrong thing) is not it.
 *
 * Set by the transport rather than by any store, because a mutation can be refused as easily as a
 * poll and both mean the same thing. It is the *observed* state of the credential and not a judgement
 * about whether one is held: a valid token and no token both leave it false, which is why the prompt
 * this drives never appears in a proxy-injecting deployment.
 */
export const authRequired = ref(false)

/** The header for a request, empty when no token is held. Merged by `client.ts` into every call. */
export function authHeaders(): Record<string, string> {
  return token.value ? { Authorization: `Bearer ${token.value}` } : {}
}

/**
 * Records what the server said about the credential.
 *
 * Only two things move it. A 401 sets it; a success **on an authenticated path** clears it.
 * Everything in between — a 503 from an unreachable store, a 409, a network failure — says nothing
 * about the token and must not clear a prompt the operator is in the middle of answering.
 *
 * The path matters, and getting it wrong is a flapping gate rather than a subtle bug: `/readyz` is
 * outside the middleware (`internal/server/http.go`) and answers 200 to anyone, and the fleet store
 * polls it *concurrently* with the reads. A rule of "any 2xx clears" would have that 200 undo the
 * `/v1` 401 landing beside it, twice a poll. So only `/v1` counts as evidence — a whitelist, so an
 * endpoint added outside the middleware later is silent here by default rather than wrong.
 */
export function noteStatus(path: string, status: number): void {
  if (status === 401) {
    authRequired.value = true
    return
  }
  if (status >= 200 && status < 300 && path.startsWith('/v1')) authRequired.value = false
}

/**
 * Replaces the token. It does **not** clear the refusal — a successful read does, through
 * [noteStatus].
 *
 * Which is the difference between claiming and knowing, and it is what lets the prompt report a
 * second refusal at all: clearing optimistically here would unmount the prompt on submit and mount a
 * fresh one when the retry failed, losing the fact that this was the operator's second try. It costs
 * one round trip of latency on the way in, on a screen where the operator is already waiting for the
 * answer to exactly that request.
 */
export function setToken(value: string): void {
  token.value = value.trim()
  save(token.value)
}

/**
 * Forgets the token. The next read decides what happens next — the prompt where a token really is
 * required, nothing at all where the server has no auth configured and this was a stale leftover.
 */
export function clearToken(): void {
  setToken('')
}
