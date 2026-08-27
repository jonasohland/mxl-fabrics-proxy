package health

import (
	"net/http"
	"time"

	"github.com/jonasohland/mxl-fabrics-proxy/legacy/go/pkg/initiator"
	"github.com/jonasohland/mxl-fabrics-proxy/legacy/go/pkg/server"
	"github.com/jonasohland/mxl-fabrics-proxy/legacy/go/pkg/target"
	"github.com/jonasohland/mxl-fabrics-proxy/legacy/go/pkg/worker"
)

// Status is the body returned by the health endpoint. The counts are
// informational: the endpoint reports whether the proxy process is up and
// serving, not whether any flow is currently being transferred. Transfers
// recover on their own and a remote peer being down must not get this proxy
// restarted, so worker and flow state is reported through metrics instead.
type Status struct {
	Status           string  `json:"status"`
	UptimeSeconds    float64 `json:"uptime_seconds"`
	ProxyVersion     string  `json:"proxy_version"`
	MXLVersion       string  `json:"mxl_version"`
	LibfabricVersion string  `json:"libfabric_version"`
	Domains          int     `json:"domains"`
	Subscriptions    int     `json:"subscriptions"`
	Targets          int     `json:"targets"`
}

type Health struct {
	started  time.Time
	versions worker.Versions

	domains *initiator.Domains
	subs    *initiator.Subscriptions
	targets *target.Targets
}

func NewHealth(versions worker.Versions, domains *initiator.Domains, subs *initiator.Subscriptions, targets *target.Targets) *Health {
	return &Health{
		started:  time.Now(),
		versions: versions,

		domains: domains,
		subs:    subs,
		targets: targets,
	}
}

func (h *Health) Mux(mux *http.ServeMux) {
	mux.Handle("GET /healthz", h)
	mux.Handle("GET /v1/health", h)
}

func (h *Health) ServeHTTP(w http.ResponseWriter, _ *http.Request) {
	server.Response(w, &Status{
		Status:           "ok",
		UptimeSeconds:    time.Since(h.started).Seconds(),
		ProxyVersion:     h.versions.Proxy,
		MXLVersion:       h.versions.MXL,
		LibfabricVersion: h.versions.Libfabric,
		Domains:          h.domains.Count(),
		Subscriptions:    h.subs.Count(),
		Targets:          h.targets.Count(),
	})
}
