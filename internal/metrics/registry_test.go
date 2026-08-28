package metrics

import (
	"bytes"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jonasohland/mxl-replicator/internal/version"
)

func TestNewCarriesTheBuildIdentity(t *testing.T) {
	families, err := New().Gather()
	require.NoError(t, err)

	var found *dto.Metric
	names := make([]string, 0, len(families))
	for _, family := range families {
		names = append(names, family.GetName())
		if family.GetName() == Control("build_info") {
			require.Len(t, family.GetMetric(), 1)
			found = family.GetMetric()[0]
		}
	}
	require.NotNil(t, found, "build_info missing from %v", names)

	assert.Equal(t, float64(1), found.GetGauge().GetValue())
	labels := map[string]string{}
	for _, pair := range found.GetLabel() {
		labels[pair.GetName()] = pair.GetValue()
	}
	assert.Equal(t, version.Get().Version, labels["version"])
	assert.Contains(t, names, "go_goroutines", "the Go collector answers §4.4's leader-churn question")
}

func TestEachRegistryIsItsOwn(t *testing.T) {
	// A combined instance builds one per role and serves them on two listeners (§4.7). Two
	// calls must not fight over a shared registerer, and a series registered on one must not
	// appear on the other.
	agent, server := New(), New()
	require.NotSame(t, agent, server)

	counter := prometheus.NewCounter(prometheus.CounterOpts{Name: Control("test_only"), Help: "."})
	require.NoError(t, agent.Register(counter))

	assert.Contains(t, gatheredNames(t, agent), Control("test_only"))
	assert.NotContains(t, gatheredNames(t, server), Control("test_only"))
}

func TestHandlerServesWhatItCanWhenACollectorFails(t *testing.T) {
	// A broken collector must not blank the endpoint: the series still being collected are the
	// ones an operator needs while something is wrong.
	reg := prometheus.NewRegistry()
	healthy := prometheus.NewCounter(prometheus.CounterOpts{Name: Control("healthy"), Help: "."})
	healthy.Inc()
	require.NoError(t, reg.Register(healthy))
	require.NoError(t, reg.Register(brokenCollector{}))

	logs := &bytes.Buffer{}
	logger := slog.New(slog.NewTextHandler(logs, nil))

	recorder := httptest.NewRecorder()
	Handler(reg, logger).ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/metrics", nil))

	assert.Equal(t, http.StatusOK, recorder.Code)
	assert.Contains(t, recorder.Body.String(), Control("healthy")+" 1")
	assert.Contains(t, logs.String(), "collector is broken", "a gather failure must reach the process logger")
}

func TestHandlerRendersTheStandardExposition(t *testing.T) {
	reg := prometheus.NewRegistry()
	grains := prometheus.NewCounter(prometheus.CounterOpts{Name: Flow("grains_total"), Help: "Grains."})
	grains.Add(300)
	require.NoError(t, reg.Register(grains))

	recorder := httptest.NewRecorder()
	Handler(reg, slog.New(slog.DiscardHandler)).ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/metrics", nil))

	require.Equal(t, http.StatusOK, recorder.Code)
	body := recorder.Body.String()
	// The TYPE lines the worker never emits and the legacy supervisor hand-wrote.
	assert.Contains(t, body, "# TYPE mxl_grains_total counter")
	assert.Contains(t, body, "mxl_grains_total 300")
}

func gatheredNames(t *testing.T, gatherer prometheus.Gatherer) []string {
	t.Helper()

	families, err := gatherer.Gather()
	require.NoError(t, err)

	names := make([]string, 0, len(families))
	for _, family := range families {
		names = append(names, family.GetName())
	}
	return names
}

// brokenCollector describes a metric it then refuses to produce.
type brokenCollector struct{}

func (brokenCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- prometheus.NewDesc(Control("broken"), ".", nil, nil)
}

func (brokenCollector) Collect(ch chan<- prometheus.Metric) {
	ch <- prometheus.NewInvalidMetric(
		prometheus.NewDesc(Control("broken"), ".", nil, nil),
		errors.New("collector is broken"),
	)
}
