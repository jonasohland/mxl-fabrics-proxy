// Package fake is an in-process [worker.Launcher] for tests.
//
// It is written in the same milestone as the interface it implements, not after it, because
// it is the reason the interface exists at all: with it, the whole control plane — discovery,
// inventory, requests, paths, sessions, assignments, epoch convergence — is testable in a temp
// directory on any machine, with no MXL, no libfabric and no RDMA hardware (§17).
//
// It starts no processes and touches no files. What it does do is behave like the real worker
// in the three ways the control plane depends on:
//
//   - it produces a target_info blob shaped like the library's, so [epoch.Decode] accepts it
//     and epochs computed from it are real epochs;
//   - it produces a **different** blob on every start by default, and can be pinned to produce
//     a byte-identical one, which is the degenerate case the incarnation nonce exists for
//     (§5.2);
//   - it dies when told to, which is what the agent's restart, backoff and DEGRADED
//     classification paths are written against.
//
// Everything it was asked to run is recorded, in order, so a test can assert on what did *not*
// happen — "the server restarted and no worker was restarted" is the shape of half the tests
// in M7, and it is an assertion about [Launcher.Starts].
//
// Not safe against misuse and not meant to be: it is a test double, and a Spec it refuses is a
// bug in the caller.
package fake

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/jonasohland/mxl-replicator/internal/api"
	"github.com/jonasohland/mxl-replicator/internal/worker"
)

// closed is a channel that is always ready, used as the "not holding anything back" case.
var closed = func() chan struct{} {
	c := make(chan struct{})
	close(c)
	return c
}()

// Launcher is a [worker.Launcher] that runs nothing.
//
// The zero value is ready to use. Every method is safe to call concurrently, including from a
// test goroutine while the code under test is running.
type Launcher struct {
	mu sync.Mutex

	starts   []worker.Spec
	handles  []*Handle
	nStarts  int
	failNext error
	metrics  []worker.Sample

	fixedInfo string
	infoFn    func(spec worker.Spec, seq int) string

	hold chan struct{}
}

// New returns a Launcher. The zero value works too; this exists to read better at call sites.
func New() *Launcher { return &Launcher{} }

// Start implements [worker.Launcher].
//
// It validates the spec exactly as a real launcher does, so a control-plane test that builds a
// nonsensical assignment fails in that test rather than in a restart loop that looks like a
// fabric problem. A refused start is still recorded in [Launcher.Starts].
func (l *Launcher) Start(ctx context.Context, spec worker.Spec) (worker.Handle, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := spec.Validate(); err != nil {
		return nil, err
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	l.starts = append(l.starts, spec)
	l.nStarts++

	if err := l.failNext; err != nil {
		l.failNext = nil
		return nil, err
	}

	h := &Handle{
		launcher: l,
		spec:     spec,
		seq:      l.nStarts,
		dead:     make(chan struct{}),
		exited:   make(chan worker.Exit, 1),
	}
	if spec.IsTarget() {
		h.info = l.targetInfoLocked(spec, l.nStarts)
	}
	l.handles = append(l.handles, h)
	return h, nil
}

func (l *Launcher) targetInfoLocked(spec worker.Spec, seq int) string {
	switch {
	case l.fixedInfo != "":
		return l.fixedInfo
	case l.infoFn != nil:
		return l.infoFn(spec, seq)
	default:
		return TargetInfo(spec, seq)
	}
}

// SetTargetInfo pins every subsequent target to produce exactly this blob.
//
// This is how a test reaches the case the nonce exists for: a target that restarts and reports
// a **byte-identical** target_info must still cause its initiator to reconnect, because the
// rkeys behind those bytes are gone. Measured against a real tcp target, that case is not
// hypothetical — `fabricAddress` is identical across a restart because the agent reuses the
// port by design, `addr` is `"0"` in every region because the provider reports no mapping
// address at all, and only the rkey varied (§5.2).
//
// Pass "" to go back to a fresh blob per start.
func (l *Launcher) SetTargetInfo(blob string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.fixedInfo = blob
}

// SetTargetInfoFunc installs a generator, called once per target start with the spec and a
// 1-based start sequence number. Ignored while [Launcher.SetTargetInfo] holds a fixed blob.
// Pass nil to restore the default.
//
// It is called while the launcher is locked, so it must not call back into the Launcher.
func (l *Launcher) SetTargetInfoFunc(fn func(spec worker.Spec, seq int) string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.infoFn = fn
}

// HoldTargetInfo makes [Handle.TargetInfo] block, on every handle, until it is called again
// with false — a target that started but has not come up yet.
//
// The establishment path is only correct if the agent waits for the blob rather than reporting
// a target ready without one, and if a target that never comes up eventually gives up on the
// caller's terms rather than the launcher's. Holding is how a test drives both.
func (l *Launcher) HoldTargetInfo(hold bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if hold {
		if l.hold == nil {
			l.hold = make(chan struct{})
		}
		return
	}
	if l.hold != nil {
		close(l.hold)
		l.hold = nil
	}
}

func (l *Launcher) holdChan() <-chan struct{} {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.hold == nil {
		return closed
	}
	return l.hold
}

// FailNextStart makes the next [Launcher.Start] return err. One-shot.
func (l *Launcher) FailNextStart(err error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.failNext = err
}

// SetMetrics installs what every handle returns from [Handle.Metrics].
func (l *Launcher) SetMetrics(samples []worker.Sample) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.metrics = slices.Clone(samples)
}

// Starts returns every spec this launcher was asked to run, in order, including specs it
// refused.
//
// The history rather than the current state, because the assertions that matter are usually
// negative: a server restart with the fleet already in the desired state must produce **no**
// new starts (§7.3), and an assignment differing only in incidental fields must produce none
// either (§7.3, [worker.Spec.Key]). Neither is visible in a snapshot of what is running.
func (l *Launcher) Starts() []worker.Spec {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.starts)
}

// StartCount is len(Starts()), including refused starts.
func (l *Launcher) StartCount() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.nStarts
}

// Handles returns every handle this launcher has created, dead ones included, in start order.
func (l *Launcher) Handles() []*Handle {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.handles)
}

// Running returns the handles that have not exited, in start order.
func (l *Launcher) Running() []*Handle {
	all := l.Handles()
	live := make([]*Handle, 0, len(all))
	for _, h := range all {
		if h.Running() {
			live = append(live, h)
		}
	}
	return live
}

// Find returns the running handle for a session and role, or nil. Panics if two are running,
// which is a duplicate worker and never something a test should tolerate.
func (l *Launcher) Find(sessionID string, role api.Role) *Handle {
	var found *Handle
	for _, h := range l.Running() {
		spec := h.Spec()
		if spec.SessionID != sessionID || spec.Role != role {
			continue
		}
		if found != nil {
			panic(fmt.Sprintf("fake: two running %s workers for session %s", role, sessionID))
		}
		found = h
	}
	return found
}

// Handle is one fake worker.
type Handle struct {
	launcher *Launcher
	spec     worker.Spec
	seq      int
	info     string

	mu     sync.Mutex
	exit   worker.Exit
	done   bool
	nStops int

	dead   chan struct{}
	exited chan worker.Exit
}

var _ worker.Handle = (*Handle)(nil)

// Spec implements [worker.Handle].
func (h *Handle) Spec() worker.Spec { return h.spec }

// Seq is the 1-based start sequence number of this handle within its launcher. Useful for
// asserting *which* start a handle came from when a session has restarted.
func (h *Handle) Seq() int { return h.seq }

// TargetInfo implements [worker.Handle].
func (h *Handle) TargetInfo(ctx context.Context) (string, error) {
	if !h.spec.IsTarget() {
		return "", worker.ErrNotTarget
	}
	select {
	case <-h.launcher.holdChan():
		if !h.Running() {
			return "", worker.ErrExited
		}
		return h.info, nil
	case <-h.dead:
		return "", worker.ErrExited
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

// Metrics implements [worker.Handle].
func (h *Handle) Metrics(ctx context.Context) ([]worker.Sample, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !h.Running() {
		return nil, worker.ErrExited
	}
	h.launcher.mu.Lock()
	defer h.launcher.mu.Unlock()
	return slices.Clone(h.launcher.metrics), nil
}

// Exited implements [worker.Handle].
func (h *Handle) Exited() <-chan worker.Exit { return h.exited }

// Stop implements [worker.Handle]. Idempotent, and a no-op on a worker that already died.
func (h *Handle) Stop(_ context.Context) error {
	h.mu.Lock()
	h.nStops++
	h.mu.Unlock()
	h.finish(worker.Exit{At: time.Now(), Stopped: true})
	return nil
}

// Die makes this worker exit on its own, as if the process had failed. err may be nil, which
// is a clean exit nobody asked for — the worker self-terminating on its idle timeout.
func (h *Handle) Die(err error) {
	h.finish(worker.Exit{At: time.Now(), Err: err})
}

func (h *Handle) finish(exit worker.Exit) {
	h.mu.Lock()
	if h.done {
		h.mu.Unlock()
		return
	}
	h.done = true
	h.exit = exit
	h.mu.Unlock()

	close(h.dead)
	h.exited <- exit
	close(h.exited)
}

// Running reports whether this worker has not exited.
func (h *Handle) Running() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return !h.done
}

// Stops is how many times [Handle.Stop] was called, including calls after the worker was
// already dead.
func (h *Handle) Stops() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.nStops
}

// TargetInfo builds the default blob for a target start: the library's schema (WRS §4), with a
// distinct id and rkey per start so that consecutive incarnations differ.
//
// The values mirror what a real tcp target reported on mxl 1.1.0-rc1 rather than being freely
// invented — in particular `addr` is `"0"` in every region, because the tcp provider reports no
// mapping address at all, and `len` does not change across a restart. That leaves the rkey as
// the only field that varies, which is exactly the thin margin the incarnation nonce exists to
// not rely on (§5.2).
//
// `fabricAddress` is the bind endpoint in the clear rather than the library's base64, because
// it is opaque to everything in this project and a legible one makes a failing test legible.
func TargetInfo(spec worker.Spec, seq int) string {
	type region struct {
		Addr string `json:"addr"`
		Len  string `json:"len"`
		RKey string `json:"rkey"`
	}
	blob, err := json.Marshal(struct {
		ID            string   `json:"id"`
		AddressFormat int      `json:"addressFormat"`
		FabricAddress string   `json:"fabricAddress"`
		Provider      string   `json:"provider"`
		Regions       []region `json:"regions"`
	}{
		ID:            fmt.Sprintf("%d", 1000+seq),
		AddressFormat: 1,
		FabricAddress: fmt.Sprintf("%s:%s", spec.BindAddress, spec.Service),
		Provider:      string(spec.Interface.Provider),
		Regions: []region{{
			Addr: "0",
			Len:  "1048576",
			RKey: fmt.Sprintf("%d", 17918262359965949928+uint64(seq)),
		}},
	})
	if err != nil {
		panic(err) // unreachable: every field is a string, an int or a slice of those
	}
	return string(blob)
}
