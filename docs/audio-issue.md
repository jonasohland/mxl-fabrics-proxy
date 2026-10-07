# Replicated audio drifts behind and drops ~200 ms every ~35 s

Every replicated audio flow falls behind its source at about 5.5 ms per second. Once the replica
trails the source by about 200 ms, the initiator gets `TOO_LATE` from the reader and jumps to the
source's head. That skips about 9,700 samples, roughly 200 ms of audio. The cycle repeats about
every 35 s on every audio flow, on every target node. So each replica loses about 0.6% of its
samples, and there is an audible gap in every cycle.

Video is not affected.

Found on 2026-10-05 on the `fabrics` cluster, with the agent at `0.0.1-alpha.4`. The source flows
come from `mxl-gst-testsrc`: 48 kHz float32 audio with a 480-sample batch (10 ms) and a ring
buffer of 19,456 samples (405 ms).

## 1. What the metrics show

`mxl_flow_latency_grains` from mxl-exporter is the current TAI time converted to an index, minus
the flow's head index. For audio, one grain is one sample. Here is the same flow,
`1675da09-0001-4000-8000-000000000000` in `basic-sources-n1`, on all four nodes, with one sample
every 5 s:

| Node | Role | Latency (ms) |
|---|---|---|
| fabrics-k8n1 | source (testsrc) | 32 31 31 31 31 32 31 32 30 31 31 32 |
| fabrics-k8n2 | replica | 98 137 175 211 247 276 **68** 105 150 189 227 274 |
| fabrics-k8n3 | replica | 88 121 145 174 209 **34** 70 94 125 151 184 214 |
| fabrics-k8n4 | replica | 109 158 187 218 259 **39** 79 117 158 199 247 287 |

The source is flat. Every replica climbs about 28 ms per 5 s sample, then drops back to near the
source latency.

Over one hour on k8n3, the latency per audio flow has a minimum of 30–37 ms, a median of
131–137 ms and a maximum of 234–238 ms. The video flows sit at 2 grains (40 ms) on both the source
and the replicas.

At about 70 ms resolution (scraping the exporter directly for 3 s), the replica head advances
smoothly in multiples of 480 samples, and latency still rises steadily: 135 → 151 ms in 3 s. So
the drift is continuous; the replica isn't sending in large bursts.

Replicator metrics for the same flow, `increase(...[10m])`:

| Node | `mxl_grains_total` | `mxl_grains_lost` |
|---|---|---|
| fabrics-k8n2 target | 59,512 (expected 60,000) | 240,565 |
| fabrics-k8n3 target | 59,646 | 174,252 |
| fabrics-k8n4 target | 59,513 | 240,081 |

`avg by (node) (rate(mxl_grains_total{direction="target", format="audio"}[10m]))` is 98.7–99.2
per second on every node, where 100 is expected (one batch per 10 ms).

## 2. Cause

### Pacing is relative, so it drifts

`Initiator::transferSamples`, `src/initiator.cpp:262-311`:

```cpp
(void)reader.getSamplesNonBlocking(headIndex, batchSize);
// ...
lastReadTime = ::mxlGetTime();
// ... transferSamples, makeProgress ...
headIndex += batchSize;
::mxlSleepUntil(lastReadTime + interval);
```

The next deadline is computed from `lastReadTime`, which is taken *after* the thread wakes up and
reads. Each iteration therefore lasts `interval` plus the wake-up latency plus the time spent
reading. Nothing pulls the loop back to the source's timeline. The loop sends one batch of 480
samples per iteration, so it sends fewer samples than the source produces.

At the measured drift of about 5.5 ms per second, each 10 ms iteration overshoots by about 55 µs.
That is close to Linux's default 50 µs timer slack for a non-real-time thread. So most of the
overshoot is probably the slack on `mxlSleepUntil`, plus the read. I haven't measured that
breakdown on the node.

### The catch-up is the reader's window

A continuous-flow reader can only reach back `bufferLength / 2` samples behind the head
(`PosixContinuousFlowReader.cpp:125` in libmxl, and `docs/Architecture.md:54` upstream). For these
flows that is 9,728 samples, or 202.7 ms. When the initiator's `headIndex` falls out of that
window, `getSamplesNonBlocking` throws `isTooLate()` and the loop jumps:

```cpp
if (ex.isTooLate()) {
  headIndex = reader.getHeadIndex();
  continue;
}
```

This matches the numbers:

- **Peak latency.** Source latency (~31 ms) plus the reader window (202.7 ms) is ~234 ms, the
  observed maximum.
- **Latency after the jump.** It returns to roughly the source latency: 34–68 ms.
- **Period.** 202.7 ms ÷ 5.5 ms/s ≈ 37 s, against an observed ~35 s.
- **Loss.** About 17 jumps in 10 minutes × ~9,700 samples ≈ 165k samples. The k8n3 counter shows
  174k.

### Why video is fine

`Initiator::transferGrains` uses `reader.getGrain(index, 100ms)`, a blocking read for a specific
index. The source's write paces the loop, so there is no free-running timer to drift.

## 3. Fix

Pace the sample loop on the flow's timeline instead of on the last wake-up. Either of these works:

1. **Deadline from the index.** Sleep until the batch the loop wants next is complete in the
   source:

   ```cpp
   headIndex += batchSize;
   ::mxlSleepUntil(::mxlIndexToTimestamp(&rate, headIndex + batchSize));
   ```

   Any wake-up latency then delays one iteration, but it doesn't add up across iterations. The
   source's own latency (about 31 ms here) means the batch is usually not readable yet at that
   timestamp. The existing `isTooEarly()` path handles that, but see point 2.

2. **Blocking read.** The wrapper already has a timed `ContinuousFlowReader::getSamples(headIndex,
   count, timeout)` (`src/mxl.hpp:197`). Use it the way the video path uses `getGrain`, and drop
   the `mxlSleepUntil`. The writer then wakes the loop, and both the timer and the
   `isTooEarly()` path go away.

Option 2 matches the discrete path and follows the source's real write times, so I'd take it.

## 4. Related things found on the way

- **`isTooEarly()` busy-spins.** It does `continue` without sleeping. With index-based pacing it
  runs on every iteration where the source is behind its own timestamps, so it needs a short sleep
  or a blocking read.
- **`mxl_grains_lost` has different units per format.** For audio it counts samples:
  `Target::transferSamples` adds `headIndex - _lastIndex`. For video it counts grains. The metric
  help text says "Index gap observed since the previous grain". Dashboards that sum it across
  formats mix units. Either document the unit or count lost batches for audio.
- **Video targets also run slightly below their rate.** `rate(mxl_grains_total{direction="target",
  format="video"}[10m])` is 49.5–49.8 on k8n1, k8n3 and k8n4, with 1.5k–3k grains lost per node
  every 10 minutes. The cause is something else, because video latency doesn't drift. I haven't
  investigated it.

## 5. Verifying the fix

After a fix, these should hold for audio flows on the replicas:

- `max_over_time(mxl_flow_latency_grains[1h]) / 48` stays near the source's value plus the
  transfer time, instead of ~234 ms.
- `increase(mxl_grains_lost{direction="target", format="audio"}[10m])` is 0.
- `rate(mxl_grains_total{direction="target", format="audio"}[10m])` is 100.

The "MXL - Node flow overview" Grafana dashboard (uid `mxl-node-flow-overview`) shows the sawtooth
under "Per-flow detail: audio" for any replica node.
