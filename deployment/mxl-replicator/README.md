# mxl-replicator Helm chart

Installs the control-plane **server** (a Deployment) and a per-node **agent** (a DaemonSet).

This chart provisions the fleet. It does **not** decide what gets replicated: that is requested
through the API with `mxl-replicator apply -f <manifest>`, which is the whole point of the design.
What lives here is the part that changes when a host is built — which nodes participate, what
filesystem authority they grant, and what they can be reached on.

```bash
helm install mxl-replicator ./deployment/mxl-replicator \
    --namespace mxl --create-namespace \
    --set image.tag=v0.3.0 \
    --set server.persistence.node=<node> \
    --set-json 'agent.fabrics=[{"provider":"tcp","fabric":"dc1-data","interface":"eth1"}]'
```

Then label the nodes that should run an agent:

```bash
kubectl label node <node> mxl.ebu.org/mxl-replicator=true
```

`helm test mxl-replicator` runs `mxl-replicator status` against the deployed server, which
exercises the Service, the token and the user API in one call.

## What you have to decide

Three things have no sensible default, and the chart will not guess at any of them.

| | |
|---|---|
| **`server.persistence.node`** | The store is a directory on one node's disk. The chart refuses to render without it. |
| **`agent.fabrics`** | What each node can be reached on. Get this wrong and nothing replicates between hosts; leave it empty and the agent assumes `shm`, which only ever pairs with itself. |
| **`agent.areas`** | The whole of this project's authority over a node's filesystem. |

Everything else has a default that works.

## Storage

`server.persistence.type` picks how the sqlite store is held. The default is the shape asked for
by a single control-plane node: a directory on one node, no storage class, no provisioner, no CSI
driver.

| `type` | What it makes | Notes |
|---|---|---|
| `hostPath` *(default)* | A directory on `persistence.node`, created if missing. | No PV and no PVC — the pod names the directory. Least friction. |
| `local` | A `local` PersistentVolume with node affinity, bound to a PVC by `claimRef`. | The directory **must already exist**; the local volume plugin will not create it. The better object once someone has provisioned the mount. |
| `existingClaim` | Nothing. Uses `persistence.existingClaim`. | `node` is unused. |
| `storageClass` | A PVC against a provisioner. | |
| `emptyDir` | Nothing durable. | **The request set does not survive a restart.** Development only. |

For `hostPath` and `local` the server pod is pinned to `persistence.node` with a nodeSelector, and
it has to be: the data is on that node's disk. Draining that node takes the control plane down
until it comes back — which does not stop running media, because agents are fail-static and act on
the last assignment set they retrieved, but it does stop reconciliation. Move to the etcd backend
when that matters.

The `local` PV's reclaim policy is `Retain`, so `helm uninstall` removes the PV and the claim and
leaves the directory. A reinstall binds straight back to it.

The image runs as a non-root user and a directory kubelet just created is owned by root, so a
short root init container takes ownership first — hostPath volumes get no `fsGroup` treatment from
Kubernetes, which is why this is an init container rather than a pod securityContext. Turn it off
with `server.persistence.initChown.enabled=false` if the directory is already owned correctly.

### HA

```yaml
server:
  replicas: 3
  store:
    backend: etcd
    etcd:
      endpoints: [http://etcd-0:2379, http://etcd-1:2379, http://etcd-2:2379]
```

Every replica then serves the API and one of them is elected reconciler leader. No PVC is created
and no sticky sessions are required in front of it. The chart does not install etcd.

## Fabrics

Nodes declare `(provider, fabric, selector)` triples, not bare provider names. `fabric` is an
operator-assigned opaque label, and **two nodes may pair on a provider only if they share it**.

```yaml
agent:
  fabrics:
    - {provider: verbs, fabric: ib-fabric-a, device: mlx5_0, ip_version: 4}
    - {provider: tcp,   fabric: dc1-data,    network: 10.1.0.0/16}
```

Provider availability is not reachability. Two nodes both offering `verbs` may be on different
InfiniBand fabrics; two both offering `efa` may be in different VPCs. Intersecting provider *names*
would assign a session that cannot connect, and it fails invisibly — the target comes up clean and
the initiator's connect loop spins.

Selectors come in two classes. A **naming** selector — `address`, `interface`, `device`, or none
when the node has exactly one of that provider — says which interface, at most one per attachment.
**Narrowing** selectors — `network` and `ip_version` — say which of its addresses counts, and
compose with a name and with each other. The agent resolves the lot at startup by running the
worker's `--interfaces` probe and asking libfabric what the node actually has; exactly one probe
entry must survive, and zero or several is a loud startup error, not a silent drop.

Which naming selector to use depends on the provider. `device` is the **libfabric** device name —
`mlx5_0` for a Mellanox HCA, `rdmap0s6-rdm` for an EFA adapter — and `interface` is the netdev,
which exists for tcp and verbs and **not for efa**: an `efa` attachment carrying an `interface` is
refused at startup, because the probe has no netdev name to match it against.

A name alone is often ambiguous — an HCA with both an IPv4 and a link-local IPv6 address reports
two entries under one device name:

```
dropping a configured fabric attachment  reason="device: mlx5_0 matches 2 verbs interfaces"
```

`address` resolves that and is exact and always unique, but it is per-node, and **every value in
this chart is fleet-wide**: pinning one costs a `pools:` entry or a per-node overlay to state a
fact ("we are on v4") that is true of the whole fleet. `device` plus `ip_version: 4` states it once
in the shared list. `network: 10.1.0.0/16` goes further and names no hardware at all — it picks
each node's own address inside a prefix, which stays one value even where the netdev is `eth1` on
some nodes and `ens5f0` on the rest, and it is usually the right selector for tcp. Neither says
anything about reachability; two nodes inside one prefix may still have no route between them, and
that is what the `fabric` label decides.

Areas and fabrics go into a ConfigMap rather than onto the command line — they are lists of
records, which is exactly what does not fit on one — and the DaemonSet carries a checksum of it, so
changing either rolls the agents. Everything else the agent takes stays a flag.

## EFA

```yaml
image:
  tag: v0.3.0                     # the agent gets v0.3.0-efa
agent:
  efa:
    enabled: true
  nodeSelector:
    node.kubernetes.io/instance-type: c5n.18xlarge
  fabrics:
    - {provider: efa, fabric: vpc1-subnet-a, device: rdmap0s6-rdm}
```

`efa.enabled` does two things: it requests the extended resource
`vpc.amazonaws.com/efa: 1` (in both requests and limits, as Kubernetes requires) and it switches
the agent's image tag to the `-efa` variant, which is built on the EFA-enabled libfabric base. The
server keeps the stock image — it never touches a fabric.

Four things it does **not** do, because each is a claim only an operator can make:

- **Install the device plugin.** `aws-efa-k8s-device-plugin` must already be running on the
  cluster. Requesting the resource is what attaches the adapter to the pod; without the request the
  device is on the node and invisible in the container.
- **Pick the nodes.** Narrow `agent.nodeSelector` to instance types that have an adapter, or the
  DaemonSet will schedule pods that stay `Pending` on the resource.
- **Pick the fabric label.** EFA does not route, so nodes in different subnets must not share one.
  Two nodes sharing a label is an assertion that they can reach each other.
- **Set the count.** `efa.count` is 1. Instances with several adapters want the real number, and
  the number is per-node hardware rather than something the chart can see.

`agent.devices.infiniband` stays on: an EFA adapter presents as `/dev/infiniband/uverbs*`.

The container is privileged by default, which is what gives the RDMA providers their unlimited
locked memory. Dropping to `IPC_LOCK` + `SYS_RESOURCE` works on some clusters and is worth trying,
but it is not what this is tested with.

## Why the agent runs as root

`agent.runAsRoot` is on, and on a cluster with the runtime's default `RLIMIT_MEMLOCK` it is not
really optional.

The image's own user is `mxl` (uid 1000). **A container that starts as a non-root uid holds an
empty *effective* capability set whatever `privileged` says** — the capabilities are in the
bounding set and do nothing. So CAP_IPC_LOCK is not held, `RLIMIT_MEMLOCK` stays at the runtime
default (8 KiB on containerd), and the worker dies registering the first flow:

```
Failed to set up target: Failed to register memory region: Cannot allocate memory, code -12
```

A 1080p v210 ring is about 60 MiB, so this hits immediately and on every flow. What it looks like
from the outside is a session that establishes, restarts three times and reports `DEGRADED` — which
reads as a fabric problem rather than a uid one. `kubectl exec` into the agent and check
`grep CapEff /proc/self/status`: all zeroes means this.

Kubernetes has no ulimit field and no GA ambient-capability support, so raising the limit for a
non-root uid is a node-level containerd change. Make it and you can set `runAsRoot: false`.

One consequence worth knowing: domains the agent materialises are then owned by root. Consumers
read them, so 0755 is enough — but a media function that expects to *write* into a replicated
domain will not be able to.

## nodeSelector and Helm's map merge

`agent.nodeSelector` defaults to `{mxl.ebu.org/mxl-replicator: "true"}`, and Helm **deep-merges
maps**: setting your own key *adds* to that default rather than replacing it. So

```yaml
agent:
  nodeSelector: {kubernetes.io/hostname: node-0}
```

produces a DaemonSet requiring *both* labels, and if the nodes are not also carrying
`mxl.ebu.org/mxl-replicator` it schedules nowhere and reports `0 desired` — which looks like
nothing happened rather than like a selector mistake. Either label the nodes, which is the
intended flow, or drop the default explicitly:

```yaml
agent:
  nodeSelector:
    mxl.ebu.org/mxl-replicator: null
    kubernetes.io/hostname: node-0
```

## Mixed fleets

A cluster whose nodes are not alike — an EFA pool beside a verbs pool — uses `agent.pools`. Each
entry is merged over everything in `agent`, so a pool states only what differs, and **a non-empty
`pools` replaces the single DaemonSet rather than adding to it**: a default pool left running
beside these would put a second agent on the nodes they select, and two agents claiming one node
name is a real failure mode. Their node selectors must not overlap; the chart cannot check that and
the server will reject the second claimant.

```yaml
agent:
  pools:
    - name: efa
      nodeSelector: {mxl.ebu.org/fabric: efa}
      efa: {enabled: true}
      fabrics: [{provider: efa, fabric: vpc1-subnet-a, device: rdmap0s6-rdm}]
    - name: verbs
      nodeSelector: {mxl.ebu.org/fabric: verbs}
      fabrics: [{provider: verbs, fabric: ib-fabric-a, device: mlx5_0, ip_version: 4}]
```

Lists replace rather than append, so a pool declaring `fabrics` declares all of them.

See [`examples/`](examples/) for complete values files.

## Areas

An **area** is a directory this node has designated as somewhere MXL domains live, with two
independent grants: `read` lets this project discover and observe domains under it, `write` lets
replication create them. Neither implies the other, both default false, and an area granting
neither is refused at startup.

A domain inside an area is addressed fleet-wide as `<area>/<elements>` — a directory
`/dev/shm/mxl/ingest` under the `fast` area is `fast/ingest`, and that is its identity for life.
Areas may nest; the innermost containing one names a directory.

**Every area path must be inside an `agent.hostMounts` entry** or the container cannot see it. The
chart checks this and refuses to render otherwise: the alternative presents as a node that
discovers no domains and accepts no destinations, a long way from the line that caused it.

```yaml
agent:
  hostMounts:
    - {name: dev-shm, hostPath: /dev/shm,   mountPath: /dev/shm,   type: Directory}
    - {name: nvme,    hostPath: /mnt/nvme,  mountPath: /mnt/nvme,  type: Directory}
  areas:
    - {name: media, path: /dev/shm/mxl0,    read: true}
    - {name: fast,  path: /dev/shm/mxl,     read: true, write: true}
    - {name: bulk,  path: /mnt/nvme/mxl,    read: true, write: true}
```

## Auth

A single shared bearer token, read from `MXL_REPLICATOR_AUTH_TOKEN` by both roles. On by default.

The chart generates one on first install and reads it back on upgrade, so `helm upgrade` does not
rotate it out from under a running fleet. `helm template` has no cluster to read and emits a fresh
token on every render — do not pipe `helm template` into `kubectl apply` for an existing release.
Set `auth.existingSecret` to manage it from a real secret store, or `auth.token` to pin a literal.

```bash
kubectl -n mxl get secret mxl-replicator-token -o jsonpath='{.data.token}' | base64 -d
```

`/healthz`, `/readyz` and `/metrics` are unauthenticated by design — they are for a load balancer,
a kubelet and a scrape job, none of which carry a token — so the ServiceMonitor and PodMonitor need
no credentials.

## Upgrades

**The server rolls first.** It must tolerate agents a version or more behind, agents may assume the
server is at least as new, and the server refuses an agent newer than itself. Helm has no ordering
between a Deployment and a DaemonSet, so on a version bump that matters:

```bash
helm upgrade mxl-replicator ./deployment/mxl-replicator --set image.tag=vNEXT --set agent.enabled=false
kubectl -n mxl rollout status deploy/mxl-replicator-server
helm upgrade mxl-replicator ./deployment/mxl-replicator --set image.tag=vNEXT
```

The agent DaemonSet rolls one node at a time (`maxUnavailable: 1`). **An agent restart glitches
every flow on that node** — accepted, and made short rather than avoided. A server restart does
not: the settling window and deterministic session IDs mean the new process adopts running sessions
rather than re-establishing them.

## Observability

Prometheus metrics on the server's `:2283/metrics` and each agent's `:2284/metrics`. Both pods
carry `prometheus.io/scrape` annotations by default (`metrics.annotations`).

For prometheus-operator, `metrics.serviceMonitor.enabled` covers the server and
`metrics.podMonitor.enabled` covers every agent pool. The agents have no Service, deliberately —
hostNetwork with a hostPort is how a node's metrics are reachable — so they are scraped as pods.

## Values

See [`values.yaml`](values.yaml). Every key is commented there; this file covers only the ones with
a decision behind them.
