package agent

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jonasohland/mxl-utils/pkg/testutil"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jonasohland/mxl-replicator/internal/api"
	"github.com/jonasohland/mxl-replicator/internal/metrics"
	"github.com/jonasohland/mxl-replicator/internal/worker"
)

// workerSamples is what a real worker puts on its socket (WRS §6), trimmed to one quantile.
func workerSamples() []worker.Sample {
	return []worker.Sample{
		worker.Counter("mxl_octets_total", 1234567),
		worker.Counter("mxl_grains_total", 300),
		worker.Counter("mxl_grains_lost", 0),
		worker.Quantile("mxl_source_latency_ns", 0.5, 498000),
	}
}

// expose collects the agent's metrics the way Prometheus would, through the real handler.
func (h *harness) expose() string {
	h.t.Helper()

	registry := prometheus.NewRegistry()
	require.NoError(h.t, registry.Register(h.Agent.Collector()))

	recorder := httptest.NewRecorder()
	metrics.Handler(registry, discard()).ServeHTTP(recorder,
		httptest.NewRequest(http.MethodGet, "/metrics", nil))

	// Anything less than 200 means the gather failed outright, which is the failure mode the
	// overall deadline exists to prevent — worth failing loudly rather than on a missing line.
	require.Equal(h.t, http.StatusOK, recorder.Code, recorder.Body.String())
	return recorder.Body.String()
}

// runningTarget brings up one target worker and returns its session id.
func (h *harness) runningTarget(sessionID string) string {
	h.t.Helper()

	h.server.assign("edge-01", targetAssignment(sessionID))
	h.eventually("the target to be running", func() bool {
		return h.launcher.Find(sessionID, api.RoleTarget) != nil
	})
	return sessionID
}

func TestWorkerMetricsCarryTheirLabels(t *testing.T) {
	h := newHarness(t, harnessOptions{})
	h.launcher.SetMetrics(workerSamples())
	h.run()
	h.runningTarget("s1")

	body := h.expose()

	// The labels this project applies itself, in one line, because the exposition is what a
	// dashboard is actually written against. `format` is the definition's
	// "urn:x-nmos:format:video" with the urn prefix taken off, which is what anyone would type.
	const own = `direction="target",domain="ingest",flow_id="5592a23b-0974-45bb-9388-89ea81c42537",format="video",media_type="",session="s1"`
	assert.Contains(t, body, `mxl_grains_total{`+own+`} 300`)
	assert.Contains(t, body, `mxl_octets_total{`+own+`} 1.234567e+06`)

	// The domain label is the *name*, never the path the worker was given — which for an output
	// domain is a directory under a root the operator configured (§10.6, §12).
	assert.NotContains(t, body, h.outputDomain)

	// A quantile is a gauge with a quantile label, rendered the way Prometheus renders one —
	// "0.5", not the legacy proxy's "0.500". The encoder sorts label names, so it lands between
	// flow_id and session.
	assert.Contains(t, body, "# TYPE mxl_source_latency_ns gauge")
	assert.Contains(t, body, `quantile="0.5",session="s1"} 498000`)
	// And specifically not a summary, which would need a count and a sum the worker never gives.
	assert.NotContains(t, body, "mxl_source_latency_ns_count")
	assert.NotContains(t, body, "mxl_source_latency_ns_sum")

	// This assignment's flow does not exist in the domain, so the agent is not observing it. No
	// liveness gauges at all rather than a zero, which would report "nothing is reading this"
	// when the truth is "nobody looked".
	assert.NotContains(t, body, "mxl_writer_active")
	assert.NotContains(t, body, "mxl_reader_active")
	// Restarts do not depend on observing anything.
	assert.Contains(t, body, `mxl_worker_restarts{`+own+`} 0`)
}

func TestLivenessGaugesFollowTheFlow(t *testing.T) {
	// mxl_writer_active and mxl_reader_active, sourced from the flow's own runtime info rather
	// than the legacy proxy's hand-rolled mmap at fixed offsets (WRS §10 item 11). Both are
	// deltas of a counter, never a comparison against a clock — LastReadTime is TAI nanoseconds
	// and means nothing absolute without a configured offset (§11.1).
	h := newHarness(t, harnessOptions{})
	h.launcher.SetMetrics(workerSamples())
	h.run()

	// The flow lives in the *destination* domain, which does not exist until the assignment is
	// accepted and is observed only because the agent adds it to its own watch set (§10.6). If
	// that step were missing this test would hang here rather than fail somewhere legible, which
	// is exactly how the bug presents in production.
	require.NoDirExists(t, h.outputDomain)

	flow, err := testutil.RandomVideoFlow(h.outputDomain)
	require.NoError(t, err)

	def, err := json.Marshal(flow.Definition())
	require.NoError(t, err)
	assignment := targetAssignment("s1")
	assignment.FlowID, assignment.FlowDef = flow.ID(), def

	h.server.assign("edge-01", assignment)
	h.eventually("the target to be running", func() bool {
		return h.launcher.Find("s1", api.RoleTarget) != nil
	})

	// Created by the target worker in reality; created here because the launcher is a fake.
	require.NoError(t, flow.Create())
	h.eventually("the gauges to appear once the flow is observed", func() bool {
		return strings.Contains(h.expose(), "mxl_writer_active")
	})

	// A flow that merely has a head index is not being written to. Only movement says so.
	assert.Regexp(t, `mxl_writer_active\{[^}]*\} 0`, h.expose())
	assert.Regexp(t, `mxl_reader_active\{[^}]*\} 0`, h.expose())

	writing := keepMoving(t, flow, 1, 0)
	h.eventually("writer_active to go high", func() bool {
		return regexp.MustCompile(`mxl_writer_active\{[^}]*\} 1`).MatchString(h.expose())
	})
	assert.Regexp(t, `mxl_reader_active\{[^}]*\} 0`, h.expose(), "writing is not reading")
	writing()

	reading := keepMoving(t, flow, 0, 1000)
	h.eventually("reader_active to go high", func() bool {
		return regexp.MustCompile(`mxl_reader_active\{[^}]*\} 1`).MatchString(h.expose())
	})
	reading()

	// The definition's media type reaches the labels, which is the other half of what a
	// flow-definition label is for.
	assert.Contains(t, h.expose(), `media_type="`+flow.Definition().MediaType+`"`)
}

func TestRestartsAreCountedMonotonically(t *testing.T) {
	// The count must not decay. The windowed list DEGRADED is classified from does decay, on
	// purpose (§15.1), and a counter that shrank would read as a reset to rate() every time the
	// window slid.
	h := newHarness(t, harnessOptions{})
	h.launcher.SetMetrics(workerSamples())
	h.run()
	h.runningTarget("s1")

	restarts := regexp.MustCompile(`mxl_worker_restarts\{[^}]*\} (\d+)`)
	count := func() string {
		match := restarts.FindStringSubmatch(h.expose())
		require.NotNil(t, match, "mxl_worker_restarts is missing")
		return match[1]
	}
	require.Equal(t, "0", count())

	for range 2 {
		handle := h.launcher.Find("s1", api.RoleTarget)
		require.NotNil(t, handle)
		handle.Die(errors.New("crashed"))
		h.eventually("the worker to come back", func() bool {
			replacement := h.launcher.Find("s1", api.RoleTarget)
			return replacement != nil && replacement != handle
		})
	}

	h.eventually("both restarts to be counted", func() bool { return count() == "2" })

	// A window shorter than the test cannot take the count back down.
	h.consistently("the count to hold", 200*time.Millisecond, func() bool { return count() == "2" })
}

func TestAWorkerInBackoffStillReportsItsRestarts(t *testing.T) {
	// The one case where supervision-level and worker-level series must disagree: a crash-looping
	// worker has no process to scrape, which is exactly when its restart count is worth reading.
	h := newHarness(t, harnessOptions{tweak: func(cfg *Config) {
		cfg.BackoffMin, cfg.BackoffMax = 2*time.Second, 2*time.Second
	}})
	h.launcher.SetMetrics(workerSamples())
	h.run()
	h.runningTarget("s1")

	h.launcher.Find("s1", api.RoleTarget).Die(errors.New("crashed"))

	h.eventually("the worker counters to go away", func() bool {
		return !strings.Contains(h.expose(), "mxl_grains_total")
	})

	body := h.expose()
	assert.Regexp(t, `mxl_worker_restarts\{[^}]*\} 1`, body)
	assert.Contains(t, body, "mxl_repl_workers_scraped 0", "nothing was running to scrape")
	assert.Contains(t, body, "mxl_repl_worker_scrapes_failed_total 0", "and nothing failed")
}

func TestAWorkerThatIsNotRunningReportsNothing(t *testing.T) {
	// The liveness half of the on-demand argument: a series disappears with the process it
	// describes, which is what lets Prometheus stop a rate rather than carry the last value
	// forward. A cached snapshot would keep serving these counters until its next refresh.
	h := newHarness(t, harnessOptions{})
	h.launcher.SetMetrics(workerSamples())
	h.run()
	h.runningTarget("s1")

	require.Contains(t, h.expose(), "mxl_grains_total")

	handle := h.launcher.Find("s1", api.RoleTarget)
	require.NotNil(t, handle)
	handle.Die(nil)

	h.eventually("the worker's series to go away", func() bool {
		return !strings.Contains(h.expose(), "mxl_grains_total")
	})

	// The scrape itself is still healthy — there is simply nothing to scrape.
	assert.Contains(t, h.expose(), "mxl_repl_workers_scraped 0")
}

func TestTwoSessionsOnOneFlowDoNotCollide(t *testing.T) {
	// One flow replicated to two destinations puts two initiators on the source node whose
	// direction, domain and flow_id are identical. Without the session label that is one series
	// collected twice, which is a gather error that discards the whole family — every worker's
	// counters, not just these two.
	h := newHarness(t, harnessOptions{})
	h.launcher.SetMetrics(workerSamples())
	h.run()

	// Same domain and same flow id on both, which is what initiatorAssignmentFor produces —
	// only the session differs.
	h.server.assign("edge-01",
		initiatorAssignmentFor(t, "s1"),
		initiatorAssignmentFor(t, "s2"))

	h.eventually("both initiators to be running", func() bool {
		return h.launcher.Find("s1", api.RoleInitiator) != nil &&
			h.launcher.Find("s2", api.RoleInitiator) != nil
	})

	body := h.expose()
	assert.Contains(t, body, `session="s1"`)
	assert.Contains(t, body, `session="s2"`)
	assert.Contains(t, body, "mxl_repl_workers_scraped 2")
}

func TestUserLabelsAreUnionedAcrossWorkers(t *testing.T) {
	// A metric family must have one label dimension. User labels come from the request that
	// created each session, so two sessions routinely disagree about which keys exist, and the
	// union with empty fill is what keeps that from being a gather error.
	h := newHarness(t, harnessOptions{})
	h.launcher.SetMetrics(workerSamples())
	h.run()

	tenant := targetAssignment("s1")
	tenant.Labels = map[string]string{"tenant": "studio-a"}
	studio := targetAssignment("s2")
	studio.Labels = map[string]string{"studio": "b"}

	h.server.assign("edge-01", tenant, studio)
	h.eventually("both targets to be running", func() bool {
		return h.launcher.Find("s1", api.RoleTarget) != nil &&
			h.launcher.Find("s2", api.RoleTarget) != nil
	})

	body := h.expose()
	assert.Contains(t, body, `session="s1",studio="",tenant="studio-a"`)
	assert.Contains(t, body, `session="s2",studio="b",tenant=""`)
}

func TestUnusableUserLabelsAreDropped(t *testing.T) {
	// A label name this project sets itself would either overwrite the real value or duplicate a
	// dimension; an invalid one invalidates the metric at collection time and takes its family
	// with it. Both are dropped rather than mangled.
	h := newHarness(t, harnessOptions{})
	h.launcher.SetMetrics(workerSamples())
	h.run()

	assignment := targetAssignment("s1")
	assignment.Labels = map[string]string{
		"direction":  "sideways",
		"quantile":   "0.99",
		"not-valid":  "dashes",
		"__reserved": "prometheus owns this",
		"tenant":     "studio-a",
	}
	h.server.assign("edge-01", assignment)
	h.eventually("the target to be running", func() bool {
		return h.launcher.Find("s1", api.RoleTarget) != nil
	})

	body := h.expose()
	assert.Contains(t, body, `direction="target"`)
	assert.NotContains(t, body, "sideways")
	assert.NotContains(t, body, "not-valid")
	assert.NotContains(t, body, "__reserved")
	assert.Contains(t, body, `tenant="studio-a"`, "a usable label alongside unusable ones survives")
}

func TestOnlyFlowNamespaceMetricsAreExported(t *testing.T) {
	// The worker's names arrive fully qualified rather than being constructed here, so this is
	// the one place the prefix rule can be enforced on them (invariant 12).
	h := newHarness(t, harnessOptions{})
	h.launcher.SetMetrics([]worker.Sample{
		worker.Counter("mxl_grains_total", 300),
		worker.Counter("mxl_repl_reconciles_total", 7),
		worker.Counter("go_goroutines", 12),
		worker.Counter("garbage", 1),
	})
	h.run()
	h.runningTarget("s1")

	body := h.expose()
	assert.Contains(t, body, "mxl_grains_total")
	assert.NotContains(t, body, "mxl_repl_reconciles_total")
	assert.NotContains(t, body, "go_goroutines")
	assert.NotContains(t, body, "garbage")
}

func TestNaNQuantilesSurviveTheRoundTrip(t *testing.T) {
	// A summary with no observations in its 30 s window emits nan, which means "nothing
	// measured". Zeroing it would report a latency of zero, which is a different claim.
	h := newHarness(t, harnessOptions{})
	h.launcher.SetMetrics([]worker.Sample{
		worker.Quantile("mxl_source_latency_ns", 0.99, math.NaN()),
	})
	h.run()
	h.runningTarget("s1")

	assert.Contains(t, h.expose(), `quantile="0.99",session="s1"} NaN`)
}

func TestAWedgedWorkerDoesNotCostTheEndpoint(t *testing.T) {
	// The one way scraping on demand loses to a cache: a worker that accepts and never answers
	// occupies a pool slot. The overall deadline is what keeps that from pushing the collection
	// past the collecting scraper's own timeout and losing the healthy workers' series too.
	h := newHarness(t, harnessOptions{tweak: func(cfg *Config) {
		cfg.Launcher = &wedgingLauncher{Launcher: cfg.Launcher, wedge: "s2"}
		cfg.ScrapeConcurrency = 2
		cfg.WorkerScrapeTimeout = 50 * time.Millisecond
		cfg.ScrapeTimeout = 500 * time.Millisecond
	}})
	h.launcher.SetMetrics(workerSamples())
	h.run()

	h.server.assign("edge-01", targetAssignment("s1"), targetAssignment("s2"))
	h.eventually("both targets to be running", func() bool {
		return h.launcher.Find("s1", api.RoleTarget) != nil &&
			h.launcher.Find("s2", api.RoleTarget) != nil
	})

	started := time.Now()
	body := h.expose()
	elapsed := time.Since(started)

	assert.Less(t, elapsed, 2*time.Second, "a wedged worker must not hold the endpoint open")
	assert.Contains(t, body, `mxl_grains_total{`, "the healthy worker still reports")
	assert.Regexp(t, `mxl_grains_total\{[^}]*session="s1"`, body)
	// The wedged worker contributes no *worker* counters. Its restart count still appears, which
	// is supervision's own knowledge and does not depend on reaching the process.
	assert.NotRegexp(t, `mxl_grains_total\{[^}]*session="s2"`, body)
	assert.Regexp(t, `mxl_worker_restarts\{[^}]*session="s2"`, body)
	assert.Contains(t, body, "mxl_repl_worker_scrapes_failed_total 1")
	assert.Contains(t, body, "mxl_repl_workers_scraped 2", "both were attempted")
	assert.Contains(t, body, "mxl_repl_worker_scrape_duration_seconds")
}

// keepMoving advances a flow's counters until the returned function is called.
//
// One bump is not enough to assert on. Liveness is movement *seen*, and it decays after the idle
// threshold — 50 ms in this harness — so a single step can go high and back down between two
// polls. Something has to keep moving, which is also what a real producer does.
func keepMoving(t *testing.T, flow *testutil.DummyFlow, head, read uint64) func() {
	t.Helper()

	stop, stopped := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(stopped)
		runtime := flow.Runtime()
		for {
			select {
			case <-stop:
				return
			default:
			}
			runtime.HeadIndex += head
			runtime.LastReadTime += read
			if err := flow.UpdateRuntime(runtime); err != nil {
				return
			}
			time.Sleep(2 * time.Millisecond)
		}
	}()

	var once sync.Once
	return func() {
		once.Do(func() {
			close(stop)
			<-stopped
		})
	}
}

// wedgingLauncher makes one session's worker accept a scrape and never answer it — a process
// that is stuck rather than gone, which is the case the deadlines exist for.
type wedgingLauncher struct {
	worker.Launcher
	wedge string
}

func (l *wedgingLauncher) Start(ctx context.Context, spec worker.Spec) (worker.Handle, error) {
	handle, err := l.Launcher.Start(ctx, spec)
	if err != nil || spec.SessionID != l.wedge {
		return handle, err
	}
	return wedgedHandle{Handle: handle}, nil
}

type wedgedHandle struct{ worker.Handle }

func (h wedgedHandle) Metrics(ctx context.Context) ([]worker.Sample, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}
