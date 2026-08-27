package leader

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	clientv3 "go.etcd.io/etcd/client/v3"
	"go.etcd.io/etcd/client/v3/concurrency"

	"github.com/jonasohland/mxl-replicator/internal/store"
)

// Etcd elects a leader with etcd's concurrency package (§8.2).
type Etcd struct {
	cli  *clientv3.Client
	opts Options

	// prefix is the deployment's key prefix, the same one the store namespaces its keys with.
	// The election keys must live inside it or two deployments sharing a cluster elect one
	// leader between them — the failure being that half the fleet's reconciler is reconciling
	// the other half's assignments.
	prefix string
}

// NewEtcd builds an elector on an existing client.
//
// It takes the client rather than opening its own, so election and storage share one
// connection, one set of credentials and one set of keepalives. Two connections would allow a
// partition that takes out storage while leaving election intact — a leader that cannot read
// what it is leading — or the reverse.
func NewEtcd(cli *clientv3.Client, storePrefix string, opts Options) *Etcd {
	opts.setDefaults()
	return &Etcd{
		cli:    cli,
		opts:   opts,
		prefix: strings.TrimRight(storePrefix, "/") + store.PrefixElection,
	}
}

// Name identifies this replica.
func (e *Etcd) Name() string { return e.opts.Replica }

// Run campaigns until ctx ends, calling lead for each term won.
func (e *Etcd) Run(ctx context.Context, lead func(context.Context) error) error {
	for {
		err := e.term(ctx, lead)
		switch {
		case ctx.Err() != nil:
			return nil
		case err != nil:
			e.opts.Logger.Warn("leader election restarting", "error", err)
			select {
			case <-ctx.Done():
				return nil
			case <-time.After(e.opts.TTL / 3):
			}
		}
	}
}

// term is one session: campaign, lead, and give up leadership when the session ends.
func (e *Etcd) term(ctx context.Context, lead func(context.Context) error) error {
	session, err := concurrency.NewSession(e.cli,
		concurrency.WithContext(ctx),
		concurrency.WithTTL(int(max(e.opts.TTL.Seconds(), 1))))
	if err != nil {
		return fmt.Errorf("election session: %w", err)
	}
	defer func() {
		// Closing revokes the lease, which hands leadership over immediately rather than after a
		// TTL. On a clean shutdown that is the difference between a new leader in milliseconds
		// and a fleet whose reconciler is absent for fifteen seconds.
		_ = session.Close()
	}()

	election := concurrency.NewElection(session, e.prefix+"reconciler")

	e.opts.Logger.Debug("campaigning for leadership", "replica", e.opts.Replica)
	if err := election.Campaign(ctx, e.opts.Replica); err != nil {
		if ctx.Err() != nil {
			return nil
		}
		return fmt.Errorf("campaign: %w", err)
	}

	// Leadership is held for as long as the session lease is alive. The term's context is
	// cancelled when it is not, so the reconciler stops of its own accord rather than having to
	// be told — and its writes are CAS'd regardless, because a partitioned leader believes it
	// still leads until its lease expires (§4.6).
	termCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	go func() {
		select {
		case <-session.Done():
			cancel()
		case <-termCtx.Done():
		}
	}()

	e.opts.Logger.Info("elected leader", "replica", e.opts.Replica)
	err = lead(termCtx)

	// Resigning is best effort and deliberately not bound to termCtx, which is usually the
	// reason we are here.
	resignCtx, resignCancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer resignCancel()
	if resignErr := election.Resign(resignCtx); resignErr != nil && !errors.Is(resignErr, context.Canceled) {
		e.opts.Logger.Debug("could not resign cleanly", "error", resignErr)
	}
	e.opts.Logger.Info("leadership ended", "replica", e.opts.Replica)

	return err
}
