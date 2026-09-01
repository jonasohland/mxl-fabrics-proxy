/**
 * The names an operator types, and the rules the server will hold them to.
 *
 * Every check here **mirrors** a server-side one and exists only so that the operator finds out
 * while typing rather than on apply. The server is the authority on all of it, and none of this is
 * a substitute for the dry run — `ui.md` §7a is explicit that a client-side reimplementation of
 * validation is the thing not to build, and that structural hints for immediate feedback are fine.
 * The line between the two is that everything below is decidable from the text in the field alone;
 * anything that needs the fleet is the server's.
 *
 * Two of them are worth naming as the mirrors they are, because a divergence would be silent:
 *
 * - **The domain element rule is `internal/api/domain.go`'s `ValidDomainName`.** It is the
 *   character-set half of the invariant that stops this API being a remote arbitrary-filesystem-write
 *   primitive: an element that passes has no separator and is not `..`, so joining it onto an area
 *   yields a direct child by construction. Copying it here is deliberate — the destination editor is
 *   the one place in this UI that turns typing into elements, and it says so where it does it.
 * - **The request-name rule is the *server's*, not the wire type's.** `api.RequestSpec.Validate`
 *   stays permissive because it is the wire contract; `internal/server/userapi.go`'s
 *   `validateRequestName` is stricter, because the name is also a URL segment and a store key. The
 *   stricter one is what an operator will actually hit, so it is the one mirrored.
 */

import { MAX_DOMAIN_ELEMENTS, MAX_DOMAIN_NAME_LEN, MAX_DOMAIN_PATH_LEN } from '@/api/types'
import type { Domain } from '@/api/types'
import { renderDomain } from '@/api/types'

/** 253, the DNS-subdomain limit Kubernetes object names use, as the server does. */
export const MAX_REQUEST_NAME_LEN = 253

/**
 * One element of a domain — `internal/api/domain.go`'s `ValidDomainName`.
 *
 * A leading dot is a hidden directory, invisible to an operator listing the area and skipped by
 * discovery, so a domain named that way could never be observed and its path could never reach
 * `ACTIVE`. A leading dash is a name that reads as a flag in every tool it will ever be handed to.
 */
export function elementError(element: string): string | undefined {
  if (element === '') return 'an empty name'
  if (element.length > MAX_DOMAIN_NAME_LEN) return `"${element}" is longer than ${MAX_DOMAIN_NAME_LEN} bytes`
  if (element === '.' || element === '..') {
    return `"${element}" names an existing directory rather than a new one`
  }
  if (element.startsWith('.') || element.startsWith('-')) {
    return `"${element}" must not begin with "${element[0]}"`
  }
  if (!/^[A-Za-z0-9._-]+$/.test(element)) {
    return `"${element}": letters, digits, "-", "_" and "." only`
  }
  return undefined
}

/**
 * Split what the operator typed into elements.
 *
 * **This is the UI acting as a parser, and it is bounded to the one place that is allowed.** The
 * rule the design turns on is that nothing outside the CLI's manifest parser turns a domain *name*
 * into an area and elements — a field that accepted `fast/studio-a/cam1` and split off the area
 * would make the UI a second implementation of the identity grammar. The area here is never typed:
 * it comes from a picker over what the node advertises and grants `write` on. What is left is a
 * multi-element entry convenience over a field whose every part is validated against the mirror
 * above, which `ui.md` §3 permits explicitly.
 *
 * Empty segments are dropped rather than refused, so a trailing or doubled `/` while typing is not
 * an error message that appears and disappears under the operator's hands.
 */
export function parseElements(text: string): string[] {
  return text.split('/').map((part) => part.trim()).filter((part) => part.length > 0)
}

/** The whole element list: at least one, at most eight, each a plain name. */
export function elementsError(elements: string[]): string | undefined {
  if (elements.length === 0) return 'a domain name is required'
  if (elements.length > MAX_DOMAIN_ELEMENTS) {
    return `${elements.length} elements, at most ${MAX_DOMAIN_ELEMENTS}`
  }
  for (const element of elements) {
    const bad = elementError(element)
    if (bad) return bad
  }
  return undefined
}

/**
 * The whole domain, area included.
 *
 * The 255-byte cap is on the **rendered** name and therefore counts the area segment, which is why
 * it is checked here rather than beside the element list: measuring only the elements would let the
 * cap loosen silently now that the area is part of the name.
 */
export function domainError(domain: Domain): string | undefined {
  if (domain.area === '') return 'no area'
  const area = elementError(domain.area)
  if (area) return `area ${area}`
  const elements = elementsError(domain.elements)
  if (elements) return elements
  const rendered = renderDomain(domain)
  if (rendered.length > MAX_DOMAIN_PATH_LEN) {
    return `"${rendered}" is ${rendered.length} bytes long, at most ${MAX_DOMAIN_PATH_LEN}`
  }
  return undefined
}

/**
 * A request name — `internal/server/userapi.go`'s `validateRequestName`.
 *
 * The colon is in the set because operators reach for it when they name a request after a source
 * and a destination. It is half the request's ID and its idempotency key, so it is not an
 * implementation detail: names end up in the manifest and in `delete` commands.
 */
export function requestNameError(name: string): string | undefined {
  if (name === '') return 'a name is required'
  if (name.length > MAX_REQUEST_NAME_LEN) return `longer than ${MAX_REQUEST_NAME_LEN} characters`
  if (!/^[A-Za-z0-9._:-]+$/.test(name)) {
    return `"${name}": only letters, digits and the characters - _ . :`
  }
  if (name.startsWith('.')) return `"${name}" must not begin with a dot`
  return undefined
}

/**
 * A name suggestion out of something the operator already chose — a group hint, a domain element.
 *
 * Suggested rather than imposed: `ui.md` §7a asks for a name as soon as there is something to name
 * one after, kept editable, because the operator owns it.
 */
export function slug(text: string): string {
  return text
    .toLowerCase()
    .replace(/[^a-z0-9]+/g, '-')
    .replace(/^-+|-+$/g, '')
    .slice(0, 40)
}
