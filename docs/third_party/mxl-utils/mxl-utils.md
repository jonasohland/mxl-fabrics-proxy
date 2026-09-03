# mxl-utils

Go utilities for observing [MXL](https://github.com/dmf-mxl/mxl) domains and flows on a Linux host.

```
import "github.com/jonasohland/mxl-utils/pkg/mxl"
```

The packages are read-only observers. They read what the MXL runtime writes to the filesystem and do not link against the MXL C library, do not participate in data transport, and never write to a domain. Linux only: they use inotify, `/proc/mounts`, `CLOCK_TAI` and inode numbers from `syscall.Stat_t`.

| Package | Purpose |
|---|---|
| `pkg/mxl` | Everything an application needs: reading flows, discovering domains, watching for flows, TAI grain timing |
| `pkg/mxlsys` | Go mirrors of the binary structures in `<mxl/flowinfo.h>` |
| `pkg/testutil` | Builders for synthetic flows, for tests of this module and its consumers |

## What a domain looks like

A domain is any directory that contains `<flow-id>.mxl-flow` subdirectories. Everything in this module is derived from that shape:

```
<domain>/
└── ae8b77bd-a796-4aed-90ad-57a7dc388f85.mxl-flow/
    ├── data           # mxlFlowInfo, 2048 bytes, native endian
    ├── flow_def.json  # NMOS flow definition
    └── grains/        # payload, not touched by this module
```

---

## Reading a flow

`Open` maps `<domain>/<id>.mxl-flow/data` read-only. The mapping stays live, so the runtime values a producer writes become visible without reopening.

```go
flow, err := mxl.Open("/mnt/domain", "ae8b77bd-a796-4aed-90ad-57a7dc388f85")
if err != nil {
    return err
}
defer flow.Close()
```

| Function | Description |
|---|---|
| `Open(domain, id string) (*Flow, error)` | Opens and maps the flow's `data` file. Returns the underlying `os` error if the flow does not exist |
| `(*Flow) Close() error` | Unmaps and closes the file descriptor. Always call it, a `Flow` holds an fd for its lifetime |
| `(*Flow) GetInfo() (*FlowConfigInfo, *FlowRuntimeInfo, error)` | Decodes the flow info out of the mapping. Cheap enough to call on every scrape |
| `(*Flow) GetDefinition() (*FlowDefinition, error)` | Reads and parses `flow_def.json`. Opens the file on every call |
| `(*Flow) IsValid() bool` | Reports whether the mapping still refers to the flow that is on disk now |

### `IsValid` and reopening

A flow is created by writing it to a temporary directory and renaming that into place, so a flow that is deleted and re-created under the same id is a *different* `data` file. The mapping of the old file keeps working and keeps returning stale values forever. `IsValid` compares the inode behind the mapping with the inode on disk, and returns `false` when the flow was replaced or removed. A long-running consumer should check it periodically and reopen:

```go
if !flow.IsValid() {
    _ = flow.Close()
    if flow, err = mxl.Open(domain, id); err != nil {
        // the flow is gone, or not fully created yet
    }
}
```

### Flow info

`FlowConfigInfo` is immutable for the life of a flow, `FlowRuntimeInfo` changes as the producer writes.

| `FlowConfigInfo` | |
|---|---|
| `ID string` | Flow uuid as stored in the shared memory. Should match the directory name |
| `Format DataFormat` | `DataFormatVideo`, `DataFormatAudio`, `DataFormatData` or `DataFormatUnspec` |
| `Rate Rational` | Grain rate for video and data flows, **sample rate** for audio flows |
| `MaxCommitBatchSizeHint`, `MaxSyncBatchSizeHint int` | Producer batching hints, in slices for discrete flows and samples for continuous ones |
| `PayloadLocation PayloadLocation` | `PayloadLocationHost` or `PayloadLocationDevice` |
| `DeviceIndex int` | Device the payload lives on, `-1` for host memory |
| `Discrete *DiscreteFlowConfigInfo` | Non-nil for video and data flows: `SliceSizes [4]uint32`, `GrainCount uint32` |
| `Continuous *ContinuousFlowConfigInfo` | Non-nil for audio flows: `Channels`, `BufferLength` (in samples per channel) |

Exactly one of `Discrete` and `Continuous` is non-nil; which one follows from `Format`, since the two share the same 64 bytes in the file. Flows with an unspecified format are decoded as continuous.

| `FlowRuntimeInfo` | |
|---|---|
| `HeadIndex uint64` | Current write position of the ring buffer, in grains or samples |
| `LastWriteTime uint64` | TAI nanoseconds since the epoch of the last producer write |
| `LastReadTime uint64` | TAI nanoseconds since the epoch of the last consumer read |

### Flow definition

`FlowDefinition` is the NMOS IS-04 flow descriptor from `flow_def.json`, with `Tags` holding the raw NMOS tag map. `GetGroupHint` parses the `urn:x-nmos:tag:grouphint/v1.0` tag, splitting it at the last colon:

```go
def, err := flow.GetDefinition()
if err != nil {
    return err
}

hint, err := def.GetGroupHint() // "Studio A:Camera 1:video"
switch {
case errors.Is(err, mxl.ErrMissingGroupHint): // no grouphint tag
case errors.Is(err, mxl.ErrInvalidGroupHint): // not exactly one value, or no colon
case err == nil:
    _ = hint.Name // "Studio A:Camera 1"
    _ = hint.Type // "video"
}
```

---

## Grain timing

MXL indexes ring buffers by a grain (or sample) index derived from TAI, not from the wall clock, so any two hosts with a synchronized clock agree on which index is current. Index `0` starts at the epoch defined by SMPTE ST 2059, which is also where `time.Unix(0, 0)` sits, so the conversions below need no offset of their own.

| Function | Description |
|---|---|
| `Now() (time.Time, error)` | Reads `CLOCK_TAI`. Equal to UTC on hosts that have no TAI offset configured |
| `TimestampToIndex(tm time.Time, rate Rational) uint64` | Index that covers `tm` at `rate`, rounded to nearest |
| `IndexToTimestamp(index uint64, rate Rational) time.Time` | Start of `index` at `rate` |
| `CurrentIndex(rate Rational) uint64` | `TimestampToIndex(Now(), rate)`, returns `0` if the clock cannot be read |

Both conversions are exact for fractional rates such as `24000/1001`, and round trip losslessly. The usual consumer measurement is how far a producer lags behind the current index:

```go
config, runtime, err := flow.GetInfo()
if err != nil {
    return err
}

latency := int64(mxl.CurrentIndex(config.Rate)) - int64(runtime.HeadIndex)
```

---

## Discovery

Discovery is push based and splits into three components. Each takes a slice of receivers and reports into them; wire them together in whatever shape an application needs.

```
                  search paths
                       │
                       ▼
                 ┌───────────┐  AddDomain / RemoveDomain
                 │ Discoverer├──────────┬───────────────┬───────────────┐
                 └───────────┘          │               │               │
                                        ▼               ▼               ▼
                                  ┌──────────┐  ┌────────────────────┐  ┌───────────────┐
                                  │ Watcher  │  │FilesystemDiscoverer│  │ your receiver │
                                  └────┬─────┘  └─────────┬──────────┘  └───────────────┘
                       AddFlow /       │                  │  AddFilesystem / UpdateFilesystem /
                       RemoveFlow      ▼                  ▼  RemoveFilesystem
                                ┌───────────────┐  ┌───────────────┐
                                │ your receiver │  │ your receiver │
                                └───────────────┘  └───────────────┘
```

`Watcher` and `FilesystemDiscoverer` are themselves `DomainReceiver`s, which is why they can be plugged straight into the `Discoverer`.

### Receiver interfaces

```go
type DomainReceiver interface {
    AddDomain(path string)
    RemoveDomain(path string)
}

type FlowReceiver interface {
    AddFlow(domain, id string)
    RemoveFlow(domain, id string)
}

type FilesystemReceiver interface {
    AddFilesystem(path string)
    UpdateFilesystem(path string, domains []string)
    RemoveFilesystem(path string)
}
```

Receivers are called from the discovery goroutines and, in the case of `Watcher`, while its internal lock is held. Implementations must be safe to call concurrently and must not block or call back into the component that invoked them.

### `Discoverer`

```go
func NewDiscoverer(ctx context.Context, wg *sync.WaitGroup, recv []DomainReceiver, paths, static []string) *Discoverer
```

Walks each of `paths` recursively and reports every directory that contains at least one `*.mxl-flow` entry. Domains nested below another domain are reported in their own right. Missing or unreadable search paths are skipped.

- Rescans every 7 seconds, and reports only what changed since the previous scan.
- `static` domains are reported once at construction, before the function returns, and are never added or removed by scanning even if they appear under a search path. Use them for a domain that is known up front and should be monitored while it is temporarily empty.
- When `ctx` is cancelled it retracts every domain it reported, static ones included, then returns. Wait on `wg` to observe that.

### `Watcher`

```go
func NewWatcher(ctx context.Context, wg *sync.WaitGroup, recv []FlowReceiver) (*Watcher, error)
```

Watches the domains handed to `AddDomain` with inotify and reports the flows inside them.

- `AddDomain` reports the flows that already exist synchronously, before it returns.
- Flow directories appearing later are reported as they are created, including the rename-into-place a producer does when publishing a flow.
- `RemoveDomain` retracts all flows of that domain and stops watching it.
- Every 4 seconds it re-checks the inode of each watched domain directory. A domain directory that was removed and re-created is a different directory that the original inotify watch does not follow, so the watcher retracts the old flows and re-attaches to the new directory.
- A domain directory that merely disappears keeps its flows, on the assumption that it will come back. Retract it explicitly with `RemoveDomain` if that is not wanted.
- Cancelling `ctx` shuts down the watch loop and releases the inotify instance. It does not retract flows by itself; that happens when something calls `RemoveDomain`, which a `Discoverer` holding the watcher as a receiver does during its own shutdown.

Known limitation: a flow directory that is *moved out* of a domain arrives as a rename rather than a delete and is not retracted.

### `FilesystemDiscoverer`

```go
func NewFilesystemDiscoverer(recv []FilesystemReceiver) *FilesystemDiscoverer
```

Maps domains onto the filesystem they are stored on, by matching the domain path against `/proc/mounts` and taking the longest match on a path boundary. Useful for reporting capacity per filesystem rather than per domain, since several domains often share one mount.

- Runs entirely on the caller's goroutine, no context and no waitgroup.
- The first domain on a filesystem triggers `AddFilesystem`, then `UpdateFilesystem` with the full domain list. Further domains only update the list.
- Removing the last domain of a filesystem triggers `RemoveFilesystem`. Filesystems the removed domain was not on are not notified.
- A domain whose filesystem cannot be determined is logged and skipped, so it is never reported.

### Path helpers

| Function | Description |
|---|---|
| `GetFlowIDFromPath(path string) (string, bool)` | Extracts the flow id from a `<uuid>.mxl-flow` path, using only its last element. The id is empty when the second return is `false` |
| `IsFlowDir(path string) bool` | Same check without the id |

### Wiring it together

```go
wg := &sync.WaitGroup{}
ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
defer cancel()

filesystems := mxl.NewFilesystemDiscoverer([]mxl.FilesystemReceiver{myFilesystemReceiver})

watcher, err := mxl.NewWatcher(ctx, wg, []mxl.FlowReceiver{myFlowReceiver})
if err != nil {
    return err
}

mxl.NewDiscoverer(ctx, wg,
    []mxl.DomainReceiver{watcher, filesystems, myDomainReceiver},
    []string{"/mnt/domains"}, // search paths
    nil,                      // static domains
)

<-ctx.Done()
wg.Wait()
```

---

## `pkg/mxlsys`

Plain Go structs matching the binary layout of `mxlFlowInfo` and friends, to be read with `binary.Read` and `binary.NativeEndian`. Every structure is padded to the fixed size the header specifies, and a test asserts those sizes.

| Type | Size | Mirrors |
|---|---|---|
| `Rational` | 16 | `mxlRational` |
| `CommonFlowConfigInfo` | 128 | `mxlCommonFlowConfigInfo` |
| `DiscreteFlowConfigInfo` | 64 | `mxlDiscreteFlowConfigInfo` |
| `ContinuousFlowConfigInfo` | 64 | `mxlContinuousFlowConfigInfo` |
| `FlowConfigInfo` | 192 | `mxlFlowConfigInfo`, with the format specific union as an opaque `[64]byte` |
| `FlowRuntimeInfo` | 64 | `mxlFlowRuntimeInfo` |
| `FlowInfo` | 2048 | `mxlFlowInfo` |

Use this package only to decode a `data` file by hand. `pkg/mxl` already exposes the interesting parts in a friendlier form, with the uuid, the data format and the payload location resolved.

---

## `pkg/testutil`

Builds flows on disk that `pkg/mxl` can open, so consumers can be tested against a temporary directory instead of a running MXL installation. The generated flows carry metadata only, there is no payload and no producer.

```go
domain := t.TempDir()

flow, err := testutil.RandomVideoFlow(domain)
require.NoError(t, err)
require.NoError(t, flow.Create())

opened, err := mxl.Open(domain, flow.ID())
require.NoError(t, err)
defer opened.Close()
```

| Function | Description |
|---|---|
| `NewVideoFlowDef(size FlowSize, rate FlowRate) *mxl.FlowDefinition` | Video definition with a random id and label, a group hint tag and a `video/v210` media type. `FlowSize1080`, `FlowSize2160`; `FlowRate25`, `FlowRate50`, `FlowRate23`, `FlowRate59` |
| `NewAudioFlowDef() *mxl.FlowDefinition` | 64 channel `audio/float32` definition at 48 kHz |
| `NewDummyFlow(domain string, def *mxl.FlowDefinition) (*DummyFlow, error)` | Prepares a flow in `domain` from a definition, without touching the filesystem yet |
| `RandomVideoFlow(domain)` / `RandomAudioFlow(domain)` | Shorthand for the two above |

The data format is taken from the definition's NMOS `format` field, falling back to the media type, and decides whether a discrete or continuous config is written and whether the grain rate or the sample rate is stored. `NewDummyFlow` returns `ErrUnknownFormat` when neither field says what the flow is, and `ErrMissingRate` when the definition carries no rate at all.

| Method | Description |
|---|---|
| `Create() error` | Writes `data` and `flow_def.json` into a temporary directory and renames it into place, the way a real producer publishes a flow |
| `Remove() error` | Deletes the flow directory |
| `UpdateRuntime(runtime mxlsys.FlowRuntimeInfo) error` | Rewrites the runtime info in place, so a reader holding a mapping observes the change. Use it to simulate a producer advancing the head index |
| `ID()`, `Domain()`, `Dir()`, `Definition()`, `Runtime()` | Accessors. `Dir()` is empty until the flow is created |

Calling `Create` twice on definitions that share an id, with a `Remove` in between or not, produces the delete-and-re-create case that `Flow.IsValid` is meant to catch.
