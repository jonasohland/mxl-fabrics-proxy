package metrics

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestNamesCarryTheRightPrefix(t *testing.T) {
	// Invariant 12. The whole point of the package: these two strings are what dashboards are
	// written against, and they are expensive to change once they exist.
	assert.Equal(t, "mxl_grains_total", Flow("grains_total"))
	assert.Equal(t, "mxl_repl_reconcile_duration_seconds", Control("reconcile_duration_seconds"))
}

func TestFlowNamesMatchTheWorkersOwn(t *testing.T) {
	// The supervisor-level series sit in the same namespace as the ones scraped off the socket,
	// and the legacy proxy emitted these three under exactly these names
	// (legacy/go/pkg/worker/metrics.go:100-107). A rename here is a silently broken dashboard.
	for _, name := range []string{"mxl_worker_restarts", "mxl_writer_active", "mxl_reader_active"} {
		assert.True(t, IsFlow(name), "%s must stay in the flow namespace", name)
	}

	// And every name the worker itself emits (WRS §6) survives the gate unchanged.
	for _, name := range []string{
		"mxl_octets_total", "mxl_payload_octets_total", "mxl_grains_total", "mxl_grains_lost",
		"mxl_source_latency_ns", "mxl_network_latency_ns", "mxl_last_grain",
	} {
		assert.True(t, IsFlow(name), "%s comes off the metrics socket and must be exportable", name)
		assert.False(t, IsControl(name))
	}
}

func TestBadNamesPanicRatherThanBeingEmitted(t *testing.T) {
	// Both constructors are meant to be called from package-level vars, so a panic here is a
	// startup failure rather than a metric nobody notices is misnamed.
	for name, bad := range map[string]string{
		"already prefixed":   "mxl_grains_total",
		"control prefixed":   "mxl_repl_paths",
		"empty":              "",
		"uppercase":          "Grains_Total",
		"dashes":             "grains-total",
		"leading digit":      "1_grains",
		"leading underscore": "_grains",
		"recording rule":     "mxl:grains:rate5m",
	} {
		t.Run(name, func(t *testing.T) {
			assert.Panics(t, func() { Flow(bad) })
			assert.Panics(t, func() { Control(bad) })
		})
	}
}

func TestTheTwoNamespacesDoNotOverlap(t *testing.T) {
	// A worker that named a metric into the control plane's subsystem — upstream change,
	// garbled line, anything — would collide with a series that means something else. The gate
	// on scraped names is what stops that, so it has to be exclusive in both directions.
	control := Control("agents_leased")
	assert.True(t, IsControl(control))
	assert.False(t, IsFlow(control))

	flow := Flow("grains_total")
	assert.True(t, IsFlow(flow))
	assert.False(t, IsControl(flow))
}

func TestGarbageIsNotAFlowMetric(t *testing.T) {
	for _, name := range []string{
		"",              // an empty line
		"mxl_",          // a prefix and nothing else
		"mxl",           // the namespace without its separator
		"mxl_Grains",    // not lower snake_case
		"mxl_grains[0]", // a summary line whose quantile was never split off
		"go_goroutines", // another namespace entirely
		"grains_total",  // unprefixed
	} {
		assert.False(t, IsFlow(name), "%q must not be exportable", name)
		assert.False(t, IsControl(name), "%q must not be exportable", name)
	}
}
