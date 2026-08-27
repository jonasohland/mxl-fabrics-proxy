// Package leader elects the one replica that reconciles (§8.2).
//
// Every replica serves the API; only the leader writes to /derived/. Without that, replicas
// fight: CAS retries, assignment thrash, and two servers making the same decision from slightly
// different snapshots and writing different answers.
//
// The [Elector] interface exists so the server has no backend conditional in it. With sqlite
// there is one process by definition, and it is always the leader; with etcd, leadership is a
// campaign on a session lease, and losing it cancels the reconciler's context.
package leader

import (
	"context"
	"log/slog"
	"time"
)

// Elector runs work while this replica is the leader.
type Elector interface {
	// Run campaigns for leadership and calls lead each time it is won, with a context that is
	// cancelled when it is lost. It returns when ctx ends.
	//
	// lead may be called more than once: leadership can be lost and won again, and each call is
	// a fresh term. It must therefore be safe to start over — which the reconciler is, since it
	// derives everything from the store and observes the settling window on every term (§7.3).
	Run(ctx context.Context, lead func(context.Context) error) error

	// Name identifies this replica, for the reconciler record and for logs.
	Name() string
}

// Always is the elector for a single-process deployment: this replica leads, immediately and
// forever.
//
// Not a stub. With the sqlite backend there is exactly one process that can write the store at
// all (§8.2), so election is not a thing being skipped — it is a question with one possible
// answer.
type Always struct {
	Replica string
}

// Run calls lead once, with ctx.
func (a Always) Run(ctx context.Context, lead func(context.Context) error) error {
	return lead(ctx)
}

// Name identifies this replica.
func (a Always) Name() string {
	if a.Replica == "" {
		return "single"
	}
	return a.Replica
}

// Options are shared by the elector implementations.
type Options struct {
	// Replica identifies this process. A hostname plus a random suffix is the usual choice: it
	// has to be unique, or two replicas campaign as one identity.
	Replica string

	// TTL is the election lease. Leadership is lost this long after a leader stops renewing, so
	// it bounds how long a partitioned leader can still believe it leads — which is why every
	// derived write is a CAS rather than trusting the election alone (§4.6).
	TTL time.Duration

	Logger *slog.Logger
}

func (o *Options) setDefaults() {
	if o.TTL <= 0 {
		o.TTL = DefaultTTL
	}
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
}

// DefaultTTL is the election lease TTL.
//
// Short enough that a crashed leader is replaced promptly, long enough to survive a GC pause or
// a scheduling stall on a node running SCHED_FIFO workers — where a missed renewal is lost
// leadership, and leadership churn means repeated settling windows and reconcile thrash (plan
// §4.4).
const DefaultTTL = 15 * time.Second
