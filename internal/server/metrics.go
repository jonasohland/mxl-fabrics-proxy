package server

import (
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/jonasohland/mxl-replicator/internal/api"
	"github.com/jonasohland/mxl-replicator/internal/metrics"
	"github.com/jonasohland/mxl-replicator/internal/server/reconcile"
)

// Registry is this server's metrics registry, exposed so a combined instance can see that it is
// not the agent's (§4.7).
func (s *Server) Registry() *prometheus.Registry { return s.registry }

// registerMetrics builds the registry and puts every control-plane collector on it.
func (s *Server) registerMetrics() {
	s.registry = metrics.New()
	s.registry.MustRegister(append(s.metrics.collectors(), &fleetCollector{server: s})...)
}

// controlMetrics is one server's instruments.
//
// Per server rather than package-level, because two servers in one process is a real
// configuration — the in-process integration suite (§17) builds several, and a combined instance
// already runs two registries side by side (§4.7). Globals would have them share counters, which
// makes a test's assertions depend on what ran before it and a fleet's numbers depend on how many
// servers a binary happened to construct.
type controlMetrics struct {
	storeDuration         *prometheus.HistogramVec
	storeFailures         *prometheus.CounterVec
	reconcileDuration     prometheus.Histogram
	reconciles            *prometheus.CounterVec
	epochTransitions      *prometheus.CounterVec
	leaderChanges         prometheus.Counter
	registrationsRejected *prometheus.CounterVec
	eventsRecorded        *prometheus.CounterVec
	eventsDropped         *prometheus.CounterVec
}

func (m *controlMetrics) collectors() []prometheus.Collector {
	return []prometheus.Collector{
		m.storeDuration, m.storeFailures, m.reconcileDuration,
		m.reconciles, m.epochTransitions, m.leaderChanges, m.registrationsRejected,
		m.eventsRecorded, m.eventsDropped,
	}
}

// fleetCollector reports what the fleet looks like, from the last reconcile (§12).
//
// # Why the last reconcile and not a fresh read
//
// Every one of these numbers is a property of the whole store, and the only cheap way to have
// them is to count them while something is already holding a consistent read. Loading the fleet
// per scrape would put a full List on every Prometheus interval on every replica — for etcd, a
// quorum read of the entire key space to answer a question nothing is waiting on.
//
// # Why a follower reports nothing here
//
// Only the leader runs a reconcile loop, so only the leader has an observation. A follower
// exporting zeroes would be indistinguishable from a fleet that genuinely has no paths, which is
// the difference between "nothing is replicating" and "ask the other replica" — and those must
// not render the same. [metrics.Control]("leader") is what tells them apart, and it is the one
// series here that every replica always exports.
type fleetCollector struct{ server *Server }

var _ prometheus.Collector = (*fleetCollector)(nil)

// Describe implements [prometheus.Collector]. Unchecked, like the agent's: what a follower emits
// is a strict subset of what a leader does, and a checked collector must describe everything it
// might ever send.
func (c *fleetCollector) Describe(chan<- *prometheus.Desc) {}

// Collect implements [prometheus.Collector].
func (c *fleetCollector) Collect(ch chan<- prometheus.Metric) {
	ch <- prometheus.MustNewConstMetric(leaderDesc, prometheus.GaugeValue,
		boolValue(c.server.loop.Leading()))

	observation := c.server.loop.Observation()
	if observation == nil {
		return
	}

	ch <- prometheus.MustNewConstMetric(nodesDesc, prometheus.GaugeValue, float64(observation.Nodes))
	ch <- prometheus.MustNewConstMetric(leasesDesc, prometheus.GaugeValue, float64(observation.Leases))
	ch <- prometheus.MustNewConstMetric(frozenDesc, prometheus.GaugeValue, float64(observation.Frozen))
	ch <- prometheus.MustNewConstMetric(revisionDesc, prometheus.GaugeValue, float64(observation.Revision))

	for state, count := range observation.Requests {
		ch <- prometheus.MustNewConstMetric(requestsDesc, prometheus.GaugeValue, float64(count), string(state))
	}
	for state, count := range observation.Paths {
		ch <- prometheus.MustNewConstMetric(pathsDesc, prometheus.GaugeValue, float64(count), string(state))
	}
	for state, count := range observation.Workers {
		ch <- prometheus.MustNewConstMetric(sessionsDesc, prometheus.GaugeValue, float64(count), string(state))
	}
	for version, count := range observation.Versions {
		ch <- prometheus.MustNewConstMetric(agentVersionsDesc, prometheus.GaugeValue, float64(count),
			version.Replicator, strconv.Itoa(version.Protocol))
	}
}

func boolValue(v bool) float64 {
	if v {
		return 1
	}
	return 0
}

var (
	leaderDesc = prometheus.NewDesc(metrics.Control("leader"),
		"1 on the replica currently running the reconciler, 0 on every other. The fleet gauges below are only present on the 1.",
		nil, nil)

	nodesDesc = prometheus.NewDesc(metrics.Control("nodes_registered"),
		"Nodes with a registration record, whether or not they currently hold a lease.", nil, nil)

	leasesDesc = prometheus.NewDesc(metrics.Control("agents_leased"),
		"Agents holding a live liveness lease.", nil, nil)

	frozenDesc = prometheus.NewDesc(metrics.Control("sessions_frozen"),
		"Sessions whose assignments were carried forward because an endpoint's agent is not leased.", nil, nil)

	revisionDesc = prometheus.NewDesc(metrics.Control("reconciled_revision"),
		"Store revision the last completed reconcile was computed from. A flat line is a reconciler that has stopped.",
		nil, nil)

	requestsDesc = prometheus.NewDesc(metrics.Control("requests"),
		"Replication requests by aggregate status.", []string{"state"}, nil)

	pathsDesc = prometheus.NewDesc(metrics.Control("paths"),
		"Replication paths by status.", []string{"state"}, nil)

	sessionsDesc = prometheus.NewDesc(metrics.Control("sessions"),
		"Session endpoints by the worker state their agent last reported.", []string{"state"}, nil)

	agentVersionsDesc = prometheus.NewDesc(metrics.Control("agent_versions"),
		"Leased agents by reported build, which is the fleet's version spread.",
		[]string{"replicator", "protocol"}, nil)
)

func newControlMetrics() *controlMetrics {
	return &controlMetrics{
		storeDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name: metrics.Control("store_operation_duration_seconds"),
			Help: "Latency of store operations, by operation.",
			// Spanning sqlite's local writes and etcd's network round trips, which is two orders of
			// magnitude apart — hence a range wider than the default buckets, starting below a
			// millisecond and reaching the seconds where a struggling etcd lives.
			Buckets: prometheus.ExponentialBuckets(0.0005, 3, 9),
		}, []string{"operation"}),

		storeFailures: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: metrics.Control("store_operations_failed_total"),
			Help: "Store operations that returned an error, excluding not-found and failed compares.",
		}, []string{"operation"}),

		reconcileDuration: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name:    metrics.Control("reconcile_duration_seconds"),
			Help:    "Time for one load, compute and apply pass.",
			Buckets: prometheus.ExponentialBuckets(0.001, 3, 9),
		}),

		// reconciles counts passes by outcome, and `refused` is the one that matters.
		//
		// It is the store-wipe signal (plan §4.2): a fleet with leased agents and no observed state is
		// a store that lost its contents, not a fleet with nothing to do, and the reconciler declines
		// to act on it. That decision is correct and it is also invisible — the fleet keeps running on
		// assignments nobody is rewriting — so it needs to be countable.
		reconciles: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: metrics.Control("reconciles_total"),
			Help: "Reconcile passes by outcome: ok, refused (leased agents but no observed state), or failed.",
		}, []string{"outcome"}),

		// epochTransitions is the flapping signal the epoch cannot carry on its own.
		//
		// An epoch is a content hash with no ordering (§5.2), so a session changing epoch is exactly a
		// target restarting, and nothing downstream can see that by looking at one value. Labelled by
		// the node hosting the target rather than by session: a per-session counter is unbounded over
		// a long-running leader, since a session that goes away leaves its series behind, and "which
		// node is flapping" is the actionable question anyway.
		epochTransitions: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: metrics.Control("epoch_transitions_total"),
			Help: "Sessions observed to change epoch, by the node hosting the target. Each one is a target that restarted.",
		}, []string{"node"}),

		// leaderChanges counts this replica *acquiring* leadership, not the fleet's total.
		//
		// Frequent changes are the symptom §4.4 warns about — RT workers starving the Go runtime on a
		// combined node, a missed keepalive, a lost lease — and they are otherwise invisible, because
		// each individual handover looks like an ordinary startup in the log.
		leaderChanges: prometheus.NewCounter(prometheus.CounterOpts{
			Name: metrics.Control("leader_acquisitions_total"),
			Help: "Times this replica has acquired leadership. Repeated increments are leader churn.",
		}),

		// registrationsRejected covers the two ways a node is turned away: another instance holds its
		// name (§7.1), or it is newer than this server (§13.1). Both are loud in the log and both are
		// conditions an operator wants alerting on rather than reading about.
		// eventsRecorded and eventsDropped are the event log's own accounting (§12.1).
		//
		// The dropped counter is labelled by **where** the loss happened, because the two are
		// different problems: `queue` is an agent whose in-memory batch overflowed before it could
		// report, `ring` is history aged out of a full ring, `store` and `contention` are this
		// server failing to write. Both are expected in a bad hour and only the first says
		// something was never seen at all.
		eventsRecorded: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: metrics.Control("events_recorded_total"),
			Help: "Event-log entries written, by kind.",
		}, []string{"kind"}),

		eventsDropped: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: metrics.Control("events_dropped_total"),
			Help: "Event-log entries lost, by where: queue, ring, store or contention.",
		}, []string{"reason"}),

		registrationsRejected: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: metrics.Control("registrations_rejected_total"),
			Help: "Node registrations refused, by reason.",
		}, []string{"reason"}),
	}
}

// reconcileHooks are what the loop reports through (§12).
//
// The duration is recorded only for a pass that completed. A refused pass measures how long it
// took to decide not to act, and a failed one is however far it got before the store said no —
// mixing either into the histogram would make the reconciler look faster the worse things got.
func (m *controlMetrics) reconcileHooks() reconcile.Hooks {
	return reconcile.Hooks{
		Pass: func(outcome reconcile.Outcome, took time.Duration) {
			m.reconciles.WithLabelValues(string(outcome)).Inc()
			if outcome == reconcile.OutcomeOK {
				m.reconcileDuration.Observe(took.Seconds())
			}
		},
		EpochChanged: func(node string) { m.epochTransitions.WithLabelValues(node).Inc() },
	}
}

// eventRecorded and eventDropped are the recorder's callbacks (§12.1).
//
// Callbacks rather than the recorder holding a registry, for the same reason the reconcile loop
// takes [reconcile.Hooks]: the event log stays free of a Prometheus dependency, and a test can
// assert on what it decided without gathering an exposition.
func (m *controlMetrics) eventRecorded(kind api.EventKind) {
	m.eventsRecorded.WithLabelValues(string(kind)).Inc()
}

func (m *controlMetrics) eventDropped(reason string) {
	m.eventsDropped.WithLabelValues(reason).Inc()
}
