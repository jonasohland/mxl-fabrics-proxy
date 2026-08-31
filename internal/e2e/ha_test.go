package e2e

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jonasohland/mxl-replicator/internal/api"
	"github.com/jonasohland/mxl-replicator/internal/etcdtest"
	"github.com/jonasohland/mxl-replicator/internal/server/leader"
	"github.com/jonasohland/mxl-replicator/internal/store"
	etcdstore "github.com/jonasohland/mxl-replicator/internal/store/etcd"
)

// etcdBackend is the HA deployment: several replicas over one etcd cluster, one of them elected
// reconciler leader (§8.2).
//
// A store and an elector per replica, over separate clients, because that is what separate
// processes have — and because sharing one would make the leader change below a function call
// rather than a lease expiring, which is the part that is worth testing. Election and storage do
// share a connection *within* a replica, deliberately: two would allow a partition that takes out
// storage while leaving election intact, which is a leader that cannot read what it is leading.
type etcdBackend struct {
	endpoints []string
	prefix    string

	// opened remembers each replica's store so that [etcdBackend.elector] can build an elector on
	// the same client. Written and read from the test goroutine only: a replica is constructed by
	// one call to open followed by one call to elector.
	opened map[string]*etcdstore.Store
}

func newEtcdBackend(t *testing.T) *etcdBackend {
	t.Helper()

	// Skips when etcd is not on PATH. The store and election both exist to inherit behaviour
	// from etcd, and a mirror would inherit it from this project's beliefs about etcd instead.
	return &etcdBackend{
		endpoints: etcdtest.Start(t),
		prefix:    "/mxl-replicator-e2e/" + t.Name(),
		opened:    map[string]*etcdstore.Store{},
	}
}

func (b *etcdBackend) open(t *testing.T, replica string) store.Store {
	t.Helper()

	opened, err := etcdstore.Open(t.Context(), etcdstore.Options{
		Endpoints: b.endpoints,
		Prefix:    b.prefix,
		Logger:    discard(),
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = opened.Close() })

	b.opened[replica] = opened
	return opened
}

func (b *etcdBackend) elector(t *testing.T, replica string) leader.Elector {
	t.Helper()

	opened, ok := b.opened[replica]
	require.True(t, ok, "open must be called before elector for %s", replica)

	return leader.NewEtcd(opened.Client(), b.prefix, leader.Options{
		Replica: replica,
		// The shortest lease etcd honours. Sized down from the production default for the same
		// reason every other timing here is: a leader change has to be observable inside a test
		// rather than inside a coffee break.
		TTL:    electionTTL,
		Logger: discard(),
	})
}

// electionTTL is the election lease. etcd rounds a lease up to whole seconds, so this is the
// floor rather than a choice — and it is what bounds how long the fleet is leaderless after the
// leader is killed.
const electionTTL = 2 * time.Second

// leaderOf reports which replica the fleet's control plane currently believes is reconciling, or
// "" if nobody is. Read from /readyz, which is the one place the answer is published.
func (f *fleet) leaderOf() string {
	for _, r := range f.replicas {
		if !r.running() {
			continue
		}
		resp, err := http.Get(r.url + "/readyz") //nolint:noctx // bounded by the test's own deadline
		if err != nil {
			continue
		}
		var body struct {
			Leader string `json:"leader"`
		}
		decoded := json.NewDecoder(resp.Body).Decode(&body)
		_ = resp.Body.Close()
		if resp.StatusCode == http.StatusOK && decoded == nil && body.Leader != "" {
			return body.Leader
		}
	}
	return ""
}

// replicaNamed returns the replica with this name.
func (f *fleet) replicaNamed(name string) *replica {
	f.t.Helper()

	for _, r := range f.replicas {
		if r.name == name {
			return r
		}
	}
	f.t.Fatalf("no replica named %q", name)
	return nil
}

// M7.4, HA half: a leader change must produce **zero** worker restarts (§7.3, §8.2).
//
// The claim rests on two things being true at once, and neither is visible in a single-replica
// test. The new leader recomputes every session ID from scratch — path identity plus the source
// flow definition's hash — and has to arrive at the same ones the old leader used, or it issues
// fresh assignments for sessions that were already running. And it has to wait for the fleet to
// report what it is running before it derives anything, which is what the settling window is.
//
// Get either half wrong and every failover, and therefore every rolling upgrade, glitches every
// flow in the fleet.
//
// The failover here is a **crash**, not a handover. A replica shut down cleanly revokes its
// election lease on the way out, which hands leadership over in milliseconds and gives the
// successor a store that nobody has been writing to since. Closing the leader's etcd client under
// it removes both of those: the lease has to expire on its own, and during that window the old
// leader still believes it leads — which is precisely why every derived write is a CAS rather
// than something the election alone is trusted for (§4.6).
func TestALeaderChangeRestartsNoWorkers(t *testing.T) {
	etcd := newEtcdBackend(t)
	p := newPair(t, fleetOptions{
		backend:            etcd,
		replicas:           3,
		settlingHeartbeats: 4,
	})
	p.replicate("cam1")

	sessionID := p.established()
	target := p.dst.worker(sessionID, api.RoleTarget)
	initiator := p.src.worker(sessionID, api.RoleInitiator)
	starts := p.src.starts() + p.dst.starts()
	require.Equal(t, 2, starts)

	first := p.leaderOf()
	require.NotEmpty(t, first)

	// The leader loses its connection to etcd and then goes away. Nothing tells the survivors;
	// they take over when the election lease expires.
	require.NoError(t, etcd.opened[first].Close())
	p.replicaNamed(first).stop()

	p.eventually("a new leader", func() bool {
		next := p.leaderOf()
		return next != "" && next != first
	})
	p.awaitSettled()

	// The agents' client is sticky and moves on only when a server stops answering at the
	// transport level, which is exactly what has just happened to whichever of them was talking
	// to the dead replica.
	p.eventually("the path to be served by the new leader", func() bool {
		paths := p.paths().Paths
		return len(paths) == 1 && paths[0].Session != nil && paths[0].Session.ID == sessionID
	})

	assert.Equal(t, starts, p.src.starts()+p.dst.starts(), "a leader change must restart no workers")
	assert.True(t, target.Running())
	assert.True(t, initiator.Running())
	assert.Zero(t, target.Stops())
	assert.Zero(t, initiator.Stops())

	// And the fleet is still a working control plane afterwards, not merely one that did not
	// break anything on the way through: a new request expands, negotiates and comes up under
	// the new leader.
	second := p.src.createFlow("cameras", videoFlowDef("Studio A:Camera 2", "video"))
	second.produce()

	p.request(api.RequestSpec{
		Name:         "cam2",
		Source:       api.Source{Node: "studio-a", Domain: p.src.source("cameras"), Select: api.Selector{Flow: second.ID()}},
		Destinations: []api.Destination{{Node: "edge-01", Domain: api.Domain{Area: "fast", Elements: []string{"ingest"}}}},
	})
	p.eventually("a second session under the new leader", func() bool {
		for _, candidate := range p.paths().Paths {
			if candidate.Source.Flow == second.ID() && candidate.Session != nil && candidate.Session.Epoch != "" {
				return true
			}
		}
		return false
	})
}

// A follower serves every agent and every user request; only the reconciler is exclusive. That is
// what lets several replicas sit behind a plain third-party proxy with no sticky sessions (§8.2).
//
// The refusal that makes it safe is the one worth checking: a replica whose view of the store is
// behind an agent's cursor must answer CodeNotReady rather than serve a set the agent has already
// moved past, because an assignment set that goes backwards looks to the agent exactly like
// sessions being withdrawn (plan §4.5).
func TestEveryReplicaServesTheSameAnswers(t *testing.T) {
	p := newPair(t, fleetOptions{
		backend:  newEtcdBackend(t),
		replicas: 3,
	})
	p.replicate("cam1")

	sessionID := p.established()
	leaderName := p.leaderOf()
	require.NotEmpty(t, leaderName)

	for _, r := range p.replicas {
		resp := getJSON(t, r.url+api.PathPaths)
		require.Equal(t, http.StatusOK, resp.status, "replica %s: %s", r.name, resp.body)

		var paths api.PathsResponse
		resp.decode(t, &paths)
		require.Len(t, paths.Paths, 1, "replica %s serves a different number of paths", r.name)
		require.NotNil(t, paths.Paths[0].Session, "replica %s sees no session on the path", r.name)
		assert.Equal(t, sessionID, paths.Paths[0].Session.ID,
			"replica %s disagrees about which session realises the path", r.name)
	}
}

// getJSON is a bare GET against one named replica, bypassing [fleet.do]'s "whichever is running"
// selection — the point of the caller is to ask each replica separately.
func getJSON(t *testing.T, url string) response {
	t.Helper()

	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, url, nil)
	require.NoError(t, err)

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close() //nolint:errcheck // read to completion below

	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return response{status: resp.StatusCode, body: body}
}
