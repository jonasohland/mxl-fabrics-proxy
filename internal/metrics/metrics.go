// Package metrics is the naming layer every metric in this project goes through, and the
// registry that serves them.
//
// # The prefix split
//
// Metrics are prefixed by *what they describe*, not by which process emits them (§2.2, §12).
//
//   - [Flow] — `mxl_` — anything about a flow or a transfer. These names come off the worker's
//     metrics socket (WRS §6) and describe MXL rather than this project, so they are unchanged
//     from the legacy proxy and existing dashboards and alerts keep working across the
//     migration. That is worth more than nominal consistency.
//   - [Control] — `mxl_repl_` — control-plane metrics that exist only because of this project.
//
// This is Prometheus' `<namespace>_<subsystem>_` convention with namespace `mxl` and subsystem
// `repl`, so the whole project stays under one selectable namespace and `{__name__=~"mxl_.*"}`
// still catches everything.
//
// Both constructors panic on a name that is already prefixed or is not lower snake_case, and
// they are meant to be called from package-level `var` declarations, so a mistake is a startup
// failure rather than a metric nobody notices is misnamed. This is invariant 12 of the plan's
// checklist and the reason this package exists at all: prefixes have to be right before the
// first metric is emitted, because they are expensive to change once dashboards exist.
package metrics

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/prometheus/client_golang/prometheus"
)

const (
	// Namespace prefixes every metric this project emits, worker-derived or not.
	Namespace = "mxl"

	// Subsystem is the control plane's subsystem within [Namespace].
	Subsystem = "repl"
)

var (
	flowPrefix    = Namespace + "_"
	controlPrefix = Namespace + "_" + Subsystem + "_"
)

// nameRE is what this project will construct: lower snake_case, starting with a letter.
//
// Deliberately stricter than Prometheus itself, which also allows uppercase and `:`. The colon
// is reserved by convention for recording rules, and a name we build ourselves has no business
// needing either.
var nameRE = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)

// Flow builds the name of a metric describing a flow or a transfer.
//
// Pass the bare name: Flow("grains_total") is "mxl_grains_total", which is exactly what the
// worker already puts on its socket for that metric.
//
// Panics on a name that is already prefixed or is not lower snake_case.
func Flow(name string) string {
	return build("", name)
}

// Control builds the name of a control-plane metric.
//
// Pass the bare name: Control("reconcile_duration_seconds") is
// "mxl_repl_reconcile_duration_seconds".
//
// Panics on a name that is already prefixed or is not lower snake_case.
func Control(name string) string {
	return build(Subsystem, name)
}

func build(subsystem, name string) string {
	switch {
	case !nameRE.MatchString(name):
		panic(fmt.Sprintf("metrics: %q is not a valid metric name (want lower snake_case)", name))
	case strings.HasPrefix(name, flowPrefix):
		// The overwhelmingly likely mistake, because the worker's names arrive fully qualified
		// and copying one in here would silently produce mxl_mxl_grains_total.
		panic(fmt.Sprintf("metrics: %q is already prefixed; pass the bare name", name))
	}
	return prometheus.BuildFQName(Namespace, subsystem, name)
}

// IsFlow reports whether name belongs to the flow namespace: prefixed `mxl_`, but not
// `mxl_repl_`.
//
// This is the gate on names read off a worker's metrics socket. The worker emits its names
// fully qualified and this project does not construct them, so they are checked rather than
// built — and a worker naming a metric into the control plane's subsystem, whether by a future
// change upstream or by a garbled line, must not be able to collide with a control-plane series
// that means something else entirely.
func IsFlow(name string) bool {
	rest, ok := strings.CutPrefix(name, flowPrefix)
	return ok && !strings.HasPrefix(name, controlPrefix) && nameRE.MatchString(rest)
}

// IsControl reports whether name belongs to the control-plane subsystem.
func IsControl(name string) bool {
	rest, ok := strings.CutPrefix(name, controlPrefix)
	return ok && nameRE.MatchString(rest)
}

// Label names for worker metrics (§12).
//
// Two of the three are unchanged from the legacy proxy. The exceptions are recorded on the
// constants themselves, because each one is a dashboard or alert that needs editing at
// migration time and neither should be discovered rather than decided.
const (
	// LabelDirection is "initiator" or "target" — the values of api.Role, which is where it
	// comes from. Do not build a second mapping for it.
	LabelDirection = "direction"

	// LabelDomain is the domain *name*, where the legacy proxy put the domain's filesystem
	// path — it had nothing else, since a legacy worker config named a path directly.
	//
	// The name is the better label in the new model and not only for tidiness: it is what the
	// operator configured, what the API speaks and what is stable across hosts (§6.2, §7.2),
	// whereas a path is a per-node detail that makes the same logical domain look like a
	// different one on every node that spells it differently.
	LabelDomain = "domain"

	// LabelFlowID is the MXL flow id. The legacy proxy spelled this label `flowID`; snake_case
	// is the Prometheus convention and what the plan asks for.
	LabelFlowID = "flow_id"

	// LabelSession is the session id, and it is not decoration: without it, one flow replicated
	// to two destinations has two initiators on the source node whose other three labels are
	// identical, which is one duplicate series and a gather error that takes the whole metric
	// family with it. It also makes a series joinable to `GET /v1/paths`.
	LabelSession = "session"

	// LabelFormat and LabelMediaType come from the flow definition and say what kind of media is
	// moving — "video" and "video/v210". Low cardinality and stable for a flow's life, which is
	// what qualifies a flow-definition field to be a label at all.
	LabelFormat    = "format"
	LabelMediaType = "media_type"

	// LabelQuantile carries a summary's quantile. Reserved for that, so a user label may not
	// use it.
	LabelQuantile = "quantile"
)

// WorkerLabelNames are the labels this project puts on worker metrics itself, in exposition
// order. A user label may not use one of these, nor [LabelQuantile].
func WorkerLabelNames() []string {
	return []string{LabelDirection, LabelDomain, LabelFlowID, LabelSession, LabelFormat, LabelMediaType}
}

// labelNameRE is Prometheus' label grammar, which is looser than [nameRE] — this validates
// names supplied by someone else rather than names this project constructs.
var labelNameRE = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]*$`)

// ValidLabelName reports whether name may be used as a Prometheus label name.
//
// This exists for *user* labels (§9.1), which ride in on a request and are the one part of a
// metric's label set this project does not choose. An invalid name does not degrade a metric,
// it invalidates it at collection time and takes its family down with it, so the answer to one
// is to drop it. Names beginning `__` are reserved for Prometheus' own use.
func ValidLabelName(name string) bool {
	return labelNameRE.MatchString(name) && !strings.HasPrefix(name, "__")
}
