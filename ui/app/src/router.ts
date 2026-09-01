import { createRouter, createWebHistory } from 'vue-router'
import type { RouteLocationRaw, RouteRecordRaw } from 'vue-router'

import type { Domain, DomainName } from '@/api/types'
import { renderDomain } from '@/api/types'
import Landing from '@/views/Landing.vue'
import Topology from '@/views/Topology.vue'
import Workspace from '@/views/Workspace.vue'
import DomainDetail from '@/views/DomainDetail.vue'
import FlowDetail from '@/views/FlowDetail.vue'
import NodeDetail from '@/views/NodeDetail.vue'
import PathDetail from '@/views/PathDetail.vue'
import RequestDetail from '@/views/RequestDetail.vue'
import SessionDetail from '@/views/SessionDetail.vue'

/**
 * Three altitudes, and the split is `ui.md` §7's: `status` counts the fleet and names only what is
 * not active, and that is fleet-wide; the workspace is one namespace at a time; `describe` is
 * everything known about one thing, and that is a route per noun.
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
  { path: '/', name: 'landing', component: Landing },

  // Fleet-wide, and no `:namespace` on purpose: a chain may cross namespaces, so a scoped topology
  // would draw two unrelated stubs where the thing worth seeing is one route through three hosts
  // (`model/topology.ts`). It takes a namespace as a *highlight* inside the view instead.
  { path: '/topology', name: 'topology', component: Topology },

  { path: '/ns/:namespace', name: 'workspace', component: Workspace, props: true },

  // The identity of a request is `(namespace, name)` and not the name alone, so its route carries
  // both — a route keyed on the name would merge two shows the moment both name a request `wall`
  // (`ui.md` §5 trap 8).
  { path: '/ns/:namespace/requests/:name', name: 'request', component: RequestDetail, props: true },

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

export const topologyRoute = (): RouteLocationRaw => ({ name: 'topology' })

export const nodeRoute = (node: string): RouteLocationRaw => ({ name: 'node', params: { node } })

export const flowRoute = (flow: string): RouteLocationRaw => ({ name: 'flow', params: { flow } })

export const pathRoute = (id: string): RouteLocationRaw => ({ name: 'path', params: { id } })

export const sessionRoute = (id: string): RouteLocationRaw => ({ name: 'session', params: { id } })

export const namespaceRoute = (namespace: string): RouteLocationRaw =>
  ({ name: 'workspace', params: { namespace } })

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
