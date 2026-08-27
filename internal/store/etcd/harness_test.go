package etcd

import (
	"context"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/jonasohland/mxl-replicator/internal/etcdtest"
)

// The tests in this package run against a real etcd, started by [etcdtest.Start]. See that
// package for why, and for how to point them at a cluster of your own instead.
func startEtcd(t *testing.T) []string { return etcdtest.Start(t) }

func freePort(t *testing.T) int { return etcdtest.FreePort(t) }

// newStore opens a store against endpoints, closed when the test ends.
func newStore(t *testing.T, endpoints []string, prefix string) *Store {
	t.Helper()

	s, err := Open(context.Background(), Options{
		Endpoints: endpoints,
		Prefix:    prefix,
		Logger:    slog.New(slog.DiscardHandler),
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })

	return s
}
