/**
 * The user API's wire types, mirroring `internal/api/`.
 *
 * Ground truth is the Go package, not this file — it is a translation, and where the two disagree
 * the Go is right. What TypeScript adds is the part prose cannot enforce: the two tagged unions are
 * unions here rather than structs with optional fields, so "two kinds set" is a compile error
 * instead of a 400 (`ui.md` §3). Ignoring a selector kind would silently *widen* what gets
 * replicated, which is the wrong direction to fail in for something that moves uncompressed video.
 *
 * Only the **user** API is modelled. `/agent/v1` is the privileged surface and the browser is given
 * no route to it (`ui.md` §2, §6); there is deliberately nothing here to call it with.
 */

// ---------------------------------------------------------------------------
// Domains
// ---------------------------------------------------------------------------

/**
 * A domain's fleet-wide identity: an area, and elements relative to it (`internal/api/domain.go`).
 *
 * **This is the form that may be sent.** A destination domain is always an area the operator
 * granted `write` on plus validated elements, never a path — which is what stops this API being a
 * remote arbitrary-filesystem-write primitive on every node in the fleet (`ui.md` §3, architecture
 * §13). It holds regardless of what authentication is configured.
 */
export interface Domain {
  area: string
  elements: string[]
}

/**
 * A domain rendered as `<area>/<elements>` — what identifies a domain that already exists.
 *
 * Branded, so it cannot be passed where a {@link Domain} is wanted. The rule it enforces is
 * `ui.md` §3 trap 1 and §10.6's "parsed at exactly one boundary": the manifest parser is the only
 * thing in the system that turns a domain string into an area and elements, and a UI text box that
 * split one would make the UI the second parser. Rendering is one-way here by construction —
 * {@link renderDomain} exists and its inverse deliberately does not.
 */
export type DomainName = string & { readonly __domainName: unique symbol }

export const DOMAIN_SEPARATOR = '/'

/** Per architecture §10.6. Enforced server-side; mirrored for feedback while typing. */
export const MAX_DOMAIN_ELEMENTS = 8
export const MAX_DOMAIN_NAME_LEN = 64
export const MAX_DOMAIN_PATH_LEN = 255

export function renderDomain(domain: Domain): DomainName {
  const elements = domain.elements ?? []
  const rendered = elements.length === 0
    ? domain.area
    : domain.area + DOMAIN_SEPARATOR + elements.join(DOMAIN_SEPARATOR)
  return rendered as DomainName
}

export function domainEquals(a: Domain, b: Domain): boolean {
  const left = a.elements ?? []
  const right = b.elements ?? []
  return a.area === b.area &&
    left.length === right.length &&
    left.every((element, i) => element === right[i])
}

/**
 * Whether `inner` lies inside `outer` — `fast/a` inside `fast/a/b`.
 *
 * Within one area only: two domains in different areas are two directory trees and cannot nest,
 * whatever their elements look like. An element-wise prefix test, which is what the structured form
 * buys — the string spelling of this question has to work around `studio-ab` looking like a child
 * of `studio-a`. Nesting is the one destination-name collision that survives
 * (`domain_name_in_use`); `fast/ingest` and `bulk/ingest` are simply two domains.
 */
export function domainNestedIn(inner: Domain, outer: Domain): boolean {
  const innerElements = inner.elements ?? []
  const outerElements = outer.elements ?? []
  if (inner.area !== outer.area || outerElements.length >= innerElements.length) return false
  return outerElements.every((element, i) => element === innerElements[i])
}

// ---------------------------------------------------------------------------
// Selectors — the two tagged unions
// ---------------------------------------------------------------------------

/** The NMOS group hint as observed on a flow. */
export interface GroupHint {
  name: string
  type: string
}

/**
 * `type` omitted selects every flow sharing the name — which is how a camera's video and audio
 * travel together, and the selector operators actually want.
 */
export interface GroupHintSelector {
  name: string
  type?: string
}

/**
 * Which source flows a request replicates. **Exactly one kind** (`internal/api/selector.go`).
 *
 * `{all: true}` is a kind like any other and an absent `select` is an error on the wire: making the
 * zero value mean "everything" is precisely what the union exists to prevent. Note there are two
 * spellings of "everything" and the difference is worth stating in the interface — `all` takes the
 * whole domain, a group hint with no `type` takes one group of it.
 */
export type Selector =
  | { flow: string; group_hint?: never; all?: never }
  | { group_hint: GroupHintSelector; flow?: never; all?: never }
  | { all: true; flow?: never; group_hint?: never }

export type SelectorKind = 'flow' | 'group_hint' | 'all'

export function selectorKind(selector: Selector): SelectorKind {
  if (selector.flow !== undefined) return 'flow'
  if (selector.group_hint !== undefined) return 'group_hint'
  return 'all'
}

/**
 * Which of a node's domains a request replicates from. **Exactly one kind**
 * (`internal/api/domainselector.go`).
 *
 * `name` is a structured {@link Domain}, not the rendered string — sending `"media/cameras"` is a
 * decode error. `labels` matches every domain on that node carrying all of those keys with exactly
 * those values: equality, ANDed, and never empty, because an empty map would match every domain on
 * the node by omission rather than by intent.
 */
export type DomainSelector =
  | { name: Domain; labels?: never }
  | { labels: Record<string, string>; name?: never }

export type DomainSelectorKind = 'name' | 'labels'

export function domainSelectorKind(selector: DomainSelector): DomainSelectorKind {
  return selector.name !== undefined ? 'name' : 'labels'
}

// ---------------------------------------------------------------------------
// States
// ---------------------------------------------------------------------------

/** The seven states **one thing** can be in. A path is never `PARTIAL` and never `DISABLED`. */
export type PathState =
  | 'WAITING'
  | 'INVALID'
  | 'ESTABLISHING'
  | 'PAUSED'
  | 'ACTIVE'
  | 'DEGRADED'
  | 'FAILED'

/**
 * The two aggregate-only states (architecture §11).
 *
 * `PARTIAL` describes disagreement among many things; `DISABLED` describes a spec rather than a
 * fleet, and is *derived* — there is no `disabled` field on a request to read. Both may be shown on
 * a request row and must never be expected underneath one.
 */
export type AggregateState = 'PARTIAL' | 'DISABLED'

export type RequestState = PathState | AggregateState

/**
 * Wire order — the order a path moves through, matching `api.States()`.
 *
 * Iterate this rather than the keys of a counts map: `status.counts` omits zeros, so a request with
 * one establishing path returns `{"ESTABLISHING": 1}` and nothing else, and a chart built from the
 * keys shows a gap where it should show a floor.
 */
export const PATH_STATES: readonly PathState[] = [
  'WAITING', 'INVALID', 'ESTABLISHING', 'PAUSED', 'ACTIVE', 'DEGRADED', 'FAILED',
]

/** `api.RequestStates()` — the seven plus the two aggregates. */
export const REQUEST_STATES: readonly RequestState[] = [...PATH_STATES, 'PARTIAL', 'DISABLED']

export type WorkerState = 'starting' | 'ready' | 'failed'

// ---------------------------------------------------------------------------
// Reasons and errors
// ---------------------------------------------------------------------------

/**
 * Stable machine-readable reasons (`internal/api/wire.go`). Switch on these; render `reason`.
 *
 * The three negotiation failures are three *different operator problems*, which is why they are
 * three codes rather than one — a UI that matched on English could not tell them apart.
 */
export type ReasonCode =
  | 'unknown_area'
  | 'area_not_writable'
  | 'malformed_domain_name'
  | 'domain_name_in_use'
  | 'same_endpoint'
  | 'duplicate_source_flow'
  | 'node_not_registered'
  | 'no_shared_fabric'
  | 'no_shared_provider'
  | 'no_shared_capability'
  | 'pin_not_viable'
  | 'sched_prio_unavailable'
  | 'flow_conflict'
  | 'loop'
  | 'namespace_overlap'
  | 'flow_not_found'
  | 'agent_not_leased'
  | 'source_idle'
  | 'all_destinations_disabled'
  | 'worker_restarts'
  | 'fabric_gone'

export type ErrorCode =
  | 'invalid_request'
  | 'unauthorized'
  | 'not_found'
  | 'node_claimed'
  | 'not_ready'
  | 'reregister'
  | 'version_skew'
  | 'internal'

/**
 * An error body.
 *
 * Note `reason_code` lives under `details` here, while on a {@link Request} it is
 * `status.reason_code`. Two shapes for the same information, and both are load-bearing: the prose
 * `message` is better than anything the UI would write and should be rendered verbatim, while the
 * code decides only what to highlight.
 */
export interface ApiErrorBody {
  code: ErrorCode
  message: string
  details?: Record<string, string>
}

/** Outcome of a create-or-update. The status code cannot tell you this — an unchanged apply is a 200. */
export type Outcome = 'created' | 'updated' | 'unchanged'

export const HEADER_OUTCOME = 'X-Mxl-Outcome'

// ---------------------------------------------------------------------------
// Capabilities and nodes
// ---------------------------------------------------------------------------

export type Provider = 'tcp' | 'verbs' | 'efa' | 'shm'
export type CapFlag = 'REMOTE_WRITE' | 'SEND_RECEIVE' | 'BLOCKING_OPERATIONS'

/**
 * A genuine `uint64` on the wire, and providers report `UINT64_MAX`.
 *
 * `JSON.parse` turns `18446744073709551615` into `18446744073709552000`, so the client quotes
 * anything too long for a double before parsing and the value arrives as a string. Use
 * {@link asBigInt} rather than reading it as a number (`ui.md` §5 trap 2).
 */
export type Uint64 = number | string

export function asBigInt(value: Uint64 | undefined): bigint | undefined {
  if (value === undefined) return undefined
  try {
    return BigInt(value)
  } catch {
    return undefined
  }
}

export const UINT64_MAX = 18446744073709551615n

export interface InterfaceConfig {
  provider: Provider
  caps_flags: CapFlag[]
  max_message_size: Uint64
}

export interface FabricAttachment {
  provider: Provider
  fabric: string
  address: string
  caps_flags: CapFlag[]
  max_message_size: Uint64
  device?: string
}

export interface Versions {
  protocol: number
  replicator: string
  proxy?: string
  mxl?: string
  libfabric?: string
}

/**
 * A directory an operator designated as somewhere MXL domains live, with two independent grants.
 *
 * Neither implies the other, and both default false: a node with no readable area offers no sources
 * and a node with no writable area accepts no destinations. These grants are the whole of this
 * project's authority over that node's filesystem (architecture §10.6, §13).
 *
 * `path` is advertised for diagnostics only and **may be absent** — guard it. It is worth showing
 * under a destination name as the operator types, which for an otherwise abstract name is the
 * strongest affordance available.
 */
export interface Area {
  name: string
  path?: string
  read: boolean
  write: boolean
}

export interface Capabilities {
  fabrics: FabricAttachment[]
  versions: Versions
  sched_prio: boolean
  port_range?: string
  areas?: Area[]
}

export interface Node {
  name: string
  /**
   * The liveness lease, and the *only* health signal. Registration is durable and survives the
   * agent being down; a node registered but not leased is information, not an alarm.
   */
  live: boolean
  instance?: string
  /** Absent, not null, not epoch — `omitzero`. Guard every timestamp. */
  registered_at?: string
  /**
   * When the lease was **taken**, not the last heartbeat — a heartbeat renews the TTL and
   * deliberately writes nothing, so a healthy node can show this an hour ago. Never render it as
   * staleness or drive a health indicator from it (`ui.md` §5 trap 3).
   */
  last_seen?: string
  capabilities: Capabilities
}

export interface NodeList {
  nodes: Node[]
}

/** Areas this node grants writing on — the only ones that can be a destination. */
export function writableAreas(node: Node): Area[] {
  return (node.capabilities?.areas ?? []).filter((area) => area.write)
}

// ---------------------------------------------------------------------------
// Requests
// ---------------------------------------------------------------------------

/**
 * Scalar, array, or absent — and it round-trips in the form it was written.
 *
 * A pin is honoured or the request fails; it is **never substituted**. Do not build an affordance
 * that reads as "fall back automatically" — landing on tcp when verbs was asked for is a
 * performance cliff whose symptom looks like a source problem.
 */
export type ProviderPin = Provider | Provider[]

export interface Source {
  /** Pinned, not selected — only the domain and the flows are selected. */
  node: string
  domain: DomainSelector
  select: Selector
}

export interface Destination {
  node: string
  domain: Domain
  /** Overrides the request-level pin for this destination alone, rather than intersecting with it. */
  provider?: ProviderPin
  /**
   * Parks this leg: the entry stays in the spec and expands to nothing. The model's only spelling
   * of *off*.
   *
   * **Absent when false.** Anything that decodes a poll *over* the previous response rather than
   * replacing it leaves a stale `true` in place and shows a leg as parked forever after it came
   * back (`ui.md` §5 trap 15). Replace, never merge — and when un-parking, delete the key rather
   * than writing `disabled: false`, or the spec is not byte-identical to one that was never parked
   * and an apply writes to the store for nothing.
   */
  disabled?: boolean
}

export interface RequestSpec {
  /** A real property, not a label. Empty reads as `default`. */
  namespace?: string
  name: string
  /** Always a list, with no singular `source:` beside it. At least one. */
  sources: Source[]
  /** At least one. Entries, not *enabled* entries — a fully parked request is legal. */
  destinations: Destination[]
  provider?: ProviderPin
  idle_teardown_ms?: number
  sched_prio?: number
  labels?: Record<string, string>
}

/** The (node, domain, flow) triple. The domain component is mandatory — a flow ID is not a location. */
export interface FlowAddress {
  node: string
  domain: DomainName
  flow: string
}

export interface PathStatus {
  /** 32 hex characters. Truncating for display is fine; matching is exact. */
  id: string
  source: FlowAddress
  destination: Destination
  state: PathState
  reason?: string
  reason_code?: ReasonCode
  session_id?: string
}

/**
 * One source of a request, folded over the paths that source produced.
 *
 * The breakdown to lead with when a request has several sources: "studio B is dark, studio A is
 * fine" is the answer an operator needs from a fan-in, and it has no meaning in a one-source model.
 * `paths` carries IDs to join against {@link RequestStatus.paths}, not a second copy of each status.
 */
export interface SourceStatus {
  source: Source
  state: RequestState
  reason?: string
  reason_code?: ReasonCode
  counts?: Partial<Record<RequestState, number>>
  paths?: string[]
}

/** One flow the expansion deliberately left out. There is one reason today. */
export interface Exclusion {
  node: string
  domain: DomainName
  flow: string
  reason: 'self_output'
}

export interface RequestStatus {
  state: RequestState
  reason?: string
  reason_code?: ReasonCode
  /** Omits zeros — iterate {@link REQUEST_STATES} with a floor of 0. */
  counts?: Partial<Record<RequestState, number>>
  sources?: SourceStatus[]
  paths: PathStatus[]
  excluded?: Exclusion[]
  /** Non-zero means the cap discarded entries. Render it — a silent cap reads as "nothing else". */
  excluded_dropped?: number
}

export interface Request extends RequestSpec {
  /** The rendered `<namespace>/<name>` pair — the joinable spelling a path's refcount list carries. */
  id: string
  created_at: string
  updated_at?: string
  status: RequestStatus
}

export interface RequestList {
  requests: Request[]
}

// ---------------------------------------------------------------------------
// Paths and sessions
// ---------------------------------------------------------------------------

export interface SessionEndpoint {
  node: string
  state: WorkerState
  address?: string
  service?: string
  restarts: number
  started_at?: string
  reason?: string
  reason_code?: ReasonCode
}

/**
 * The concrete worker pair realising a path. Ephemeral, and a separate layer on purpose — a path
 * outlives any particular session, so the two are kept apart here as the CLI keeps `describe path`
 * and `describe session` apart.
 *
 * `target` and `initiator` are each absent until reported: a session in `ESTABLISHING` legitimately
 * has a fabric and an interface config and no endpoints at all. The user API never discloses
 * `target_info` — a set of RDMA rkeys that lives only on the agent API.
 */
export interface Session {
  id: string
  epoch?: string
  fabric: string
  interface: InterfaceConfig
  target?: SessionEndpoint
  initiator?: SessionEndpoint
}

/**
 * The deduplicated edge, refcounted.
 *
 * `requests[]` **is** the refcount and it is the only statement of ownership in the API. A request's
 * own `status.paths[]` is not: the loser of a namespace overlap lists the contested path with the
 * incumbent's state, so a request carrying nothing can show `{"ACTIVE": 1}` (`ui.md` §5 trap 14).
 * Anything computing a cell state or a cancellation preview must cross-check against this.
 */
export interface Path {
  id: string
  source: FlowAddress
  destination: Destination
  state: PathState
  reason?: string
  reason_code?: ReasonCode
  /** Rendered `<namespace>/<name>`. Length > 1 means another request keeps this leg alive. */
  requests: string[]
  /** Absent on a path in `WAITING`. */
  session?: Session
}

export interface PathsResponse {
  /**
   * The server has not yet run its first reconcile — it just started, or an HA leader changed. Said
   * explicitly rather than reporting everything as `WAITING`, precisely so a restart does not look
   * like a fleet-wide outage. Absent when false.
   */
  settling?: boolean
  paths: Path[]
}

// ---------------------------------------------------------------------------
// Flows and domains
// ---------------------------------------------------------------------------

export interface FlowInventory {
  id: string
  /**
   * Verbatim NMOS content, including fields nothing in this tree models. Display it, pretty-print
   * it, but do not decode-and-re-encode it into anything that goes back over the wire: the session
   * identity hashes these bytes, so a re-serialisation that reordered keys would read as a different
   * flow and rebuild a healthy session.
   */
  flow_def: unknown
  group_hint?: GroupHint
  /**
   * True exactly while one of that node's own target workers is writing this flow — a live fact,
   * not a stored one. It is why a label selector skipped something (`self_output`), and rendering
   * it is what makes an otherwise undiagnosable omission legible.
   */
  replicated?: boolean
  producing: boolean
}

/** One entry per `(node, domain, flow)`. The same UUID on two nodes is success, not duplication. */
export interface FlowEntry extends FlowInventory {
  node: string
  domain: DomainName
}

export interface FlowList {
  flows: FlowEntry[]
}

/**
 * An observed domain joined against its label record.
 *
 * `observed` tells the two apart: a labelled-but-unobserved domain is how an operator sees a label
 * they applied before the producer came up, and it is information rather than an error.
 */
export interface DomainInfo {
  domain: Domain
  observed: boolean
  labels?: Record<string, string>
  flows?: FlowInventory[]
}

export interface DomainList {
  node: string
  /** Set while the label join would render every label with nothing observed beside it. */
  settling?: boolean
  domains: DomainInfo[]
}

export interface DomainLabels {
  node: string
  domain: Domain
  labels?: Record<string, string>
  /** The key set the last `apply` declared and therefore owns. */
  declared?: string[]
}

export interface DomainLabelPatch {
  set?: Record<string, string>
  remove?: string[]
}

/**
 * A label write. **`patch` is what an interactive UI wants**, not `apply`.
 *
 * An apply owns the keys it declares — it removes the ones it declared last time and no longer
 * does — so a read-modify-write with `apply` would silently adopt, then later delete, keys someone
 * else's manifest declared. A patch sets and removes exactly what it names and merges against
 * nothing.
 */
export type DomainLabelWrite =
  | { domain: Domain; apply: Record<string, string>; patch?: never }
  | { domain: Domain; patch: DomainLabelPatch; apply?: never }

/** The resulting record plus its blast radius, as full {@link Path} objects. */
export interface DomainLabelResult extends DomainLabels {
  stopped?: Path[]
  started?: Path[]
}

// ---------------------------------------------------------------------------
// Namespaces
// ---------------------------------------------------------------------------

/**
 * Whether requests inside this namespace may share a path.
 *
 * The API's default is `shared` and `default` is auto-created that way, so `exclusive` is an active
 * choice on every create path. The matrix is an editor only over an `exclusive` namespace; a
 * `shared` one gets the ledger (`ui.md` §7b, §7c).
 */
export type PathPolicy = 'shared' | 'exclusive'

export const DEFAULT_NAMESPACE = 'default'

export interface Namespace {
  name: string
  paths?: PathPolicy
  description?: string
}

export interface NamespaceInfo extends Namespace {
  /** What makes a refused DELETE legible before the operator tries one. */
  requests: number
}

export interface NamespaceList {
  namespaces: NamespaceInfo[]
}

// ---------------------------------------------------------------------------
// Health
// ---------------------------------------------------------------------------

/** `GET /readyz`. The leader name is the only place the API exposes which replica is reconciling. */
export interface Readyz {
  status: 'ok'
  leader: string
}
