package exec

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/jonasohland/mxl-replicator/internal/api"
	"github.com/jonasohland/mxl-replicator/internal/worker"
)

// config is the worker's JSON config file (WRS §3), read once at startup and never again.
//
// Only keys the C++ side actually reads appear here. The legacy supervisor also wrote
// `proxy_id`, `efa_use_wait` and `labels`, none of which exist anywhere in `src/` — they were
// supervisor bookkeeping that happened to ride along in the same struct, and `efa_use_wait`
// was documenting a flag that had already been deleted on both sides.
type config struct {
	Target bool `json:"target"`

	Domain   string       `json:"domain"`
	Node     string       `json:"node"`
	Service  string       `json:"service"`
	Provider api.Provider `json:"provider"`

	// CapsFlags and MaxMessageSize are the negotiated interface configuration and **must be
	// identical on both ends** — the library performs no negotiation of its own (WRS §3,
	// §10.3). They arrive here already agreed, in the same spelling the probe printed them,
	// so nothing on this path translates between names and bits.
	CapsFlags      []api.CapFlag `json:"caps_flags"`
	MaxMessageSize uint64        `json:"max_message_size"`

	// IdleTimeoutMS and ConnectTimeoutMS are written unconditionally, never omitted.
	//
	// This is the one place the "0 means wait indefinitely" sentinel can be lost, and losing it
	// is expensive: an omitted `idle_timeout_ms` is not "wait forever", it is the worker's
	// built-in 10 s, which puts every paused session into a permanent ~13 s restart cycle and
	// a control-plane round trip per cycle (§11.1). `omitempty` on either of these would do
	// exactly that, silently, for the most common configured value.
	IdleTimeoutMS    int64 `json:"idle_timeout_ms"`
	ConnectTimeoutMS int64 `json:"connect_timeout_ms"`

	MetricsSocket string `json:"metrics_socket"`

	// TargetInfo is two different things by role, which is the sharpest asymmetry in the
	// worker's interface (WRS §3): an **output file path** for a target, which writes its blob
	// there once the fabric endpoint is up, and the blob itself **inline** for an initiator.
	TargetInfo string `json:"target_info"`

	FlowID string `json:"flow_id,omitempty"`

	// FlowDef is the flow definition JSON as a *string* — JSON embedded in a JSON string.
	FlowDef string `json:"flow_def,omitempty"`

	NoNetworkLatencyMeasurement bool `json:"no_network_latency_measurement"`

	// SchedPrio is omitted rather than null when unset. The worker treats absent and null
	// alike, but omitting says what is meant.
	SchedPrio *int `json:"sched_prio,omitempty"`
}

// buildConfig renders a spec into the worker's config, given the two paths the launcher owns.
func buildConfig(spec worker.Spec, metricsSocket, targetInfoPath string) config {
	cfg := config{
		Target:                      spec.IsTarget(),
		Domain:                      spec.DomainPath,
		Node:                        spec.BindAddress,
		Service:                     spec.Service,
		Provider:                    spec.Interface.Provider,
		CapsFlags:                   spec.Interface.CapFlags,
		MaxMessageSize:              spec.Interface.MaxMessageSize,
		IdleTimeoutMS:               spec.IdleTimeout.Milliseconds(),
		ConnectTimeoutMS:            spec.ConnectTimeout.Milliseconds(),
		MetricsSocket:               metricsSocket,
		FlowID:                      spec.FlowID,
		NoNetworkLatencyMeasurement: spec.NoNetworkLatencyMeasurement,
		SchedPrio:                   spec.SchedPrio,
	}

	if spec.IsTarget() {
		cfg.TargetInfo = targetInfoPath
		cfg.FlowDef = string(spec.FlowDef)
	} else {
		cfg.TargetInfo = spec.TargetInfo
	}

	return cfg
}

// writeConfig writes the config file the worker is started with.
func writeConfig(path string, cfg config) error {
	// The blob a target's peer will consume passes through here, and so does whatever the
	// operator's flow definitions contain, so keep it to the worker's own user.
	file, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("worker: create config: %w", err)
	}
	defer func() { _ = file.Close() }()

	encoder := json.NewEncoder(file)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(cfg); err != nil {
		return fmt.Errorf("worker: write config: %w", err)
	}
	return file.Close()
}
