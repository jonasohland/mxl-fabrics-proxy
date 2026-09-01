package agent

import (
	"context"
	"encoding/json"
	"maps"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/jonasohland/mxl-replicator/internal/metrics"
	"github.com/jonasohland/mxl-replicator/internal/worker"
)

// Defaults for the worker scrape (§12).
const (
	// DefaultScrapeConcurrency is how many worker sockets are read at once.
	//
	// The point of a bound is that the fan-out is a fixed cost rather than one goroutine and one
	// connection per worker per request — the legacy proxy had no bound and multiplied it by
	// however many collectors arrived at once. Eight against §14's design point of 50 flows per
	// node is seven rounds of a sub-millisecond read.
	DefaultScrapeConcurrency = 8

	// DefaultWorkerScrapeTimeout bounds one worker's scrape. Generous next to a Unix-socket read
	// of a few hundred bytes, because the failure it is sized for is a *wedged* worker rather
	// than a slow one, and there is no value between "immediately" and "not coming".
	DefaultWorkerScrapeTimeout = time.Second

	// DefaultScrapeTimeout bounds a whole collection, and it is the one that protects the
	// endpoint.
	//
	// Without it, N wedged workers cost ceil(N/concurrency) × the per-worker timeout, which at a
	// large enough N exceeds the collecting scraper's own timeout and loses the *healthy*
	// workers' series along with the wedged ones'. Comfortably inside Prometheus' 10 s default.
	DefaultScrapeTimeout = 5 * time.Second
)

// Collector exposes this node's worker metrics (§12).
//
// Register it on the agent's registry; there is one per agent and registering it twice is a
// duplicate-collector error from the registry, which is the right answer.
func (a *Agent) Collector() prometheus.Collector { return &collector{agent: a} }

// collector scrapes every running worker on demand, inside the request.
//
// # Why not a cached snapshot
//
// Because the collecting scraper decides the rate, and nothing here is in a position to second-
// guess it (§12). A snapshot refreshed on an interval C and served against a scrape interval S
// makes consecutive scrapes return byte-identical counters whenever the two are close — `rate()`
// reads zero, and then jumps — which is a beat frequency in every transfer graph rather than a
// uniform small lag. It would also lie about liveness in both directions, serving a dead
// worker's frozen counters and hiding a new worker until the next refresh, where scraping on
// demand makes a series appear and disappear with the process it describes.
//
// The cost that would have justified caching is not there: the worker snapshots its counters on
// its own listen thread (WRS §6), so a scrape does not touch the transfer loop, and the payload
// is a few hundred bytes over a Unix socket.
type collector struct {
	agent *Agent

	mu sync.Mutex
	// failures accumulates across collections, which is why it is state here rather than
	// something derived per pass.
	failures uint64
}

var _ prometheus.Collector = (*collector)(nil)

// Describe implements [prometheus.Collector].
//
// Deliberately unchecked — it sends nothing. The label set of a worker metric includes the
// user's labels from the request that created the session (§9.1), so it is not knowable until
// there are workers, and an "unchecked" collector is exactly the escape hatch the registry
// provides for that. The cost is that the registry cannot detect a name collision with another
// collector at registration time; there is no other collector in this process emitting `mxl_`.
func (c *collector) Describe(chan<- *prometheus.Desc) {}

// Collect implements [prometheus.Collector].
//
// The deadlines are the collector's own, not the request's: [prometheus.Collector.Collect] takes
// no context and the registry calls it without one, so there is nothing to derive them from.
// That is a limitation of the interface rather than a decision, and it is why the timeouts are
// configured rather than negotiated.
func (c *collector) Collect(ch chan<- prometheus.Metric) {
	started := c.agent.now()

	// Background, not the agent's root context: an agent shutting down has an HTTP server
	// shutting down with it, and a collection that outlives either is bounded by the timeout
	// below regardless.
	ctx, cancel := context.WithTimeout(context.Background(), c.agent.cfg.ScrapeTimeout)
	defer cancel()

	targets := c.agent.scrapeTargets()
	results := c.scrapeAll(ctx, targets)

	// One label set for the whole family, computed before anything is emitted — see labelNames.
	descs := newDescCache(labelNames(targets))
	failed := uint64(0)

	for _, result := range results {
		c.emitSupervision(ch, descs, result.target)

		if result.err != nil {
			failed++
			// Debug: a worker that died between the snapshot and the scrape is ordinary, and this
			// runs on every collection. The failure counter below is the signal.
			c.agent.log.Debug("scraping a worker's metrics failed",
				"session", result.target.key.Session, "role", string(result.target.key.Role),
				"error", result.err)
			continue
		}
		emit(ch, descs, result)
	}

	c.mu.Lock()
	c.failures += failed
	failures := c.failures
	c.mu.Unlock()

	// Emitted from inside the collection that measured them, so the duration always describes the
	// exposition it is served with rather than the previous one.
	ch <- prometheus.MustNewConstMetric(scrapeDurationDesc, prometheus.GaugeValue,
		c.agent.now().Sub(started).Seconds())
	ch <- prometheus.MustNewConstMetric(scrapedWorkersDesc, prometheus.GaugeValue,
		float64(running(targets)))
	ch <- prometheus.MustNewConstMetric(scrapeFailuresDesc, prometheus.CounterValue,
		float64(failures))

	c.emitStartGate(ch)
}

// emitStartGate exposes what rate control on worker starts is doing (§6.3, §12).
//
// Three series, unlabelled, because the gate is one thing per node rather than one per worker —
// and it is the thing that makes a paced node legible. Without them a fleet that is deliberately
// spreading a re-establishment over seconds looks identical to one whose workers are failing to
// come up, which is the state an operator would otherwise reach for the restart counters to
// diagnose and find nothing.
//
// The counters are cumulative and the gauge is instantaneous, deliberately in that split: a
// permit wait is over in seconds, so a gauge alone would usually read zero during exactly the
// event worth seeing, and a counter alone could not say that the node is queued *now*.
func (c *collector) emitStartGate(ch chan<- prometheus.Metric) {
	waiting, delayed, waited := c.agent.starts.stats()

	ch <- prometheus.MustNewConstMetric(startsWaitingDesc, prometheus.GaugeValue, float64(waiting))
	ch <- prometheus.MustNewConstMetric(startsDelayedDesc, prometheus.CounterValue, float64(delayed))
	ch <- prometheus.MustNewConstMetric(startDelayDesc, prometheus.CounterValue, waited.Seconds())
}

var (
	scrapeDurationDesc = prometheus.NewDesc(
		metrics.Control("worker_scrape_duration_seconds"),
		"Time taken to scrape every worker on this node for the exposition being served.",
		nil, nil)

	scrapedWorkersDesc = prometheus.NewDesc(
		metrics.Control("workers_scraped"),
		"Number of running workers this node attempted to scrape.",
		nil, nil)

	scrapeFailuresDesc = prometheus.NewDesc(
		metrics.Control("worker_scrapes_failed_total"),
		"Worker scrapes that returned an error or timed out.",
		nil, nil)

	startsWaitingDesc = prometheus.NewDesc(
		metrics.Control("worker_starts_waiting"),
		"Workers currently waiting for a permit before this node will start them.",
		nil, nil)

	startsDelayedDesc = prometheus.NewDesc(
		metrics.Control("worker_starts_delayed_total"),
		"Worker starts that had to wait for a permit. Unthrottled starts are not counted.",
		nil, nil)

	startDelayDesc = prometheus.NewDesc(
		metrics.Control("worker_start_delay_seconds_total"),
		"Total time worker starts have spent waiting for a permit.",
		nil, nil)
)

// target is one supervised worker: what labels it, what supervision knows about it, and the
// handle to scrape — nil when nothing is running.
type target struct {
	key      unitKey
	spec     worker.Spec
	flow     flowLabels
	restarts uint64
	handle   worker.Handle
}

// result is one worker's scrape.
type result struct {
	target  target
	samples []worker.Sample
	err     error
}

// scrapeTargets snapshots every supervised worker, running or not.
//
// Both cases are wanted, and they carry different series. A unit between attempts — backing off
// after a death — has no process, so its *worker* counters are absent, and a series that
// disappears is what lets Prometheus' staleness handling stop a rate rather than extrapolate one.
// Its restart count is the opposite: a worker in a crash loop is exactly when that number is
// worth reading, and it is supervision's own knowledge rather than the process's.
func (a *Agent) scrapeTargets() []target {
	a.mu.Lock()
	units := slices.Collect(maps.Values(a.units))
	a.mu.Unlock()

	targets := make([]target, 0, len(units))
	for _, u := range units {
		targets = append(targets, target{
			key:      u.key,
			spec:     u.desired(),
			flow:     u.flow,
			restarts: u.restartCount(),
			handle:   u.running(),
		})
	}

	slices.SortFunc(targets, func(a, b target) int { return a.key.compare(b.key) })
	return targets
}

// running counts the targets with a process behind them.
func running(targets []target) int {
	live := 0
	for _, t := range targets {
		if t.handle != nil {
			live++
		}
	}
	return live
}

// scrapeAll reads every target through a bounded pool, returning one result each.
//
// A worker that fails or times out yields a result carrying its error rather than dropping out,
// because the difference between "this worker reported nothing" and "this worker was not asked"
// is the whole of the failure counter.
func (c *collector) scrapeAll(ctx context.Context, targets []target) []result {
	results := make([]result, len(targets))

	slots := make(chan struct{}, max(1, c.agent.cfg.ScrapeConcurrency))
	var scraping sync.WaitGroup

	for i, t := range targets {
		results[i].target = t
		if t.handle == nil {
			// Nothing running: no error, because nothing failed. Its supervision-level series are
			// still emitted.
			continue
		}

		select {
		case slots <- struct{}{}:
		case <-ctx.Done():
			// The overall deadline expired with targets still waiting for a slot. Counted as
			// failures, because from the operator's side a worker that was never reached is the
			// same missing series as one that was reached and did not answer — and the count is
			// how "the pool is not keeping up" becomes visible at all.
			results[i] = result{target: t, err: ctx.Err()}
			continue
		}

		scraping.Go(func() {
			defer func() { <-slots }()

			workerCtx, cancel := context.WithTimeout(ctx, c.agent.cfg.WorkerScrapeTimeout)
			defer cancel()

			samples, err := t.handle.Metrics(workerCtx)
			results[i] = result{target: t, samples: samples, err: err}
		})
	}

	scraping.Wait()
	return results
}

// labelNames is the label set every worker metric in this collection carries.
//
// It has to be one set for the whole pass. A Prometheus metric family must have consistent label
// dimensions, and user labels come from the request that created each session (§9.1), so two
// sessions on one node routinely disagree about which keys exist. The union is taken and a
// worker without a given key reports it empty — the alternative is a gather error that discards
// the family, healthy workers included.
//
// The set changing between collections is fine and expected: it is a different series set, which
// is exactly what a relabelled request is.
func labelNames(targets []target) []string {
	user := map[string]struct{}{}
	for _, t := range targets {
		for name := range t.spec.Labels {
			if usableLabel(name) {
				user[name] = struct{}{}
			}
		}
	}
	return append(metrics.WorkerLabelNames(), slices.Sorted(maps.Keys(user))...)
}

// usableLabel reports whether a user-supplied label name can be applied.
//
// Two ways it cannot: it is not a valid Prometheus label name, or it collides with a label this
// project sets itself, where honouring it would either overwrite the real value or produce a
// duplicate dimension. Both are dropped rather than mangled into something unique, because a
// silently renamed label is worse than an absent one. This belongs in request validation on the
// server, where it could be an error the user sees; here it can only be a defence.
func usableLabel(name string) bool {
	if !metrics.ValidLabelName(name) || name == metrics.LabelQuantile {
		return false
	}
	return !slices.Contains(metrics.WorkerLabelNames(), name)
}

// labelValues renders one worker's labels in the order labelNames produced.
func labelValues(names []string, t target) []string {
	values := make([]string, 0, len(names))
	for _, name := range names {
		switch name {
		case metrics.LabelDirection:
			values = append(values, string(t.spec.Role))
		case metrics.LabelDomain:
			values = append(values, t.spec.Domain)
		case metrics.LabelFlowID:
			values = append(values, t.spec.FlowID)
		case metrics.LabelSession:
			values = append(values, t.spec.SessionID)
		case metrics.LabelNamespace:
			values = append(values, t.spec.Namespace)
		case metrics.LabelFormat:
			values = append(values, t.flow.format)
		case metrics.LabelMediaType:
			values = append(values, t.flow.mediaType)
		default:
			values = append(values, t.spec.Labels[name])
		}
	}
	return values
}

// emitSupervision emits what the agent knows about a worker without asking the worker (§12).
//
// Three series the legacy proxy also had, under the same names, because they describe a flow and
// a transfer rather than this project (§2.2) and existing dashboards select on them.
//
// The two liveness gauges are emitted only for a flow this agent is actually observing. A
// destination flow does not exist until its target creates it, and a zero for "I am not looking
// at it" would be a different claim from "nothing is reading it" reported identically — the same
// mistake as a fabricated `_count 0`, and here it would read as a dead consumer.
func (c *collector) emitSupervision(ch chan<- prometheus.Metric, descs *descCache, t target) {
	values := labelValues(descs.names, t)

	ch <- prometheus.MustNewConstMetric(descs.desc(metrics.Flow("worker_restarts")),
		prometheus.CounterValue, float64(t.restarts), values...)

	live, ok := c.agent.cfg.Inventory.Look(t.spec.DomainPath, t.spec.FlowID)
	if !ok {
		return
	}
	ch <- prometheus.MustNewConstMetric(descs.desc(metrics.Flow("writer_active")),
		prometheus.GaugeValue, boolValue(live.Writing), values...)
	ch <- prometheus.MustNewConstMetric(descs.desc(metrics.Flow("reader_active")),
		prometheus.GaugeValue, boolValue(live.Reading), values...)
}

func boolValue(v bool) float64 {
	if v {
		return 1
	}
	return 0
}

// flowLabels say what kind of media a worker is moving (§12).
//
// Two fields and no more. `format` and `media_type` are low-cardinality, stable for a flow's
// life, and answer the question a transfer dashboard actually asks — is this video or audio, and
// in what. The tempting others are all worse: `label` churns when someone renames a flow, which
// splits a series; `source_id` and `device_id` are UUIDs whose cardinality `flow_id` already
// carries.
type flowLabels struct {
	format    string
	mediaType string
}

// flowLabelsFor resolves them once, from whichever side of the session has the definition.
//
// A target is handed the source definition verbatim, because it cannot create its local flow
// without it (§5.3). An initiator is not — it opens an existing local flow by ID — so its
// definition comes from inventory, which must already have the flow or the server would not have
// assigned the session. Either way the answer is taken once and frozen by the caller.
func (a *Agent) flowLabelsFor(spec worker.Spec) flowLabels {
	def := spec.FlowDef
	if len(def) == 0 {
		if live, ok := a.cfg.Inventory.Look(spec.DomainPath, spec.FlowID); ok {
			def = live.Definition
		}
	}
	if len(def) == 0 {
		return flowLabels{}
	}

	var parsed struct {
		Format    string `json:"format"`
		MediaType string `json:"media_type"`
	}
	if err := json.Unmarshal(def, &parsed); err != nil {
		// Not an error worth failing anything for: the definition is opaque to this project
		// everywhere else, and a worker that runs with unlabelled metrics beats one that does not
		// run.
		a.log.Debug("cannot read flow-definition labels", "flow", spec.FlowID, "error", err)
		return flowLabels{}
	}

	// "urn:x-nmos:format:video" is what the definition carries and "video" is what anyone wants
	// to type in a query. An unrecognised value passes through untouched rather than being mapped
	// through a table this project would have to keep current.
	return flowLabels{
		format:    strings.TrimPrefix(parsed.Format, "urn:x-nmos:format:"),
		mediaType: parsed.MediaType,
	}
}

// descCache builds one descriptor per metric name per collection, rather than one per sample —
// every worker on a node reports the same handful of names, so the cache hits N-1 times out of N.
type descCache struct {
	names     []string
	plain     map[string]*prometheus.Desc
	quantiled map[string]*prometheus.Desc
}

func newDescCache(names []string) *descCache {
	return &descCache{
		names:     names,
		plain:     map[string]*prometheus.Desc{},
		quantiled: map[string]*prometheus.Desc{},
	}
}

func (d *descCache) desc(name string) *prometheus.Desc {
	if desc, ok := d.plain[name]; ok {
		return desc
	}
	desc := prometheus.NewDesc(name, workerHelp(name), d.names, nil)
	d.plain[name] = desc
	return desc
}

func (d *descCache) quantileDesc(name string) *prometheus.Desc {
	if desc, ok := d.quantiled[name]; ok {
		return desc
	}
	desc := prometheus.NewDesc(name, workerHelp(name),
		append(slices.Clone(d.names), metrics.LabelQuantile), nil)
	d.quantiled[name] = desc
	return desc
}

// emit turns one worker's samples into metrics.
//
// # Quantiles are gauges, not a Prometheus summary
//
// The worker reports quantile *estimates* over a sliding 30 s window and has no observation
// count or sum to give (WRS §6). Modelling that as a [prometheus.Summary] means inventing both:
// `_count 0` alongside a populated p50 is self-contradictory, and it is a new series that says
// something false rather than nothing. A gauge per quantile is what the data actually is, and
// the series an existing dashboard selects — `mxl_source_latency_ns{quantile="0.5"}` — is
// identical either way.
//
// A name the worker emits that is not in the flow namespace is dropped. The worker's names are
// not constructed here, they arrive fully qualified, so this is the one place the prefix rule
// can be enforced on them (invariant 12).
func emit(ch chan<- prometheus.Metric, descs *descCache, r result) {
	values := labelValues(descs.names, r.target)

	for _, sample := range r.samples {
		if !metrics.IsFlow(sample.Name) {
			continue
		}

		if !sample.IsSummary() {
			ch <- prometheus.MustNewConstMetric(descs.desc(sample.Name),
				prometheus.CounterValue, sample.Value, values...)
			continue
		}

		// NaN is passed through: a summary with no observations in its window emits exactly
		// that, and it means "nothing measured", which a zero would misreport as "measured
		// zero".
		ch <- prometheus.MustNewConstMetric(descs.quantileDesc(sample.Name),
			prometheus.GaugeValue, sample.Value,
			append(slices.Clone(values), formatQuantile(*sample.Quantile))...)
	}
}

// formatQuantile renders a quantile the way Prometheus itself does: the shortest representation
// that round-trips, so 0.01, 0.5, 0.99.
//
// The legacy proxy wrote three fixed decimals — `quantile="0.010"` — which nothing else in the
// ecosystem produces, so a dashboard carried over from it selects on a label value that no
// longer exists. Recorded as one of the migration's label changes rather than preserved.
func formatQuantile(q float64) string {
	return strconv.FormatFloat(q, 'g', -1, 64)
}

// supervisionHelp describes the three series the agent produces without asking a worker. They
// sit in the flow namespace because they describe a flow and its transfer, and because the legacy
// proxy emitted them under these names (§2.2).
var supervisionHelp = map[string]string{
	metrics.Flow("worker_restarts"): "Times this worker has been restarted since the agent began supervising it.",
	metrics.Flow("writer_active"):   "1 when the flow's head index has advanced recently. Hysteretic and coarse.",
	metrics.Flow("reader_active"):   "1 when something has read from the flow recently. Hysteretic and coarse.",
}

// workerHelp describes the worker's own metrics (WRS §6). The worker emits no HELP of its own.
func workerHelp(name string) string {
	if help, ok := workerMetricHelp[name]; ok {
		return help
	}
	if help, ok := supervisionHelp[name]; ok {
		return help
	}
	// A metric a newer worker emits and this build has never heard of. Exported anyway — an
	// unknown counter is more useful than a missing one, and the alternative is that a worker
	// upgrade silently drops data until the supervisor catches up.
	return "Reported by the mxl-fabrics-proxy-worker binary."
}

var workerMetricHelp = map[string]string{
	metrics.Flow("octets_total"): "Sum of grain payload sizes. For continuous flows this is a sample count, not octets.",
	// The naming is inverted from what you would expect and it is the worker's, not ours: the
	// "payload" counter is the larger one, octets_total plus 4096 per grain (WRS §6).
	metrics.Flow("payload_octets_total"): "Rough on-wire size including per-grain header overhead.",
	metrics.Flow("grains_total"):         "Grains transferred, or sample batches for a continuous flow.",
	metrics.Flow("grains_lost"):          "Index gap observed since the previous grain.",
	metrics.Flow("source_latency_ns"):    "Age of the media at this hop, as sliding-window quantile estimates.",
	metrics.Flow("network_latency_ns"):   "Receive time minus transmit time, target only, as sliding-window quantile estimates.",
	metrics.Flow("last_grain"):           "Always zero: declared by the worker and never updated (WRS §6).",
}
