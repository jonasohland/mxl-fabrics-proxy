package leader

import (
	"context"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	clientv3 "go.etcd.io/etcd/client/v3"

	"github.com/jonasohland/mxl-replicator/internal/etcdtest"
)

// With sqlite there is exactly one process that can write the store at all, so election is a
// question with one possible answer rather than a step being skipped.
func TestAlwaysLeads(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(t.Context())

	var led atomic.Bool
	err := Always{Replica: "solo"}.Run(ctx, func(termCtx context.Context) error {
		led.Store(true)
		cancel()
		<-termCtx.Done()
		return nil
	})

	require.NoError(t, err)
	assert.True(t, led.Load())
	assert.Equal(t, "solo", Always{Replica: "solo"}.Name())
	assert.Equal(t, "single", Always{}.Name())
}

func client(t *testing.T, endpoints []string) *clientv3.Client {
	t.Helper()

	cli, err := clientv3.New(clientv3.Config{
		Endpoints:   endpoints,
		DialTimeout: 5 * time.Second,
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = cli.Close() })
	return cli
}

func testOptions(replica string) Options {
	return Options{
		Replica: replica,
		TTL:     2 * time.Second,
		Logger:  slog.New(slog.DiscardHandler),
	}
}

// One leader at a time, and the second candidate takes over when the first stands down. This is
// the property the whole reconciler design leans on: several replicas serve the API, exactly one
// writes /derived/ (§8.2).
func TestEtcdElectsOneLeaderAndHandsOver(t *testing.T) {
	endpoints := etcdtest.Start(t)
	cli := client(t, endpoints)

	type run struct {
		elector *Etcd
		ctx     context.Context
		cancel  context.CancelFunc
		leading chan string
		done    chan struct{}
	}

	start := func(replica string) *run {
		ctx, cancel := context.WithCancel(t.Context())
		r := &run{
			elector: NewEtcd(cli, "/mxl-replicator", testOptions(replica)),
			ctx:     ctx,
			cancel:  cancel,
			leading: make(chan string, 4),
			done:    make(chan struct{}),
		}
		go func() {
			defer close(r.done)
			assert.NoError(t, r.elector.Run(ctx, func(termCtx context.Context) error {
				r.leading <- replica
				<-termCtx.Done()
				return nil
			}))
		}()
		t.Cleanup(func() {
			cancel()
			<-r.done
		})
		return r
	}

	first := start("replica-a")
	select {
	case who := <-first.leading:
		assert.Equal(t, "replica-a", who)
	case <-time.After(20 * time.Second):
		t.Fatal("the first candidate never won the election")
	}

	second := start("replica-b")
	select {
	case who := <-second.leading:
		t.Fatalf("%s led while another replica held leadership", who)
	case <-time.After(500 * time.Millisecond):
	}

	// The leader stands down cleanly, which revokes its session lease and hands over in
	// milliseconds rather than after a TTL.
	first.cancel()
	<-first.done

	select {
	case who := <-second.leading:
		assert.Equal(t, "replica-b", who)
	case <-time.After(20 * time.Second):
		t.Fatal("leadership was not handed over")
	}
}

// The term's context ends when leadership does, so the reconciler stops of its own accord rather
// than having to be told — and, on the way out, every derived write is a CAS anyway, because a
// partitioned leader believes it still leads until its lease expires (§4.6).
func TestEtcdCancelsTheTermContext(t *testing.T) {
	endpoints := etcdtest.Start(t)
	cli := client(t, endpoints)

	ctx, cancel := context.WithCancel(t.Context())
	elector := NewEtcd(cli, "/mxl-replicator", testOptions("replica-a"))

	cancelled := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		assert.NoError(t, elector.Run(ctx, func(termCtx context.Context) error {
			<-termCtx.Done()
			close(cancelled)
			return nil
		}))
	}()

	time.Sleep(500 * time.Millisecond)
	cancel()

	select {
	case <-cancelled:
	case <-time.After(20 * time.Second):
		t.Fatal("the term context was not cancelled when the elector stopped")
	}
	<-done
}

// The election keys must live under the deployment's own prefix, or two deployments sharing a
// cluster elect one leader between them — and half the fleet's reconciler then reconciles the
// other half's assignments.
func TestElectionKeysAreNamespaced(t *testing.T) {
	endpoints := etcdtest.Start(t)
	cli := client(t, endpoints)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	elector := NewEtcd(cli, "/deployment-a/", testOptions("replica-a"))
	leading := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		assert.NoError(t, elector.Run(ctx, func(termCtx context.Context) error {
			close(leading)
			<-termCtx.Done()
			return nil
		}))
	}()

	select {
	case <-leading:
	case <-time.After(20 * time.Second):
		t.Fatal("never elected")
	}

	resp, err := cli.Get(t.Context(), "/deployment-a/election/", clientv3.WithPrefix())
	require.NoError(t, err)
	require.NotEmpty(t, resp.Kvs, "the election key must be under the deployment prefix")
	assert.Equal(t, "replica-a", string(resp.Kvs[0].Value))

	cancel()
	<-done
}
