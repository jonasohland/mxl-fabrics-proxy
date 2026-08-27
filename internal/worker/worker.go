package worker

import (
	"context"
	"errors"
	"time"
)

var (
	// ErrNotTarget is [Handle.TargetInfo] called on an initiator. Only a target produces a
	// blob (WRS §3, §4).
	ErrNotTarget = errors.New("worker: not a target")

	// ErrExited is a worker that died before it could answer. Returned by [Handle.TargetInfo]
	// when the process goes away before writing its blob — the common case being a bad domain
	// path, which kills a target before its metrics socket even exists (WRS §5.1) — and by
	// [Handle.Metrics] for a worker that is no longer running.
	ErrExited = errors.New("worker: exited")
)

// Launcher starts workers. The one interface the agent's supervision code is written against.
type Launcher interface {
	// Start runs one worker and returns a handle to it.
	//
	// ctx bounds the *start*, not the worker's lifetime. This is the important thing about
	// this method and the easy thing to get wrong: a started worker outlives ctx and is
	// stopped only by [Handle.Stop]. Binding a worker's life to the context of the reconcile
	// pass that started it would mean a cancelled poll tears down media, which is the fail-static
	// invariant read backwards (§4.2).
	//
	// The spec is validated before anything is started, so a Start that returns an error has
	// created nothing.
	Start(ctx context.Context, spec Spec) (Handle, error)
}

// Handle is one running worker.
//
// Methods are safe to call concurrently, and every one of them is safe to call on a worker
// that has already exited — the agent finds out that a worker died by watching [Handle.Exited],
// which races with whatever else it happens to be doing.
type Handle interface {
	// Spec returns what this worker was started with, unchanged. The worker reads its config
	// exactly once at startup (WRS §1), so this stays true for the handle's whole life, and
	// Spec().Key() is the agent's "already correct" test in one expression (§7.3).
	Spec() Spec

	// TargetInfo blocks until the target's blob is available, the worker exits, or ctx ends.
	// Target role only; an initiator gets [ErrNotTarget].
	//
	// Blocking rather than polling is deliberate. The blob is the agent's signal that the
	// target is up (WRS §5.1 step 6), and the peer's initiator cannot be assigned until it has
	// been reported (§5.3), so this sits directly on the establishment path that §6.1 wants
	// inside 1–2 s. The exec implementation uses inotify; the legacy 200 ms → 2 s backoff poll
	// spent most of that budget on its own.
	//
	// The blob is returned verbatim and treated as opaque (WRS §4).
	TargetInfo(ctx context.Context) (string, error)

	// Metrics returns one point-in-time scrape (WRS §6). Unlabelled: the caller knows the
	// session, the role and the flow, and the worker emits neither labels nor TYPE lines.
	Metrics(ctx context.Context) ([]Sample, error)

	// Exited delivers this worker's [Exit] exactly once and is then closed.
	//
	// Single-consumer: there is one supervising goroutine per handle, and a second receive
	// yields the zero Exit. Watch it rather than polling — the agent learns that a worker died
	// from here, and everything downstream (restart backoff, DEGRADED classification, the
	// status report that tells the peer its epoch is gone) hangs off it.
	Exited() <-chan Exit

	// Stop terminates the worker and releases everything it holds — for the exec
	// implementation, SIGTERM, a grace period, SIGKILL, and the work directory.
	//
	// Idempotent, and safe on a worker that has already died. ctx bounds how long the caller
	// is willing to wait for a clean exit; an implementation must still get rid of the process
	// even when it expires, because a worker left behind holds a port, a memory registration
	// and a flow (§7.4, WRS §9).
	Stop(ctx context.Context) error
}

// Exit is how a worker ended.
type Exit struct {
	// At is when the exit was observed.
	At time.Time

	// Stopped reports that this exit was asked for — [Handle.Stop] was called. The agent knows
	// a death was unexpected because it did not ask for it, and that is the only signal here
	// with any classification value.
	Stopped bool

	// Err describes an unclean exit, and is **diagnostics only**.
	//
	// Never classify a failure from it (§15.1, WRS §8). The worker's exit status distinguishes
	// success from failure and nothing more: one non-zero bucket holds invalid config and
	// unusable providers, which are permanent, alongside timeouts and the flow-not-found
	// startup race, which are transient. The signals that work are behavioural and the agent
	// can compute all of them — restart rate over a window, time to death, and source liveness
	// from the head index (§11.1).
	Err error
}

// Sample is one line of a worker's metrics output (WRS §6).
type Sample struct {
	Name string

	// Quantile is set for a summary line (`name[q] value`) and nil for a counter. The worker's
	// summaries are CKMS estimates over a sliding 30 s window.
	Quantile *float64

	// Value may be NaN: a summary with no observations in its window emits `nan`, which means
	// "nothing measured", not zero. Pass it through rather than dropping or zeroing it —
	// Prometheus renders NaN natively and a zeroed latency quantile is a lie.
	Value float64
}

// IsSummary reports whether this sample is a summary quantile rather than a counter.
func (s Sample) IsSummary() bool { return s.Quantile != nil }

// Counter builds a counter sample.
func Counter(name string, value float64) Sample {
	return Sample{Name: name, Value: value}
}

// Quantile builds a summary sample.
func Quantile(name string, quantile, value float64) Sample {
	return Sample{Name: name, Quantile: &quantile, Value: value}
}
