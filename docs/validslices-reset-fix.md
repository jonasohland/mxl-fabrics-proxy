# Replicated grains are committed with `validSlices = 0`

Since libmxl 1.2, every grain the replicator commits on a target reads `validSlices = 0` against
`totalSlices = 1080`. No reader that waits for a complete grain ever gets one. `mxlsrc` in
gst-mxl-rs waits forever, which is how this was found. The thumbnail pipeline of the domain
explorer never returned for any replicated flow.

The initiator's latency stamp does the same thing to the **source** flow. Both come from one
library change, and both need a fix here.

Found on 2026-10-05 on the `fabrics` cluster. The agent there runs `0.0.1-alpha.3` and links
`libmxl.so.1.2`.

## 1. Cause

Upstream commit `759f335b`, "Reset validSlices when opening a new grain", dated 2026-09-14, changed
`PosixDiscreteFlowWriter::openGrain`:

```cpp
auto const reopening = (_currentIndex == in_index);
// ...
grain->header.info.flags &= ~MXL_GRAIN_FLAG_INVALID;
if (!reopening)
{
    grain->header.info.validSlices = 0;
}
*out_grainInfo = grain->header.info;
```

So `openGrain` now writes to shared memory. It sets `validSlices` to 0 and clears
`MXL_GRAIN_FLAG_INVALID` in the slot's header before it returns a copy of that header. Before 1.2,
`openGrain` only set `index`. Opening an existing grain, then committing or cancelling it, used to
leave the grain as it was. Now it destroys the grain's state.

The replicator opens grains that already hold data in two places:

| Where | Flow | What it does after `openGrain` | Effect |
|---|---|---|---|
| `Target::transferGrains`, `src/target.cpp:95` | target (replica) | commits when `Access` is dropped | commits the zeroed header and moves `headIndex` to it |
| `Initiator::transferGrains`, `src/initiator.cpp:174-182` | **source** | `writeTxTimestamp`, then `cancel()` | leaves the zeroed header in the producer's ring |

The upstream `tools/mxl-fabrics-demo/demo.cpp:823-832` has the target pattern too, so the change
breaks upstream's own demo as well.

## 2. The target fix

### What happens to one grain

1. The initiator reads with `reader.getGrain`, which is `mxlFlowReaderGetGrain`. That call waits
   for `MXL_GRAIN_VALID_SLICES_ALL`, so the initiator only sends complete grains.
2. The fabrics library writes the payload and the header into the target's ring by RDMA.
   `RMAGrainIngressProtocol::read` then sets `validSlices` from the immediate data
   (`lib/fabrics/ofi/src/internal/ProtocolIngressRMA.cpp:70`). At this point the grain is correct.
3. `writer.openGrain(index)` sets the header's `validSlices` to 0 and returns that copy.
4. `~Access()` commits the copy (`src/mxl.cpp:430`). The zeroed header is written back, and
   `headIndex` advances to a grain that can never be complete.

### The fix

Put the slice count back before the commit:

```cpp
{
    auto access = writer.openGrain(index);
    // libmxl >= 1.2 sets validSlices to 0 in openGrain. The grain was filled
    // completely by RDMA, because the initiator only sends complete grains.
    access.validSlices(access.totalSlices());
    // ...
    // committed when access object is dropped
}
```

The `Access::validSlices(std::uint16_t)` setter already exists, and `src/mock/src.cpp:166` does the
same thing.

### Limits

- **Invalid grains.** A grain the producer marked `MXL_GRAIN_FLAG_INVALID` reaches the initiator.
  The reader returns it whatever its slice count, and the initiator sends it with 0 slices. With
  this fix, the target commits it as complete, holding whatever was in that slot before. Today the
  target already loses the flag, because `openGrain` clears it. So the flag isn't a new loss, but
  a grain that would never have completed now looks valid. To handle this properly, the target
  needs the flags and slice count the initiator sent. Section 4 covers that.
- **It depends on the initiator sending whole grains.** If the initiator ever sends partial grains
  (`getGrainSlices`), the count from the immediate data has to be carried through instead of
  `totalSlices()`.

## 3. The latency stamp

The initiator stamps a TX timestamp into the last 8 bytes of the header padding of each grain in
the source's ring. It needs a writer mapping for that, because the reader maps the grain files
read-only (`r--s`). So it opens a writer on a flow it does not own:

```cpp
auto writeAccess = writer->openGrain(index);
writeAccess.writeTxTimestamp(::mxlGetTime());
writeAccess.cancel();
```

With libmxl 1.2, `openGrain` sets the source grain's `validSlices` to 0 and clears its invalid
flag. `cancel()` only resets the writer's `_currentIndex`. It does not restore the header. Every
local reader of a replicated source then saw incomplete grains. That is why
`--server-no-network-latency-measurement` fixed the flows on their own nodes.

### Applying the same fix

The idea is the same: put back what `openGrain` cleared. Committing is the wrong way to do it on
this side. `commit` sets `headIndex = index` and bumps the sync counter. The initiator usually
runs a few grains behind the producer, so a commit would move the source's `headIndex` backwards,
in a ring the producer is writing at the same time.

Restore the header without committing. The values come from the reader's copy, which was taken
before `openGrain`:

```cpp
auto grainAccess = reader.getGrain(index, std::chrono::milliseconds(100));
// ...
auto writeAccess = writer->openGrain(index);
writeAccess.writeTxTimestamp(::mxlGetTime());
// libmxl >= 1.2 sets validSlices to 0 and clears the invalid flag in
// openGrain. Restore both in shared memory; do not commit (see above).
writeAccess.restoreHeader(grainAccess.validSlices(), grainAccess.flags());
writeAccess.cancel();
```

`restoreHeader` would be a new `Access` method in the style of `writeTxTimestamp`. It writes
through the writer's mapping, at `_payload - MXL_GRAIN_PAYLOAD_OFFSET`, where the grain's
`mxlGrainInfo` sits. `DiscreteFlowReader::Access` also needs a `flags()` accessor.

This is a worse hack than the timestamp itself:

- **Internal layout.** `MXL_GRAIN_PAYLOAD_OFFSET` (8192) is internal to libmxl
  (`lib/internal/include/mxl-internal/Flow.hpp:34`), not public API. `writeTxTimestamp` already
  depends on the header sitting right before the payload. This also depends on the size of the
  header.
- **A short window.** Between `openGrain` and `restoreHeader`, the source grain reads
  `validSlices = 0`. A reader that checks the grain in that window gets `TOO_EARLY` and waits for
  the next sync-counter wake-up, which comes one grain later. The grain is not lost.
- **A race with the producer.** If the producer reuses the slot in the window, the restore
  overwrites its fresh `validSlices` with the old grain's value. The producer comes back to a slot
  only one ring length later, so the initiator would have to stall that long between the two
  calls. With 10 grains at 50 fps, that is 200 ms.

The clean fix is the one `LATENCY_WRITER_GRACE` in `src/initiator.cpp` already names: the
initiator must never co-write a flow it doesn't own. Until there is a reader-side way to stamp,
`--server-no-network-latency-measurement` is the safe setting. `restoreHeader` makes the
measurement usable again, at the costs listed above.

## 4. Upstream

`759f335b` is a reasonable fix for writers that produce the data themselves. It breaks every
caller that opens a grain only to publish it, and that includes the fabrics target pattern in
upstream's own demo. Either of these would remove both workarounds here:

- **The fabrics target commits RDMA-written grains itself**, using the header it received
  (`validSlices` from the immediate data, `flags` from the RDMA write). It would return the index
  only after the commit. The replicator's target would then not open the grain at all.
- **libmxl offers a commit that keeps the header in shared memory**, for example an `openGrain`
  flag or `mxlFlowWriterPublishGrain(index)`.

Neither one helps the latency stamp, which needs its own reader-side mechanism.

## 5. Checking

On any node, for a slot of a replicated flow:

```sh
od -A n -j 24 -N 4 -t u2 /dev/shm/<domain>/<flow>.mxl-flow/grains/data.0
```

It prints `totalSlices validSlices`. Correct is `1080 1080`, and the bug shows as `1080 0`.
The `mxlGrainInfo` offsets are: version 0, size 4, index 8, flags 16, grainSize 20,
totalSlices 24, validSlices 26.

To check end to end, this should return in well under a second:

```sh
gst-launch-1.0 mxlsrc video-flow-id=<id> domain=<domain> num-buffers=1 ! fakesink
```
