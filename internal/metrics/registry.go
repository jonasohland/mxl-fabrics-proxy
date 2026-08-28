package metrics

import (
	"fmt"
	"log/slog"
	"net/http"
	"strings"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/jonasohland/mxl-replicator/internal/version"
)

// New builds the registry behind one role's /metrics endpoint.
//
// Deliberately not [prometheus.DefaultRegisterer]. A combined instance runs both roles in one
// process and serves them on two listeners (§4.7), and the two endpoints expose different
// things: the agent's is a node's workers, the server's is the fleet's control plane. Sharing
// a process-global registry would put every series on both, and make which-endpoint-has-what
// depend on package import order.
//
// The Go and process collectors are included on both. They are not this project's metrics and
// carry no `mxl_` prefix, which is why the namespace claim in the package doc is about the
// metrics this project defines. They earn their place because a combined node's control plane
// competes with RT workers for the runtime (§4.4) and an OOM kill there now costs media
// (§4.3) — both are questions about the process, and neither is answerable from the series
// this project emits.
func New() *prometheus.Registry {
	reg := prometheus.NewRegistry()
	reg.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
		buildInfo(version.Get()),
	)
	return reg
}

// buildInfo is the conventional always-1 gauge carrying the build identity in its labels.
//
// The server already surfaces the fleet's version spread from what agents report at
// registration (§13.1), but that is the control plane's view of the fleet and it stops at the
// process that would have to be running to serve it. This is each process's own claim about
// itself, on the endpoint the operator's monitoring already scrapes.
func buildInfo(info version.Info) prometheus.Collector {
	gauge := prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: Control("build_info"),
		Help: "Build identity of this mxl-replicator process. Always 1.",
	}, []string{"version", "revision", "go"})

	// Version already carries "-dirty" when the build stamped it from `git describe --dirty`
	// (see the Makefile), so Modified is not a fourth label.
	gauge.WithLabelValues(info.Version, info.Revision, info.Go).Set(1)
	return gauge
}

// maxRequestsInFlight bounds concurrent collections.
//
// The agent's endpoint scrapes every worker's socket inside the request (§12), so the cost of a
// collection is real and two collectors — or one collector retrying after a timeout, or a human
// with curl in a loop — would otherwise multiply the fan-out by however many arrive at once.
// Excess requests get a 503, which is the right answer: a scraper that is already being served
// is not owed a second concurrent pass.
const maxRequestsInFlight = 4

// Handler serves a gatherer over HTTP.
//
// Errors continue rather than fail: a single collector that cannot gather must not blank a
// node's whole endpoint, because the series still being collected are the ones an operator
// needs while something is wrong. A gather that produces nothing at all is still a 500.
//
// No handler-level timeout. A collector that scrapes worker sockets carries its own per-worker
// and overall deadlines and returns partial results (§12); a second bound here would cut off a
// collection that was going to answer, and turn a slow scrape into no scrape.
func Handler(gatherer prometheus.Gatherer, logger *slog.Logger) http.Handler {
	return promhttp.HandlerFor(gatherer, promhttp.HandlerOpts{
		ErrorLog:            promLogger{logger: logger},
		ErrorHandling:       promhttp.ContinueOnError,
		MaxRequestsInFlight: maxRequestsInFlight,
	})
}

// promLogger adapts the process logger to promhttp's Println-only logger interface.
//
// Same argument as the etcd client's bridge: handed no logger, promhttp writes gather errors
// through the standard log package, which means a second differently-shaped format
// interleaved into the operator's output — and suppressing them instead would hide exactly
// the failures that make a metric go missing.
type promLogger struct{ logger *slog.Logger }

func (l promLogger) Println(v ...any) {
	l.logger.Error(strings.TrimSuffix(fmt.Sprintln(v...), "\n"))
}
