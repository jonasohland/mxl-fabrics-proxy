package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/jonasohland/mxl-replicator/internal/server/leader"
	"github.com/jonasohland/mxl-replicator/internal/store"
	etcdstore "github.com/jonasohland/mxl-replicator/internal/store/etcd"
	"github.com/jonasohland/mxl-replicator/internal/store/sqlite"
)

// Open opens the configured backend and returns it with the matching elector (§8.2).
//
// The two come out of one call because they are one decision. With sqlite there is a single
// process that can write the store at all, so it is always the leader and election is a question
// with one possible answer; with etcd, leadership is a campaign, and it runs on the *same*
// client as the store — one connection, one set of credentials, one set of keepalives, so there
// is no partition that can take out storage while leaving election intact.
func (s StoreFlags) Open(ctx context.Context, replica string, leaseTTL time.Duration, logger *slog.Logger) (store.Store, leader.Elector, func(), error) {
	logger = logger.With("module", "store")

	switch s.Backend {
	case "sqlite":
		backing, err := sqlite.Open(ctx, s.SqlitePath, sqlite.Options{Logger: logger})
		if err != nil {
			return nil, nil, nil, err
		}
		return backing, leader.Always{Replica: replica}, func() { _ = backing.Close() }, nil

	case "etcd":
		tlsConfig, err := s.etcdTLS()
		if err != nil {
			return nil, nil, nil, err
		}

		backing, err := etcdstore.Open(ctx, etcdstore.Options{
			Endpoints: s.EtcdEndpoints,
			Prefix:    s.EtcdPrefix,
			TLS:       tlsConfig,
			Username:  s.EtcdUsername,
			Password:  s.EtcdPassword,
			Logger:    logger,
		})
		if err != nil {
			return nil, nil, nil, err
		}

		elector := leader.NewEtcd(backing.Client(), backing.Prefix(), leader.Options{
			Replica: replica,
			// The election lease and the agents' liveness leases are sized together on purpose:
			// both answer "how long after something stops responding do we act", and having two
			// unrelated numbers for that is how a fleet ends up with a leaderless window inside
			// its own settling window.
			TTL:    leaseTTL,
			Logger: logger.With("module", "election"),
		})

		return backing, elector, func() { _ = backing.Close() }, nil

	default:
		return nil, nil, nil, fmt.Errorf("unknown store backend %q", s.Backend)
	}
}

// etcdTLS builds the client TLS configuration, if any was asked for.
func (s StoreFlags) etcdTLS() (*tls.Config, error) {
	if s.EtcdCAFile == "" && s.EtcdCertFile == "" && s.EtcdKeyFile == "" {
		return nil, nil
	}

	config := &tls.Config{MinVersion: tls.VersionTLS12}

	if s.EtcdCAFile != "" {
		pem, err := os.ReadFile(s.EtcdCAFile)
		if err != nil {
			return nil, fmt.Errorf("read --store-etcd-ca: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("--store-etcd-ca %s contains no certificates", s.EtcdCAFile)
		}
		config.RootCAs = pool
	}

	if s.EtcdCertFile != "" {
		pair, err := tls.LoadX509KeyPair(s.EtcdCertFile, s.EtcdKeyFile)
		if err != nil {
			return nil, fmt.Errorf("load etcd client certificate: %w", err)
		}
		config.Certificates = []tls.Certificate{pair}
	}

	return config, nil
}
