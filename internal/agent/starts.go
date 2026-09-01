package agent

import (
	"context"
	"sync"
	"time"

	"golang.org/x/time/rate"
)

// startGate paces worker starts across this node (§6.3).
//
// Every start passes through it — a first start, a replacement for a new epoch, a restart after a
// death — because what it protects is consumed by the *act of starting* a worker rather than by
// running one, and a worker coming back from a crash loop consumes it exactly as a freshly
// assigned one does. **Stops never pass through it.** A withdrawal made to wait for a permit
// would hold a service and a flow open for no reason, and it would take away §6's property that a
// fleet-wide withdrawal costs one grace period rather than N.
//
// # Why a token bucket rather than a limit on starts in flight
//
// A concurrency gate is the more adaptive shape and it was the first answer: it releases a slot
// the moment a start completes, so it costs nothing on a host that is keeping up and paces itself
// on one that is not. It needs a signal saying a start has *finished*, and only half the workers
// have one. A target's is `target-info.json` (§5.3 step 3); an initiator has none at all, because
// "ready" for it means the process is up and its connect loop has begun (§6) — which is before it
// has registered anything. A gate would therefore release an initiator's slot immediately and
// protect nothing on the side of a session that a fleet-wide re-establishment produces just as
// many of, and giving the initiator a readiness signal is a change to WRS rather than to this
// package.
//
// So: a bucket, whose **burst is the operationally meaningful number** — how many workers may go
// into setup at the same instant, which is the quantity a node's pinned memory, registration
// limits and CPU are actually measured against — and whose rate bounds the tail.
type startGate struct {
	// limiter is nil when rate control is off, which is what a rate of zero means (§6.3). The
	// sentinel deliberately points that way: a zero that meant "admit nothing" would be a
	// configuration typo that silently stops every flow on the node, where a zero that means "no
	// limit" is the behaviour this project had before the knob existed. It is the same direction
	// §2.2 takes for the worker idle timeout.
	limiter *rate.Limiter

	// mu guards the counters. They are written by every supervision goroutine that has to wait and
	// read by the metrics collector (§12).
	mu      sync.Mutex
	waiting int
	delayed uint64
	waited  time.Duration
}

// newStartGate builds the gate. A rate of zero or less admits everything immediately.
func newStartGate(perSecond float64, burst int) *startGate {
	if perSecond <= 0 {
		return &startGate{}
	}
	// A burst below one is the one setting that could stop a node's media outright: the limiter
	// would admit nothing, forever, and every flow on the node would sit in `starting` with a
	// reason nobody reads until they look. Refused at parse time (`--agent-start-burst`), and
	// clamped here as well, because the failure is bad enough to be worth two guards.
	if burst < 1 {
		burst = 1
	}
	return &startGate{limiter: rate.NewLimiter(rate.Limit(perSecond), burst)}
}

// admit blocks until this node may start another worker. It reports false only when ctx ended
// first, which is the caller's signal to stop rather than to start.
//
// onWait is called if — and only if — the start actually has to wait. That is what keeps the
// ordinary case free: an unthrottled start neither logs, nor moves a counter, nor changes the
// reason on a session, so it produces no status snapshot that differs from the last one and no
// store write anywhere in the fleet (§6, §8.3).
//
// Cancellation is the whole of the error handling. With a burst of at least one and no deadline
// on ctx, [rate.Limiter.Wait] fails for no other reason, and a cancelled wait puts its
// reservation back — so a worker withdrawn while queued does not spend a permit on its way out.
func (g *startGate) admit(ctx context.Context, onWait func()) bool {
	if ctx.Err() != nil {
		return false
	}
	if g.limiter == nil || g.limiter.Allow() {
		return true
	}

	// Wall clock, deliberately: the limiter schedules on it, so a wait measured against anything
	// else could not describe the same thing.
	started := time.Now()

	g.mu.Lock()
	g.waiting++
	g.delayed++
	g.mu.Unlock()

	if onWait != nil {
		onWait()
	}

	err := g.limiter.Wait(ctx)

	g.mu.Lock()
	g.waiting--
	g.waited += time.Since(started)
	g.mu.Unlock()

	return err == nil
}

// stats renders the gate for the metrics collector (§12).
func (g *startGate) stats() (waiting int, delayed uint64, waited time.Duration) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.waiting, g.delayed, g.waited
}
