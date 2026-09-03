import { createRouter, createWebHistory } from 'vue-router'
import type { LocationQueryRaw, RouteLocationRaw, RouteRecordRaw } from 'vue-router'

import type { Domain, DomainName } from '@/api/types'
import { renderDomain } from '@/api/types'
import { selectionQuery } from '@/model/filters'
import DomainDetail from '@/views/DomainDetail.vue'
import FlowDetail from '@/views/FlowDetail.vue'
import Health from '@/views/Health.vue'
import NodeDetail from '@/views/NodeDetail.vue'
import Nodes from '@/views/Nodes.vue'
import PathDetail from '@/views/PathDetail.vue'
import Paths from '@/views/Paths.vue'
import RequestDetail from '@/views/RequestDetail.vue'
import Requests from '@/views/Requests.vue'
import SessionDetail from '@/views/SessionDetail.vue'
import Topology from '@/views/Topology.vue'
import Workspace from '@/views/Workspace.vue'

/**
 * The CLI's three read verbs, each with its own altitude, plus the workspace they exist around
 * (`ui.md` §7). `status` counts the fleet and names only what is not active, fleet-wide; `get` lists
 * so a name can be found, also fleet-wide; `describe` is everything known about one thing, and that
 * is a route per noun. Do not add a fourth spelling of any of them.
 *
 * **The index routes are what this file was missing.** Six detail routes existed and every one of
 * them was reachable only by drilling from a screen that happened to mention the thing — so a fleet
 * with nothing wrong had no route to a node at all (health names only findings, and a healthy fleet
 * has none), and a path could be reached only through whichever namespace happened to route it.
 * They cost no reads: nodes, paths and requests are all already on the single fleet poll, which is
 * the same thing that made the topology free.
 *
 * **Six detail routes, not four.** `path` and `session` stay apart even though they are 1:1 in
 * practice, exactly as the CLI keeps `describe path` and `describe session` apart: a path is derived
 * state that outlives any particular session, and collapsing them would quietly assert that a path
 * dies when its workers do, which is the opposite of what the design guarantees.
 *
 * History mode, so the server's static route has to fall through to the index for client-side paths
 * — see `ui.md` §6 on serving from the binary.
 */
export const routes: RouteRecordRaw[] = [
  // Fleet health, and it is the landing page because it is the screen to open when the phone rings.
  // It was the namespace picker's null option until the index routes arrived; a page reached by
  // choosing "not a namespace" from a namespace picker was always the wrong shape for the one
  // screen that is deliberately never namespace-scoped (`views/Health.vue`).
  { path: '/', name: 'health', component: Health },

  { path: '/nodes', name: 'nodes', component: Nodes },
  { path: '/paths', name: 'paths', component: Paths },
  { path: '/requests', name: 'requests', component: Requests },

  // Fleet-wide, and no `:namespace` on purpose: a chain may cross namespaces, so a scoped topology
  // would draw two unrelated stubs where the thing worth seeing is one route through three hosts
  // (`model/topology.ts`). It takes the current namespace as a *highlight* instead
  // (`stores/current.ts`).
  { path: '/topology', name: 'topology', component: Topology },

  // The identity of a request is `(namespace, name)` and not the name alone, so its route carries
  // both — a route keyed on the name would merge two shows the moment both name a request `wall`
  // (`ui.md` §5 trap 8). Declared before the workspace so the more specific pattern is tried first,
  // though the view param's own vocabulary already keeps them apart.
  { path: '/ns/:namespace/requests/:name', name: 'request', component: RequestDetail, props: true },

  /*
   * The workspace, with **which of its two screens in the URL**.
   *
   * It was a component-local `ref`, so `/ns/nab` named the namespace and not the screen and the
   * claims view could not be linked at all. The param is optional and its absence is a real answer
   * rather than a missing one: bare `/ns/nab` means *the natural screen for this namespace*, which
   * is the mode-agnostic link to send someone when you do not know or care whether it is exclusive.
   * A redirect could not do this — routing happens before the first poll, so nothing here knows the
   * mode yet.
   */
  {
    path: '/ns/:namespace/:view(grid|claims)?',
    name: 'workspace',
    component: Workspace,
    props: true,
  },

  { path: '/nodes/:node', name: 'node', component: NodeDetail, props: true },
  { path: '/nodes/:node/domains/:domain+', name: 'domain', component: DomainDetail, props: true },
  { path: '/flows/:flow', name: 'flow', component: FlowDetail, props: true },
  { path: '/paths/:id', name: 'path', component: PathDetail, props: true },
  { path: '/sessions/:id', name: 'session', component: SessionDetail, props: true },
]

export const router = createRouter({
  history: createWebHistory(),
  routes,
})

// ---------------------------------------------------------------------------
// The fleet-wide reads
// ---------------------------------------------------------------------------

/**
 * A filter, in the one spelling `model/filters.ts` reads back.
 *
 * Every link into an index table goes through here rather than composing a query string, so a
 * landing-page count that promises `?state=FAILED` and a chip that writes it cannot drift apart —
 * and an empty filter renders as no key at all, the way an unfiltered list is spelled.
 */
export type Filter = Record<string, string | readonly string[]>

function filterQuery(filter: Filter): LocationQueryRaw {
  const selection: Record<string, readonly string[]> = {}
  for (const [key, value] of Object.entries(filter)) {
    selection[key] = typeof value === 'string' ? [value] : value
  }
  return selectionQuery(selection, '')
}

export const healthRoute = (): RouteLocationRaw => ({ name: 'health' })

export const nodesRoute = (filter: Filter = {}): RouteLocationRaw =>
  ({ name: 'nodes', query: filterQuery(filter) })

export const pathsRoute = (filter: Filter = {}): RouteLocationRaw =>
  ({ name: 'paths', query: filterQuery(filter) })

export const requestsRoute = (filter: Filter = {}): RouteLocationRaw =>
  ({ name: 'requests', query: filterQuery(filter) })

export const topologyRoute = (): RouteLocationRaw => ({ name: 'topology' })

// ---------------------------------------------------------------------------
// The workspace
// ---------------------------------------------------------------------------

/** The two screens a namespace has. `undefined` asks for whichever its mode allows. */
export type WorkspaceView = 'grid' | 'claims'

export const namespaceRoute = (namespace: string, view?: WorkspaceView): RouteLocationRaw =>
  ({ name: 'workspace', params: view ? { namespace, view } : { namespace } })

export const gridRoute = (namespace: string): RouteLocationRaw => namespaceRoute(namespace, 'grid')

export const claimsRoute = (namespace: string): RouteLocationRaw =>
  namespaceRoute(namespace, 'claims')

// ---------------------------------------------------------------------------
// The detail views
// ---------------------------------------------------------------------------

export const nodeRoute = (node: string): RouteLocationRaw => ({ name: 'node', params: { node } })

export const flowRoute = (flow: string): RouteLocationRaw => ({ name: 'flow', params: { flow } })

export const pathRoute = (id: string): RouteLocationRaw => ({ name: 'path', params: { id } })

export const sessionRoute = (id: string): RouteLocationRaw => ({ name: 'session', params: { id } })

/**
 * A domain's route, whichever spelling the caller is holding.
 *
 * Both appear in one response and the asymmetry is load-bearing (`ui.md` §5 trap 1):
 * `path.destination.domain` is structured, `path.source.domain` and `flow.domain` are rendered
 * strings. Rather than normalise either way, this takes both and puts the **segments** of the
 * rendered name in the URL, so the address bar reads `.../domains/media/cameras` — the name as an
 * operator says it.
 *
 * Note what does *not* happen at the other end: {@link DomainDetail} joins the segments back into a
 * rendered name and looks the domain up **by rendered equality** against what the node reports. It
 * never reconstructs an `{area, elements}` from them, so the object handed to the label writer is
 * always the server's own. Splitting a rendered domain into parts *to send it* is the one thing the
 * design forbids outright, and this route is deliberately built so the question never arises.
 */
export function domainRoute(node: string, domain: Domain | DomainName): RouteLocationRaw {
  const rendered = typeof domain === 'string' ? domain : renderDomain(domain)
  return { name: 'domain', params: { node, domain: rendered.split('/') } }
}

/** A request's route from the `<namespace>/<name>` id a path's refcount list carries. */
export function requestRoute(id: string): RouteLocationRaw {
  const cut = id.indexOf('/')
  // A name may not contain a slash (`naming.ts`) and neither may a namespace, so the first one is
  // the separator and there is nothing to disambiguate.
  return cut < 0
    ? { name: 'request', params: { namespace: 'default', name: id } }
    : { name: 'request', params: { namespace: id.slice(0, cut), name: id.slice(cut + 1) } }
}
