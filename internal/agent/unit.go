package agent

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"slices"
	"sync"
	"time"

	"github.com/jonasohland/mxl-replicator/internal/api"
	"github.com/jonasohland/mxl-replicator/internal/epoch"
	"github.com/jonasohland/mxl-replicator/internal/worker"
)

// unitKey identifies one supervised worker: a session and which end of it this is.
//
// One session has at most one worker per role on any node, which is what makes this a key rather
// than a list — and what makes a duplicate worker for a session impossible to produce by
// reconciling rather than something that has to be checked for afterwards.
type unitKey struct {
	Session string
	Role    api.Role
}

// String is also the port allocator's owner name, so a worker restarted for a new epoch, after a
// crash, or after its backoff gets the same service back (§7.4).
func (k unitKey) String() string { return k.Session + "/" + string(k.Role) }

func (k unitKey) compare(other unitKey) int {
	if c := cmpString(k.Session, other.Session); c != 0 {
		return c
	}
	return cmpString(string(k.Role), string(other.Role))
}

// unit supervises one worker: start it, watch it, classify its death, back off, start it again.
//
// It owns the whole lifecycle on its own goroutine, so that a reconcile pass never blocks on a
// process coming up — which matters because a target's start includes waiting for its blob, the
// one part of establishment with an unbounded-looking wait in it.
type unit struct {
	key  unitKey
	spec worker.Spec

	// pathID is the object this unit's event-log entries are anchored on (§12.1). Empty when the
	// server did not send one, in which case what this unit observes lands on the node's log
	// rather than being dropped — a server one version behind must not make a node's diagnostics
	// disappear (§13.1).
	pathID string

	agent *Agent
	log   *slog.Logger

	// flow labels the metrics with what kind of media this is, resolved once here and frozen for
	// the unit's life (§12). Frozen deliberately: a label whose value changes mid-life splits one
	// series into two, so it must not depend on anything that arrives later.
	flow flowLabels

	cancel context.CancelFunc
	done   chan struct{}

	mu         sync.Mutex
	state      api.WorkerState
	epoch      string
	targetInfo string
	reason     string
	reasonCode api.ReasonCode
	startedAt  time.Time

	// restarts is the *windowed* list DEGRADED and FAILED are classified from (§15.1); total is
	// the monotonic count `mxl_worker_restarts` reports. Two representations of one event on
	// purpose: a counter that decays reads as a reset to `rate()` every time the window slides,
	// and a total that never decays cannot classify a burst.
	restarts []time.Time
	total    uint64

	// lastTail is the output of the most recent worker start, captured as the attempt unwinds
	// because the handle that holds it is withdrawn before the death is classified.
	lastTail string

	// tailSent reports that this crash loop's worker log has already been pushed (§12.2).
	//
	// **One tail per crash loop, not one per restart.** Forty-seven restarts produce forty-seven
	// copies of the same fatal line, and the forty-seventh is not evidence, it is volume.
	//
	// What re-arms it is *time to death*, not the worker having reached ready — the same signal
	// §15.1 classifies from, and for the same reason. A target that binds, writes its blob and
	// then dies on a timeout reaches ready on **every** cycle, so a readiness-based reset would
	// push a tail per restart while looking like it did not.
	tailSent bool

	// handle is the worker currently running, or nil between attempts. Held so that /metrics can
	// scrape it (§12) — it is the only reader, and the supervision loop is the only writer.
	handle worker.Handle
}

func (a *Agent) newUnit(key unitKey, spec worker.Spec, pathID string) *unit {
	return &unit{
		key:    key,
		spec:   spec,
		pathID: pathID,
		agent:  a,
		flow:   a.flowLabelsFor(spec),
		log: a.log.With(
			"session", key.Session,
			"role", string(key.Role),
			"domain", spec.DomainPath,
			"flow", spec.FlowID),
		done:  make(chan struct{}),
		state: api.WorkerStarting,
	}
}

// start launches the supervision goroutine.
//
// ctx is [Agent.root] — the one Run was given — never a poll's or a reconcile pass's. A worker
// whose life was bound to the pass that started it would be torn down by a cancelled poll, which
// is fail-static read backwards (§4.2). In practice a unit ends because [unit.stop] was called;
// the context is the backstop for the agent itself going away.
func (u *unit) start(ctx context.Context) {
	ctx, u.cancel = context.WithCancel(ctx)
	go u.run(ctx)
}

// stop terminates the worker and waits for the supervision goroutine to finish.
//
// Blocking is deliberate: the caller is about to start a replacement for the same session, and a
// second worker overlapping the first would hold the same port and write into the same flow.
func (u *unit) stop() {
	u.cancel()
	<-u.done
}

// run is the supervision loop.
func (u *unit) run(ctx context.Context) {
	defer close(u.done)

	backoff := u.agent.cfg.BackoffMin

	for {
		if ctx.Err() != nil {
			return
		}

		startedAt := u.agent.now()
		exit, keepGoing := u.attempt(ctx)
		if !keepGoing {
			return
		}
		if exit.Stopped || ctx.Err() != nil {
			return
		}

		lived := u.agent.now().Sub(startedAt)
		u.died(exit, lived)

		// Time to death is the signal, not the exit status (§15.1). A worker that ran for a
		// while and then died is a transient failure and starts over at the shortest delay; one
		// dying immediately, every attempt, is a permanent error and the delay stretches toward
		// minutes.
		if lived >= u.agent.cfg.BackoffReset {
			backoff = u.agent.cfg.BackoffMin
		}

		u.log.Info("worker died; restarting", "lived", lived.Round(time.Millisecond), "in", backoff, "error", exit.Err)
		if !sleep(ctx, backoff) {
			return
		}
		backoff = nextBackoff(backoff, u.agent.cfg.BackoffMin, u.agent.cfg.BackoffMax)
	}
}

// attempt runs one worker to completion.
//
// It returns the exit and whether the loop should carry on. A failure to *start* is returned as a
// synthetic exit rather than as an error, because the supervision policy is identical either way:
// count it, back off, try again. Classifying a start failure differently from a death would need
// the distinction to mean something, and it does not — a bad domain path kills a target before
// its metrics socket exists, which looks like neither (WRS §5.1).
func (u *unit) attempt(ctx context.Context) (worker.Exit, bool) {
	// Rate control, and note where it sits: inside the supervision goroutine, so a start that has
	// to wait for a permit delays this worker and nothing else (§6.3). Reconcile never blocks on a
	// start (§6), which is what keeps a queued start from stopping the node from noticing the
	// assignment that would withdraw it — including this one.
	//
	// Before the state is stamped, because the queue is not the process: [unit.startedAt] anchors
	// how long this worker has been *running*, which is what the server classifies from (§15.1),
	// and a permit wait is not running.
	if !u.agent.starts.admit(ctx, u.waitingForPermit) {
		return worker.Exit{At: u.agent.now(), Stopped: true}, false
	}

	u.setState(api.WorkerStarting, "", "")

	// The worker does not create its domain directory (WRS §5.1). Writable areas are pre-created
	// at startup and the reconciler materialises the domain itself (§6.1, §10.6); this covers one
	// that has been removed since, and costs a stat when it has not.
	if err := os.MkdirAll(u.spec.DomainPath, 0o755); err != nil {
		return u.startFailed("create domain directory " + u.spec.DomainPath + ": " + err.Error()), true
	}

	// One nonce per target worker *start*, held in memory alongside the handle and persisted
	// nowhere. That is what makes a target which restarts and produces a byte-identical blob
	// still produce a new epoch, and therefore still make its initiator reconnect — the failure
	// it prevents being an initiator running happily against rkeys that no longer exist (§5.2).
	nonce := epoch.NewNonce()

	handle, err := u.agent.cfg.Launcher.Start(ctx, u.spec)
	if err != nil {
		return u.startFailed("start worker: " + err.Error()), true
	}

	// Published for the metrics collector and withdrawn on every exit from this attempt, so a
	// scrape never holds a handle to a worker the supervision loop has moved past. A scrape that
	// races a death is not special — it gets [worker.ErrExited] and contributes nothing.
	u.setHandle(handle)
	defer func() {
		// The tail is taken here rather than in [unit.died], because by the time that runs the
		// handle has been withdrawn — and the handle is the only thing holding this start's
		// output (§12.2). The pumps are drained before an exit is published, so what is captured
		// here is complete.
		u.captureTail(handle)
		u.setHandle(nil)
	}()

	if u.spec.IsTarget() {
		if !u.captureTargetInfo(ctx, handle, nonce) {
			u.terminate(ctx, handle)
			return worker.Exit{At: u.agent.now()}, true
		}
	} else {
		// An initiator has no readiness signal of its own: it opens the local flow and enters its
		// connect loop, and the first thing that says the pairing worked is media arriving at the
		// destination. "Ready" here means the process is up, which is all an agent can report —
		// whether media is moving is the flow's answer, and the server derives ACTIVE from it
		// (§11).
		u.ready("", "")
	}

	select {
	case exit := <-handle.Exited():
		return exit, true
	case <-ctx.Done():
		u.terminate(ctx, handle)
		return worker.Exit{At: u.agent.now(), Stopped: true}, false
	}
}

// captureTargetInfo waits for the target's blob and derives the epoch from it (§5.3 step 4).
func (u *unit) captureTargetInfo(ctx context.Context, handle worker.Handle, nonce string) bool {
	waitCtx, cancel := context.WithTimeout(ctx, u.agent.cfg.TargetInfoTimeout)
	defer cancel()

	blob, err := handle.TargetInfo(waitCtx)
	if err != nil {
		switch {
		case ctx.Err() != nil:
			return false
		case errors.Is(err, worker.ErrExited):
			u.setState(api.WorkerFailed, "target died before it produced a target info blob", "")
		default:
			u.setState(api.WorkerFailed, "waiting for target info: "+err.Error(), "")
		}
		return false
	}

	info, unknown, err := epoch.Decode(blob)
	if err != nil {
		// A blob that will not decode fails here, with a message, rather than in an initiator on
		// another node where it would look like a fabric problem.
		u.setState(api.WorkerFailed, "target info: "+err.Error(), "")
		return false
	}
	if len(unknown) > 0 {
		// Warn, never fail: this is the live half of the mxl-fabrics coupling guard, and an
		// unknown field is far more likely to be additive than epoch-relevant (§5.2). Failing
		// closed would take out replication on an unrelated mxl upgrade.
		u.log.Warn("target info carries fields this build does not know about; they are not in the epoch",
			"fields", unknown)
	}

	u.ready(epoch.Compute(nonce, info), blob)
	return true
}

// terminate stops a worker with a bounded patience, on a context that is not the one that just
// ended — otherwise a shutdown would skip the signal sequence it exists to perform.
func (u *unit) terminate(ctx context.Context, handle worker.Handle) {
	stopCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), u.agent.cfg.StopGrace)
	defer cancel()

	if err := handle.Stop(stopCtx); err != nil {
		u.log.Warn("stopping the worker did not complete cleanly", "error", err)
	}
}

// waitingForPermit records that this worker is queued behind the start gate (§6.3).
//
// Reported rather than left silent: a worker that has not been launched yet is otherwise
// indistinguishable from one that was launched and is coming up slowly, and an operator watching
// a bulk re-establishment needs to be able to tell "the node is pacing itself" from "this session
// is stuck". It is only ever reached on a start that actually waited, so the unthrottled path
// still produces no status change at all.
//
// Not [unit.setState]: the state is already `starting` and re-stamping [unit.startedAt] here
// would count the queue as running time.
func (u *unit) waitingForPermit() {
	u.log.Info("waiting for a permit to start")

	u.mu.Lock()
	u.state, u.reason, u.reasonCode = api.WorkerStarting, "waiting for a permit to start", ""
	u.mu.Unlock()

	// A queued start is otherwise indistinguishable from one that launched and is coming up
	// slowly, which is exactly the distinction an operator watching a slow recovery needs (§6.3).
	// The status says so while it lasts; this says it happened.
	// Info, not a warning: pacing is what §6.3 is *for*, and its defaults are deliberately
	// conservative, so a queued start is the node working correctly under a bulk re-establishment.
	// It is recorded because a queued start is otherwise indistinguishable from a stuck one — which
	// is an argument for writing it down, not for alarming about it.
	u.emit(api.EventWorkerStartQueued, api.SeverityInfo,
		"start is queued behind the node's start rate limit", "", "")

	u.agent.Notify()
}

// emit queues one entry about this unit, anchored on its path (§12.1).
func (u *unit) emit(kind api.EventKind, severity api.EventSeverity, message string, code api.ReasonCode, log string) {
	u.agent.emit(api.AgentEvent{
		Path:       u.pathID,
		Kind:       kind,
		Severity:   severity,
		At:         u.agent.now(),
		Message:    message,
		ReasonCode: code,
		Session:    u.key.Session,
		Role:       u.key.Role,
		Log:        log,
	})
}

func (u *unit) setState(state api.WorkerState, reason string, code api.ReasonCode) {
	u.mu.Lock()
	u.state, u.reason, u.reasonCode = state, reason, code
	if state == api.WorkerStarting {
		u.startedAt = u.agent.now()
	}
	u.mu.Unlock()

	u.agent.Notify()
}

// ready publishes a running worker, and for a target the epoch and blob its peer needs.
func (u *unit) ready(epochValue, blob string) {
	u.mu.Lock()
	u.state, u.reason, u.reasonCode = api.WorkerReady, "", ""
	u.epoch, u.targetInfo = epochValue, blob
	u.mu.Unlock()

	if epochValue != "" {
		u.log.Info("target is up", "epoch", epochValue, "service", u.spec.Service)
	} else {
		u.log.Info("initiator is up", "service", u.spec.Service)
	}

	// Immediately, not on the next tick: the peer's initiator cannot be assigned until this epoch
	// reaches the server, and that sits directly on §6.1's 1–2 s re-establishment budget.
	u.agent.Notify()
}

// startFailed records a worker that never got as far as running.
func (u *unit) startFailed(reason string) worker.Exit {
	u.log.Error("worker did not start", "reason", reason)
	u.setState(api.WorkerFailed, reason, "")

	// No tail: there is no process, so there is no output to have captured. The reason string is
	// the whole diagnosis here, which is why it carries the resolved path or the allocation error
	// verbatim rather than a summary.
	u.emit(api.EventWorkerExited, api.SeverityError, "worker did not start: "+reason, "", "")

	return worker.Exit{At: u.agent.now(), Err: errors.New(reason)}
}

// died records an unexpected exit, and pushes the worker's own account of it (§12.2).
//
// The tail is what makes this entry worth more than the status it duplicates: `FAILED` /
// `worker_restarts` says a worker keeps dying, and `fatal: unknown error: failed to create flow
// writer` says why. Without this the second sentence is on the node, in the agent's log, reachable
// only with shell access to a fleet member — which is what centralising the control plane was
// supposed to remove.
func (u *unit) died(exit worker.Exit, lived time.Duration) {
	reason := "worker exited unexpectedly after " + lived.Round(time.Millisecond).String()
	if exit.Err != nil {
		reason += ": " + exit.Err.Error()
	}

	u.mu.Lock()
	tail := u.lastTail
	// A worker that ran for a while before dying is a new incident rather than the next turn of a
	// loop, so it gets a fresh tail: whatever killed it after minutes of healthy transfer is not
	// what the first attempt's output describes.
	send := tail != "" && (!u.tailSent || lived >= u.agent.cfg.BackoffReset)
	if send {
		u.tailSent = true
	}
	u.state, u.reason, u.reasonCode = api.WorkerFailed, reason, api.ReasonWorkerRestarts
	// A dead target's blob describes memory registrations that died with it, so it is worse than
	// no answer: reporting it would let the server keep an initiator assigned against rkeys that
	// no longer exist.
	u.epoch, u.targetInfo = "", ""
	u.restarts = append(u.restarts, u.agent.now())
	u.total++
	u.mu.Unlock()

	if !send {
		tail = ""
	}
	u.emit(api.EventWorkerExited, api.SeverityError, reason, api.ReasonWorkerRestarts, tail)
}

// status renders this worker as the server sees it (§9.2).
func (u *unit) status(now time.Time, window time.Duration) api.SessionStatus {
	u.mu.Lock()
	defer u.mu.Unlock()

	// Restart *rate* over a window is what DEGRADED and FAILED are classified from, never an
	// exit code (§15.1), so the count decays as failures age out rather than accumulating for the
	// life of the process.
	cutoff := now.Add(-window)
	u.restarts = slices.DeleteFunc(u.restarts, func(at time.Time) bool { return at.Before(cutoff) })

	status := api.SessionStatus{
		SessionID:  u.key.Session,
		Role:       u.key.Role,
		State:      u.state,
		Epoch:      u.epoch,
		TargetInfo: u.targetInfo,
		Restarts:   len(u.restarts),
		StartedAt:  u.startedAt,
		Reason:     u.reason,
		ReasonCode: u.reasonCode,
	}

	if u.spec.IsTarget() {
		// What this target actually bound. The agent has ground truth and the server cannot
		// verify a port it hands out, so this is the only place it can come from (§7.4).
		status.Address, status.Service = u.spec.BindAddress, u.spec.Service
	}

	return status
}

// desired returns the spec this unit is supervising.
func (u *unit) desired() worker.Spec { return u.spec }

// captureTail retains one start's output, so the death that follows can be explained.
func (u *unit) captureTail(handle worker.Handle) {
	tail := handle.LogTail()

	u.mu.Lock()
	defer u.mu.Unlock()
	u.lastTail = tail
}

func (u *unit) setHandle(handle worker.Handle) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.handle = handle
}

// restartCount is the monotonic total for `mxl_worker_restarts`.
func (u *unit) restartCount() uint64 {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.total
}

// running returns the handle to scrape, or nil when nothing is running.
//
// The handle is taken under the lock and used outside it. Holding [unit.mu] across a scrape
// would put a worker's socket latency in front of every status report this unit produces, and
// the report loop is the establishment path (§5.3).
func (u *unit) running() worker.Handle {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.handle
}

func sortSessions(sessions []api.SessionStatus) {
	slices.SortFunc(sessions, func(a, b api.SessionStatus) int {
		return unitKey{a.SessionID, a.Role}.compare(unitKey{b.SessionID, b.Role})
	})
}

func cmpString(a, b string) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	default:
		return 0
	}
}
