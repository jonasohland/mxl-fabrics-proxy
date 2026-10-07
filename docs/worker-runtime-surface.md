# `mxl-replicator-worker` — runtime surface

This is a reference for anyone writing a new supervisor or replication manager that drives the C++
worker. It describes the contract as implemented in `src/`, and how the current supervisor (the
Go agent under `internal/`, mainly `internal/worker/exec` and `internal/agent`) drives the worker.
Source references are `file:line`.

The Go agent under `internal/` has replaced the legacy Go tree (`legacy/go/`). The legacy tree is
still in the repository for reference, and this document cites it where its behaviour explains a
current decision. The worker binary was kept. This document defines the boundary between the
worker and its supervisor.

---

## 1. What the worker is

One process handles **one flow, in one direction, with one peer, in one role**.

- It only moves data. It has no discovery, no control plane, no HTTP, no signalling, no
  dynamic reconfiguration and no multi-flow support.
- All configuration comes from a JSON file passed as `argv[1]` and read once at startup
  (`src/main.cpp:208`). To change anything, kill the process and start a new one.
- It runs until it receives a signal or hits a fatal error. It expects to be supervised and
  restarted.

The `target` boolean in the config selects one of two roles:

| Role | `target` | Reads from | Writes to | Direction |
|---|---|---|---|---|
| **Initiator** (sender) | `false` | local MXL flow (by `flow_id`) | RDMA to remote target | egress |
| **Target** (receiver) | `true` | RDMA from remote initiator | local MXL flow (created from `flow_def`) | ingress |

The flow's data format selects one of two transport paths (`src/mxl.cpp:151-160` when opening a
reader, `src/mxl.cpp:170` when creating a writer):

- `MXL_DATA_FORMAT_VIDEO`, `MXL_DATA_FORMAT_DATA` → **discrete** (grain-based)
- `MXL_DATA_FORMAT_AUDIO` → **continuous** (sample-batch-based)
- any other format → `std::runtime_error{"invalid data format"}` at startup

Role and format together select one of four code paths in `src/initiator.cpp` /
`src/target.cpp`. The caller does not choose the path; it follows from the flow.

---

## 2. Command line

```
mxl-replicator-worker [OPTIONS] <CONFIG-FILE>
```

The argument parser (`src/main.cpp:169-201`) is intentionally minimal.

| Form | Behaviour | Exit |
|---|---|---|
| `<config-file>` | normal operation | see §8 |
| `-v`, `--version` | prints versions to **stderr**, exits | 0 |
| `--interfaces` | prints the available fabric interfaces to **stdout** as JSON, exits | 0 / 1 |
| `-h`, `--help` | prints usage to **stderr**, exits | 0 |
| no args / two positional args | usage to stderr | 1 |

There are **no other flags**. Everything else goes in the config file.

### `-v` output format

`-v` writes one `<name><padding><value>` line per component to **stderr**
(`src/main.cpp:156-164`):

```
proxy     0.0.1
mxl       1.1.0-rc1
libfabric 2.6
```

To parse, split each line on the first space and trim (`internal/worker/exec/probe.go:46-60`).
The keys are `proxy`, `mxl` and `libfabric`. Running `-v` is the cheapest way to check that the
binary exists and loads (all shared libraries resolve), and to report versions. The current agent
runs it, followed by `--interfaces`, at startup and on every re-registration with the server
(`cmd/mxl-replicator/agent.go:404-415`). If either probe fails, the agent does not register, so
it is assigned no work, and it retries with backoff (`internal/agent/agent.go:344-395`). The
legacy Go supervisor ran `-v` once at startup and exited if it failed
(`legacy/go/cmd/mxl-fabrics-proxy/main.go:113`).

### `--interfaces` output format

`--interfaces` writes to **stdout**, because its output is data rather than diagnostics
(`src/main.cpp:64-154`). It calls `mxlFabricsGetInterfaces()` and prints a JSON array with one
object per `(interface, address, provider)` combination. The same physical interface therefore
appears several times if it is reachable through several providers or has several addresses.

```json
[
  {
    "provider": "tcp",
    "node": "10.135.0.123",
    "caps": {
      "flags": ["REMOTE_WRITE", "SEND_RECEIVE", "BLOCKING_OPERATIONS"],
      "max_message_size": 18446744073709551615
    },
    "attr": {
      "device_name": "wlan0",
      "ep_addr_format": "FI_SOCKADDR_IN",
      "ep_protocol": "FI_PROTO_SOCK_TCP",
      "ep_type": "FI_EP_MSG",
      "fi_domain_name": "wlan0"
    }
  }
]
```

A supervisor that matches this output against its own configuration depends on each of the
following:

- `node` is the value to put in the config's `node` key for this interface. It is an IP address
  for `tcp` and `verbs`, a link-local device address for `efa`, and the **hostname** for `shm`.
- **There is deliberately no `service` field.** The library reports a service alongside the
  address, but it is empty for every provider except `shm`. For `shm` it is specific to the
  process that ran the probe, so a later worker cannot bind it. The supervisor allocates
  `service` from its own port range for every provider, `shm` included (§9).
- **There is no interface-name field**, because the library's API does not provide one. A
  supervisor has to be designed around this; it cannot be patched over. Where the physical
  interface is known at all, it is in `attr.device_name`. For `tcp` that is the netdev name
  (`eth1`, `wlan0`, `lo`). For `verbs` and `efa` it is the **libfabric device name** (`mlx5_0`,
  `rdmap0s6-rdm`), which is not the netdev name an operator would write. There is no reliable
  way to map a configured `ib0` or `efa0` to an entry in this list. §10 item 3 describes the
  matching rules that follow from this.
- `attr` is the library's best-effort attribute set, passed through unchanged and omitted when
  the library reports none. Its contents vary by platform and hardware, so treat every key as
  optional. `shm` reports no `device_name`.
- `caps.max_message_size` is a `uint64`, and providers do report `UINT64_MAX`. Decode it into a
  64-bit unsigned integer, not a float.

The probe exits 0 on success and 1 on failure. It needs no domain from the caller: it creates a
temporary domain and removes it afterwards, because `mxlFabricsGetInterfaces()` requires an mxl
instance and an mxl instance requires an existing domain directory.

⚠️ **stdout is also the log stream** (§7), and libfabric's diagnostics are routed into it. The
worker therefore redirects stdout to stderr while the probe runs and restores it before printing
the result, so stdout carries only the JSON (`src/main.cpp:92-107`). Diagnostics still appear on
stderr, so capture the two streams separately.

---

## 3. Config file (JSON)

`Config::read` in `src/config.cpp:96` parses the file. It is a flat JSON object: no nested
objects, and the only array is `caps_flags`. Unknown keys are silently ignored.

| Key | Type | Req. | Default | Used by | Meaning |
|---|---|---|---|---|---|
| `target` | bool | no | `false` | both | `true` = target/receiver, `false` = initiator/sender |
| `domain` | string | **yes** | — | both | Local MXL domain path, e.g. `/dev/shm/mxl0`. Passed to `mxlCreateInstance`. |
| `node` | string | **yes** | — | both | Local fabric bind address. Provider-dependent (IP for `tcp`/`verbs`, device address for `efa`). |
| `service` | string | **yes** | — | both | Local fabric endpoint name, as a **string**. A port number for `tcp`/`verbs`/`efa`. For `shm` it is not a port, only a name that must be unique on the host. May be `""` (the provider chooses), but the key must be present. A supervisor that needs to know where its target bound should not leave it empty (§9). |
| `provider` | string | no | `"tcp"` | both | One of `any`, `tcp`, `verbs`, `efa`, `shm`. Parsed by `mxlFabricsProviderFromString`; any other value is a fatal `MXL_ERR_INVALID_ARG` at startup. |
| `caps_flags` | array of string | no | `["REMOTE_WRITE","BLOCKING_OPERATIONS"]` | both | Negotiated interface capabilities, using the names `--interfaces` prints: `REMOTE_WRITE`, `SEND_RECEIVE`, `BLOCKING_OPERATIONS`. An unknown name is a fatal `MXL_ERR_INVALID_ARG`. **Must be identical on both ends.** |
| `max_message_size` | uint64 | no | `0` | both | Negotiated maximum message size in bytes. `0` leaves it to the library, which logs a warning that the field will be required in a future version. **Must be identical on both ends.** |
| `idle_timeout_ms` | int | no | `10000` | both | How long to go without reading (initiator) or receiving (target) a grain before terminating. `0` or negative = wait indefinitely. |
| `connect_timeout_ms` | int | no | `60000` | initiator only | How long the connect loop waits for the target to become reachable. `0` or negative = wait indefinitely, which was the behaviour before this key existed. |
| `metrics_socket` | string | **yes** | — | both | Path where the worker **creates** an `AF_UNIX` listening socket. See §6. |
| `target_info` | string | **yes** | — | both | **Meaning depends on the role — see below.** |
| `flow_id` | string | no | `""` | initiator only | UUID of the local flow to read and send. |
| `flow_def` | string | no | `""` | target only | The **flow definition JSON, as a string** (JSON embedded in a JSON string). Used to create the local flow. |
| `no_network_latency_measurement` | bool | no | `false` | both | Disables the tx-timestamp mechanism (§5.3). Must match on both ends. |
| `sched_prio` | int | no | disabled | both | `SCHED_FIFO` priority for the transfer loop. Absent or non-numeric (including JSON `null`) = scheduling is left unchanged. |

"Required" means that a missing key or a value of the wrong type throws
`MXL_ERR_INVALID_ARG: missing required field: <key>` before anything else starts
(`src/config.cpp:70-76`). A bad config therefore fails within milliseconds, not after a
connection attempt.

### `target_info` means different things per role

This is the most important asymmetry in the interface (`src/target.cpp:45`,
`src/initiator.cpp:100`):

- **Target role:** an **output file path**. The worker writes the serialised target info JSON to
  this path once, right after the fabric endpoint is set up and before the receive loop starts.
  The supervisor must poll for the file to appear.
- **Initiator role:** the **target info JSON itself**, inline as a string in the config. The
  supervisor is responsible for fetching it from the peer's target.

### The negotiated interface config must match on both ends

`provider`, `caps_flags` and `max_message_size` together form the interface configuration passed
to `mxlFabricsTargetSetup` / `mxlFabricsInitiatorSetup` (`src/fabrics.cpp:28-47`). The library
does **no negotiation of its own**. Its documentation says both ends must be given the same
capabilities and maximum message size, and that the caller must agree them over its own
out-of-band channel. Choosing these values separately on each side is therefore a bug, not a
configuration option. Whatever sets up the pairing must compute one interface config and write it
into both workers' configs, in the same way that `no_network_latency_measurement` must match
(§5.3).

The names in `caps_flags` are the same strings `--interfaces` prints. A supervisor can intersect
the flag sets reported by two nodes and write the result into the config without translating
anything.

### Fields the C++ worker ignores

The legacy Go supervisor (`legacy/go/pkg/worker/config.go:3-20`) also wrote `proxy_id`,
`efa_use_wait` and `labels`. **The worker reads none of them** (verified: none occur in `src/`).
They were supervisor-side bookkeeping that happened to share the struct. `efa_use_wait` is dead on
both sides: the README documents an `--efa-use-wait` flag that the legacy Go side no longer has
either. The current supervisor writes only keys the worker reads
(`internal/worker/exec/config.go:18-60`). Do not add these fields back expecting the worker to act
on them.

### Minimal examples

Initiator:

```json
{
  "target": false,
  "domain": "/dev/shm/mxl0",
  "flow_id": "5592a23b-0974-45bb-9388-89ea81c42537",
  "node": "10.0.1.7",
  "service": "24011",
  "provider": "verbs",
  "metrics_socket": "/run/mxl/w-1234/metrics.sock",
  "target_info": "{\"id\":\"...\",\"addressFormat\":...,\"fabricAddress\":\"...\",\"regions\":[...],\"provider\":\"verbs\"}",
  "caps_flags": ["REMOTE_WRITE", "BLOCKING_OPERATIONS"],
  "max_message_size": 1048576,
  "idle_timeout_ms": 0,
  "connect_timeout_ms": 60000,
  "no_network_latency_measurement": false,
  "sched_prio": null
}
```

Target:

```json
{
  "target": true,
  "domain": "/dev/shm/mxl1",
  "flow_def": "{\"urn:x-nmos:format:video\": ... }",
  "node": "10.0.2.4",
  "service": "24012",
  "provider": "verbs",
  "metrics_socket": "/run/mxl/w-5678/metrics.sock",
  "target_info": "/run/mxl/w-5678/target-info.json",
  "caps_flags": ["REMOTE_WRITE", "BLOCKING_OPERATIONS"],
  "max_message_size": 1048576,
  "idle_timeout_ms": 0,
  "no_network_latency_measurement": false,
  "sched_prio": 10
}
```

---

## 4. Target info

Target info is the blob the target produces and the initiator consumes. The target produces it
with `mxlFabricsTargetInfoToString`; the initiator parses it with `mxlFabricsTargetInfoFromString`
(`src/fabrics.cpp:115-152`).

**Treat it as an opaque string.** The worker inspects only one field: it requires a top-level
string `"id"` and otherwise throws `MXL_ERR_INVALID_ARG: invalid target info`
(`src/fabrics.cpp:119-123`).

The file the target writes contains the JSON and nothing else. `mxlFabricsTargetInfoToString`
reports a length that includes the NUL terminator, so the file used to end in a NUL byte, which
most JSON parsers reject after the top-level value. `src/fabrics.cpp:143-149` now strips it.

For reference, the mxl library's schema is:

```json
{
  "id": "<decimal uint64 as string>",
  "addressFormat": <number>,
  "fabricAddress": "<base64>",
  "provider": "tcp|verbs|efa|shm",
  "regions": [{"addr": "<uint64 str>", "len": "<uint64 str>", "rkey": "<uint64 str>"}],
  "bounceBufferInfo": {"entryCount": "<uint64 str>", "entrySize": "<uint64 str>"}
}
```

The blob contains **RDMA memory registration keys (rkeys) for the memory mappings of one specific
target process**. The supervisor must respect the consequences:

- **Any target restart invalidates it.** An initiator given stale target info cannot reconnect,
  because the rkeys it refers to no longer exist.
- The pairing is therefore stateful: when the target restarts, the initiator **must** be restarted
  with the new blob. The current supervisor enforces this with an epoch. Each target start gets
  a new nonce, and the agent computes the epoch from the nonce and the blob
  (`internal/agent/unit.go:208`, `:284`). The server assigns an initiator only while its target
  reports ready with an epoch, and copies that epoch and blob into the initiator's assignment
  (`internal/server/reconcile/reconcile.go:1379-1405`). A new epoch changes the initiator's
  `worker.Spec.Key`, so the initiator's agent stops the old initiator and starts a new one
  (`internal/agent/reconcile.go:161-165`). The legacy Go supervisor instead compared target info
  on every keepalive and tore down the subscription when it had changed
  (`legacy/go/pkg/initiator/subscriptions.go:233`).
- The `provider` in the blob must be compatible with the initiator's configured `provider`.

---

## 5. Lifecycle and sequencing

### 5.1 Target (receiver)

`src/target.cpp:28-56`

1. `mxlCreateInstance(domain)`. The domain directory must already exist and be a directory; the
   worker does **not** create it. (The current agent creates it with `os.MkdirAll` before every
   start — `internal/agent/unit.go:197-202`.)
2. The fabrics instance is created.
3. `Metrics` is constructed → **the metrics socket is bound and listening.**
4. `createFlow(flow_def)` creates (or attaches to) the local flow. This step decides between
   discrete and continuous.
5. The fabric target is created and bound to `node:service`.
6. **The `target_info` file is written.** ← This is the supervisor's signal that the target is
   ready.
7. `sched_prio` is applied (if set) for the duration of the transfer loop.
8. The receive loop runs until shutdown.

Steps 1–3 happen in the constructor. C++ initialises members in **declaration order**
(`src/target.hpp:44-47`), not in the order the initialiser list is written. In practice this means
a bad `domain` kills the worker *before* the metrics socket exists, so a supervisor waiting for
the socket must also handle the process simply exiting.

The target has no "wait for connection" step. It is passive.

### 5.2 Initiator (sender)

`src/initiator.cpp:84-114`

1. `mxlCreateInstance(domain)`, then the fabrics instance, then `Metrics`. The same
   declaration-order caveat applies (`src/initiator.hpp:48-51`).
2. `openFlow(flow_id)` **fails if the flow does not exist yet**. This is a common startup race;
   the supervisor's restart loop is what gets past it.
3. The fabric initiator is created on `node:service`, and `addTarget(parse(target_info))` is
   called. (If latency measurement is on, the writer described in §5.3 is not opened here but
   by the transfer loop, on the first grain it reads.)
4. **Connect loop:** calls `makeProgress(500ms)` until connected, bounded by `connect_timeout_ms`
   (default 60 s, `0` = no limit — `src/initiator.cpp:16-37`). When the timeout expires it throws
   `MXL_ERR_TIMEOUT: timed out waiting to connect to the target` and exits 1, so an initiator that
   cannot reach its target reports an error instead of hanging.
5. `sched_prio` is applied, and the transfer loop runs until shutdown.

### 5.3 The tx-timestamp mechanism (`no_network_latency_measurement`)

Understand this mechanism before carrying it forward, because it writes into memory the
initiator does not own.

When measurement is enabled (the default), the initiator opens a `DiscreteFlowWriter` on **the
same flow it is reading**. `createFlow(reader.getFlowDefinition())` attaches a writer to the
existing flow rather than creating a new one (`src/initiator.cpp:116-125`). For each grain it is
about to send, the initiator writes a nanosecond timestamp into the **last 8 bytes of the grain
header's reserved area**, then calls `cancel()` on the write access so nothing is committed
(`src/initiator.cpp:178-183`). The header is in shared memory, and the RDMA transfer copies header
and payload, so the target reads the timestamp from its own copy (`src/target.cpp:101-104`) and
computes `mxl_network_latency_ns` from it.

The writer is opened in the transfer loop on the first grain read, not at startup, and is
released after 1 s without a grain (`LATENCY_WRITER_GRACE` in `src/initiator.cpp`). It is reopened
on the next grain, and that grain is still stamped. The writer holds a shared lock on the flow's
files while open, and releasing it during a pause lets the flow's real owner delete the flow when
it shuts down. The 1 s grace period is not configurable.

The header comment in `src/mxl.hpp:125` marks this mechanism as `(Bad!)`. Implications:

- The initiator **modifies shared memory owned by the real producer**, in place, on the live
  flow. This is harmless today only because the reserved bytes are otherwise unused.
- The initiator must hold a writer on a flow it does not own.
- The setting **must match on both ends**. If the initiator writes timestamps and the target has
  measurement off, the target ignores them (no error). If the initiator has it off and the target
  has it on, the target reads whatever happens to be in those bytes and reports meaningless
  latency. The supervisor must keep the two settings in sync. The current server takes the value
  from one server-wide setting and writes it into both ends' assignments
  (`internal/server/reconcile/reconcile.go:1361`). The legacy Go supervisor sent it in the
  subscription request (`legacy/go/pkg/target/target.go:341`).
- It applies to discrete flows only. Continuous flows never measure network latency (§6).

### 5.4 Signals and shutdown

`src/main.cpp:203-204`. `SIGTERM` and `SIGINT` set a `volatile sig_atomic_t` flag. Every loop
checks it through `utils::ExitSignal` at the top of each iteration and returns cleanly. The
longest delay before a signal is noticed is the timeout of the inner blocking call: **500 ms** in
the connect loop and the target receive loops, **1000 ms** in the initiator's `makeProgress`
drain.

No other signals are handled. `SIGKILL` leaves the metrics socket file behind, because only the
destructor removes it (`remove_all` on a clean exit — `src/metrics.cpp:130`).

The current supervisor sends `SIGTERM`, waits 5 s, then sends `SIGKILL`
(`internal/worker/exec/exec.go:43`, `internal/worker/exec/handle.go:232-260`). The legacy Go
supervisor used the same 5 s (`legacy/go/pkg/worker/exec.go:189-190`). Treat 5 s as a minimum
and keep it.

---

## 6. Metrics socket

Apart from logs, this socket is the worker's only runtime output (`src/metrics.cpp`).

**Protocol** (intentionally minimal):

1. At construction the worker calls `bind()` and `listen()` on the `metrics_socket` path.
2. The client connects. **The client sends nothing.**
3. On `accept()`, the worker renders a snapshot of all metrics into a string
   (`src/metrics.cpp:213`) and writes it without blocking.
4. The worker closes the connection once the buffer is written. **The client reads to EOF.**

Each connection is one point-in-time scrape. The listen backlog is 16, and the epoll loop serves
several concurrent scrapers.

**Hard constraints:**

- The worker calls `unlink()` on the socket path before binding (`src/metrics.cpp:64`), so a file
  left behind by `SIGKILL` no longer causes a fatal `EADDRINUSE`. Still give each worker instance
  a **fresh directory**: it keeps an old `target-info.json` from a previous run out of the way.
  The current supervisor creates one with `os.MkdirTemp` under its work root on every start
  (`internal/worker/exec/handle.go:44`) and removes it when the worker is stopped (`:223`). The
  legacy Go supervisor also used `os.MkdirTemp` on every restart
  (`legacy/go/pkg/worker/exec.go:163`).
- **Keep the full path under 108 bytes**, the size of `sockaddr_un.sun_path`. A path that is too
  long is now a fatal `ENAMETOOLONG` that states the actual length (`src/metrics.cpp:51`).
  Previously the path was silently truncated and the worker bound a different path from the one
  configured. Two workers with long paths sharing a prefix then collided on a path neither had
  asked for, and one reported `EADDRINUSE` caused by an unrelated instance.

### Wire format

Line-oriented text, each line `\n`-terminated, values printed with roughly `%.15g` precision:

```
mxl_octets_total 1234567
mxl_payload_octets_total 1238663
mxl_grains_total 300
mxl_grains_lost 0
mxl_source_latency_ns[0.01] 412000
mxl_source_latency_ns[0.5] 498000
...
mxl_network_latency_ns[0.5] 121000
```

Counters are `name value`. Summary quantiles are `name[quantile] value`. To parse, split on the
first space, then check the name for a `[...]` suffix (`internal/worker/metrics/metrics.go:68-91`).

### Metrics reference

| Name | Type | Emitted by | Meaning |
|---|---|---|---|
| `mxl_octets_total` | counter | both | Sum of grain payload sizes. For continuous flows this is the **sample count**, not octets. |
| `mxl_payload_octets_total` | counter | both | `mxl_octets_total + 4096` per grain — a rough estimate of bytes on the wire, including the header. **The names are the reverse of what you would expect**: the "payload" counter is the larger one. |
| `mxl_grains_total` | counter | both | Grains (discrete) or sample batches (continuous). |
| `mxl_grains_lost` | counter | both | Index gap since the previous grain. See the note below. |
| `mxl_source_latency_ns` | summary | both | `now - indexToTimestamp(index)` — how old the media is at this hop. |
| `mxl_network_latency_ns` | summary | **target only** | `rx_time - tx_time` from §5.3. Emitted only when `no_network_latency_measurement` is false. Never emitted for continuous flows. |
| `mxl_last_grain` | counter | both | **Always 0.** Declared at `src/metrics.hpp:44` and never updated. Unused. |

Summaries are CKMS quantile estimates over a **sliding 30 s window** (3 buckets, rotated every
10 s), for quantiles `0.01, 0.1, 0.5, 0.9, 0.99`, with target error 0.05
(`src/summary.cpp:10-13`, `src/metrics.hpp:45-48`). A summary with no observations in the window
prints `nan`.

The worker emits no labels and no Prometheus `# TYPE` lines; the supervisor adds both. The
current agent attaches `direction`, `domain`, `flow_id`, `session`, `namespace`, `format` and
`media_type`, plus the user labels from the request (`internal/metrics/metrics.go:154-156`,
`internal/agent/metrics.go:299-309`). It also adds three supervisor-level series that the worker
knows nothing about: the counter `mxl_worker_restarts` and the gauges `mxl_writer_active` and
`mxl_reader_active` (`internal/agent/metrics.go:360-374`). The legacy Go supervisor attached
`direction`, `domain`, `flowID` and user-configured labels, and added the same three series
(`legacy/go/pkg/worker/metrics.go:84-107`).

`mxl_grains_lost` used to be permanently 0 on the *initiator*, because an inner-scope
redeclaration of `skipped` shadowed the variable that was reported. This is fixed
(`src/initiator.cpp:199-207`). The target-side calculation (`src/target.cpp:108-115`) was always
correct. A supervisor that learned to ignore the initiator's value should stop ignoring it.

---

## 7. Logging

- Logs go to **stdout**, not stderr. stderr is used only for `-v`/`-h`.
- The mxl library installs a `spdlog` colour logger **named `console`** as the default logger and
  calls `spdlog::cfg::load_env_levels("MXL_LOG_LEVEL")` (mxl `lib/internal/src/Instance.cpp:44`).
  The worker itself never configures a logger or a pattern.
- **The `MXL_LOG_LEVEL` environment variable sets the log level**, inherited from the parent
  process. The legacy Go supervisor never set it, so the `spdlog::debug` calls in the transfer
  loops, although compiled in, were silent at the default `info` level. The current launcher sets
  it from the agent's own log level (`internal/worker/exec/exec.go:155-163`). A supervisor should
  pass it through.
- **`FI_LOG_LEVEL` is a second environment variable.** mxl's own libfabric log bridge reads it
  (`lib/fabrics/ofi/src/internal/FILogging.cpp`) and accepts `trace|debug|info|warn`. It sends
  libfabric's diagnostics **into the same spdlog logger**, not to a separate stream. At `debug`, a
  single failed startup produces about 160 extra lines on stdout. When it is unset, libfabric
  logs nothing.

### Line format

The format varies more than "spdlog's default pattern" suggests. The examples below were captured,
not inferred, from mxl 1.2.0-dev / libfabric 2.6 across these cases: a bad domain path, a malformed
flow definition, a missing config key, an unparseable provider, an idle-source timeout, and a run
with `FI_LOG_LEVEL=debug`:

```
[2026-08-27 22:47:02.625] [error] fatal: unknown error: failed to create flow writer
[2026-08-27 22:47:01.623] [console] [error] [flow.cpp:244] Failed to create flow : Invalid JSON …
[2026-08-27 22:45:04.409] [info] [RCTarget.cpp:32] Setting up RC target with source address: …
[2026-08-27 22:45:22.285] [info] [libfabric:core:core:372] variable prefer_sysconfig=<not set>
```

A parser must handle three things. The legacy translator gets each of them wrong. The current
translator (`internal/worker/logs/logs.go:50-89`) handles all three.

1. **The logger name is optional, and changes within a single run.** The first two lines above
   came from the same process. Lines logged before the mxl instance installs its named default
   logger (`Instance.cpp:44`) have no name; lines logged after it have `[console]`. A parser that
   requires one form or the other drops half of a failing worker's output.
2. **libfabric's diagnostics come through this logger**, with
   `[libfabric:<subsys>:<provider>:<line>]` in the source-location position. Splitting that token
   on the *first* colon yields `libfabric` as the file and `core:core:372` as the line number,
   which is not a number, so the token stays in the message. Split on the last colon instead.
3. **The source-location bracket is optional, and a message can itself start with `[`.** For
   example, a real message ends `… Not a directory [/nonexistent/domain]`. The legacy parser
   consumes every leading bracket (`legacy/go/pkg/worker/exec.go:301-376`), so it takes the first
   token of such a message and records it as the source location.

Levels seen: `trace|debug|info|warning|error|critical`. Timestamps are local time, in the format
`2006-01-02 15:04:05.000`.

Colour codes: the logger uses a `stdout_color_mt` sink, which omits ANSI escapes when stdout is not
a TTY. A supervisor pipes stdout (it has to, to read it), so it receives plain text. The TTY case
is still worth knowing because the escapes are not where you would expect: they wrap the **level
token itself**, as in `[<esc>[31m<esc>[1merror<esc>[m]`. A parser that does not strip them fails
to recognise the level at all, rather than just displaying it oddly.

A supervisor must also **pass through lines it cannot parse** instead of dropping them. Nothing
guarantees that every line on stdout comes from spdlog. The legacy translator silently skips
(`continue`) any line it fails to parse, which can discard the one message that explains a
failure. The current supervisor logs such lines unchanged at warning level
(`internal/worker/exec/handle.go:302-310`).

---

## 8. Exit codes and failure modes

| Condition | Log | Exit code |
|---|---|---|
| Clean shutdown on `SIGTERM`/`SIGINT` | — | 0 |
| `-v` / `-h` | — | 0 |
| `--interfaces` | JSON on stdout, or `fatal: <msg>` on stderr | 0 / 1 |
| Bad/missing arguments | usage on stderr | 1 |
| `mxl::Exception` with `isInterrupted()` | `interrupted, exiting` | 0 |
| Any other `mxl::Exception` | `fatal: <msg>` | 1 |
| Any other `std::exception` | `fatal: <msg>` | 1 |

**Do not use the exit code to classify why a worker died.** It separates success from failure
and nothing more. `mxl::Exception` covers both permanent errors (invalid config, unusable
provider) and transient ones (timeouts, the flow-not-found startup race), and all of them exit 1.
Classifying them would need a distinct exit code per error class. What does work is behavioural:
restart rate over a time window, time from start to exit, and whether the source is live. A
supervisor can compute all of these without help from the worker.

One diagnostic gap: if the `domain` path is missing or not a directory, `mxlCreateInstance`
returns `nullptr` and the worker throws the generic
`std::runtime_error{"failed to create mxl instance"}` (`src/mxl.cpp:119-124`) → exit 1. The actual
cause (`Path does not exist or is not a directory`) appears only in the mxl library's own log line,
not in the worker's `fatal:` message.

### Self-terminating conditions

Both of the following exit the process; neither is retried inside the worker. Both are controlled
by `idle_timeout_ms` (default 10 s, `0` = never):

- **Initiator, discrete:** no grain read successfully within the timeout →
  `MXL_ERR_TIMEOUT: timed out waiting for a grain to be published to the flow`
  (`src/initiator.cpp:239-244`). Each read attempt times out after 100 ms, and `TOO_EARLY`/`TOO_LATE`
  are handled by resyncing to `getHeadIndex() + 1`, so this only fires when the source has
  actually stopped.
- **Target, discrete and continuous:** no grain or sample batch received within the timeout →
  `MXL_ERR_TIMEOUT: timed out waiting for a grain` (discrete, `src/target.cpp:146-148`) or
  `MXL_ERR_TIMEOUT: timed out waiting for samples` (continuous, `src/target.cpp:202-204`).
  Each receive attempt times out after 500 ms.

Set `idle_timeout_ms: 0` if a paused session should stay up. With the default, a source that is
simply not producing puts its workers into a permanent restart cycle. Because a target restart
invalidates its `target_info` (§4), each cycle costs a full re-pairing, not just a process start.
The continuous initiator has no idle timeout at all: it sleeps until the next batch interval and
never exits because the source is idle.

`connect_timeout_ms` bounds the initiator's **connect** phase separately (§5.2).

All other failures (remote peer disappearing, fabric errors, source flow deleted) are raised as an
`mxl::Exception` and the worker exits.

**Consequence for the design: restarting is the only way to recover.** The worker has no
reconnect logic, so every supervisor needs a restart loop. The current agent backs off
exponentially: it waits 1 s after the first death and doubles the wait after each further death,
up to 2 min. If a worker ran for at least 1 min before it died, the wait goes back to 1 s
(`internal/agent/agent.go:41-53`, `internal/agent/unit.go:137-173`). The legacy Go supervisor
waited a flat 3 s between restarts (`legacy/go/pkg/worker/exec.go:139`).

---

## 9. Host and deployment requirements

Runtime shared libraries: `libmxl`, `libmxl-fabrics`, `libfabric`, `libspdlog`, `libuuid`
(`CMakeLists.txt:5-22`). The runtime image installs `libspdlog1.15` and `libuuid1`
(`Dockerfile:77-82`), as the legacy image did (`Dockerfile.legacy:40-45`).

| Requirement | Why | Needed for |
|---|---|---|
| Read/write access to the domain path (e.g. `/dev/shm/mxl0`) | shared-memory flow storage | always |
| Domain directory must already exist | worker does not `mkdir` | target role |
| `/dev/infiniband` | device access | `verbs`, `efa` |
| `CAP_IPC_LOCK` | memory registration / pinning | `verbs`, `efa` |
| `CAP_SYS_RESOURCE` or raised `RLIMIT_MEMLOCK` | pinned memory limits | `verbs`, `efa` |
| `CAP_SYS_NICE` or `RLIMIT_RTPRIO` | `sched_setscheduler(SCHED_FIFO)` | `sched_prio` set |
| Host networking / routable `node` address | the fabric endpoint binds `node:service` | always |

The reference DaemonSet runs with `privileged: true` plus `IPC_LOCK` and `SYS_RESOURCE`. The
legacy manifest `deployment/mxl-fabrics-proxy.yaml` no longer exists. The current Helm chart sets
this in `deployment/mxl-replicator/templates/agent.yaml:254-268`, with the defaults in
`deployment/mxl-replicator/values.yaml:363-369`.

**A `sched_prio` failure is fatal.** `ScopedRTScheduling` throws `std::system_error` if
`sched_setscheduler` fails (`src/rt.cpp:33`). The worker then exits 1, *after* the connection has
been established. It does not fall back to normal scheduling. Either check the capability before
setting `sched_prio`, or leave it unset.

**The supervisor allocates ports.** The worker binds whatever `service` says and has no fallback.
The legacy Go supervisor picked `rand.Intn(20000) + 20000` with **no collision detection and no
retry** (`legacy/go/pkg/worker/exec.go:171`). A collision caused a bind failure and a restart loop
that continued until a later restart happened to pick a free number. The current agent allocates
from an operator-configured range (`--port-range`, default `24000-24999`,
`cmd/mxl-replicator/agent.go:91`) in `internal/agent/ports/alloc.go:65-94`. It keys each port by
session and role, so a restarted worker gets the same port back, and releases it only when the
session is no longer assigned to the node (`internal/agent/reconcile.go:170-177`). It skips ports it
has already handed out, and for `tcp` it also test-binds each candidate port to skip ports another
process holds (`internal/agent/ports/alloc.go:154-179`).

This applies to `shm` too, even though an `shm` service is not a port. `shm` endpoint names must be
unique within the host, and the `service` the probe reports cannot be used as that name (§2). So
the supervisor allocates `shm` services from the same range, which gives host-wide uniqueness
through the same mechanism. There is one allocator and one collision domain, with no special case
per provider. The current allocator does this (`internal/agent/ports/range.go:9-12`).

---

## 10. What the supervisor must provide

These are the things any supervisor must do around the worker. Each item says how the current
supervisor in `internal/` does it, and where the legacy Go tree did it differently.

1. **Per-instance working directory.** A fresh directory for each start (not for each logical
   worker), holding `config.json`, `metrics.sock` and, for targets, `target-info.json`. It must be
   removed on teardown. It is needed because of the socket-rebind constraint (§6). Today:
   `os.MkdirTemp` under `/run/mxl-replicator` on each start, removed when the worker is stopped
   (`internal/worker/exec/handle.go:40-53`, `:223`), and any directories left by a previous agent
   process are removed at startup (`internal/worker/exec/exec.go:182-206`).
2. **Config generation.** Write the JSON described in §3 before exec. Today:
   `internal/worker/exec/config.go:63-88`.
3. **Interface discovery and capability agreement.** Run `--interfaces` (§2) at startup to learn
   what libfabric actually offers on this host. Then agree one
   `(provider, caps_flags, max_message_size)` per session across both nodes; the library does none
   of this (§3). The legacy Go tree did none of this: it configured `provider` on each side and
   left the capabilities at the worker's built-in default. Today the agent runs the probe at
   startup and on re-registration (`cmd/mxl-replicator/agent.go:404-415`) and reports the
   matching attachments to the server. The server intersects the two nodes' capability flags,
   takes the smaller `max_message_size` (`internal/server/negotiate/negotiate.go:88-128`), and
   writes the result into both assignments.

   Matching the probe output against operator configuration needs care, because the probe does
   not name interfaces (§2). There are four *naming* selectors, and each configured attachment
   uses at most one:

   | Configured | Match against | Works for |
   |---|---|---|
   | `address:` | probe `node`, exactly | all providers |
   | `interface:` | resolve the netdev's addresses locally, match probe `node` against that set | `tcp`, `verbs` |
   | `device:` | probe `attr.device_name`, exactly | wherever the library reports one |
   | nothing | the provider alone, which **must** match exactly one entry | the common case |

   The last row is what makes `efa` and `shm` configurable at all. Neither can be named by netdev,
   and a node almost always has exactly one of each, so using no selector is both the simplest
   config and an unambiguous one. When it *is* ambiguous, the supervisor must not guess: it
   refuses the attachment and logs every candidate entry, which gives the operator the exact
   strings they could have configured.

   Because the probe prints one entry per `(interface, address, provider)`, a name alone is often
   not enough: one device with an IPv4 address and a link-local IPv6 address is two entries with
   the same `attr.device_name`. Two *narrowing* selectors can be combined with a naming selector
   and with each other, and the exactly-one-entry rule applies to the combination:
   `network:` (probe `node` parsed and tested for membership in a CIDR prefix) and `ip_version:`
   (probe `node` parsed, 4 or 6). Both work entirely from the probe's `node` field, so neither
   requires the worker to report anything more.

   A configured attachment that matches nothing is a configuration error and must be reported
   loudly, so the operator can tell "this node has no verbs" apart from "someone mistyped `ib0`".

   The agent implements these rules in `probe.Join` (`internal/agent/probe/probe.go:254`).
4. **Port allocation** for `service` (§9), for every provider including `shm`. Today:
   `internal/agent/ports`, from an operator-configured range.
5. **Flow definition transport.** A target cannot create its local flow without the definition
   JSON of the *remote* flow. Today the source node's agent reports each flow's `flow_def.json`
   bytes in its inventory (`internal/agent/inventory/inventory.go:1-6`), and the server copies them
   into the target's assignment (`internal/server/reconcile/reconcile.go:1376`). There is no
   agent-to-agent traffic. The legacy target fetched the definition over HTTP from the peer proxy
   (`GET /v1/flows{domain}?id={flowID}` → `legacy/go/pkg/target/target.go:265-281`) and
   re-encoded it into `flow_def`.
6. **Target-info transport.** Wait until the target's `target_info` file appears, then deliver it
   to the peer's initiator. Today the launcher watches the work directory with inotify instead of
   polling (`internal/worker/exec/handle.go:318-364`), the agent waits up to 30 s for the file
   (`internal/agent/agent.go:55-58`) and reports the blob and its epoch in the worker's status,
   and the server copies both into the initiator's assignment
   (`internal/server/reconcile/reconcile.go:1403-1405`). The legacy target sent the blob to the
   peer in a `POST /v1/subscriptions` (`legacy/go/pkg/target/target.go:330-351`) after polling for
   the file, starting at 200 ms and backing off to 2 s.
7. **Pairing liveness.** Both ends must be torn down together, and target info must be delivered
   again whenever the target restarts (§4). Today the server drives this through assignments.
   When a target worker dies, its agent clears the reported epoch and blob
   (`internal/agent/unit.go:409-412`). The server then withdraws the initiator's assignment until
   the target reports ready with a new epoch
   (`internal/server/reconcile/reconcile.go:1379-1390`), and the new epoch restarts the initiator
   (§4). The legacy Go supervisor used a `PATCH` keepalive every 9 s, 20 s expiry on the initiator
   side, and a check for changed target info that forcibly ended the pairing
   (`legacy/go/pkg/initiator/subscriptions.go:120`, `:233`).
8. **Restart supervision.** Restart delay, `SIGTERM` with a grace period, restart counter. Today:
   exponential backoff from 1 s to 2 min (§8), `SIGTERM` with a 5 s grace period then `SIGKILL`
   (§5.4), and a restart count reported to the server over a 5 min window
   (`internal/agent/agent.go:37-39`) and exported as `mxl_worker_restarts`. The legacy Go
   supervisor used a flat 3 s delay and the same 5 s grace period.
9. **Metrics scraping and labelling** (§6). The current agent still scrapes *every* worker on
   each `/metrics` request, but reads at most 8 sockets at once, with 1 s per worker and 5 s for
   the whole collection (`internal/agent/metrics.go:19-40`). The legacy Go supervisor had no
   concurrency limit and a 3 s budget (`legacy/go/pkg/metrics/metrics.go:123`).
10. **Log translation** (§7). Today: `internal/worker/logs/logs.go`.
11. **Flow liveness observation.** This is independent of the worker. The legacy Go supervisor
    mmaps the flow's `data` file and reads `headIndex` / `lastReadTime` at fixed offsets to derive
    `mxl_writer_active` / `mxl_reader_active` (`legacy/go/pkg/mxl/mxl.go`). The current agent reads
    the same two fields through the `pkg/mxl` package of the external `mxl-utils` module
    (`internal/agent/inventory/inventory.go:654-661`), which also mmaps the flow's data file. It
    still depends on the MXL on-disk layout and will break if that layout changes, so it should be
    replaced with a supported API.

---

## 11. Known quirks to carry forward

Collected from the sections above, roughly ordered by how likely they are to cause trouble:

| # | Issue | Location |
|---|---|---|
| 1 | `mxl_last_grain` declared but never updated; always 0 | `src/metrics.hpp:44` |
| 2 | `mxl_payload_octets_total` / `mxl_octets_total` naming is inverted | `src/metrics.hpp:40-41`, `src/initiator.cpp:219`, `src/target.cpp:129` |
| 3 | `sun_path` is 108 bytes; an over-long path is now a clear error, but it is still a hard limit on the work directory | `src/metrics.cpp:51` |
| 4 | tx-timestamp measurement writes into another writer's grain headers | `src/initiator.cpp:178-183` |
| 5 | `sched_prio` failure is fatal, post-connection | `src/rt.cpp:33` |
| 6 | Config keys `proxy_id`, `efa_use_wait`, `labels` were written by the legacy Go supervisor and are ignored by C++; the current supervisor does not write them | `legacy/go/pkg/worker/config.go:3-20` |
| 7 | The initiator's continuous path has no idle timeout at all, so `idle_timeout_ms` does not apply to it | `src/initiator.cpp:256-306` |

None of these prevent reusing the worker.

**Fixed since the first version of this document.** These are listed because a supervisor written
against the earlier version may still work around them:

- The non-interrupt `mxl::Exception` path now exits 1 rather than 0 (§8).
- `mxl_grains_lost` is no longer always 0 on the initiator (a shadowed variable,
  `src/initiator.cpp:199-207`).
- The connect loop has a timeout (§5.2).
- The metrics socket path is unlinked before bind (§6).
- `target-info.json` no longer ends in a NUL byte (§4).
- `~Metrics` no longer aborts the worker during teardown. It used to close the epoll fd to stop
  its thread, but that does not wake a blocked `epoll_wait`. A scrape arriving in that window was
  accepted onto the descriptor number just freed, `epoll_ctl` failed (`EINVAL`, or `EBADF`), and
  the throw from the thread called `std::terminate`. The worker died with `signal: aborted`
  instead of logging its real `fatal:` reason, because `Metrics` is destroyed while the exception
  that ends the worker is still unwinding. The destructor now wakes the thread through an
  `eventfd`, joins it, and only then closes the descriptors, including the listen fd it used to
  leak (`src/metrics.cpp:109-131`).
