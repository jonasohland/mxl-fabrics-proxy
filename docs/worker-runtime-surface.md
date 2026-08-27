# `mxl-fabrics-proxy-worker` — runtime surface

Reference for anyone driving the C++ worker from a new supervisor/replication manager.
Everything below is the *contract as implemented*, derived from `src/` and from how
`legacy/go/pkg/worker` currently drives it. Source references are `file:line`.

The Go tree (`legacy/go/`) is being replaced. The worker binary is being kept. This document is
the boundary between the two.

---

## 1. What the worker is

One process = **one flow, one direction, one peer, one role**.

- It is a *data-plane leaf*. It has no discovery, no control plane, no HTTP, no signalling,
  no dynamic reconfiguration, no multi-flow support.
- It is configured entirely by a JSON file passed as `argv[1]`, read once at startup
  (`src/main.cpp:208`). To change anything, kill it and start a new one.
- It runs until it is signalled or until it hits a fatal error. It is designed to be
  supervised and restarted.

Two roles, selected by the `target` boolean in the config:

| Role | `target` | Reads from | Writes to | Direction |
|---|---|---|---|---|
| **Initiator** (sender) | `false` | local MXL flow (by `flow_id`) | RDMA to remote target | egress |
| **Target** (receiver) | `true` | RDMA from remote initiator | local MXL flow (created from `flow_def`) | ingress |

Two transport paths, selected automatically from the flow's data format
(`src/mxl.cpp:134`, `src/mxl.cpp:154`):

- `MXL_DATA_FORMAT_VIDEO`, `MXL_DATA_FORMAT_DATA` → **discrete** (grain-based)
- `MXL_DATA_FORMAT_AUDIO` → **continuous** (sample-batch-based)
- anything else → `std::runtime_error{"invalid data format"}` at startup

The role and format combination picks one of four code paths in `src/initiator.cpp` /
`src/target.cpp`. Callers do not select this; it follows from the flow.

---

## 2. Command line

```
mxl-fabrics-proxy-worker [OPTIONS] <CONFIG-FILE>
```

`src/main.cpp:167-201`. The argument parser is deliberately minimal.

| Form | Behaviour | Exit |
|---|---|---|
| `<config-file>` | normal operation | see §8 |
| `-v`, `--version` | prints versions to **stderr**, exits | 0 |
| `--interfaces` | prints the available fabric interfaces to **stdout** as JSON, exits | 0 / 1 |
| `-h`, `--help` | prints usage to **stderr**, exits | 0 |
| no args / two positional args | usage to stderr | 1 |

There are **no other flags**. Everything else is in the config file.

### `-v` output format

Written to **stderr**, one `<name><padding><value>` line each (`src/main.cpp:156-164`):

```
proxy     0.0.1
mxl       1.1.0-rc1
libfabric 2.6
```

Parse by splitting on the first space and trimming (`legacy/go/pkg/worker/exec.go:47-62`).
Keys are `proxy`, `mxl`, `libfabric`. This is the cheapest way to probe that the binary
exists, is loadable (all shared libs resolve), and to report versions — the current Go
code runs it once at startup and refuses to launch if it fails
(`legacy/go/cmd/mxl-fabrics-proxy/main.go:113`).

### `--interfaces` output format

Written to **stdout**, because it is data rather than diagnostics (`src/main.cpp:64-154`).
Calls `mxlFabricsGetInterfaces()` and prints a JSON array, one object per
`(interface, address, provider)` combination — the same physical interface appears several
times when it is reachable through several providers or carries several addresses.

```json
[
  {
    "provider": "tcp",
    "node": "10.135.0.123",
    "service": "",
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

Field notes, all of them load-bearing for a supervisor that joins this against its own
configuration:

- `node` is what goes in the config's `node` key for this interface: an IP for `tcp` and
  `verbs`, a link-local device address for `efa`, and the **hostname** for `shm`.
- `service` is empty except for `shm`, where the library reports a per-process value.
- **There is no interface-name field**, because the library's API has none. The physical
  interface, where it is known at all, is `attr.device_name`. For `tcp` it is the netdev
  name (`eth1`, `wlan0`, `lo`); for `verbs`/`efa` it is the libfabric device name, which is
  *not* the netdev name. Matching a configured `interface: ib0` against this is therefore
  best-effort — matching on `node` is the reliable join.
- `attr` is the library's best-effort attribute blob, passed through verbatim and omitted
  when it reports none. Contents vary by platform and hardware; treat every key as
  optional.
- `caps.max_message_size` is a `uint64` and providers do report `UINT64_MAX`. Decode it into
  a 64-bit unsigned integer, not a float.

Exit 0 on success, 1 on failure. The probe needs no domain: it creates and removes a
throwaway one, because `mxlFabricsGetInterfaces()` requires an mxl instance and an mxl
instance requires a domain directory that exists.

⚠️ **stdout is also the log stream** (§7), and libfabric's own diagnostics are routed into
it. The worker therefore redirects stdout to stderr for the duration of the probe and
restores it before printing, so stdout carries the JSON and nothing else
(`src/main.cpp:85-114`). Diagnostics still appear on stderr — capture the two separately.

---

## 3. Config file (JSON)

Parsed by `Config::read` in `src/config.cpp:96`. Plain JSON object, no nesting, no arrays.
Unknown keys are ignored silently.

| Key | Type | Req. | Default | Used by | Meaning |
|---|---|---|---|---|---|
| `target` | bool | no | `false` | both | `true` = target/receiver, `false` = initiator/sender |
| `domain` | string | **yes** | — | both | Local MXL domain path, e.g. `/dev/shm/mxl0`. Passed to `mxlCreateInstance`. |
| `node` | string | **yes** | — | both | Local fabric bind address. Provider-dependent (IP for `tcp`/`verbs`, device address for `efa`). |
| `service` | string | **yes** | — | both | Local fabric port, as a **string**. May be `""` (let provider choose), but the key must be present. |
| `provider` | string | no | `"tcp"` | both | One of `any`, `tcp`, `verbs`, `efa`, `shm`. Parsed via `mxlFabricsProviderFromString`; anything else is a fatal `MXL_ERR_INVALID_ARG` at startup. |
| `caps_flags` | array of string | no | `["REMOTE_WRITE","BLOCKING_OPERATIONS"]` | both | Negotiated interface capabilities. Names as printed by `--interfaces`: `REMOTE_WRITE`, `SEND_RECEIVE`, `BLOCKING_OPERATIONS`. An unknown name is a fatal `MXL_ERR_INVALID_ARG`. **Must be identical on both ends.** |
| `max_message_size` | uint64 | no | `0` | both | Negotiated maximum message size, in bytes. `0` leaves it to the library, which logs a warning that the field will be required in a future version. **Must be identical on both ends.** |
| `idle_timeout_ms` | int | no | `10000` | both | How long to wait without reading (initiator) or receiving (target) a grain before terminating. `0` or negative = wait indefinitely. |
| `connect_timeout_ms` | int | no | `60000` | initiator only | How long the connect loop waits for the target to become reachable. `0` or negative = wait indefinitely, which was the behaviour before this key existed. |
| `metrics_socket` | string | **yes** | — | both | Path where the worker **creates** an `AF_UNIX` listening socket. See §6. |
| `target_info` | string | **yes** | — | both | **Role-dependent — see below.** |
| `flow_id` | string | no | `""` | initiator only | UUID of the local flow to read and send. |
| `flow_def` | string | no | `""` | target only | The **flow definition JSON, as a string** (i.e. JSON embedded in a JSON string). Used to create the local flow. |
| `no_network_latency_measurement` | bool | no | `false` | both | Disables the tx-timestamp hack (§5.3). Must match on both ends. |
| `sched_prio` | int | no | disabled | both | `SCHED_FIFO` priority for the transfer loop. Absent or non-numeric (incl. JSON `null`) = leave scheduling alone. |

"Required" means: missing or wrong type throws `MXL_ERR_INVALID_ARG: missing required field: <key>`
before anything else starts (`src/config.cpp:17-23`). This is a fast, cheap validation —
a bad config fails in milliseconds, not after a connection attempt.

### `target_info` is two different things

This is the single most important asymmetry in the interface (`src/target.cpp:45`,
`src/initiator.cpp:31`):

- **Target role**: an **output file path**. The worker writes the serialised target info
  JSON to this path once, immediately after the fabric endpoint is set up and before the
  receive loop starts. The supervisor must poll for this file to appear.
- **Initiator role**: the **target info JSON content itself**, inline in the config string.
  The supervisor is responsible for having transported it from the peer's target.

### The negotiated interface config must match on both ends

`provider`, `caps_flags` and `max_message_size` together are the interface configuration
handed to `mxlFabricsTargetSetup` / `mxlFabricsInitiatorSetup` (`src/fabrics.cpp:28-47`).
The library performs **no negotiation of its own** and documents that both ends must be
given the same capabilities and maximum message size, with the caller's out-of-band channel
responsible for agreeing them. Deciding them per side is therefore not a configuration
choice, it is a bug: whatever agrees the pairing must compute one interface config and write
it into both workers' configs, the same way `no_network_latency_measurement` has to match
(§5.3).

The names in `caps_flags` are exactly the ones `--interfaces` prints, so a supervisor can
intersect two nodes' reported flag sets and write the result straight back out without a
translation step.

### Fields the C++ worker ignores

`legacy/go/pkg/worker/config.go` also emits `proxy_id`, `efa_use_wait`, and `labels`. **None are
read by the worker** (verified: no occurrence in `src/`). They are supervisor-side
bookkeeping that happens to ride along in the same struct. `efa_use_wait` in particular is
dead — the README documents an `--efa-use-wait` flag that no longer exists on the Go side
either. Do not carry these forward expecting the worker to honour them.

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

The blob the target produces and the initiator consumes. Produced by
`mxlFabricsTargetInfoToString`, consumed by `mxlFabricsTargetInfoFromString`
(`src/fabrics.cpp:36-64`).

**Treat it as an opaque string.** The worker itself only peeks at one field: it requires a
top-level string `"id"` and throws `MXL_ERR_INVALID_ARG: invalid target info` otherwise
(`src/fabrics.cpp:39-45`).

The file the target writes contains the JSON and nothing else. The library's
`mxlFabricsTargetInfoToString` reports a length that counts the NUL terminator, so the blob
used to be written with a trailing NUL byte that most JSON parsers reject after the
top-level value; `src/fabrics.cpp:143-149` strips it.

For orientation, the mxl library's schema is:

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

It encodes **RDMA memory registration keys for a specific process's specific memory
mappings**. Consequences the supervisor must respect:

- It is **invalidated by any target restart**. A stale target info will not reconnect —
  it points at rkeys that no longer exist.
- Therefore the pairing is stateful: if the target restarts, the initiator **must** be
  restarted with the new blob. The current Go layer enforces this by comparing target info
  on every keepalive and tearing down the subscription when it changed
  (`legacy/go/pkg/initiator/subscriptions.go:233`).
- The `provider` inside the blob must be compatible with the initiator's configured
  `provider`.

---

## 5. Lifecycle and sequencing

### 5.1 Target (receiver)

`src/target.cpp:28-56`

1. `mxlCreateInstance(domain)`. The domain directory must exist and be a directory; the
   worker does **not** create it. (The Go layer `MkdirAll`s it first —
   `legacy/go/pkg/target/target.go:237`.)
2. Fabrics instance created.
3. `Metrics` constructed → **metrics socket is bound and listening.**
4. `createFlow(flow_def)` → creates (or attaches to) the local flow. Discrete/continuous
   decided here.
5. Fabric target created and bound to `node:service`.
6. **`target_info` file written.** ← the supervisor's signal that the target is ready.
7. `sched_prio` applied (if set), scoped to the transfer loop.
8. Receive loop forever.

Steps 1–3 are the constructor, and run in **member declaration order**
(`src/target.hpp:44-47`), not the order written in the initialiser list. The practical
consequence: a bad `domain` kills the worker *before* the metrics socket exists, so a
supervisor polling for the socket must also handle the process simply dying.

There is no "wait for connection" step on the target — it is passive.

### 5.2 Initiator (sender)

`src/initiator.cpp:14-46`

1. `mxlCreateInstance(domain)`, fabrics instance, then `Metrics` — same declaration-order
   caveat as above (`src/initiator.hpp:45-48`).
2. `openFlow(flow_id)` → **fails if the flow does not exist yet**. This is a common startup
   race; the supervisor restart loop is what papers over it.
3. If latency measurement is on: open a *writer* on the same flow (see §5.3).
4. Create fabric initiator on `node:service`, `addTarget(parse(target_info))`.
5. **Connect loop** — `makeProgress(500ms)` until connected, bounded by
   `connect_timeout_ms` (default 60 s, `0` = forever — `src/initiator.cpp:17-40`). On
   expiry it throws `MXL_ERR_TIMEOUT: timed out waiting to connect to the target` and exits
   1, so a stuck initiator reports rather than hangs.
6. `sched_prio` applied, transfer loop forever.

### 5.3 The tx-timestamp mechanism (`no_network_latency_measurement`)

Worth understanding before you carry it forward, because it is invasive.

When enabled (the default), the initiator opens a `DiscreteFlowWriter` on **the very flow
it is reading** — `createFlow(reader.getFlowDefinition())` returns a writer attached to the
existing flow, not a new one (`src/initiator.cpp:48-57`). For each grain it is about to
send, it writes a nanosecond timestamp into the **last 8 bytes of the grain header's
reserved area**, then `cancel()`s the write access so nothing is committed
(`src/initiator.cpp:126-130`). Because the header lives in shared memory and the RDMA
transfer copies header + payload, the target reads the value back out of its own copy
(`src/target.cpp:100-102`) and derives `mxl_network_latency_ns`.

The header comment in `src/mxl.hpp:125` calls this out as `(Bad!)`. Implications:

- The initiator **mutates shared memory owned by the real producer**, in place, on the live
  flow. Benign today only because the reserved bytes are unused.
- It requires the initiator to hold a writer on a flow it does not own.
- The setting **must match on both ends**. If the initiator writes timestamps and the target
  has measurement off, the target simply won't read them (no error). If the initiator has it
  off and the target has it on, the target reads whatever garbage is in those bytes and
  reports nonsense latency. The supervisor is responsible for keeping the two in sync — the
  current Go code does this by shipping the flag in the subscription request
  (`legacy/go/pkg/target/target.go:341`).
- Discrete flows only. Continuous flows never measure network latency (§6).

### 5.4 Signals and shutdown

`src/main.cpp:202-204`. `SIGTERM` and `SIGINT` set a `volatile sig_atomic_t` flag; every loop
polls it via `utils::ExitSignal` at its top and returns cleanly. Worst-case latency to
notice a signal is bounded by the inner blocking call: **500 ms** in the connect and target
receive loops, **1000 ms** in the initiator's `makeProgress` drain.

No other signal is handled. `SIGKILL` leaves the metrics socket file behind (the
destructor `remove_all`s it on a clean exit — `src/metrics.cpp:95`).

The current supervisor sends `SIGTERM` and allows 5 s before escalating
(`legacy/go/pkg/worker/exec.go:189-190`). That is a reasonable floor; keep it.

---

## 6. Metrics socket

The worker's only runtime output channel besides logs. `src/metrics.cpp`.

**Protocol** — deliberately trivial:

1. Worker `bind()`s + `listen()`s on the `metrics_socket` path at construction.
2. Client connects. **Send nothing.**
3. On `accept()`, the worker snapshots all metrics into a string
   (`src/metrics.cpp:164`) and writes it non-blocking.
4. Worker closes the connection when the buffer is drained. **Read to EOF.**

One connection = one point-in-time scrape. Backlog is 16, and the epoll loop handles
multiple concurrent scrapers.

**Hard constraints:**

- The worker `unlink()`s the socket path before binding (`src/metrics.cpp:57`), so a
  leftover file from a `SIGKILL` is no longer a fatal `EADDRINUSE`. Give each worker
  instance a **fresh directory** anyway — it is what keeps `target-info.json` from a
  previous incarnation out of the way, and the current Go code already does it with
  `os.MkdirTemp` per restart (`legacy/go/pkg/worker/exec.go:163`).
- **Keep the full path under 108 bytes** — the `sockaddr_un.sun_path` limit. An over-long
  path is now a fatal `ENAMETOOLONG` naming the actual length (`src/metrics.cpp:44`)
  rather than a silent truncation that binds a different path than the one configured. Two
  workers whose long paths shared a prefix used to collide on a path neither asked for and
  report `EADDRINUSE` from an unrelated instance.

### Wire format

Line-oriented text, `\n`-terminated, `%.15g`-ish precision:

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

Counters are `name value`. Summary quantiles are `name[quantile] value`. Parse by splitting
on the first space, then checking for a `[...]` suffix on the name
(`legacy/go/pkg/worker/metrics.go:39-62`).

### Metrics reference

| Name | Type | Emitted by | Meaning |
|---|---|---|---|
| `mxl_octets_total` | counter | both | Sum of grain payload sizes. For continuous flows this is **sample count**, not octets. |
| `mxl_payload_octets_total` | counter | both | `mxl_octets_total + 4096` per grain — a rough on-wire estimate including header. **The naming is inverted from what you'd expect**: the "payload" counter is the larger one. |
| `mxl_grains_total` | counter | both | Grains (discrete) or sample batches (continuous). |
| `mxl_grains_lost` | counter | both | Index gap since last grain. See caveat below. |
| `mxl_source_latency_ns` | summary | both | `now - indexToTimestamp(index)` — age of the media at this hop. |
| `mxl_network_latency_ns` | summary | **target only** | `rx_time - tx_time` from §5.3. Only emitted when `no_network_latency_measurement` is false. Never emitted for continuous flows. |
| `mxl_last_grain` | counter | both | **Always 0.** Declared at `src/metrics.hpp:44`, never updated. Dead. |

Summaries are CKMS quantile estimates over a **sliding 30 s window** (3 buckets rotating
every 10 s), quantiles `0.01, 0.1, 0.5, 0.9, 0.99`, target error 0.05
(`src/summary.cpp:10-13`, `src/metrics.hpp:45-48`). A summary with no observations in the
window emits `nan`.

The supervisor is expected to add labels and Prometheus `# TYPE` lines itself — the worker
emits neither. The current Go layer attaches `direction`, `domain`, `flowID` plus
user-configured labels, and adds three supervisor-level counters that the worker knows
nothing about: `mxl_worker_restarts`, `mxl_writer_active`, `mxl_reader_active`
(`legacy/go/pkg/worker/metrics.go:84-107`).

`mxl_grains_lost` used to be permanently 0 on the *initiator* side — an inner-scope
redeclaration of `skipped` shadowed the variable that gets reported. Fixed
(`src/initiator.cpp:157`); the target-side calculation (`src/target.cpp:106-113`) always
was correct. A supervisor that learned to ignore the initiator's value should stop.

---

## 7. Logging

- Destination: **stdout**. Not stderr — that's only used for `-v`/`-h`.
- The mxl library installs a `spdlog` colour logger **named `console`** as the default
  logger and calls `spdlog::cfg::load_env_levels("MXL_LOG_LEVEL")`
  (mxl `lib/internal/src/Instance.cpp:44`). The worker itself never configures a logger or
  a pattern.
- **Log level is controlled by the `MXL_LOG_LEVEL` environment variable**, inherited from
  the parent. This is the only environment knob the worker has, and it is not currently
  exercised by the Go layer — `spdlog::debug` calls in the transfer loops are compiled in
  but silent at the default `info` level. A new supervisor should plumb this through.

Line format (spdlog's default pattern, plus mxl's own source-location prefixes):

```
[2026-08-27 14:03:11.842] [console] [info] connected
[2026-08-27 14:03:11.900] [console] [debug] [Flow.cpp:88] ...
```

The existing translator (`legacy/go/pkg/worker/exec.go:301-376`) parses this by consuming
leading `[...]` tokens, dropping the literal `console`, then treating token 0 as a timestamp
(`2006-01-02 15:04:05.000`), token 1 as the level (`trace|debug|info|warning|error`), and
token 2 — if present — as a source location. The remainder is the message. Note this parser
is positional and will mis-handle a message that itself starts with `[`.

Colour codes: the logger is a `stdout_color_mt` sink, which suppresses ANSI escapes when
stdout is not a TTY. Piping it (as the supervisor does) gives clean text.

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

**Do not use the exit code to classify *why* a worker died.** It now distinguishes success
from failure, which is all it was ever going to do: `mxl::Exception` covers invalid config
and unusable providers (permanent) alongside timeouts and the flow-not-found startup race
(transient), so both land in the same non-zero bucket. Meaningful classification would need
distinct exit codes per error class. The signals that work are behavioural — restart rate
over a window, time to death, and source liveness — and a supervisor can compute all of
them without the worker's help.

One diagnostic gap worth knowing: if the `domain` path is missing or not a directory,
`mxlCreateInstance` returns `nullptr` and the worker throws the generic
`std::runtime_error{"failed to create mxl instance"}` (`src/mxl.cpp:113-117`) → exit 1. The
actual cause (`Path does not exist or is not a directory`) is only visible in the mxl
library's own log line, not in the worker's `fatal:` message.

### Self-terminating conditions

Both of these exit the process — they are not retried internally. Both are governed by
`idle_timeout_ms` (default 10 s, `0` = never):

- **Initiator, discrete**: no successfully read grain within the timeout →
  `MXL_ERR_TIMEOUT: timed out waiting for a grain to be published to the flow`
  (`src/initiator.cpp:179`). Note the per-attempt read timeout is 100 ms and
  `TOO_EARLY`/`TOO_LATE` are handled by resyncing to `getHeadIndex() + 1`, so this fires
  only on a genuinely dead source.
- **Target, discrete and continuous**: no grain/sample batch received within the timeout →
  `MXL_ERR_TIMEOUT: timed out waiting for a grain` (`src/target.cpp:146`,
  `src/target.cpp:202`). Per-attempt timeout 500 ms.

Set `idle_timeout_ms: 0` when a paused session should stay up. With the default, a source
that is simply not producing puts its workers into a permanent restart cycle — and since a
target restart invalidates its `target_info` (§4), that cycle costs a re-pairing every time,
not just a process start. The initiator, continuous variant has no idle timeout in any case:
it sleeps to the next batch interval and never self-terminates on an idle source.

The initiator's **connect** phase is bounded separately by `connect_timeout_ms` (§5.2).

Everything else — remote peer vanishing, fabric errors, source flow being deleted — surfaces
as an `mxl::Exception` and exits.

**Design consequence: restart is the only recovery mechanism.** The worker has no reconnect
logic. Any supervisor must implement a restart loop; the current one uses a flat 3 s delay
(`legacy/go/pkg/worker/exec.go:139`).

---

## 9. Host and deployment requirements

Runtime shared libraries: `libmxl`, `libmxl-fabrics`, `libfabric`, `libspdlog`, `libuuid`
(`CMakeLists.txt:5-22`). The published image installs `libspdlog1.15`, `libuuid1`
(`Dockerfile.legacy:40-45`).

| Requirement | Why | Needed for |
|---|---|---|
| Read/write access to the domain path (e.g. `/dev/shm/mxl0`) | shared-memory flow storage | always |
| Domain directory must already exist | worker does not `mkdir` | target role |
| `/dev/infiniband` | device access | `verbs`, `efa` |
| `CAP_IPC_LOCK` | memory registration / pinning | `verbs`, `efa` |
| `CAP_SYS_RESOURCE` or raised `RLIMIT_MEMLOCK` | pinned memory limits | `verbs`, `efa` |
| `CAP_SYS_NICE` or `RLIMIT_RTPRIO` | `sched_setscheduler(SCHED_FIFO)` | `sched_prio` set |
| Host networking / routable `node` address | the fabric endpoint binds `node:service` | always |

The reference DaemonSet runs `privileged: true` plus `IPC_LOCK` and `SYS_RESOURCE`
(`deployment/mxl-fabrics-proxy.yaml:158-163`).

**`sched_prio` fails hard**: `ScopedRTScheduling` throws `std::system_error` if
`sched_setscheduler` fails (`src/rt.cpp:33`), which kills the worker with exit 1 *after* the
connection is established. There is no graceful degradation. Either verify the capability
before setting `sched_prio`, or don't set it.

**Port allocation is the supervisor's job.** The worker binds whatever `service` says and
has no fallback. The current Go code picks `rand.Intn(20000) + 20000` with **no collision
detection and no retry** (`legacy/go/pkg/worker/exec.go:171`) — a collision produces a bind failure
and a restart loop that eventually rolls a different number. A replication manager should
own an explicit port range and allocate deterministically.

---

## 10. What the supervisor must provide

Everything the Go tree does *around* the worker, i.e. what needs reimplementing:

1. **Per-instance working directory.** Fresh dir per start (not per logical worker), holding
   `config.json`, `metrics.sock`, and — for targets — `target-info.json`. Must be removed on
   teardown. Required because of the socket-rebind constraint (§6).
2. **Config generation.** The JSON in §3, written before exec.
3. **Interface discovery and capability agreement.** Run `--interfaces` (§2) at startup to
   learn what libfabric actually offers on this host, and agree one `(provider, caps_flags,
   max_message_size)` per session across both nodes — the library does none of this itself
   (§3). Nothing in the Go tree does this today: it configures `provider` per side and
   leaves the capabilities at the worker's built-in default.
4. **Port allocation** for `service` (§9).
5. **Flow definition transport.** A target cannot create its local flow without the *remote*
   flow's definition JSON. Today this is fetched over HTTP from the peer proxy
   (`GET /v1/flows{domain}?id={flowID}` → `legacy/go/pkg/target/target.go:265-281`) and re-encoded
   into `flow_def`.
6. **Target-info transport.** Poll for the target's `target_info` file to appear, then get it
   to the peer's initiator. Today: a `POST /v1/subscriptions` carrying the blob
   (`legacy/go/pkg/target/target.go:330-351`). Polling starts at 200 ms and backs off to 2 s.
7. **Pairing liveness.** Both ends must be torn down together, and target info must be
   re-delivered whenever the target restarts (§4). Today: 9 s `PATCH` keepalive, 20 s
   expiry on the initiator side, plus a target-info-changed check that force-terminates
   the pairing (`legacy/go/pkg/initiator/subscriptions.go:120`, `:233`).
8. **Restart supervision.** 3 s delay, `SIGTERM` with a 5 s grace period, restart counter.
9. **Metrics scraping and labelling** (§6). Note the current implementation scrapes *every*
   worker on each `/metrics` request with a 3 s budget — worth reconsidering at scale.
10. **Log translation** (§7).
11. **Flow liveness observation.** Independent of the worker: the Go layer mmaps the flow's
    `data` file and reads `headIndex` / `lastReadTime` at fixed offsets to derive
    `mxl_writer_active` / `mxl_reader_active` (`legacy/go/pkg/mxl/mxl.go`). This depends on the
    MXL on-disk layout and will break if that layout changes — treat it as a candidate for
    replacement with a supported API rather than a straight port.

---

## 11. Quirks to carry forward knowingly

Collected from the sections above, in rough order of how likely they are to bite:

| # | Issue | Location |
|---|---|---|
| 1 | `mxl_last_grain` declared but never updated; always 0 | `src/metrics.hpp:44` |
| 2 | `mxl_payload_octets_total` / `mxl_octets_total` naming is inverted | `src/metrics.cpp:74` |
| 3 | `sun_path` is 108 bytes; an over-long path is now a clear error, but it is still a hard limit on the work directory | `src/metrics.cpp:44` |
| 4 | tx-timestamp measurement writes into another writer's grain headers | `src/initiator.cpp:134-138` |
| 5 | `sched_prio` failure is fatal, post-connection | `src/rt.cpp:33` |
| 6 | `~Metrics` closes the epoll fd to unblock its thread and leaks the listen fd | `src/metrics.cpp:88-96` |
| 7 | Config keys `proxy_id`, `efa_use_wait`, `labels` are written by Go and ignored by C++ | `legacy/go/pkg/worker/config.go:7-18` |
| 8 | The initiator's continuous path has no idle timeout at all, so `idle_timeout_ms` does not apply to it | `src/initiator.cpp:196-246` |

None of these are blockers for reuse.

**Fixed since the first version of this document**, listed because a supervisor written
against it may still be working around them: the non-interrupt `mxl::Exception` path now
exits 1 rather than 0 (§8); `mxl_grains_lost` is no longer always 0 on the initiator
(a shadowed variable, `src/initiator.cpp:157`); the connect loop has a timeout (§5.2); the
metrics socket is unlinked before bind (§6); and `target-info.json` no longer carries a
trailing NUL byte (§4).
